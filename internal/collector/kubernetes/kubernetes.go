// Package kubernetes collects the state of the account's managed Kubernetes
// clusters and of every node pool in them.
//
// What runs inside a cluster is kube-state-metrics' job. This is the view from
// outside: is the cluster running, will it upgrade itself, and does each pool
// hold the nodes it is configured to hold.
package kubernetes

import (
	"context"
	"fmt"
	"sync"

	"github.com/digitalocean/godo"
	"github.com/prometheus/client_golang/prometheus"
)

// clustersPerPage is how many clusters one page request asks for.
const clustersPerPage = 200

// runningState is the cluster and node state that counts as up.
const runningState = "running"

// Metric descriptors. digitalocean_kubernetes_cluster_up keeps the name and
// labels of the older, unmaintained exporter. The pool metrics do not: that
// exporter labels a pool by its own id and name and drops the cluster, which
// leaves no way to tell whose pool it is.
//
// A pool carries both the cluster's id and its name. The name is what a
// dashboard variable and a summary line read; the id is what joins a pool to
// the cluster metrics, which are labelled by id, and it survives a cluster
// being renamed.
var (
	upDesc = prometheus.NewDesc("digitalocean_kubernetes_cluster_up",
		"Whether the cluster state is running.",
		[]string{"id", "name", "region", "version"}, nil)
	autoUpgradeDesc = prometheus.NewDesc("digitalocean_kubernetes_cluster_auto_upgrade",
		"Whether the cluster upgrades itself in its maintenance window.",
		[]string{"id", "name", "region"}, nil)
	surgeUpgradeDesc = prometheus.NewDesc("digitalocean_kubernetes_cluster_surge_upgrade",
		"Whether the cluster adds a node before replacing one.",
		[]string{"id", "name", "region"}, nil)
	haDesc = prometheus.NewDesc("digitalocean_kubernetes_cluster_ha",
		"Whether the cluster runs a highly available control plane.",
		[]string{"id", "name", "region"}, nil)
	poolNodesDesc = prometheus.NewDesc("digitalocean_kubernetes_node_pool_nodes",
		"Number of nodes the pool is configured to run.",
		[]string{"cluster_id", "cluster", "pool", "size"}, nil)
	poolRunningDesc = prometheus.NewDesc("digitalocean_kubernetes_node_pool_nodes_running",
		"Number of nodes in the pool reporting the running state.",
		[]string{"cluster_id", "cluster", "pool", "size"}, nil)
	poolAutoScaleDesc = prometheus.NewDesc("digitalocean_kubernetes_node_pool_auto_scale",
		"Whether the pool scales itself between its bounds.",
		[]string{"cluster_id", "cluster", "pool", "size"}, nil)
	poolMinDesc = prometheus.NewDesc("digitalocean_kubernetes_node_pool_min_nodes",
		"Smallest number of nodes the pool may scale to.",
		[]string{"cluster_id", "cluster", "pool", "size"}, nil)
	poolMaxDesc = prometheus.NewDesc("digitalocean_kubernetes_node_pool_max_nodes",
		"Largest number of nodes the pool may scale to.",
		[]string{"cluster_id", "cluster", "pool", "size"}, nil)
)

// descriptors lists every metric the collector can emit.
var descriptors = []*prometheus.Desc{
	upDesc, autoUpgradeDesc, surgeUpgradeDesc, haDesc,
	poolNodesDesc, poolRunningDesc, poolAutoScaleDesc, poolMinDesc, poolMaxDesc,
}

// pool is what one refresh learned about a single node pool.
type pool struct {
	name      string
	size      string
	nodes     float64
	running   float64
	autoScale float64
	minNodes  float64
	maxNodes  float64
}

// cluster is what one refresh learned about a single cluster.
type cluster struct {
	id      string
	name    string
	region  string
	version string

	up           float64
	autoUpgrade  float64
	surgeUpgrade float64
	ha           float64
	pools        []pool
}

// Collector reports the managed Kubernetes clusters of the account.
type Collector struct {
	client *godo.Client

	mu   sync.RWMutex
	snap []cluster
}

