package domains_test

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

	"github.com/kozaktomas/digitalocean_exporter/internal/collector/domains"
	"github.com/kozaktomas/digitalocean_exporter/internal/doclient"
)

// Two zones with different default TTLs. The zone file the API returns with
// every domain is included here because it is included in reality, and nothing
// in the collector may start depending on it.
const domainsJSON = `{"domains":[` +
	`{"name":"example.com","ttl":1800,"zone_file":"$ORIGIN example.com.\n$TTL 1800\n"},` +
	`{"name":"example.net","ttl":3600,"zone_file":"$ORIGIN example.net.\n$TTL 3600\n"}` +
	`],"meta":{"total":2}}`

const domainMetrics = `
# HELP digitalocean_domain_ttl_seconds Default time-to-live of the DNS zone in seconds.
# TYPE digitalocean_domain_ttl_seconds gauge
digitalocean_domain_ttl_seconds{domain="example.com"} 1800
digitalocean_domain_ttl_seconds{domain="example.net"} 3600
`

// newTestCollector wires a collector to a fake DigitalOcean API.
func newTestCollector(t *testing.T, handler http.HandlerFunc) *domains.Collector {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	client, err := doclient.New("token", srv.URL+"/", "test", 5*time.Second,
		doclient.NewMetrics(prometheus.NewRegistry()))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return domains.New(client)
}

// okHandler serves the two-zone account.
func okHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/v2/domains" {
		_, _ = w.Write([]byte(domainsJSON))
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func TestCollectAfterRefresh(t *testing.T) {
	c := newTestCollector(t, okHandler)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if err := testutil.CollectAndCompare(c, strings.NewReader(domainMetrics)); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// An account with more zones than fit on one page is paginated by page number,
// and every page has to reach the snapshot.
func TestRefreshFollowsPages(t *testing.T) {
	page := func(name string, ttl int, next bool) string {
		links := `"links":{}`
		if next {
			links = `"links":{"pages":{"next":"https://api.digitalocean.com/v2/domains?page=2"}}`
		}
		return fmt.Sprintf(`{"domains":[{"name":%q,"ttl":%d}],%s,"meta":{"total":2}}`, name, ttl, links)
	}

	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write([]byte(page("second.example", 60, false)))
			return
		}
		_, _ = w.Write([]byte(page("first.example", 1800, true)))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	const want = `
# HELP digitalocean_domain_ttl_seconds Default time-to-live of the DNS zone in seconds.
# TYPE digitalocean_domain_ttl_seconds gauge
digitalocean_domain_ttl_seconds{domain="first.example"} 1800
digitalocean_domain_ttl_seconds{domain="second.example"} 60
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want)); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// An account hosting no zones is a normal state: the refresh succeeds and there
// is simply nothing to report.
func TestRefreshWithoutDomainsSucceeds(t *testing.T) {
	c := newTestCollector(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"domains":[],"meta":{"total":0}}`))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh without domains: %v", err)
	}
	if got := testutil.CollectAndCount(c); got != 0 {
		t.Errorf("metric count without domains = %d, want 0", got)
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

	if err := testutil.CollectAndCompare(c, strings.NewReader(domainMetrics)); err != nil {
		t.Errorf("unexpected metrics after a failed refresh: %v", err)
	}
}

func TestName(t *testing.T) {
	c := newTestCollector(t, okHandler)
	if got := c.Name(); got != "domains" {
		t.Errorf("Name() = %q, want %q", got, "domains")
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
	if want := 1; count != want {
		t.Errorf("Describe sent %d descriptors, want %d", count, want)
	}
}
