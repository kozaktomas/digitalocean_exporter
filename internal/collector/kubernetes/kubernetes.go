// Package kubernetes collects the state of the account's managed Kubernetes
// clusters, of every node pool in them and of every node in those pools.
//
// What runs inside a cluster is kube-state-metrics' job. This is the view from
// outside: is the cluster running, will it upgrade itself, is there a newer
// version on offer, and does each pool hold the nodes it is configured to hold.
package kubernetes

import (
	"context"
	"log/slog"
	"slices"
	"sync"

	"github.com/digitalocean/godo"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/kozaktomas/digitalocean_exporter/internal/paging"
)

// runningState is the cluster and node state that counts as up.
const runningState = "running"

// knownNodeStates are the node states DigitalOcean documents. Every one of them
// is reported for every node on every scrape, so an alert or a panel has a
// series for the state it looks for before a node ever enters it: a state that
// only appears once something is wrong is a query returning no data exactly
// when it matters.
var knownNodeStates = []string{"provisioning", runningState, "draining", "deleting"}

// Metric descriptors. digitalocean_kubernetes_cluster_up keeps the name and
// labels of the older, unmaintained exporter, and the cluster booleans beside
// it follow its id/name convention. The pool and node metrics do not: that
// exporter labels a pool by its own id and name and drops the cluster, which
// leaves no way to tell whose pool it is.
//
// A pool carries both the cluster's id and its name, and its own id and name
// for the same reason. A name is what a dashboard variable and a summary line
// read; an id is what joins the series together, and it is the half that
// survives the thing being renamed.
//
// Everything the cluster list already answers is reported from it, since it
// costs nothing to: whether the registry is integrated is a boolean like the
// three before it, while the maintenance window is two strings and can only be
// labels, which is what cluster_info is for.
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
	registryDesc = prometheus.NewDesc("digitalocean_kubernetes_cluster_registry_enabled",
		"Whether the account's container registry is integrated with the cluster.",
		[]string{"id", "name", "region"}, nil)
	clusterInfoDesc = prometheus.NewDesc("digitalocean_kubernetes_cluster_info",
		"Always 1. Its labels describe the cluster's version and maintenance window.",
		[]string{"id", "name", "region", "version", "maintenance_day", "maintenance_start_time"}, nil)
	upgradeAvailableDesc = prometheus.NewDesc("digitalocean_kubernetes_cluster_upgrade_available",
		"Whether DigitalOcean offers the cluster at least one newer version.",
		[]string{"cluster_id", "cluster"}, nil)
	availableVersionDesc = prometheus.NewDesc("digitalocean_kubernetes_cluster_available_version_info",
		"Always 1, once per version the cluster can be upgraded to.",
		[]string{"cluster_id", "cluster", "version"}, nil)
	poolNodesDesc = prometheus.NewDesc("digitalocean_kubernetes_node_pool_nodes",
		"Number of nodes the pool is configured to run.",
		[]string{"cluster_id", "cluster", "pool_id", "pool", "size"}, nil)
	poolRunningDesc = prometheus.NewDesc("digitalocean_kubernetes_node_pool_nodes_running",
		"Number of nodes in the pool reporting the running state.",
		[]string{"cluster_id", "cluster", "pool_id", "pool", "size"}, nil)
	poolAutoScaleDesc = prometheus.NewDesc("digitalocean_kubernetes_node_pool_auto_scale",
		"Whether the pool scales itself between its bounds.",
		[]string{"cluster_id", "cluster", "pool_id", "pool", "size"}, nil)
	poolMinDesc = prometheus.NewDesc("digitalocean_kubernetes_node_pool_min_nodes",
		"Smallest number of nodes the pool may scale to.",
		[]string{"cluster_id", "cluster", "pool_id", "pool", "size"}, nil)
	poolMaxDesc = prometheus.NewDesc("digitalocean_kubernetes_node_pool_max_nodes",
		"Largest number of nodes the pool may scale to.",
		[]string{"cluster_id", "cluster", "pool_id", "pool", "size"}, nil)
	nodeStateDesc = prometheus.NewDesc("digitalocean_kubernetes_node_state",
		"Always 1 for the node's current state and 0 for every other known one.",
		[]string{"cluster_id", "cluster", "pool_id", "pool", "node_id", "node", "state"}, nil)
	nodeInfoDesc = prometheus.NewDesc("digitalocean_kubernetes_node_info",
		"Always 1. Its labels tie the node to the droplet underneath it.",
		[]string{"cluster_id", "cluster", "pool_id", "pool", "node_id", "node", "droplet_id"}, nil)
)

