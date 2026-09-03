package dropletautoscale_test

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

	"github.com/kozaktomas/digitalocean_exporter/internal/collector/dropletautoscale"
	"github.com/kozaktomas/digitalocean_exporter/internal/doclient"
)

// Two pools across two pages, one of each configuration the API allows: a pool
// scaling on a CPU target, whose bounds and utilisation are reported, and a
// pool with a fixed target, which has neither bounds nor utilisation targets.
// The fixed pool reports no current utilisation, so those two metrics are
// absent for it rather than zero.
const cpuPoolJSON = `{"autoscale_pools":[` +
	`{"id":"pool-1","name":"web","status":"active","active_resources_count":4,` +
	`"config":{"min_instances":2,"max_instances":5,"target_cpu_utilization":0.6},` +
	`"droplet_template":{"region":"fra1","size":"s-2vcpu-4gb"},` +
	`"current_utilization":{"cpu":0.55,"memory":0.35}}` +
	`],"links":{"pages":{"next":"https://api.digitalocean.com/v2/droplets/autoscale?page=2"}},` +
	`"meta":{"total":2}}`

const fixedPoolJSON = `{"autoscale_pools":[` +
	`{"id":"pool-2","name":"workers","status":"active","active_resources_count":3,` +
	`"config":{"target_number_instances":3},` +
	`"droplet_template":{"region":"ams3","size":"s-1vcpu-2gb"}}` +
	`],"links":{},"meta":{"total":2}}`

const poolMetrics = `
# HELP digitalocean_droplet_autoscale_pool_active_instances Number of active droplets in the pool.
# TYPE digitalocean_droplet_autoscale_pool_active_instances gauge
digitalocean_droplet_autoscale_pool_active_instances{id="pool-1",name="web"} 4
digitalocean_droplet_autoscale_pool_active_instances{id="pool-2",name="workers"} 3
` +
	"# HELP digitalocean_droplet_autoscale_pool_current_cpu_utilization_ratio " +
	"Average CPU utilisation across the pool's droplets, between 0 and 1.\n" +
	`# TYPE digitalocean_droplet_autoscale_pool_current_cpu_utilization_ratio gauge
digitalocean_droplet_autoscale_pool_current_cpu_utilization_ratio{id="pool-1",name="web"} 0.55
` +
	"# HELP digitalocean_droplet_autoscale_pool_current_memory_utilization_ratio " +
	"Average memory utilisation across the pool's droplets, between 0 and 1.\n" +
	`# TYPE digitalocean_droplet_autoscale_pool_current_memory_utilization_ratio gauge
digitalocean_droplet_autoscale_pool_current_memory_utilization_ratio{id="pool-1",name="web"} 0.35
` +
	"# HELP digitalocean_droplet_autoscale_pool_info " +
	"Always 1. Its labels describe where the pool creates droplets and the pool's status.\n" +
	`# TYPE digitalocean_droplet_autoscale_pool_info gauge
` +
	`digitalocean_droplet_autoscale_pool_info{id="pool-1",name="web",` +
	`region="fra1",size="s-2vcpu-4gb",status="active"} 1` + "\n" +
	`digitalocean_droplet_autoscale_pool_info{id="pool-2",name="workers",` +
	`region="ams3",size="s-1vcpu-2gb",status="active"} 1` + "\n" +
	"# HELP digitalocean_droplet_autoscale_pool_max_instances " +
	"Maximum number of droplets the pool may grow to. Absent for a pool with a fixed target.\n" +
	`# TYPE digitalocean_droplet_autoscale_pool_max_instances gauge
digitalocean_droplet_autoscale_pool_max_instances{id="pool-1",name="web"} 5
` +
	"# HELP digitalocean_droplet_autoscale_pool_min_instances " +
	"Minimum number of droplets the pool keeps. Absent for a pool with a fixed target.\n" +
	`# TYPE digitalocean_droplet_autoscale_pool_min_instances gauge
digitalocean_droplet_autoscale_pool_min_instances{id="pool-1",name="web"} 2
` +
	"# HELP digitalocean_droplet_autoscale_pool_target_cpu_utilization_ratio " +
	"Average CPU utilisation the pool scales towards, between 0 and 1. " +
	"Absent when the pool has no CPU target.\n" +
	`# TYPE digitalocean_droplet_autoscale_pool_target_cpu_utilization_ratio gauge
digitalocean_droplet_autoscale_pool_target_cpu_utilization_ratio{id="pool-1",name="web"} 0.6
` +
	"# HELP digitalocean_droplet_autoscale_pool_target_instances " +
	"Fixed number of droplets the pool is configured to run. " +
	"Absent for a pool that scales on utilisation.\n" +
	`# TYPE digitalocean_droplet_autoscale_pool_target_instances gauge
digitalocean_droplet_autoscale_pool_target_instances{id="pool-2",name="workers"} 3
`

