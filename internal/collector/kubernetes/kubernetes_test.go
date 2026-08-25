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
	`"surge_upgrade":false,"node_pools":[` +
	`{"id":"p1","name":"workers","size":"s-4vcpu-8gb","count":2,"auto_scale":true,` +
	`"min_nodes":1,"max_nodes":5,"nodes":[` +
	`{"id":"n1","status":{"state":"running"}},{"id":"n2","status":{"state":"provisioning"}}]},` +
	`{"id":"p2","name":"idle","size":"s-2vcpu-4gb","count":0,"auto_scale":false,"nodes":[]}` +
	`]}],"meta":{"total":1}}`

const clusterMetrics = `
# HELP digitalocean_kubernetes_cluster_auto_upgrade Whether the cluster upgrades itself in its maintenance window.
# TYPE digitalocean_kubernetes_cluster_auto_upgrade gauge
digitalocean_kubernetes_cluster_auto_upgrade{id="c1",name="prod",region="fra1"} 1
# HELP digitalocean_kubernetes_cluster_ha Whether the cluster runs a highly available control plane.
# TYPE digitalocean_kubernetes_cluster_ha gauge
digitalocean_kubernetes_cluster_ha{id="c1",name="prod",region="fra1"} 0
# HELP digitalocean_kubernetes_cluster_surge_upgrade Whether the cluster adds a node before replacing one.
# TYPE digitalocean_kubernetes_cluster_surge_upgrade gauge
digitalocean_kubernetes_cluster_surge_upgrade{id="c1",name="prod",region="fra1"} 0
# HELP digitalocean_kubernetes_cluster_up Whether the cluster state is running.
# TYPE digitalocean_kubernetes_cluster_up gauge
digitalocean_kubernetes_cluster_up{id="c1",name="prod",region="fra1",version="1.35.5-do.1"} 1
# HELP digitalocean_kubernetes_node_pool_auto_scale Whether the pool scales itself between its bounds.
# TYPE digitalocean_kubernetes_node_pool_auto_scale gauge
digitalocean_kubernetes_node_pool_auto_scale{cluster="prod",pool="idle",size="s-2vcpu-4gb"} 0
digitalocean_kubernetes_node_pool_auto_scale{cluster="prod",pool="workers",size="s-4vcpu-8gb"} 1
# HELP digitalocean_kubernetes_node_pool_max_nodes Largest number of nodes the pool may scale to.
# TYPE digitalocean_kubernetes_node_pool_max_nodes gauge
digitalocean_kubernetes_node_pool_max_nodes{cluster="prod",pool="idle",size="s-2vcpu-4gb"} 0
digitalocean_kubernetes_node_pool_max_nodes{cluster="prod",pool="workers",size="s-4vcpu-8gb"} 5
# HELP digitalocean_kubernetes_node_pool_min_nodes Smallest number of nodes the pool may scale to.
# TYPE digitalocean_kubernetes_node_pool_min_nodes gauge
digitalocean_kubernetes_node_pool_min_nodes{cluster="prod",pool="idle",size="s-2vcpu-4gb"} 0
digitalocean_kubernetes_node_pool_min_nodes{cluster="prod",pool="workers",size="s-4vcpu-8gb"} 1
# HELP digitalocean_kubernetes_node_pool_nodes Number of nodes the pool is configured to run.
# TYPE digitalocean_kubernetes_node_pool_nodes gauge
digitalocean_kubernetes_node_pool_nodes{cluster="prod",pool="idle",size="s-2vcpu-4gb"} 0
digitalocean_kubernetes_node_pool_nodes{cluster="prod",pool="workers",size="s-4vcpu-8gb"} 2
# HELP digitalocean_kubernetes_node_pool_nodes_running Number of nodes in the pool reporting the running state.
# TYPE digitalocean_kubernetes_node_pool_nodes_running gauge
digitalocean_kubernetes_node_pool_nodes_running{cluster="prod",pool="idle",size="s-2vcpu-4gb"} 0
digitalocean_kubernetes_node_pool_nodes_running{cluster="prod",pool="workers",size="s-4vcpu-8gb"} 1
`

// newTestCollector wires a collector to a fake DigitalOcean API.
func newTestCollector(t *testing.T, handler http.HandlerFunc) *kubernetes.Collector {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	client, err := doclient.New("token", srv.URL+"/", "test", 5*time.Second,
		doclient.NewMetrics(prometheus.NewRegistry()))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return kubernetes.New(client)
}

func okHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/v2/kubernetes/clusters" {
		_, _ = w.Write([]byte(clustersJSON))
		return
	}
	w.WriteHeader(http.StatusNotFound)
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

// A cluster that is not running, whichever state it is in, reports up 0 and
// still describes its pools.
func TestCollectReportsAClusterThatIsNotRunning(t *testing.T) {
	c := newTestCollector(t, func(w http.ResponseWriter, _ *http.Request) {
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

func TestRefreshFollowsPages(t *testing.T) {
	page := func(id, name string, next bool) string {
		links := `"links":{}`
		if next {
			links = `"links":{"pages":{"next":"https://api.digitalocean.com/v2/kubernetes/clusters?page=2"}}`
		}
		return fmt.Sprintf(`{"kubernetes_clusters":[{"id":%q,"name":%q,"region":"fra1","version":"1.35.5-do.1",`+
			`"status":{"state":"running"},"node_pools":[]}],%s,"meta":{"total":2}}`, id, name, links)
	}

	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write([]byte(page("c2", "second", false)))
			return
		}
		_, _ = w.Write([]byte(page("c1", "first", true)))
	})

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

func TestName(t *testing.T) {
	c := newTestCollector(t, okHandler)
	if got := c.Name(); got != "kubernetes" {
		t.Errorf("Name() = %q, want %q", got, "kubernetes")
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
