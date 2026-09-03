package loadbalancers_test

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

	"github.com/kozaktomas/digitalocean_exporter/internal/collector/loadbalancers"
	"github.com/kozaktomas/digitalocean_exporter/internal/doclient"
	"github.com/kozaktomas/digitalocean_exporter/internal/filter"
)

// Two load balancers: an active regional one with droplet backends, a fully
// configured health check, its own firewall and two forwarding rules — one
// terminating TLS with a certificate, one passing TLS through — and one still
// being created that selects its backends by tag and so reports none, with no
// health check and no firewall of its own.
const loadBalancersJSON = `{"load_balancers":[` +
	`{"id":"lb-1","name":"public","ip":"10.0.0.1","status":"active","size_unit":2,` +
	`"type":"REGIONAL","algorithm":"round_robin","size":"lb-small",` +
	`"vpc_uuid":"vpc-1","region":{"slug":"fra1"},"droplet_ids":[1,2,3],` +
	`"forwarding_rules":[{"entry_protocol":"https","entry_port":443,` +
	`"target_protocol":"http","target_port":80,"certificate_id":"cert-1"},` +
	`{"entry_protocol":"https","entry_port":8443,"target_protocol":"https",` +
	`"target_port":8443,"tls_passthrough":true}],` +
	`"health_check":{"protocol":"http","port":80,"path":"/healthz",` +
	`"check_interval_seconds":10,"response_timeout_seconds":5,` +
	`"healthy_threshold":3,"unhealthy_threshold":5},` +
	`"firewall":{"allow":["cidr:10.0.0.0/8","ip:192.0.2.7"],"deny":["ip:198.51.100.9"]}},` +
	`{"id":"lb-2","name":"pending","ip":"","status":"new","size_unit":1,` +
	`"type":"REGIONAL_NETWORK","algorithm":"least_connections","size":"",` +
	`"vpc_uuid":"vpc-1","region":{"slug":"ams3"},"tag":"web","droplet_ids":[],` +
	`"forwarding_rules":[]}` +
	`],"meta":{"total":2}}`