// New returns a Kubernetes collector backed by client.
func New(client *godo.Client) *Collector {
	return &Collector{client: client}
}

// Name implements collector.Collector.
func (c *Collector) Name() string { return "kubernetes" }

// Describe implements collector.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range descriptors {
		ch <- d
	}
}

// Refresh implements collector.Collector. Every page is read before the
// snapshot is replaced, so a failure halfway through leaves the previous
// clusters in place.
func (c *Collector) Refresh(ctx context.Context) error {
	opts := &godo.ListOptions{PerPage: clustersPerPage}
	var next []cluster

	for {
		page, resp, err := c.client.Kubernetes.List(ctx, opts)
		if err != nil {
			return fmt.Errorf("list kubernetes clusters: %w", err)
		}
		for _, kc := range page {
			next = append(next, newCluster(kc))
		}

		if resp == nil || resp.Links == nil || resp.Links.IsLastPage() {
			break
		}
		current, err := resp.Links.CurrentPage()
		if err != nil {
			return fmt.Errorf("next page of kubernetes clusters: %w", err)
		}
		opts.Page = current + 1
	}

	c.mu.Lock()
	c.snap = next
	c.mu.Unlock()
	return nil
}

// newCluster converts one API cluster into its snapshot form.
func newCluster(kc *godo.KubernetesCluster) cluster {
	out := cluster{
		id:           kc.ID,
		name:         kc.Name,
		region:       kc.RegionSlug,
		version:      kc.VersionSlug,
		autoUpgrade:  boolToFloat(kc.AutoUpgrade),
		surgeUpgrade: boolToFloat(kc.SurgeUpgrade),
		ha:           boolToFloat(kc.HA),
		pools:        make([]pool, 0, len(kc.NodePools)),
	}
	if kc.Status != nil {
		out.up = boolToFloat(string(kc.Status.State) == runningState)
	}
	for _, np := range kc.NodePools {
		out.pools = append(out.pools, newPool(np))
	}
	return out
}

// newPool converts one API node pool into its snapshot form. The configured
// count and the running nodes are kept apart: a pool waiting for a node to
// come up reports the two apart, which is the moment worth seeing.
func newPool(np *godo.KubernetesNodePool) pool {
	out := pool{
		name:      np.Name,
		size:      np.Size,
		nodes:     float64(np.Count),
		autoScale: boolToFloat(np.AutoScale),
		minNodes:  float64(np.MinNodes),
		maxNodes:  float64(np.MaxNodes),
	}
	for _, node := range np.Nodes {
		if node.Status != nil && node.Status.State == runningState {
			out.running++
		}
	}
	return out
}

// boolToFloat maps a boolean to the 1/0 convention Prometheus expects.
func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// Collect implements collector.Collector. Before the first successful refresh,
// and on an account with no cluster, it emits nothing.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	snap := c.snap
	c.mu.RUnlock()

	for _, cl := range snap {
		gauge(ch, upDesc, cl.up, cl.id, cl.name, cl.region, cl.version)
		gauge(ch, autoUpgradeDesc, cl.autoUpgrade, cl.id, cl.name, cl.region)
		gauge(ch, surgeUpgradeDesc, cl.surgeUpgrade, cl.id, cl.name, cl.region)
		gauge(ch, haDesc, cl.ha, cl.id, cl.name, cl.region)
		collectPools(ch, cl)
	}
}

// collectPools emits the per-pool metrics of one cluster.
func collectPools(ch chan<- prometheus.Metric, cl cluster) {
	for _, p := range cl.pools {
		labels := []string{cl.id, cl.name, p.name, p.size}
		gauge(ch, poolNodesDesc, p.nodes, labels...)
		gauge(ch, poolRunningDesc, p.running, labels...)
		gauge(ch, poolAutoScaleDesc, p.autoScale, labels...)
		gauge(ch, poolMinDesc, p.minNodes, labels...)
		gauge(ch, poolMaxDesc, p.maxNodes, labels...)
	}
}

// gauge sends one gauge sample of desc with the given label values.
func gauge(ch chan<- prometheus.Metric, desc *prometheus.Desc, value float64, labels ...string) {
	ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, value, labels...)
}
