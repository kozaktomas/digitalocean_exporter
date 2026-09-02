package kubernetes_test

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

	"github.com/kozaktomas/digitalocean_exporter/internal/collector/kubernetes"
	"github.com/kozaktomas/digitalocean_exporter/internal/doclient"
)

// One cluster with two pools: an autoscaling one whose second node is still
// provisioning, and one scaled to zero.
const clustersJSON = `{"kubernetes_clusters":[{"id":"c1","name":"prod","region":"fra1",` +
	`"version":"1.35.5-do.1","status":{"state":"running"},"ha":false,"auto_upgrade":true,` +
	`"surge_upgrade":false,"registry_enabled":true,` +
	`"maintenance_policy":{"day":"tuesday","start_time":"04:00","duration":"4h0m0s"},` +
	`"node_pools":[` +
	`{"id":"p1","name":"workers","size":"s-4vcpu-8gb","count":2,"auto_scale":true,` +
	`"min_nodes":1,"max_nodes":5,"nodes":[` +
	`{"id":"n1","name":"node-1","droplet_id":"301","status":{"state":"running"}},` +
	`{"id":"n2","name":"node-2","droplet_id":"302",` +
	`"status":{"state":"provisioning","message":"waiting for the droplet"}}]},` +
	`{"id":"p2","name":"idle","size":"s-2vcpu-4gb","count":0,"auto_scale":false,"nodes":[]}` +
	`]}],"meta":{"total":1}}`

// The cluster is two patch releases behind, and DigitalOcean offers both.
const upgradesJSON = `{"available_upgrade_versions":[` +
	`{"slug":"1.35.6-do.0","kubernetes_version":"1.35.6"},` +
	`{"slug":"1.36.1-do.0","kubernetes_version":"1.36.1"}]}`

