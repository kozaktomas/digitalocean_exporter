package cdn_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kozaktomas/digitalocean_exporter/internal/collector/cdn"
	"github.com/kozaktomas/digitalocean_exporter/internal/doclient"
)

// Two endpoints: one behind a custom domain with a certificate, and a bare one
// served from the DigitalOcean domain, which has neither.
const endpointsJSON = `{"endpoints":[` +
	`{"id":"cdn-1","origin":"assets.fra1.digitaloceanspaces.com",` +
	`"endpoint":"assets.fra1.cdn.digitaloceanspaces.com","ttl":3600,` +
	`"custom_domain":"cdn.example.com","certificate_id":"cert-1"},` +
	`{"id":"cdn-2","origin":"backup.fra1.digitaloceanspaces.com",` +
	`"endpoint":"backup.fra1.cdn.digitaloceanspaces.com","ttl":600}` +
	`],"meta":{"total":2}}`

const endpointMetrics = `
# HELP digitalocean_cdn_endpoint_info Always 1. Its labels describe the endpoint's custom domain and certificate.
# TYPE digitalocean_cdn_endpoint_info gauge
` +
	`digitalocean_cdn_endpoint_info{certificate_id="cert-1",custom_domain="cdn.example.com",` +
	`endpoint="assets.fra1.cdn.digitaloceanspaces.com",id="cdn-1",` +
	`origin="assets.fra1.digitaloceanspaces.com"} 1` + "\n" +
	`digitalocean_cdn_endpoint_info{certificate_id="",custom_domain="",` +
	`endpoint="backup.fra1.cdn.digitaloceanspaces.com",id="cdn-2",` +
	`origin="backup.fra1.digitaloceanspaces.com"} 1` + "\n" +
	`# HELP digitalocean_cdn_endpoint_ttl_seconds Cache time-to-live of the CDN endpoint in seconds.
# TYPE digitalocean_cdn_endpoint_ttl_seconds gauge
` +
	`digitalocean_cdn_endpoint_ttl_seconds{endpoint="assets.fra1.cdn.digitaloceanspaces.com",` +
	`id="cdn-1",origin="assets.fra1.digitaloceanspaces.com"} 3600` + "\n" +
	`digitalocean_cdn_endpoint_ttl_seconds{endpoint="backup.fra1.cdn.digitaloceanspaces.com",` +
	`id="cdn-2",origin="backup.fra1.digitaloceanspaces.com"} 600` + "\n"

// newTestCollector wires a collector to a fake DigitalOcean API.
func newTestCollector(t *testing.T, handler http.HandlerFunc) *cdn.Collector {
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
	return cdn.New(client, nil)
}

// okHandler serves the two-endpoint account.
func okHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/v2/cdn/endpoints" {
		_, _ = w.Write([]byte(endpointsJSON))
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func TestCollectAfterRefresh(t *testing.T) {
	c := newTestCollector(t, okHandler)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if err := testutil.CollectAndCompare(c, strings.NewReader(endpointMetrics)); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// An account with more endpoints than fit on one page is paginated by page
// number, and every page has to reach the snapshot.
func TestRefreshFollowsPages(t *testing.T) {
	page := func(id string, ttl int, next bool) string {
		links := `"links":{}`
		if next {
			links = `"links":{"pages":{"next":"https://api.digitalocean.com/v2/cdn/endpoints?page=2"}}`
		}
		return fmt.Sprintf(`{"endpoints":[{"id":%q,"origin":"o","endpoint":"e","ttl":%d}],`+
			`%s,"meta":{"total":2}}`, id, ttl, links)
	}

	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write([]byte(page("cdn-2", 60, false)))
			return
		}
		_, _ = w.Write([]byte(page("cdn-1", 3600, true)))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	const want = `
# HELP digitalocean_cdn_endpoint_ttl_seconds Cache time-to-live of the CDN endpoint in seconds.
# TYPE digitalocean_cdn_endpoint_ttl_seconds gauge
digitalocean_cdn_endpoint_ttl_seconds{endpoint="e",id="cdn-1",origin="o"} 3600
digitalocean_cdn_endpoint_ttl_seconds{endpoint="e",id="cdn-2",origin="o"} 60
`
	const metric = "digitalocean_cdn_endpoint_ttl_seconds"
	if err := testutil.CollectAndCompare(c, strings.NewReader(want), metric); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// An account with no CDN endpoints is a normal state: the refresh succeeds and
// there is simply nothing to report.
func TestRefreshWithoutEndpointsSucceeds(t *testing.T) {
	c := newTestCollector(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"endpoints":[],"meta":{"total":0}}`))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh without endpoints: %v", err)
	}
	if got := testutil.CollectAndCount(c); got != 0 {
		t.Errorf("metric count without endpoints = %d, want 0", got)
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

	if err := testutil.CollectAndCompare(c, strings.NewReader(endpointMetrics)); err != nil {
		t.Errorf("unexpected metrics after a failed refresh: %v", err)
	}
}

func TestName(t *testing.T) {
	c := newTestCollector(t, okHandler)
	if got := c.Name(); got != "cdn" {
		t.Errorf("Name() = %q, want %q", got, "cdn")
	}
}

func TestDescribeCoversEveryMetric(t *testing.T) {
	c := newTestCollector(t, okHandler)

	ch := make(chan *prometheus.Desc, 16)
	c.Describe(ch)
	close(ch)

	var count int
	for range ch {
		count++
	}
	if want := 2; count != want {
		t.Errorf("Describe sent %d descriptors, want %d", count, want)
	}
}

// The list can shift between two page requests — a resource created or
// destroyed while the pages are being read — and the same endpoint then arrives
// on both. It has to reach the snapshot once: two entries would be two series
// with identical labels, which fails the whole scrape rather than one metric.
func TestRefreshDropsADuplicateEndpointOnTwoPages(t *testing.T) {
	page := func(next bool) string {
		links := `"links":{}`
		if next {
			links = `"links":{"pages":{"next":"https://api.digitalocean.com/v2/cdn/endpoints?page=2"}}`
		}
		return fmt.Sprintf(`{"endpoints":[{"id":"cdn-1","origin":"o","endpoint":"e","ttl":3600}],`+
			`%s,"meta":{"total":1}}`, links)
	}

	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(page(r.URL.Query().Get("page") != "2")))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	const want = `
# HELP digitalocean_cdn_endpoint_ttl_seconds Cache time-to-live of the CDN endpoint in seconds.
# TYPE digitalocean_cdn_endpoint_ttl_seconds gauge
digitalocean_cdn_endpoint_ttl_seconds{endpoint="e",id="cdn-1",origin="o"} 3600
`
	const metric = "digitalocean_cdn_endpoint_ttl_seconds"
	if err := testutil.CollectAndCompare(c, strings.NewReader(want), metric); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}
