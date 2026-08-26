package firewalls_test

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

	"github.com/kozaktomas/digitalocean_exporter/internal/collector/firewalls"
	"github.com/kozaktomas/digitalocean_exporter/internal/doclient"
)

// Two firewalls covering the cases that change a metric: one fully applied and
// open to the internet on a single rule, one still being applied whose rules
// reach it from nowhere public. The second firewall's last inbound rule carries
// no sources object at all, which the API does emit and which must not count as
// open.
const firewallsJSON = `{"firewalls":[` +
	`{"id":"fw-1","name":"web","status":"succeeded","droplet_ids":[1,2],"tags":["web"],` +
	`"inbound_rules":[` +
	`{"protocol":"tcp","ports":"443","sources":{"addresses":["0.0.0.0/0","::/0"]}},` +
	`{"protocol":"tcp","ports":"22","sources":{"tags":["bastion"]}}],` +
	`"outbound_rules":[{"protocol":"tcp","ports":"0","destinations":{"addresses":["0.0.0.0/0"]}}],` +
	`"pending_changes":[]},` +
	`{"id":"fw-2","name":"db","status":"waiting","droplet_ids":[3],"tags":[],` +
	`"inbound_rules":[` +
	`{"protocol":"tcp","ports":"5432","sources":{"droplet_ids":[1,2]}},` +
	`{"protocol":"icmp","ports":"0"}],` +
	`"outbound_rules":[],` +
	`"pending_changes":[{"droplet_id":3,"removing":false,"status":"waiting"}]}` +
	`],"meta":{"total":2}}`

const firewallMetrics = `
# HELP digitalocean_firewall_droplets Number of droplets the firewall is attached to directly.
# TYPE digitalocean_firewall_droplets gauge
digitalocean_firewall_droplets{id="fw-1",name="web"} 2
digitalocean_firewall_droplets{id="fw-2",name="db"} 1
# HELP digitalocean_firewall_inbound_rules Number of inbound rules in the firewall.
# TYPE digitalocean_firewall_inbound_rules gauge
digitalocean_firewall_inbound_rules{id="fw-1",name="web"} 2
digitalocean_firewall_inbound_rules{id="fw-2",name="db"} 2
# HELP digitalocean_firewall_inbound_rules_open Number of inbound rules whose sources include 0.0.0.0/0 or ::/0.
# TYPE digitalocean_firewall_inbound_rules_open gauge
digitalocean_firewall_inbound_rules_open{id="fw-1",name="web"} 1
digitalocean_firewall_inbound_rules_open{id="fw-2",name="db"} 0
# HELP digitalocean_firewall_info Always 1. Its labels describe the firewall and how far DigitalOcean has applied it.
# TYPE digitalocean_firewall_info gauge
digitalocean_firewall_info{id="fw-1",name="web",status="succeeded"} 1
digitalocean_firewall_info{id="fw-2",name="db",status="waiting"} 1
# HELP digitalocean_firewall_outbound_rules Number of outbound rules in the firewall.
# TYPE digitalocean_firewall_outbound_rules gauge
digitalocean_firewall_outbound_rules{id="fw-1",name="web"} 1
digitalocean_firewall_outbound_rules{id="fw-2",name="db"} 0
# HELP digitalocean_firewall_pending_changes Number of droplets a change to the firewall has not been applied to yet.
# TYPE digitalocean_firewall_pending_changes gauge
digitalocean_firewall_pending_changes{id="fw-1",name="web"} 0
digitalocean_firewall_pending_changes{id="fw-2",name="db"} 1
# HELP digitalocean_firewall_tags Number of tags the firewall is attached to; droplets carrying one are covered too.
# TYPE digitalocean_firewall_tags gauge
digitalocean_firewall_tags{id="fw-1",name="web"} 1
digitalocean_firewall_tags{id="fw-2",name="db"} 0
`

// newTestCollector wires a collector to a fake DigitalOcean API.
func newTestCollector(t *testing.T, handler http.HandlerFunc) *firewalls.Collector {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	client, err := doclient.New("token", srv.URL+"/", "test", 5*time.Second,
		doclient.NewMetrics(prometheus.NewRegistry()))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return firewalls.New(client)
}

// okHandler serves the two-firewall account.
func okHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/v2/firewalls" {
		_, _ = w.Write([]byte(firewallsJSON))
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func TestCollectAfterRefresh(t *testing.T) {
	c := newTestCollector(t, okHandler)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if err := testutil.CollectAndCompare(c, strings.NewReader(firewallMetrics)); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// An account with more firewalls than fit on one page is paginated by page
// number, and every page has to reach the snapshot.
func TestRefreshFollowsPages(t *testing.T) {
	page := func(id, name string, next bool) string {
		links := `"links":{}`
		if next {
			links = `"links":{"pages":{"next":"https://api.digitalocean.com/v2/firewalls?page=2"}}`
		}
		return fmt.Sprintf(`{"firewalls":[{"id":%q,"name":%q,"status":"succeeded"}],`+
			`%s,"meta":{"total":2}}`, id, name, links)
	}

	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write([]byte(page("fw-2", "db", false)))
			return
		}
		_, _ = w.Write([]byte(page("fw-1", "web", true)))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	const want = `
# HELP digitalocean_firewall_info Always 1. Its labels describe the firewall and how far DigitalOcean has applied it.
# TYPE digitalocean_firewall_info gauge
digitalocean_firewall_info{id="fw-1",name="web",status="succeeded"} 1
digitalocean_firewall_info{id="fw-2",name="db",status="succeeded"} 1
`
	const metric = "digitalocean_firewall_info"
	if err := testutil.CollectAndCompare(c, strings.NewReader(want), metric); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// An account with no firewalls is a normal state: the refresh succeeds and
// there is simply nothing to report.
func TestRefreshWithoutFirewallsSucceeds(t *testing.T) {
	c := newTestCollector(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"firewalls":[],"meta":{"total":0}}`))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh without firewalls: %v", err)
	}
	if got := testutil.CollectAndCount(c); got != 0 {
		t.Errorf("metric count without firewalls = %d, want 0", got)
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

	if err := testutil.CollectAndCompare(c, strings.NewReader(firewallMetrics)); err != nil {
		t.Errorf("unexpected metrics after a failed refresh: %v", err)
	}
}

func TestName(t *testing.T) {
	c := newTestCollector(t, okHandler)
	if got := c.Name(); got != "firewalls" {
		t.Errorf("Name() = %q, want %q", got, "firewalls")
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
	if want := 7; count != want {
		t.Errorf("Describe sent %d descriptors, want %d", count, want)
	}
}
