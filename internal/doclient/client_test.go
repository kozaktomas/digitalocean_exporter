package doclient_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitalocean/godo"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kozaktomas/digitalocean_exporter/internal/doclient"
)

func TestClientAuthenticatesAndRecordsMetrics(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("RateLimit-Remaining", "4711")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"account":{"status":"active"}}`))
	}))
	defer srv.Close()

	reg := prometheus.NewRegistry()
	metrics := doclient.NewMetrics(reg)

	client, err := doclient.New(doclient.Config{
		Token: "secret-token", BaseURL: srv.URL + "/", UserAgent: "test-agent",
		Timeout: 5 * time.Second, Metrics: metrics,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if _, _, err := client.Account.Get(context.Background()); err != nil {
		t.Fatalf("account get: %v", err)
	}

	if gotAuth != "Bearer secret-token" {
		t.Errorf("Authorization = %q, want Bearer secret-token", gotAuth)
	}

	expected := `
# HELP digitalocean_exporter_api_requests_total Total DigitalOcean API requests by collector, resource and status.
# TYPE digitalocean_exporter_api_requests_total counter
digitalocean_exporter_api_requests_total{collector="none",resource="account",status="200"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"digitalocean_exporter_api_requests_total"); err != nil {
		t.Errorf("request counter: %v", err)
	}

	if got := testutil.ToFloat64(metrics.Remaining); got != 4711 {
		t.Errorf("rate limit remaining = %v, want 4711", got)
	}
}

func TestClientLabelsErrorsByStatus(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"id":"unauthorized","message":"Unable to authenticate you"}`))
	}))
	defer srv.Close()

	reg := prometheus.NewRegistry()
	metrics := doclient.NewMetrics(reg)
	client, err := doclient.New(doclient.Config{
		Token: "bad", BaseURL: srv.URL + "/", UserAgent: "test-agent",
		Timeout: 5 * time.Second, Metrics: metrics,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if _, _, err := client.Account.Get(context.Background()); err == nil {
		t.Fatal("expected an error from a 401 response")
	}

	if got := testutil.ToFloat64(metrics.Requests.WithLabelValues("none", "account", "401")); got != 1 {
		t.Errorf("401 counter = %v, want 1", got)
	}

	// A 401 is the client's own fault and will be a 401 again a second later.
	if got := requests.Load(); got != 1 {
		t.Errorf("requests = %d, want a single attempt with no retry", got)
	}
}

// A burst rejection is temporary: the window it names reopens, and the retry
// is what turns a rejected refresh into a slow one rather than a failed one.
func TestClientRetriesAfterABurstRejection(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			// Zero seconds keeps the test quick; the parsing is the same.
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"id":"too_many_requests","message":"API Rate limit exceeded."}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"account":{"status":"active"}}`))
	}))
	defer srv.Close()

	reg := prometheus.NewRegistry()
	client := newTestClient(t, srv.URL, reg, 0)
	if _, _, err := client.Account.Get(context.Background()); err != nil {
		t.Fatalf("account get: %v", err)
	}

	if got := requests.Load(); got != 2 {
		t.Errorf("requests = %d, want the rejection and one retry", got)
	}

	// Both attempts are counted: a retried request still spent from the budget.
	expected := `
# HELP digitalocean_exporter_api_requests_total Total DigitalOcean API requests by collector, resource and status.
# TYPE digitalocean_exporter_api_requests_total counter
digitalocean_exporter_api_requests_total{collector="none",resource="account",status="200"} 1
digitalocean_exporter_api_requests_total{collector="none",resource="account",status="429"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"digitalocean_exporter_api_requests_total"); err != nil {
		t.Errorf("request counter: %v", err)
	}
}

func TestClientStopsRetryingAtMaxAttempts(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"id":"too_many_requests","message":"API Rate limit exceeded."}`))
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL, prometheus.NewRegistry(), 3)
	if _, _, err := client.Account.Get(context.Background()); err == nil {
		t.Fatal("expected an error once the attempts ran out")
	}
	if got := requests.Load(); got != 3 {
		t.Errorf("requests = %d, want 3 attempts", got)
	}
}