// The exposition format puts one series on one line and cannot be wrapped, and
// a node series carries seven labels.
//
//nolint:lll // golden exposition text: one series per line, unwrappable.
const clusterMetrics = `
# HELP digitalocean_kubernetes_cluster_auto_upgrade Whether the cluster upgrades itself in its maintenance window.
# TYPE digitalocean_kubernetes_cluster_auto_upgrade gauge
digitalocean_kubernetes_cluster_auto_upgrade{id="c1",name="prod",region="fra1"} 1
# HELP digitalocean_kubernetes_cluster_available_version_info Always 1, once per version the cluster can be upgraded to.
# TYPE digitalocean_kubernetes_cluster_available_version_info gauge
digitalocean_kubernetes_cluster_available_version_info{cluster="prod",cluster_id="c1",version="1.35.6-do.0"} 1
digitalocean_kubernetes_cluster_available_version_info{cluster="prod",cluster_id="c1",version="1.36.1-do.0"} 1
# HELP digitalocean_kubernetes_cluster_ha Whether the cluster runs a highly available control plane.
# TYPE digitalocean_kubernetes_cluster_ha gauge
digitalocean_kubernetes_cluster_ha{id="c1",name="prod",region="fra1"} 0
# HELP digitalocean_kubernetes_cluster_info Always 1. Its labels describe the cluster's version and maintenance window.
# TYPE digitalocean_kubernetes_cluster_info gauge
digitalocean_kubernetes_cluster_info{id="c1",maintenance_day="tuesday",maintenance_start_time="04:00",name="prod",region="fra1",version="1.35.5-do.1"} 1
# HELP digitalocean_kubernetes_cluster_registry_enabled Whether the account's container registry is integrated with the cluster.
# TYPE digitalocean_kubernetes_cluster_registry_enabled gauge
digitalocean_kubernetes_cluster_registry_enabled{id="c1",name="prod",region="fra1"} 1
# HELP digitalocean_kubernetes_cluster_surge_upgrade Whether the cluster adds a node before replacing one.
# TYPE digitalocean_kubernetes_cluster_surge_upgrade gauge
digitalocean_kubernetes_cluster_surge_upgrade{id="c1",name="prod",region="fra1"} 0
# HELP digitalocean_kubernetes_cluster_up Whether the cluster state is running.
# TYPE digitalocean_kubernetes_cluster_up gauge
digitalocean_kubernetes_cluster_up{id="c1",name="prod",region="fra1",version="1.35.5-do.1"} 1
# HELP digitalocean_kubernetes_cluster_upgrade_available Whether DigitalOcean offers the cluster at least one newer version.
# TYPE digitalocean_kubernetes_cluster_upgrade_available gauge
digitalocean_kubernetes_cluster_upgrade_available{cluster="prod",cluster_id="c1"} 1
# HELP digitalocean_kubernetes_node_info Always 1. Its labels tie the node to the droplet underneath it.
# TYPE digitalocean_kubernetes_node_info gauge
digitalocean_kubernetes_node_info{cluster="prod",cluster_id="c1",droplet_id="301",node="node-1",node_id="n1",pool="workers",pool_id="p1"} 1
digitalocean_kubernetes_node_info{cluster="prod",cluster_id="c1",droplet_id="302",node="node-2",node_id="n2",pool="workers",pool_id="p1"} 1
# HELP digitalocean_kubernetes_node_pool_auto_scale Whether the pool scales itself between its bounds.
# TYPE digitalocean_kubernetes_node_pool_auto_scale gauge
digitalocean_kubernetes_node_pool_auto_scale{cluster="prod",cluster_id="c1",pool="idle",pool_id="p2",size="s-2vcpu-4gb"} 0
digitalocean_kubernetes_node_pool_auto_scale{cluster="prod",cluster_id="c1",pool="workers",pool_id="p1",size="s-4vcpu-8gb"} 1
# HELP digitalocean_kubernetes_node_pool_max_nodes Largest number of nodes the pool may scale to.
# TYPE digitalocean_kubernetes_node_pool_max_nodes gauge
digitalocean_kubernetes_node_pool_max_nodes{cluster="prod",cluster_id="c1",pool="idle",pool_id="p2",size="s-2vcpu-4gb"} 0
digitalocean_kubernetes_node_pool_max_nodes{cluster="prod",cluster_id="c1",pool="workers",pool_id="p1",size="s-4vcpu-8gb"} 5
# HELP digitalocean_kubernetes_node_pool_min_nodes Smallest number of nodes the pool may scale to.
# TYPE digitalocean_kubernetes_node_pool_min_nodes gauge
digitalocean_kubernetes_node_pool_min_nodes{cluster="prod",cluster_id="c1",pool="idle",pool_id="p2",size="s-2vcpu-4gb"} 0
digitalocean_kubernetes_node_pool_min_nodes{cluster="prod",cluster_id="c1",pool="workers",pool_id="p1",size="s-4vcpu-8gb"} 1
# HELP digitalocean_kubernetes_node_pool_nodes Number of nodes the pool is configured to run.
# TYPE digitalocean_kubernetes_node_pool_nodes gauge
digitalocean_kubernetes_node_pool_nodes{cluster="prod",cluster_id="c1",pool="idle",pool_id="p2",size="s-2vcpu-4gb"} 0
digitalocean_kubernetes_node_pool_nodes{cluster="prod",cluster_id="c1",pool="workers",pool_id="p1",size="s-4vcpu-8gb"} 2
# HELP digitalocean_kubernetes_node_pool_nodes_running Number of nodes in the pool reporting the running state.
# TYPE digitalocean_kubernetes_node_pool_nodes_running gauge
digitalocean_kubernetes_node_pool_nodes_running{cluster="prod",cluster_id="c1",pool="idle",pool_id="p2",size="s-2vcpu-4gb"} 0
digitalocean_kubernetes_node_pool_nodes_running{cluster="prod",cluster_id="c1",pool="workers",pool_id="p1",size="s-4vcpu-8gb"} 1
# HELP digitalocean_kubernetes_node_state Always 1 for the node's current state and 0 for every other known one.
# TYPE digitalocean_kubernetes_node_state gauge
digitalocean_kubernetes_node_state{cluster="prod",cluster_id="c1",node="node-1",node_id="n1",pool="workers",pool_id="p1",state="deleting"} 0
digitalocean_kubernetes_node_state{cluster="prod",cluster_id="c1",node="node-1",node_id="n1",pool="workers",pool_id="p1",state="draining"} 0
digitalocean_kubernetes_node_state{cluster="prod",cluster_id="c1",node="node-1",node_id="n1",pool="workers",pool_id="p1",state="provisioning"} 0
digitalocean_kubernetes_node_state{cluster="prod",cluster_id="c1",node="node-1",node_id="n1",pool="workers",pool_id="p1",state="running"} 1
digitalocean_kubernetes_node_state{cluster="prod",cluster_id="c1",node="node-2",node_id="n2",pool="workers",pool_id="p1",state="deleting"} 0
digitalocean_kubernetes_node_state{cluster="prod",cluster_id="c1",node="node-2",node_id="n2",pool="workers",pool_id="p1",state="draining"} 0
digitalocean_kubernetes_node_state{cluster="prod",cluster_id="c1",node="node-2",node_id="n2",pool="workers",pool_id="p1",state="provisioning"} 1
digitalocean_kubernetes_node_state{cluster="prod",cluster_id="c1",node="node-2",node_id="n2",pool="workers",pool_id="p1",state="running"} 0
`

