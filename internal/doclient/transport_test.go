package doclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// The retries are tested against the transport itself rather than through
// godo, because godo only ever sends a bodiless GET and the guard is about
// everything else.
func newTestTransport(attempts int) *transport {
	return &transport{
		base:        http.DefaultTransport,
		token:       "token",
		metrics:     NewMetrics(prometheus.NewRegistry()),
		maxAttempts: attempts,
	}
}

// A body is consumed by the first attempt, so replaying the request would send
// an empty one. The exporter never makes such a request; the guard is what
// keeps that true.
func TestTransportDoesNotRetryARequestWithABody(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		srv.URL+"/v2/account", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	start := time.Now()
	resp, err := newTestTransport(3).RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	drain(resp)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("requests = %d, want a single attempt", got)
	}
	if elapsed := time.Since(start); elapsed > baseBackoff {
		t.Errorf("the request took %v, want no backoff at all", elapsed)
	}
}

// A collector that is shutting down should not be held by a backoff it will
// never use the far side of.
func TestTransportStopsWaitingWhenTheContextIsCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v2/account", nil)
	if err != nil {
		cancel()
		t.Fatalf("new request: %v", err)
	}

	// The first attempt fails and the transport settles in for a second.
	time.AfterFunc(50*time.Millisecond, cancel)

	start := time.Now()
	resp, err := newTestTransport(3).RoundTrip(req)
	if resp != nil {
		drain(resp)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("round trip error = %v, want a cancelled context", err)
	}
	if elapsed := time.Since(start); elapsed >= baseBackoff {
		t.Errorf("the cancellation took %v to land, want it to be prompt", elapsed)
	}
}