// The client-side limit is what keeps a refresh from spending its whole
// allowance in one burst, whatever the collectors ask for.
func TestClientRateLimitPacesRequests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"account":{"status":"active"}}`))
	}))
	defer srv.Close()

	reg := prometheus.NewRegistry()
	client, err := doclient.New(doclient.Config{
		Token: "token", BaseURL: srv.URL + "/", UserAgent: "test-agent",
		Timeout: 5 * time.Second, RateLimit: 20, Metrics: doclient.NewMetrics(reg),
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	// Twenty a second is one every 50ms, and the first goes out immediately,
	// so three requests take at least two of those gaps.
	start := time.Now()
	for range 3 {
		if _, _, err := client.Account.Get(context.Background()); err != nil {
			t.Fatalf("account get: %v", err)
		}
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Errorf("three requests took %v, want at least 100ms of pacing", elapsed)
	}
}

// Zero is the documented off switch, and it has to be the whole of it: a
// limiter built at zero requests a second would let the first request through
// and hold every later one forever, which is a stub API the exporter can no
// longer read rather than one it reads unpaced.
func TestClientRateLimitZeroDoesNotPace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"account":{"status":"active"}}`))
	}))
	defer srv.Close()

	client, err := doclient.New(doclient.Config{
		Token: "token", BaseURL: srv.URL + "/", UserAgent: "test-agent",
		Timeout: 5 * time.Second, RateLimit: 0, Metrics: doclient.NewMetrics(prometheus.NewRegistry()),
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	start := time.Now()
	for range 3 {
		if _, _, err := client.Account.Get(context.Background()); err != nil {
			t.Fatalf("account get: %v", err)
		}
	}
	// Three loopback requests are the work of milliseconds. Any pacing worth
	// the name would show up well inside this.
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("three unpaced requests took %v, want them not to queue at all", elapsed)
	}
}

