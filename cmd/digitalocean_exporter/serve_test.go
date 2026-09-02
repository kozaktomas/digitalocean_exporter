package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/kozaktomas/digitalocean_exporter/internal/config"
)

// stubReadiness reports a fixed list of collectors as still waiting.
type stubReadiness struct {
	pending []string
}

// Pending implements readinessReporter.
func (s *stubReadiness) Pending() []string { return s.pending }

// discardLogger is a logger whose output goes nowhere, for tests that care
// about a response rather than about what was written about it.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// get performs a request against handler and returns its status and body.
func get(t *testing.T, handler http.Handler, path string) (int, string) {
	t.Helper()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
	response := recorder.Result()
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read %s body: %v", path, err)
	}
	return response.StatusCode, string(body)
}

// Liveness must not depend on anything a collector does: a pod whose refreshes
// are failing is not helped by a restart, and restarting it throws away every
// snapshot the other collectors still hold.
func TestHealthzIsAlwaysOK(t *testing.T) {
	handler, err := newHandler(prometheus.NewRegistry(), &stubReadiness{pending: []string{"account"}}, discardLogger())
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}

	status, body := get(t, handler, "/healthz")
	if status != http.StatusOK {
		t.Errorf("status = %d, want %d", status, http.StatusOK)
	}
	if body != "ok\n" {
		t.Errorf("body = %q, want %q", body, "ok\n")
	}
}

// Readiness is the opposite: a collector emits nothing before its first
// successful refresh, so a freshly rolled pod must stay out of the Service
// until every one of them has a snapshot — and must name the ones it is
// waiting for, since that is all an operator gets from a probe failure.
func TestReadyzWaitsForEveryCollector(t *testing.T) {
	ready := &stubReadiness{pending: []string{"account", "droplets"}}
	handler, err := newHandler(prometheus.NewRegistry(), ready, discardLogger())
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}

	status, body := get(t, handler, "/readyz")
	if status != http.StatusServiceUnavailable {
		t.Errorf("status before the first refresh = %d, want %d",
			status, http.StatusServiceUnavailable)
	}
	for _, name := range ready.pending {
		if !strings.Contains(body, name) {
			t.Errorf("body %q does not name the waiting collector %q", body, name)
		}
	}

	ready.pending = nil

	status, body = get(t, handler, "/readyz")
	if status != http.StatusOK {
		t.Errorf("status after the first refresh = %d, want %d", status, http.StatusOK)
	}
	if body != "ok\n" {
		t.Errorf("body = %q, want %q", body, "ok\n")
	}
}

// An exporter with every collector switched off has nothing to wait for, and
// answering 503 forever would leave such a pod permanently unready.
func TestReadyzIsOKWithoutCollectors(t *testing.T) {
	handler, err := newHandler(prometheus.NewRegistry(), &stubReadiness{}, discardLogger())
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}

	if status, _ := get(t, handler, "/readyz"); status != http.StatusOK {
		t.Errorf("status = %d, want %d", status, http.StatusOK)
	}
}

// The landing page is where somebody who opened the exporter in a browser
// finds out what it serves, so every endpoint has to be on it.
func TestLandingPageListsEveryEndpoint(t *testing.T) {
	handler, err := newHandler(prometheus.NewRegistry(), &stubReadiness{}, discardLogger())
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}

	status, body := get(t, handler, "/")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	for _, path := range []string{"/metrics", "/healthz", "/readyz"} {
		if !strings.Contains(body, `href="`+path+`"`) {
			t.Errorf("the landing page does not link to %s", path)
		}
	}
}

// brokenCollector reports the same label set twice, which is the shape of bug
// that makes prometheus.Gatherer fail: a collector that builds its labels from
// an API response can meet two resources that share one.
type brokenCollector struct {
	desc *prometheus.Desc
}

// Describe implements prometheus.Collector.
func (c *brokenCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

// Collect implements prometheus.Collector.
func (c *brokenCollector) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, 1, "duplicate")
	ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, 2, "duplicate")
}

// One bad series must cost its own collector and nothing else. With promhttp's
// default handling the whole scrape becomes a 500 and every other collector
// disappears at once — indistinguishable from the exporter itself going away.
func TestMetricsSurvivesOneBrokenCollector(t *testing.T) {
	reg := prometheus.NewRegistry()
	healthy := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "digitalocean_test_healthy", Help: "A metric from a collector that works.",
	})
	healthy.Set(1)
	reg.MustRegister(healthy)
	reg.MustRegister(&brokenCollector{desc: prometheus.NewDesc(
		"digitalocean_test_broken", "A metric collected twice under one label set.", []string{"name"}, nil)})

	logs := &strings.Builder{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	handler, err := newHandler(reg, &stubReadiness{}, logger)
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}

	status, body := get(t, handler, "/metrics")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d: one bad series took the whole scrape with it",
			status, http.StatusOK)
	}
	if !strings.Contains(body, "digitalocean_test_healthy 1") {
		t.Errorf("the healthy metric is missing from the exposition:\n%s", body)
	}
	if !strings.Contains(body, "promhttp_metric_handler_errors_total") {
		t.Errorf("the failure is not countable:\n%s", body)
	}
	if logs.Len() == 0 {
		t.Error("the gathering failure was not logged, so nothing points at the offending collector")
	}
}

// A stopped exporter must actually stop. serve shuts the server down on a
// context of its own, because the one it is given is already cancelled by then
// and reusing it would abort the graceful shutdown instead of bounding it.
func TestServeShutsDownWhenTheContextIsCancelled(t *testing.T) {
	cfg := &config.Config{ListenAddress: freeAddress(t)}
	handler, err := newHandler(prometheus.NewRegistry(), &stubReadiness{}, discardLogger())
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	served := make(chan error, 1)
	go func() { served <- serve(ctx, cfg, handler, discardLogger()) }()

	waitForHealthz(t, cfg.ListenAddress)
	cancel()

	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return within its shutdown timeout")
	}
}

// freeAddress returns a loopback address nothing is listening on. Binding to
// port 0 would be tighter, but the caller has to know the port to reach the
// server before cancelling it.
func freeAddress(t *testing.T) string {
	t.Helper()

	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release the reserved port: %v", err)
	}
	return address
}

// waitForHealthz blocks until the server answers, so the cancellation that
// follows cannot outrun the listener and leave a server running forever.
func waitForHealthz(t *testing.T, address string) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		response, err := http.Get("http://" + address + "/healthz") //nolint:noctx // the deadline is the loop's.
		if err == nil {
			_ = response.Body.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the server never started listening")
}