// newTestCollector wires a collector to a fake DigitalOcean API, with the
// upgrades lookup on.
func newTestCollector(t *testing.T, handler http.HandlerFunc) *kubernetes.Collector {
	t.Helper()
	return newTestCollectorWith(t, handler, true)
}

// newTestCollectorWith wires a collector to a fake DigitalOcean API.
func newTestCollectorWith(t *testing.T, handler http.HandlerFunc, upgrades bool) *kubernetes.Collector {
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
	return kubernetes.New(client, upgrades, nil)
}

func okHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/v2/kubernetes/clusters":
		_, _ = w.Write([]byte(clustersJSON))
	case "/v2/kubernetes/clusters/c1/upgrades":
		_, _ = w.Write([]byte(upgradesJSON))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestCollectAfterRefresh(t *testing.T) {
	c := newTestCollector(t, okHandler)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if err := testutil.CollectAndCompare(c, strings.NewReader(clusterMetrics)); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// A cluster already on the newest version reports the availability metric at 0
// and no version series, which is what tells it apart from a cluster whose
// upgrades have never been read.
func TestCollectReportsAClusterWithNoUpgrade(t *testing.T) {
	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/kubernetes/clusters/c1/upgrades" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"available_upgrade_versions":[]}`))
			return
		}
		okHandler(w, r)
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	//nolint:lll // golden exposition text: one series per line, unwrappable.
	const want = `
# HELP digitalocean_kubernetes_cluster_upgrade_available Whether DigitalOcean offers the cluster at least one newer version.
# TYPE digitalocean_kubernetes_cluster_upgrade_available gauge
digitalocean_kubernetes_cluster_upgrade_available{cluster="prod",cluster_id="c1"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want),
		"digitalocean_kubernetes_cluster_upgrade_available",
		"digitalocean_kubernetes_cluster_available_version_info"); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// With the lookup switched off the refresh spends no request on it, and the
// two upgrade metrics are absent rather than zero: a zero would read as
// "you are on the newest version".
func TestUpgradesCanBeSwitchedOff(t *testing.T) {
	var asked bool
	c := newTestCollectorWith(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/upgrades") {
			asked = true
		}
		okHandler(w, r)
	}, false)

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if asked {
		t.Error("the upgrades endpoint was called with the lookup switched off")
	}

	if err := testutil.CollectAndCompare(c, strings.NewReader(""),
		"digitalocean_kubernetes_cluster_upgrade_available",
		"digitalocean_kubernetes_cluster_available_version_info"); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// One cluster's upgrades lookup failing is not the refresh failing: the rest of
// the cluster is reported as usual and the previous upgrade values are kept,
// because a cluster that stops reporting an available upgrade looks exactly
// like one that has been upgraded.
func TestFailingUpgradesLookupKeepsPreviousValues(t *testing.T) {
	var fail bool
	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		if fail && strings.HasSuffix(r.URL.Path, "/upgrades") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		okHandler(w, r)
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}

	fail = true
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("a failing upgrades lookup failed the refresh: %v", err)
	}

	if err := testutil.CollectAndCompare(c, strings.NewReader(clusterMetrics)); err != nil {
		t.Errorf("unexpected metrics after a failing upgrades lookup: %v", err)
	}
}

// A lookup that has never succeeded emits nothing, rather than claiming the
// cluster is on the newest version.
func TestFailingUpgradesLookupWithoutPreviousValuesEmitsNothing(t *testing.T) {
	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/upgrades") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		okHandler(w, r)
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if err := testutil.CollectAndCompare(c, strings.NewReader(""),
		"digitalocean_kubernetes_cluster_upgrade_available",
		"digitalocean_kubernetes_cluster_available_version_info"); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
	// The cluster itself is still reported.
	if err := testutil.CollectAndCompare(c, strings.NewReader(`
# HELP digitalocean_kubernetes_cluster_up Whether the cluster state is running.
# TYPE digitalocean_kubernetes_cluster_up gauge
digitalocean_kubernetes_cluster_up{id="c1",name="prod",region="fra1",version="1.35.5-do.1"} 1
`), "digitalocean_kubernetes_cluster_up"); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// A state DigitalOcean invents after this was written is reported beside the
// documented ones. Left out, every state series of that node would read 0,
// which is indistinguishable from the node having disappeared.
func TestCollectReportsAnUnknownNodeState(t *testing.T) {
	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/kubernetes/clusters" {
			okHandler(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kubernetes_clusters":[{"id":"c1","name":"prod","region":"fra1",` +
			`"version":"1.35.5-do.1","status":{"state":"running"},"node_pools":[` +
			`{"id":"p1","name":"workers","size":"s-1vcpu-2gb","count":1,"nodes":[` +
			`{"id":"n1","name":"node-1","droplet_id":"301","status":{"state":"hibernating"}}]}` +
			`]}],"meta":{"total":1}}`))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	//nolint:lll // golden exposition text: one series per line, unwrappable.
	const want = `
# HELP digitalocean_kubernetes_node_state Always 1 for the node's current state and 0 for every other known one.
# TYPE digitalocean_kubernetes_node_state gauge
digitalocean_kubernetes_node_state{cluster="prod",cluster_id="c1",node="node-1",node_id="n1",pool="workers",pool_id="p1",state="deleting"} 0
digitalocean_kubernetes_node_state{cluster="prod",cluster_id="c1",node="node-1",node_id="n1",pool="workers",pool_id="p1",state="draining"} 0
digitalocean_kubernetes_node_state{cluster="prod",cluster_id="c1",node="node-1",node_id="n1",pool="workers",pool_id="p1",state="hibernating"} 1
digitalocean_kubernetes_node_state{cluster="prod",cluster_id="c1",node="node-1",node_id="n1",pool="workers",pool_id="p1",state="provisioning"} 0
digitalocean_kubernetes_node_state{cluster="prod",cluster_id="c1",node="node-1",node_id="n1",pool="workers",pool_id="p1",state="running"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want),
		"digitalocean_kubernetes_node_state"); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// Two clusters can hold a pool of the same name, and two pools can hold a node
// of the same name. The pool and node ids are what keep the series apart.
func TestCollectSeparatesPoolsOfTheSameNameInTwoClusters(t *testing.T) {
	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/upgrades") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"available_upgrade_versions":[]}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kubernetes_clusters":[` +
			`{"id":"c1","name":"prod","region":"fra1","version":"1.35.5-do.1",` +
			`"status":{"state":"running"},"node_pools":[` +
			`{"id":"p1","name":"workers","size":"s-1vcpu-2gb","count":1,"nodes":[]}]},` +
			`{"id":"c2","name":"staging","region":"fra1","version":"1.35.5-do.1",` +
			`"status":{"state":"running"},"node_pools":[` +
			`{"id":"p2","name":"workers","size":"s-1vcpu-2gb","count":1,"nodes":[]}]}` +
			`],"meta":{"total":2}}`))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	//nolint:lll // golden exposition text: one series per line, unwrappable.
	const want = `
# HELP digitalocean_kubernetes_node_pool_nodes Number of nodes the pool is configured to run.
# TYPE digitalocean_kubernetes_node_pool_nodes gauge
digitalocean_kubernetes_node_pool_nodes{cluster="prod",cluster_id="c1",pool="workers",pool_id="p1",size="s-1vcpu-2gb"} 1
digitalocean_kubernetes_node_pool_nodes{cluster="staging",cluster_id="c2",pool="workers",pool_id="p2",size="s-1vcpu-2gb"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want),
		"digitalocean_kubernetes_node_pool_nodes"); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// A cluster that is not running, whichever state it is in, reports up 0 and
// still describes its pools.
func TestCollectReportsAClusterThatIsNotRunning(t *testing.T) {
	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/upgrades") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"available_upgrade_versions":[]}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kubernetes_clusters":[{"id":"c1","name":"prod","region":"fra1",` +
			`"version":"1.35.5-do.1","status":{"state":"degraded"},"node_pools":[]}],"meta":{"total":1}}`))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	const want = `
# HELP digitalocean_kubernetes_cluster_up Whether the cluster state is running.
# TYPE digitalocean_kubernetes_cluster_up gauge
digitalocean_kubernetes_cluster_up{id="c1",name="prod",region="fra1",version="1.35.5-do.1"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want),
		"digitalocean_kubernetes_cluster_up"); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// A cluster with no maintenance policy configured still reports its info
// metric, with the two window labels empty.
func TestCollectReportsAClusterWithoutAMaintenancePolicy(t *testing.T) {
	c := newTestCollectorWith(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kubernetes_clusters":[{"id":"c1","name":"prod","region":"fra1",` +
			`"version":"1.35.5-do.1","status":{"state":"running"},"node_pools":[]}],"meta":{"total":1}}`))
	}, false)

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	//nolint:lll // golden exposition text: one series per line, unwrappable.
	const want = `
# HELP digitalocean_kubernetes_cluster_info Always 1. Its labels describe the cluster's version and maintenance window.
# TYPE digitalocean_kubernetes_cluster_info gauge
digitalocean_kubernetes_cluster_info{id="c1",maintenance_day="",maintenance_start_time="",name="prod",region="fra1",version="1.35.5-do.1"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want),
		"digitalocean_kubernetes_cluster_info"); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

func TestRefreshFollowsPages(t *testing.T) {
	page := func(id, name string, next bool) string {
		links := `"links":{}`
		if next {
			links = `"links":{"pages":{"next":"https://api.digitalocean.com/v2/kubernetes/clusters?page=2"}}`
		}
		return fmt.Sprintf(`{"kubernetes_clusters":[{"id":%q,"name":%q,"region":"fra1","version":"1.35.5-do.1",`+
			`"status":{"state":"running"},"node_pools":[]}],%s,"meta":{"total":2}}`, id, name, links)
	}

	c := newTestCollectorWith(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write([]byte(page("c2", "second", false)))
			return
		}
		_, _ = w.Write([]byte(page("c1", "first", true)))
	}, false)

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	const want = `
# HELP digitalocean_kubernetes_cluster_up Whether the cluster state is running.
# TYPE digitalocean_kubernetes_cluster_up gauge
digitalocean_kubernetes_cluster_up{id="c1",name="first",region="fra1",version="1.35.5-do.1"} 1
digitalocean_kubernetes_cluster_up{id="c2",name="second",region="fra1",version="1.35.5-do.1"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want),
		"digitalocean_kubernetes_cluster_up"); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// An account with no cluster is a normal state.