// newTestClient builds a client against srv with retries left on.
func newTestClient(t *testing.T, baseURL string, reg prometheus.Registerer, attempts int) *godo.Client {
	t.Helper()
	client, err := doclient.New(doclient.Config{
		Token: "token", BaseURL: baseURL + "/", UserAgent: "test-agent",
		Timeout: 5 * time.Second, MaxAttempts: attempts, Metrics: doclient.NewMetrics(reg),
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return client
}

// A 5xx is the API having a bad moment rather than an answer, and the retry
// is what keeps it out of the collector's error path.
func TestClientRetriesAServerError(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"account":{"status":"active"}}`))
	}))
	defer srv.Close()

	reg := prometheus.NewRegistry()
	client := newTestClient(t, srv.URL, reg, 0)
	if _, _, err := client.Account.Get(context.Background()); err != nil {
		t.Fatalf("account get: %v", err)
	}

	if got := requests.Load(); got != 2 {
		t.Errorf("requests = %d, want the failure and one retry", got)
	}

	expected := `
# HELP digitalocean_exporter_api_requests_total Total DigitalOcean API requests by collector, resource and status.
# TYPE digitalocean_exporter_api_requests_total counter
digitalocean_exporter_api_requests_total{collector="none",resource="account",status="200"} 1
digitalocean_exporter_api_requests_total{collector="none",resource="account",status="503"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"digitalocean_exporter_api_requests_total"); err != nil {
		t.Errorf("request counter: %v", err)
	}
}

// Retrying sooner than the API asked for is a rejection bought with a request
// from the hourly budget. If the wait does not fit the caller's deadline, the
// rejection is handed back instead, at once.
func TestClientDoesNotRetryPastTheDeadline(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Retry-After", "45")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"id":"too_many_requests","message":"API Rate limit exceeded."}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	client := newTestClient(t, srv.URL, prometheus.NewRegistry(), 0)
	start := time.Now()
	if _, _, err := client.Account.Get(ctx); err == nil {
		t.Fatal("expected the rejection to reach the caller")
	}

	if got := requests.Load(); got != 1 {
		t.Errorf("requests = %d, want a single attempt", got)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("the call took %v, want it to fail fast rather than wait", elapsed)
	}
}

// The hourly limit sends no Retry-After and reports nothing left. Nothing
// frees that up before the hour turns, so a retry only spends an attempt.
func TestClientDoesNotRetryTheHourlyLimit(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"id":"too_many_requests","message":"API Rate limit exceeded."}`))
	}))
	defer srv.Close()

	reg := prometheus.NewRegistry()
	metrics := doclient.NewMetrics(reg)
	client, err := doclient.New(doclient.Config{
		Token: "token", BaseURL: srv.URL + "/", UserAgent: "test-agent",
		Timeout: 5 * time.Second, Metrics: metrics,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if _, _, err := client.Account.Get(context.Background()); err == nil {
		t.Fatal("expected the rejection to reach the caller")
	}

	if got := requests.Load(); got != 1 {
		t.Errorf("requests = %d, want a single attempt", got)
	}
	if got := testutil.ToFloat64(metrics.Remaining); got != 0 {
		t.Errorf("rate limit remaining = %v, want 0", got)
	}
}

// A connection that dies mid-request is not an answer from the API. Go's own
// transport replays only a reused idle connection, so the retry has to happen
// here, and it is counted like any other attempt.
func TestClientRetriesATransportError(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			// Closing without a response is the shape of a reset connection.
			_ = conn.Close()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"account":{"status":"active"}}`))
	}))
	defer srv.Close()

	reg := prometheus.NewRegistry()
	client := newTestClient(t, srv.URL, reg, 0)
	if _, _, err := client.Account.Get(context.Background()); err != nil {
		t.Fatalf("account get: %v", err)
	}

	if got := requests.Load(); got != 2 {
		t.Errorf("requests = %d, want the broken connection and one retry", got)
	}

	expected := `
# HELP digitalocean_exporter_api_requests_total Total DigitalOcean API requests by collector, resource and status.
# TYPE digitalocean_exporter_api_requests_total counter
digitalocean_exporter_api_requests_total{collector="none",resource="account",status="200"} 1
digitalocean_exporter_api_requests_total{collector="none",resource="account",status="error"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"digitalocean_exporter_api_requests_total"); err != nil {
		t.Errorf("request counter: %v", err)
	}
}

// Every request the client makes records itself, so a client built without
// metrics would panic on the first one.
func TestNewRequiresMetrics(t *testing.T) {
	if _, err := doclient.New(doclient.Config{Token: "token", UserAgent: "test-agent"}); err == nil {
		t.Fatal("expected an error from a client without metrics")
	}
}

// The transport cannot see which collector built a request, so the name comes
// down the context the scheduler starts the refresh with. Without it the whole
// counter reads as one caller, and the per-collector request cost the
// documentation quotes cannot be checked against the exporter itself.
func TestClientAttributesRequestsToTheRefreshingCollector(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"account":{"status":"active"}}`))
	}))
	defer srv.Close()

	reg := prometheus.NewRegistry()
	client := newTestClient(t, srv.URL, reg, 0)

	if _, _, err := client.Account.Get(doclient.WithCollector(context.Background(), "account")); err != nil {
		t.Fatalf("account get: %v", err)
	}
	// A call made outside any refresh still has to land somewhere.
	if _, _, err := client.Account.Get(context.Background()); err != nil {
		t.Fatalf("account get: %v", err)
	}

	expected := `
# HELP digitalocean_exporter_api_requests_total Total DigitalOcean API requests by collector, resource and status.
# TYPE digitalocean_exporter_api_requests_total counter
digitalocean_exporter_api_requests_total{collector="account",resource="account",status="200"} 1
digitalocean_exporter_api_requests_total{collector="none",resource="account",status="200"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"digitalocean_exporter_api_requests_total"); err != nil {
		t.Errorf("request counter: %v", err)
	}
}

// Every request is timed as well as counted, so a collector that is slow
// because the API is slow can be told from one that is slow because it makes
// hundreds of calls.
func TestClientTimesRequestsPerCollector(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"account":{"status":"active"}}`))
	}))
	defer srv.Close()

	reg := prometheus.NewRegistry()
	metrics := doclient.NewMetrics(reg)
	client, err := doclient.New(doclient.Config{
		Token: "token", BaseURL: srv.URL + "/", UserAgent: "test-agent",
		Timeout: 5 * time.Second, Metrics: metrics,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if _, _, err := client.Account.Get(doclient.WithCollector(context.Background(), "account")); err != nil {
		t.Fatalf("account get: %v", err)
	}

	if got := testutil.CollectAndCount(metrics.Duration); got != 1 {
		t.Errorf("duration series = %d, want one for the collector that made the request", got)
	}
	if got := histogramCount(t, reg, "digitalocean_exporter_api_request_duration_seconds"); got != 1 {
		t.Errorf("observations = %d, want the one request", got)
	}
}

// histogramCount returns how many observations the named histogram holds, and
// fails unless it holds exactly one series, labelled for the account collector.
func histogramCount(t *testing.T, gatherer prometheus.Gatherer, name string) uint64 {
	t.Helper()

	families, err := gatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		if len(family.GetMetric()) != 1 {
			t.Fatalf("%s has %d series, want one", name, len(family.GetMetric()))
		}
		metric := family.GetMetric()[0]
		labels := make(map[string]string, len(metric.GetLabel()))
		for _, label := range metric.GetLabel() {
			labels[label.GetName()] = label.GetValue()
		}
		if labels["collector"] != "account" || labels["resource"] != "account" {
			t.Errorf("labels = %v, want the account collector on the account resource", labels)
		}
		return metric.GetHistogram().GetSampleCount()
	}
	t.Fatalf("%s was not gathered", name)
	return 0
}

