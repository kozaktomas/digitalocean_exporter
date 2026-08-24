package doclient_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

	client, err := doclient.New("secret-token", srv.URL+"/", "test-agent", 5*time.Second, metrics)
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"id":"unauthorized","message":"Unable to authenticate you"}`))
	}))
	defer srv.Close()

	reg := prometheus.NewRegistry()
	metrics := doclient.NewMetrics(reg)
	client, err := doclient.New("bad", srv.URL+"/", "test-agent", 5*time.Second, metrics)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if _, _, err := client.Account.Get(context.Background()); err == nil {
		t.Fatal("expected an error from a 401 response")
	}

	if got := testutil.ToFloat64(metrics.Requests.WithLabelValues("account", "401")); got != 1 {
		t.Errorf("401 counter = %v, want 1", got)
	}
}
