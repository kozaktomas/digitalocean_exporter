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
# HELP digitalocean_exporter_api_requests_total Total DigitalOcean API requests by resource and response status.
# TYPE digitalocean_exporter_api_requests_total counter
digitalocean_exporter_api_requests_total{resource="account",status="200"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"digitalocean_exporter_api_requests_total"); err != nil {
		t.Errorf("request counter: %v", err)
	}

	if got := testutil.ToFloat64(metrics.RateLimit); got != 4711 {
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

	if got := testutil.ToFloat64(metrics.Requests.WithLabelValues("account", "401")); got != 1 {
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
# HELP digitalocean_exporter_api_requests_total Total DigitalOcean API requests by resource and response status.
# TYPE digitalocean_exporter_api_requests_total counter
digitalocean_exporter_api_requests_total{resource="account",status="200"} 1
digitalocean_exporter_api_requests_total{resource="account",status="429"} 1
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
# HELP digitalocean_exporter_api_requests_total Total DigitalOcean API requests by resource and response status.
# TYPE digitalocean_exporter_api_requests_total counter
digitalocean_exporter_api_requests_total{resource="account",status="200"} 1
digitalocean_exporter_api_requests_total{resource="account",status="503"} 1
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
	if got := testutil.ToFloat64(metrics.RateLimit); got != 0 {
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
# HELP digitalocean_exporter_api_requests_total Total DigitalOcean API requests by resource and response status.
# TYPE digitalocean_exporter_api_requests_total counter
digitalocean_exporter_api_requests_total{resource="account",status="200"} 1
digitalocean_exporter_api_requests_total{resource="account",status="error"} 1
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