// DigitalOcean reports the ceiling and the moment the window reopens next to
// what is left of it, and both arrive in headers whose case is not guaranteed.
func TestClientRecordsTheWholeRateLimitPicture(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Written through the map rather than Set so the names reach the wire
		// exactly as spelled here: the client canonicalises what it parses,
		// which is what makes the lookup case-insensitive.
		w.Header()["ratelimit-limit"] = []string{"5000"}
		w.Header()["ratelimit-remaining"] = []string{"4711"}
		w.Header()["ratelimit-reset"] = []string{"1756742400"}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"account":{"status":"active"}}`))
	}))
	defer srv.Close()

	metrics := doclient.NewMetrics(prometheus.NewRegistry())
	client, err := doclient.New(doclient.Config{
		Token: "token", BaseURL: srv.URL + "/", UserAgent: "test-agent",
		Timeout: 5 * time.Second, Metrics: metrics,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if _, _, err := client.Account.Get(context.Background()); err != nil {
		t.Fatalf("account get: %v", err)
	}

	for _, gauge := range []struct {
		name  string
		value prometheus.Gauge
		want  float64
	}{
		{"remaining", metrics.Remaining, 4711},
		{"limit", metrics.Limit, 5000},
		{"reset", metrics.Reset, 1756742400},
	} {
		if got := testutil.ToFloat64(gauge.value); got != gauge.want {
			t.Errorf("%s = %v, want %v", gauge.name, got, gauge.want)
		}
	}
}

// A response that carries no rate-limit headers at all says nothing about the
// budget, and must not be read as one that has just run out.
func TestClientKeepsTheRateLimitGaugesWithoutHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"account":{"status":"active"}}`))
	}))
	defer srv.Close()

	metrics := doclient.NewMetrics(prometheus.NewRegistry())
	metrics.Remaining.Set(4711)
	metrics.Limit.Set(5000)
	metrics.Reset.Set(1756742400)

	client, err := doclient.New(doclient.Config{
		Token: "token", BaseURL: srv.URL + "/", UserAgent: "test-agent",
		Timeout: 5 * time.Second, Metrics: metrics,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if _, _, err := client.Account.Get(context.Background()); err != nil {
		t.Fatalf("account get: %v", err)
	}

	if got := testutil.ToFloat64(metrics.Remaining); got != 4711 {
		t.Errorf("remaining = %v, want the previous 4711", got)
	}
	if got := testutil.ToFloat64(metrics.Limit); got != 5000 {
		t.Errorf("limit = %v, want the previous 5000", got)
	}
	if got := testutil.ToFloat64(metrics.Reset); got != 1756742400 {
		t.Errorf("reset = %v, want the previous 1756742400", got)
	}
}