// descriptors lists every metric the collector can emit.
var descriptors = []*prometheus.Desc{
	upDesc, autoUpgradeDesc, surgeUpgradeDesc, haDesc, registryDesc, clusterInfoDesc,
	upgradeAvailableDesc, availableVersionDesc,
	poolNodesDesc, poolRunningDesc, poolAutoScaleDesc, poolMinDesc, poolMaxDesc,
	nodeStateDesc, nodeInfoDesc,
}

// node is what one refresh learned about a single node of a pool. The status
// message beside the state is free text DigitalOcean writes for a person to
// read; it is deliberately not a label, because it changes without the node
// changing and every wording of it would start a new series.
type node struct {
	id        string
	name      string
	dropletID string
	state     string
}

// pool is what one refresh learned about a single node pool.
type pool struct {
	id        string
	name      string
	size      string
	nodes     float64
	running   float64
	autoScale float64
	minNodes  float64
	maxNodes  float64
	members   []node
}

// upgrades is what one refresh learned about the versions a cluster can move
// to. known separates "no upgrade is offered" from "nothing has been asked
// yet": the first is a metric worth reporting, the second is not.
type upgrades struct {
	known    bool
	versions []string
}

// cluster is what one refresh learned about a single cluster.
type cluster struct {
	id      string
	name    string
	region  string
	version string

	maintenanceDay       string
	maintenanceStartTime string

	up           float64
	autoUpgrade  float64
	surgeUpgrade float64
	ha           float64
	registry     float64
	pools        []pool
	upgrades     upgrades
}

// Collector reports the managed Kubernetes clusters of the account.
type Collector struct {
	client      *godo.Client
	askUpgrades bool
	logger      *slog.Logger

	mu   sync.RWMutex
	snap []cluster
}

// New returns a Kubernetes collector backed by client. With upgrades set the
// refresh also asks what each cluster could be upgraded to, which costs one
// request per cluster. The logger records what the scheduler never sees: a
// duplicate cluster dropped from a list that shifted between two page
// requests, and an upgrades lookup that failed for one cluster. A nil logger
// discards it.
func New(client *godo.Client, upgrades bool, logger *slog.Logger) *Collector {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Collector{client: client, askUpgrades: upgrades, logger: logger}
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
	clusters, err := paging.All(ctx, c.logger, "kubernetes clusters",
		func(kc *godo.KubernetesCluster) string { return kc.ID }, c.client.Kubernetes.List)
	if err != nil {
		return err
	}

	previous := c.previousUpgrades()
	next := make([]cluster, 0, len(clusters))
	for _, kc := range clusters {
		cl := newCluster(kc)
		if err := c.refreshUpgrades(ctx, &cl, previous); err != nil {
			return err
		}
		next = append(next, cl)
	}

	c.mu.Lock()
	c.snap = next
	c.mu.Unlock()
	return nil
}

// previousUpgrades returns what the last refresh knew about each cluster's
// available versions, keyed by cluster id, so a lookup that fails this time can
// keep reporting it.
func (c *Collector) previousUpgrades() map[string]upgrades {
	c.mu.RLock()
	defer c.mu.RUnlock()

	previous := make(map[string]upgrades, len(c.snap))
	for _, cl := range c.snap {
		previous[cl.id] = cl.upgrades
	}
	return previous
}