func TestRefreshWithoutClustersSucceeds(t *testing.T) {
	c := newTestCollector(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kubernetes_clusters":[],"meta":{"total":0}}`))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh without clusters: %v", err)
	}
	if got := testutil.CollectAndCount(c); got != 0 {
		t.Errorf("metric count without clusters = %d, want 0", got)
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

	if err := testutil.CollectAndCompare(c, strings.NewReader(clusterMetrics)); err != nil {
		t.Errorf("unexpected metrics after a failed refresh: %v", err)
	}
}

// A refresh that runs out of time stops asking about the clusters that are
// left: the deadline belongs to the whole refresh, not to one lookup.
func TestCancelledRefreshFails(t *testing.T) {
	c := newTestCollector(t, okHandler)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := c.Refresh(ctx); err == nil {
		t.Fatal("expected a cancelled refresh to fail")
	}
}

func TestName(t *testing.T) {
	c := newTestCollector(t, okHandler)
	if got := c.Name(); got != "kubernetes" {
		t.Errorf("Name() = %q, want %q", got, "kubernetes")
	}
}

func TestDescribeCoversEveryMetric(t *testing.T) {
	c := newTestCollector(t, okHandler)

	ch := make(chan *prometheus.Desc, 32)
	c.Describe(ch)
	close(ch)

	var count int
	for range ch {
		count++
	}
	if want := 15; count != want {
		t.Errorf("Describe sent %d descriptors, want %d", count, want)
	}
}

