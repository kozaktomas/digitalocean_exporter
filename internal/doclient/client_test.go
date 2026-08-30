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