// refreshUpgrades fills in the versions cl can be upgraded to.
//
// One cluster's lookup failing is not the refresh failing: the cluster keeps
// the versions the last successful lookup found and the failure is logged,
// because nothing else reports it. Running out of time is the other case — the
// deadline belongs to the whole refresh, so there is no point asking about the
// clusters that are left, and the error is returned as any other.
func (c *Collector) refreshUpgrades(ctx context.Context, cl *cluster, previous map[string]upgrades) error {
	if !c.askUpgrades {
		return nil
	}

	versions, _, err := c.client.Kubernetes.GetUpgrades(ctx, cl.id)
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		cl.upgrades = previous[cl.id]
		c.logger.Warn("kubernetes upgrades lookup failed",
			"cluster", cl.name, "cluster_id", cl.id, "err", err)
		return nil
	}

	slugs := make([]string, 0, len(versions))
	for _, v := range versions {
		if v != nil && v.Slug != "" {
			slugs = append(slugs, v.Slug)
		}
	}
	cl.upgrades = upgrades{known: true, versions: slugs}
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
		registry:     boolToFloat(kc.RegistryEnabled),
		pools:        make([]pool, 0, len(kc.NodePools)),
	}
	if kc.Status != nil {
		out.up = boolToFloat(string(kc.Status.State) == runningState)
	}
	if kc.MaintenancePolicy != nil {
		out.maintenanceDay = kc.MaintenancePolicy.Day.String()
		out.maintenanceStartTime = kc.MaintenancePolicy.StartTime
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
		id:        np.ID,
		name:      np.Name,
		size:      np.Size,
		nodes:     float64(np.Count),
		autoScale: boolToFloat(np.AutoScale),
		minNodes:  float64(np.MinNodes),
		maxNodes:  float64(np.MaxNodes),
		members:   make([]node, 0, len(np.Nodes)),
	}
	for _, n := range np.Nodes {
		if n == nil {
			continue
		}
		member := node{id: n.ID, name: n.Name, dropletID: n.DropletID}
		if n.Status != nil {
			member.state = n.Status.State
		}
		if member.state == runningState {
			out.running++
		}
		out.members = append(out.members, member)
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
		gauge(ch, registryDesc, cl.registry, cl.id, cl.name, cl.region)
		gauge(ch, clusterInfoDesc, 1,
			cl.id, cl.name, cl.region, cl.version, cl.maintenanceDay, cl.maintenanceStartTime)
		collectUpgrades(ch, cl)
		collectPools(ch, cl)
	}
}

// collectUpgrades emits what the cluster can be upgraded to. A cluster whose
// lookup has never succeeded — because the collector was started with the
// lookup switched off, or because every attempt so far failed — emits nothing
// rather than a zero, which would read as "you are on the newest version".
func collectUpgrades(ch chan<- prometheus.Metric, cl cluster) {
	if !cl.upgrades.known {
		return
	}

	gauge(ch, upgradeAvailableDesc, boolToFloat(len(cl.upgrades.versions) > 0), cl.id, cl.name)
	for _, version := range cl.upgrades.versions {
		gauge(ch, availableVersionDesc, 1, cl.id, cl.name, version)
	}
}

// collectPools emits the per-pool and per-node metrics of one cluster.
func collectPools(ch chan<- prometheus.Metric, cl cluster) {
	for _, p := range cl.pools {
		labels := []string{cl.id, cl.name, p.id, p.name, p.size}
		gauge(ch, poolNodesDesc, p.nodes, labels...)
		gauge(ch, poolRunningDesc, p.running, labels...)
		gauge(ch, poolAutoScaleDesc, p.autoScale, labels...)
		gauge(ch, poolMinDesc, p.minNodes, labels...)
		gauge(ch, poolMaxDesc, p.maxNodes, labels...)
		for _, n := range p.members {
			collectNode(ch, cl, p, n)
		}
	}
}

// collectNode emits the state and the identity of a single node.
//
// A state DigitalOcean has invented since this was written is reported beside
// the documented ones: left out, it would make every series of that node read
// 0, which is indistinguishable from the node having disappeared.
func collectNode(ch chan<- prometheus.Metric, cl cluster, p pool, n node) {
	labels := []string{cl.id, cl.name, p.id, p.name, n.id, n.name}

	states := knownNodeStates
	if n.state != "" && !slices.Contains(states, n.state) {
		states = append(slices.Clone(states), n.state)
	}
	for _, state := range states {
		gauge(ch, nodeStateDesc, boolToFloat(state == n.state), append(slices.Clone(labels), state)...)
	}
	gauge(ch, nodeInfoDesc, 1, append(slices.Clone(labels), n.dropletID)...)
}

// gauge sends one gauge sample of desc with the given label values.
func gauge(ch chan<- prometheus.Metric, desc *prometheus.Desc, value float64, labels ...string) {
	ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, value, labels...)
}