// The list can shift between two page requests — a resource created or
// destroyed while the pages are being read — and the same cluster then arrives
// on both. It has to reach the snapshot once: two entries would be two series
// with identical labels, which fails the whole scrape rather than one metric.
func TestRefreshDropsADuplicateClusterOnTwoPages(t *testing.T) {
	page := func(next bool) string {
		links := `"links":{}`
		if next {
			links = `"links":{"pages":{"next":"https://api.digitalocean.com/v2/kubernetes/clusters?page=2"}}`
		}
		return fmt.Sprintf(`{"kubernetes_clusters":[{"id":"c1","name":"first","region":"fra1","version":"1.35.5-do.1",`+
			`"status":{"state":"running"},"node_pools":[]}],%s,"meta":{"total":1}}`, links)
	}

	c := newTestCollectorWith(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(page(r.URL.Query().Get("page") != "2")))
	}, false)

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	const want = `
# HELP digitalocean_kubernetes_cluster_up Whether the cluster state is running.
# TYPE digitalocean_kubernetes_cluster_up gauge
digitalocean_kubernetes_cluster_up{id="c1",name="first",region="fra1",version="1.35.5-do.1"} 1
`
	const metric = "digitalocean_kubernetes_cluster_up"
	if err := testutil.CollectAndCompare(c, strings.NewReader(want), metric); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}
