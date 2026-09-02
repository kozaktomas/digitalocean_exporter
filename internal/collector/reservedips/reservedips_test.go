package reservedips_test

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

	"github.com/kozaktomas/digitalocean_exporter/internal/collector/reservedips"
	"github.com/kozaktomas/digitalocean_exporter/internal/doclient"
)

// Two IPv4 addresses in one region: one serving a droplet, one idle. The idle
// one is the whole point of the collector — DigitalOcean bills it.
const reservedIPsJSON = `{"reserved_ips":[` +
	`{"ip":"192.0.2.1","region":{"slug":"fra1"},"project_id":"proj-1",` +
	`"droplet":{"id":42,"name":"web-1"}},` +
	`{"ip":"192.0.2.2","region":{"slug":"fra1"},"project_id":"proj-1","droplet":null}` +
	`],"links":{},"meta":{"total":2}}`

// One IPv6 address, assigned. The IPv6 listing names its region as a slug and
// reports no project at all.
const reservedIPv6JSON = `{"reserved_ipv6s":[` +
	`{"ip":"2604:a880::1","region_slug":"nyc3","reserved_at":"2026-08-01T10:00:00Z",` +
	`"droplet":{"id":7,"name":"edge-1"}}` +
	`],"links":{},"meta":{"total":1}}`

// The expected exposition. The info samples are split across source lines
// because one of them is a single line longer than this file's limit; the
// pieces concatenate back into exactly what Prometheus is handed.
const reservedIPMetrics = `
# HELP digitalocean_reserved_ip_assigned 1 when the reserved IP is assigned to a droplet, 0 when it is idle.
# TYPE digitalocean_reserved_ip_assigned gauge
digitalocean_reserved_ip_assigned{ip="192.0.2.1",region="fra1",version="4"} 1
digitalocean_reserved_ip_assigned{ip="192.0.2.2",region="fra1",version="4"} 0
digitalocean_reserved_ip_assigned{ip="2604:a880::1",region="nyc3",version="6"} 1
# HELP digitalocean_reserved_ip_info Always 1. Its labels name the droplet the address serves and its project.
# TYPE digitalocean_reserved_ip_info gauge
` +
	`digitalocean_reserved_ip_info{droplet_id="42",droplet_name="web-1",` +
	`ip="192.0.2.1",project_id="proj-1",region="fra1",version="4"} 1` + "\n" +
	`digitalocean_reserved_ip_info{droplet_id="",droplet_name="",` +
	`ip="192.0.2.2",project_id="proj-1",region="fra1",version="4"} 1` + "\n" +
	`digitalocean_reserved_ip_info{droplet_id="7",droplet_name="edge-1",` +
	`ip="2604:a880::1",project_id="",region="nyc3",version="6"} 1` + "\n" + `
# HELP digitalocean_reserved_ips Number of reserved IP addresses in this region of this IP version.
# TYPE digitalocean_reserved_ips gauge
digitalocean_reserved_ips{region="fra1",version="4"} 2
digitalocean_reserved_ips{region="nyc3",version="6"} 1
`

// newTestCollector wires a collector to a fake DigitalOcean API.
func newTestCollector(t *testing.T, handler http.HandlerFunc) *reservedips.Collector {
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
	return reservedips.New(client, nil)
}