const loadBalancerMetrics = `
# HELP digitalocean_loadbalancer_droplets The number of droplets this load balancer is proxying to.
# TYPE digitalocean_loadbalancer_droplets gauge
digitalocean_loadbalancer_droplets{id="lb-1",ip="10.0.0.1",name="public"} 3
digitalocean_loadbalancer_droplets{id="lb-2",ip="",name="pending"} 0
` +
	`# HELP digitalocean_loadbalancer_firewall_rules Number of rules of that kind on the load balancer's ` +
	`own firewall: kind is allow or deny, and both are 0 when no firewall is configured.` + "\n" +
	`# TYPE digitalocean_loadbalancer_firewall_rules gauge
digitalocean_loadbalancer_firewall_rules{id="lb-1",ip="10.0.0.1",kind="allow",name="public"} 2
digitalocean_loadbalancer_firewall_rules{id="lb-1",ip="10.0.0.1",kind="deny",name="public"} 1
digitalocean_loadbalancer_firewall_rules{id="lb-2",ip="",kind="allow",name="pending"} 0
digitalocean_loadbalancer_firewall_rules{id="lb-2",ip="",kind="deny",name="pending"} 0
` +
	`# HELP digitalocean_loadbalancer_forwarding_rule_info Always 1, one series per forwarding rule. ` +
	`certificate_id joins the rule to digitalocean_certificate_expiry_timestamp_seconds ` +
	`on that collector's id label.` + "\n" +
	`# TYPE digitalocean_loadbalancer_forwarding_rule_info gauge` + "\n" +
	`digitalocean_loadbalancer_forwarding_rule_info{certificate_id="",entry_port="8443",` +
	`entry_protocol="https",id="lb-1",ip="10.0.0.1",name="public",target_port="8443",` +
	`target_protocol="https",tls_passthrough="true"} 1` + "\n" +
	`digitalocean_loadbalancer_forwarding_rule_info{certificate_id="cert-1",entry_port="443",` +
	`entry_protocol="https",id="lb-1",ip="10.0.0.1",name="public",target_port="80",` +
	`target_protocol="http",tls_passthrough="false"} 1` + "\n" +
	`# HELP digitalocean_loadbalancer_forwarding_rules Number of forwarding rules configured on the load balancer.
# TYPE digitalocean_loadbalancer_forwarding_rules gauge
digitalocean_loadbalancer_forwarding_rules{id="lb-1",ip="10.0.0.1",name="public"} 2
digitalocean_loadbalancer_forwarding_rules{id="lb-2",ip="",name="pending"} 0
` +
	`# HELP digitalocean_loadbalancer_health_check_healthy_threshold Consecutive successful health checks ` +
	`before a backend is put back into rotation.` + "\n" +
	`# TYPE digitalocean_loadbalancer_health_check_healthy_threshold gauge
digitalocean_loadbalancer_health_check_healthy_threshold{id="lb-1",ip="10.0.0.1",name="public"} 3
` +
	`# HELP digitalocean_loadbalancer_health_check_info Always 1. Its labels describe how the load balancer ` +
	`probes its backends.` + "\n" +
	`# TYPE digitalocean_loadbalancer_health_check_info gauge` + "\n" +
	`digitalocean_loadbalancer_health_check_info{id="lb-1",ip="10.0.0.1",name="public",` +
	`path="/healthz",port="80",protocol="http"} 1` + "\n" +
	`# HELP digitalocean_loadbalancer_health_check_interval_seconds Seconds between two health checks ` +
	`of the same backend.` + "\n" +
	`# TYPE digitalocean_loadbalancer_health_check_interval_seconds gauge
digitalocean_loadbalancer_health_check_interval_seconds{id="lb-1",ip="10.0.0.1",name="public"} 10
` +
	`# HELP digitalocean_loadbalancer_health_check_timeout_seconds Seconds the health check waits for ` +
	`a backend to respond before counting a failure.` + "\n" +
	`# TYPE digitalocean_loadbalancer_health_check_timeout_seconds gauge
digitalocean_loadbalancer_health_check_timeout_seconds{id="lb-1",ip="10.0.0.1",name="public"} 5
` +
	`# HELP digitalocean_loadbalancer_health_check_unhealthy_threshold Consecutive failed health checks ` +
	`before a backend is taken out of rotation.` + "\n" +
	`# TYPE digitalocean_loadbalancer_health_check_unhealthy_threshold gauge
digitalocean_loadbalancer_health_check_unhealthy_threshold{id="lb-1",ip="10.0.0.1",name="public"} 5
` +
	`# HELP digitalocean_loadbalancer_info Always 1. Its labels describe the load balancer's placement ` +
	`and configuration; tag is the droplet tag it selects its backends by, empty when they are listed by ID.` + "\n" +
	`# TYPE digitalocean_loadbalancer_info gauge` + "\n" +
	`digitalocean_loadbalancer_info{algorithm="round_robin",id="lb-1",ip="10.0.0.1",` +
	`name="public",region="fra1",size="lb-small",tag="",type="REGIONAL",vpc_uuid="vpc-1"} 1` + "\n" +
	`digitalocean_loadbalancer_info{algorithm="least_connections",id="lb-2",ip="",` +
	`name="pending",region="ams3",size="",tag="web",type="REGIONAL_NETWORK",vpc_uuid="vpc-1"} 1` + "\n" +
	`# HELP digitalocean_loadbalancer_size_units Number of size units the load balancer is billed for.
# TYPE digitalocean_loadbalancer_size_units gauge
digitalocean_loadbalancer_size_units{id="lb-1",ip="10.0.0.1",name="public"} 2
digitalocean_loadbalancer_size_units{id="lb-2",ip="",name="pending"} 1
# HELP digitalocean_loadbalancer_status The status of the load balancer, 1 if active.
# TYPE digitalocean_loadbalancer_status gauge
digitalocean_loadbalancer_status{id="lb-1",ip="10.0.0.1",name="public"} 1
digitalocean_loadbalancer_status{id="lb-2",ip="",name="pending"} 0
`

// newTestCollector wires an unfiltered collector to a fake DigitalOcean API.
func newTestCollector(t *testing.T, handler http.HandlerFunc) *loadbalancers.Collector {
	t.Helper()
	return newFilteredCollector(t, filter.Filter{}, handler)
}

// newFilteredCollector is newTestCollector with a filter set.
func newFilteredCollector(t *testing.T, f filter.Filter, handler http.HandlerFunc) *loadbalancers.Collector {
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
	return loadbalancers.New(client, f, nil)
}

// okHandler serves the two-load-balancer account.
func okHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/v2/load_balancers" {
		_, _ = w.Write([]byte(loadBalancersJSON))
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