// newTestCollector wires a collector to a fake DigitalOcean API.
func newTestCollector(t *testing.T, handler http.HandlerFunc) *dropletautoscale.Collector {
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
	return dropletautoscale.New(client, nil)
}

// okHandler serves the two-pool account across two pages.
func okHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path != "/v2/droplets/autoscale" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if r.URL.Query().Get("page") == "2" {
		_, _ = w.Write([]byte(fixedPoolJSON))
		return
	}
	_, _ = w.Write([]byte(cpuPoolJSON))
}

// The golden case doubles as the paging test: the CPU-target pool arrives on
// page one and the fixed-target pool on page two, and both have to reach the
// snapshot with exactly the metrics their configuration carries.
func TestCollectAfterRefresh(t *testing.T) {
	c := newTestCollector(t, okHandler)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if err := testutil.CollectAndCompare(c, strings.NewReader(poolMetrics)); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// An account with no autoscale pools is a normal state: the refresh succeeds
// and there is simply nothing to report.
func TestRefreshWithoutPoolsSucceeds(t *testing.T) {
	c := newTestCollector(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"autoscale_pools":[],"meta":{"total":0}}`))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh without pools: %v", err)
	}
	if got := testutil.CollectAndCount(c); got != 0 {
		t.Errorf("metric count without pools = %d, want 0", got)
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

	if err := testutil.CollectAndCompare(c, strings.NewReader(poolMetrics)); err != nil {
		t.Errorf("unexpected metrics after a failed refresh: %v", err)
	}
}

// A failure on the second page must leave the whole previous snapshot in
// place, not swap in the half of the account that was read before it.
func TestFailureOnALaterPageKeepsPreviousSnapshot(t *testing.T) {
	var fail bool
	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		if fail && r.URL.Query().Get("page") == "2" {
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

	if err := testutil.CollectAndCompare(c, strings.NewReader(poolMetrics)); err != nil {
		t.Errorf("unexpected metrics after a failed second page: %v", err)
	}
}

func TestName(t *testing.T) {
	c := newTestCollector(t, okHandler)
	if got := c.Name(); got != "dropletautoscale" {
		t.Errorf("Name() = %q, want %q", got, "dropletautoscale")
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
	if want := 9; count != want {
		t.Errorf("Describe sent %d descriptors, want %d", count, want)
	}
}

// The list can shift between two page requests — a pool created or destroyed
// while the pages are being read — and the same pool then arrives on both. It
// has to reach the snapshot once: two entries would be two series with
// identical labels, which fails the whole scrape rather than one metric.
func TestRefreshDropsADuplicatePoolOnTwoPages(t *testing.T) {
	page := func(next bool) string {
		links := `"links":{}`
		if next {
			links = `"links":{"pages":{"next":"https://api.digitalocean.com/v2/droplets/autoscale?page=2"}}`
		}
		return fmt.Sprintf(`{"autoscale_pools":[`+
			`{"id":"pool-1","name":"web","status":"active","active_resources_count":2,`+
			`"config":{"target_number_instances":2},`+
			`"droplet_template":{"region":"fra1","size":"s-1vcpu-2gb"}}`+
			`],%s,"meta":{"total":1}}`, links)
	}

	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(page(r.URL.Query().Get("page") != "2")))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	const want = `
# HELP digitalocean_droplet_autoscale_pool_active_instances Number of active droplets in the pool.
# TYPE digitalocean_droplet_autoscale_pool_active_instances gauge
digitalocean_droplet_autoscale_pool_active_instances{id="pool-1",name="web"} 2
`
	const metric = "digitalocean_droplet_autoscale_pool_active_instances"
	if err := testutil.CollectAndCompare(c, strings.NewReader(want), metric); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}