// okHandler serves the three-address account.
func okHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/v2/reserved_ips":
		_, _ = w.Write([]byte(reservedIPsJSON))
	case "/v2/reserved_ipv6":
		_, _ = w.Write([]byte(reservedIPv6JSON))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestCollectAfterRefresh(t *testing.T) {
	c := newTestCollector(t, okHandler)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if err := testutil.CollectAndCompare(c, strings.NewReader(reservedIPMetrics)); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// An account with more reserved IPs than fit on one page is paginated by page
// number, and every page has to reach the snapshot. Both listings page.
func TestRefreshFollowsPages(t *testing.T) {
	v4 := func(ip string, next bool) string {
		links := `"links":{}`
		if next {
			links = `"links":{"pages":{"next":"https://api.digitalocean.com/v2/reserved_ips?page=2"}}`
		}
		return fmt.Sprintf(`{"reserved_ips":[{"ip":%q,"region":{"slug":"fra1"},`+
			`"project_id":"proj-1","droplet":null}],%s,"meta":{"total":2}}`, ip, links)
	}
	v6 := func(ip string, next bool) string {
		links := `"links":{}`
		if next {
			links = `"links":{"pages":{"next":"https://api.digitalocean.com/v2/reserved_ipv6?page=2"}}`
		}
		return fmt.Sprintf(`{"reserved_ipv6s":[{"ip":%q,"region_slug":"nyc3"}],%s,"meta":{"total":2}}`,
			ip, links)
	}

	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		second := r.URL.Query().Get("page") == "2"
		switch r.URL.Path {
		case "/v2/reserved_ips":
			if second {
				_, _ = w.Write([]byte(v4("192.0.2.2", false)))
				return
			}
			_, _ = w.Write([]byte(v4("192.0.2.1", true)))
		case "/v2/reserved_ipv6":
			if second {
				_, _ = w.Write([]byte(v6("2604:a880::2", false)))
				return
			}
			_, _ = w.Write([]byte(v6("2604:a880::1", true)))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	const want = `
# HELP digitalocean_reserved_ips Number of reserved IP addresses in this region of this IP version.
# TYPE digitalocean_reserved_ips gauge
digitalocean_reserved_ips{region="fra1",version="4"} 2
digitalocean_reserved_ips{region="nyc3",version="6"} 2
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want), "digitalocean_reserved_ips"); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// An account with no reserved IPs is a normal state: the refresh succeeds and
// there is simply nothing to report.
func TestRefreshWithoutReservedIPsSucceeds(t *testing.T) {
	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v2/reserved_ipv6" {
			_, _ = w.Write([]byte(`{"reserved_ipv6s":[],"links":{},"meta":{"total":0}}`))
			return
		}
		_, _ = w.Write([]byte(`{"reserved_ips":[],"links":{},"meta":{"total":0}}`))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh without reserved IPs: %v", err)
	}
	if got := testutil.CollectAndCount(c); got != 0 {
		t.Errorf("metric count without reserved IPs = %d, want 0", got)
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

	if err := testutil.CollectAndCompare(c, strings.NewReader(reservedIPMetrics)); err != nil {
		t.Errorf("unexpected metrics after a failed refresh: %v", err)
	}
}

// The IPv6 listing is read after the IPv4 one, and a failure there must fail
// the whole refresh: a snapshot holding only half the addresses would report
// the other half as having been released.
func TestFailedIPv6ListingFailsTheRefresh(t *testing.T) {
	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/reserved_ipv6" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		okHandler(w, r)
	})

	if err := c.Refresh(context.Background()); err == nil {
		t.Fatal("expected the refresh to fail")
	}
	if got := testutil.CollectAndCount(c); got != 0 {
		t.Errorf("metric count after a failed first refresh = %d, want 0", got)
	}
}

func TestName(t *testing.T) {
	c := newTestCollector(t, okHandler)
	if got := c.Name(); got != "reservedips" {
		t.Errorf("Name() = %q, want %q", got, "reservedips")
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
	if want := 3; count != want {
		t.Errorf("Describe sent %d descriptors, want %d", count, want)
	}
}

// The list can shift between two page requests — an address reserved or
// released while the pages are being read — and the same one then arrives on
// both. It has to reach the snapshot once: two entries would be two series
// with identical labels, which fails the whole scrape rather than one metric.
func TestRefreshDropsADuplicateAddressOnTwoPages(t *testing.T) {
	page := func(next bool) string {
		links := `"links":{}`
		if next {
			links = `"links":{"pages":{"next":"https://api.digitalocean.com/v2/reserved_ips?page=2"}}`
		}
		return fmt.Sprintf(`{"reserved_ips":[{"ip":"192.0.2.1","region":{"slug":"fra1"},`+
			`"project_id":"proj-1","droplet":null}],%s,"meta":{"total":1}}`, links)
	}

	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v2/reserved_ipv6" {
			_, _ = w.Write([]byte(`{"reserved_ipv6s":[],"links":{},"meta":{"total":0}}`))
			return
		}
		_, _ = w.Write([]byte(page(r.URL.Query().Get("page") != "2")))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	const want = `
# HELP digitalocean_reserved_ips Number of reserved IP addresses in this region of this IP version.
# TYPE digitalocean_reserved_ips gauge
digitalocean_reserved_ips{region="fra1",version="4"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want), "digitalocean_reserved_ips"); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}