// A region filter keeps only the load balancers in its regions: the ams3 one
// is absent from the exposition, not zeroed.
func TestRefreshFiltersByRegion(t *testing.T) {
	c := newFilteredCollector(t, filter.New(nil, []string{"fra1"}), okHandler)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	const want = `
# HELP digitalocean_loadbalancer_status The status of the load balancer, 1 if active.
# TYPE digitalocean_loadbalancer_status gauge
digitalocean_loadbalancer_status{id="lb-1",ip="10.0.0.1",name="public"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want),
		"digitalocean_loadbalancer_status"); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

func TestCollectAfterRefresh(t *testing.T) {
	c := newTestCollector(t, okHandler)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if err := testutil.CollectAndCompare(c, strings.NewReader(loadBalancerMetrics)); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// An account with more load balancers than fit on one page is paginated by
// page number, and every page has to reach the snapshot.
func TestRefreshFollowsPages(t *testing.T) {
	page := func(id, name string, next bool) string {
		links := `"links":{}`
		if next {
			links = `"links":{"pages":{"next":"https://api.digitalocean.com/v2/load_balancers?page=2"}}`
		}
		return fmt.Sprintf(`{"load_balancers":[{"id":%q,"name":%q,"ip":"10.0.0.9",`+
			`"status":"active","region":{"slug":"fra1"},"droplet_ids":[]}],%s,"meta":{"total":2}}`,
			id, name, links)
	}

	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write([]byte(page("lb-2", "second", false)))
			return
		}
		_, _ = w.Write([]byte(page("lb-1", "first", true)))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	const want = `
# HELP digitalocean_loadbalancer_status The status of the load balancer, 1 if active.
# TYPE digitalocean_loadbalancer_status gauge
digitalocean_loadbalancer_status{id="lb-1",ip="10.0.0.9",name="first"} 1
digitalocean_loadbalancer_status{id="lb-2",ip="10.0.0.9",name="second"} 1
`
	const metric = "digitalocean_loadbalancer_status"
	if err := testutil.CollectAndCompare(c, strings.NewReader(want), metric); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// An account with no load balancers is a normal state: the refresh succeeds
// and there is simply nothing to report.
func TestRefreshWithoutLoadBalancersSucceeds(t *testing.T) {
	c := newTestCollector(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"load_balancers":[],"meta":{"total":0}}`))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh without load balancers: %v", err)
	}
	if got := testutil.CollectAndCount(c); got != 0 {
		t.Errorf("metric count without load balancers = %d, want 0", got)
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

	if err := testutil.CollectAndCompare(c, strings.NewReader(loadBalancerMetrics)); err != nil {
		t.Errorf("unexpected metrics after a failed refresh: %v", err)
	}
}

func TestName(t *testing.T) {
	c := newTestCollector(t, okHandler)
	if got := c.Name(); got != "loadbalancers" {
		t.Errorf("Name() = %q, want %q", got, "loadbalancers")
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
	if want := 12; count != want {
		t.Errorf("Describe sent %d descriptors, want %d", count, want)
	}
}

// The list can shift between two page requests — a resource created or
// destroyed while the pages are being read — and the same load balancer then arrives
// on both. It has to reach the snapshot once: two entries would be two series
// with identical labels, which fails the whole scrape rather than one metric.
func TestRefreshDropsADuplicateLoadBalancerOnTwoPages(t *testing.T) {
	page := func(next bool) string {
		links := `"links":{}`
		if next {
			links = `"links":{"pages":{"next":"https://api.digitalocean.com/v2/load_balancers?page=2"}}`
		}
		return fmt.Sprintf(`{"load_balancers":[{"id":"lb-1","name":"first","ip":"10.0.0.9",`+
			`"status":"active","region":{"slug":"fra1"},"droplet_ids":[]}],%s,"meta":{"total":1}}`, links)
	}

	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(page(r.URL.Query().Get("page") != "2")))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	const want = `
# HELP digitalocean_loadbalancer_status The status of the load balancer, 1 if active.
# TYPE digitalocean_loadbalancer_status gauge
digitalocean_loadbalancer_status{id="lb-1",ip="10.0.0.9",name="first"} 1
`
	const metric = "digitalocean_loadbalancer_status"
	if err := testutil.CollectAndCompare(c, strings.NewReader(want), metric); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}
