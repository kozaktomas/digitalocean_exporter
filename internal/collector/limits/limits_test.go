package limits_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kozaktomas/digitalocean_exporter/internal/collector/limits"
	"github.com/kozaktomas/digitalocean_exporter/internal/doclient"
)

// The API answers a list request with the page asked for and the total in
// meta, which is the only figure this collector reads.
const dropletsJSON = `{"droplets":[{"id":1,"name":"web-1","status":"active"}],"meta":{"total":5}}`
const reservedIPsJSON = `{"reserved_ips":[],"meta":{"total":0}}`
const volumesJSON = `{"volumes":[{"id":"vol","name":"data"}],"meta":{"total":13}}`

const limitsMetrics = `
# HELP digitalocean_account_droplets Number of droplets on the account.
# TYPE digitalocean_account_droplets gauge
digitalocean_account_droplets 5
# HELP digitalocean_account_reserved_ips Number of reserved IP addresses on the account.
# TYPE digitalocean_account_reserved_ips gauge
digitalocean_account_reserved_ips 0
# HELP digitalocean_account_volumes Number of block storage volumes on the account.
# TYPE digitalocean_account_volumes gauge
digitalocean_account_volumes 13
`

// newTestCollector wires a collector to a fake DigitalOcean API.
func newTestCollector(t *testing.T, handler http.HandlerFunc) *limits.Collector {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	client, err := doclient.New(doclient.Config{
		Token: "token", BaseURL: srv.URL + "/", UserAgent: "test", Timeout: 5 * time.Second,
		// One attempt: retrying a stubbed failure only makes this test sit
		// through the backoff, and the retries have their own test in doclient.
		MaxAttempts: 1, Metrics: doclient.NewMetrics(prometheus.NewRegistry()),
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return limits.New(client)
}

// okHandler serves the three list endpoints one refresh counts.
func okHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/v2/droplets":
		_, _ = w.Write([]byte(dropletsJSON))
	case "/v2/reserved_ips":
		_, _ = w.Write([]byte(reservedIPsJSON))
	case "/v2/volumes":
		_, _ = w.Write([]byte(volumesJSON))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestCollectAfterRefresh(t *testing.T) {
	c := newTestCollector(t, okHandler)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if err := testutil.CollectAndCompare(c, strings.NewReader(limitsMetrics)); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// The counts come from meta.total, so the collector must ask for a single item
// per page: downloading a whole inventory of droplets and volumes every few
// minutes to arrive at three numbers would be the wrong trade entirely.
func TestRefreshDoesNotDownloadTheInventory(t *testing.T) {
	var (
		mu    sync.Mutex
		pages []string
	)
	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		pages = append(pages, r.URL.Path+"?per_page="+r.URL.Query().Get("per_page"))
		mu.Unlock()
		okHandler(w, r)
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{
		"/v2/droplets?per_page=1",
		"/v2/reserved_ips?per_page=1",
		"/v2/volumes?per_page=1",
	}
	if len(pages) != len(want) {
		t.Fatalf("requests = %v, want %v", pages, want)
	}
	for _, expected := range want {
		var found bool
		for _, got := range pages {
			if got == expected {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("request %q missing from %v", expected, pages)
		}
	}
}

// Without meta.total there is no count to report. Falling back to the length of
// the page would report 1 droplet for an account that runs a hundred.
func TestRefreshFailsWithoutTotal(t *testing.T) {
	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v2/droplets" {
			_, _ = w.Write([]byte(`{"droplets":[{"id":1,"name":"web-1"}]}`))
			return
		}
		okHandler(w, r)
	})

	if err := c.Refresh(context.Background()); err == nil {
		t.Fatal("expected a response without meta.total to fail the refresh")
	}
	if got := testutil.CollectAndCount(c); got != 0 {
		t.Errorf("metric count after a failed first refresh = %d, want 0", got)
	}
}

func TestCollectBeforeRefreshEmitsNothing(t *testing.T) {
	c := newTestCollector(t, okHandler)
	if got := testutil.CollectAndCount(c); got != 0 {
		t.Errorf("metric count before the first refresh = %d, want 0", got)
	}
}

func TestFailedRefreshKeepsPreviousSnapshot(t *testing.T) {
	var fail bool
	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		okHandler(w, r)
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}

	fail = true
	if err := c.Refresh(context.Background()); err == nil {
		t.Fatal("expected the second refresh to fail")
	}

	if err := testutil.CollectAndCompare(c, strings.NewReader(limitsMetrics)); err != nil {
		t.Errorf("unexpected metrics after a failed refresh: %v", err)
	}
}

func TestName(t *testing.T) {
	c := newTestCollector(t, okHandler)
	if got := c.Name(); got != "limits" {
		t.Errorf("Name() = %q, want %q", got, "limits")
	}
}

func TestDescribeCoversEveryMetric(t *testing.T) {
	c := newTestCollector(t, okHandler)

	ch := make(chan *prometheus.Desc, 8)
	c.Describe(ch)
	close(ch)

	var count int
	for range ch {
		count++
	}
	if want := 3; count != want {
		t.Errorf("Describe sent %d descriptors, want %d", count, want)
	}
}
