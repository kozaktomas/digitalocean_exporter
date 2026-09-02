// Package databases collects the state of the account's managed database
// clusters: whether they are online, how many nodes they run, how much storage
// they hold and whether maintenance is waiting.
//
// What the clusters are actually doing — connections, queries, disk in use —
// is not here. DigitalOcean serves those from a separate Prometheus endpoint
// per cluster, with credentials of its own.
package databases

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/digitalocean/godo"
	"github.com/prometheus/client_golang/prometheus"
)

// databasesPerPage is how many clusters one page request asks for.
const databasesPerPage = 200

// bytesPerMebibyte converts the storage figure the API reports.
const bytesPerMebibyte = 1024 * 1024

// onlineStatus is the cluster status that counts as up.
const onlineStatus = "online"

// Metric descriptors. The status and node metrics keep the names and the
// descriptive labels of the older, unmaintained exporter; its three
// maintenance-window labels are deliberately not carried, because a label that
// flips with every pending maintenance ends one series and starts another.
var (
	statusDesc = prometheus.NewDesc("digitalocean_database_status",
		"Whether the cluster is online.",
		[]string{"id", "name", "region", "size", "engine", "version"}, nil)
	nodesDesc = prometheus.NewDesc("digitalocean_database_nodes",
		"Number of nodes in the cluster.",
		[]string{"id", "name", "region", "size", "engine", "version"}, nil)
	storageDesc = prometheus.NewDesc("digitalocean_database_storage_bytes",
		"Storage allocated to the cluster.", []string{"id", "name", "region"}, nil)
	maintenanceDesc = prometheus.NewDesc("digitalocean_database_maintenance_pending",
		"Whether maintenance is pending for the cluster.", []string{"id", "name", "region"}, nil)
)

// descriptors lists every metric the collector can emit.
var descriptors = []*prometheus.Desc{statusDesc, nodesDesc, storageDesc, maintenanceDesc}

// cluster is what one refresh learned about a single database cluster.
type cluster struct {
	id      string
	name    string
	region  string
	size    string
	engine  string
	version string

	online      float64
	nodes       float64
	storage     float64
	maintenance float64
}

// Collector reports the managed database clusters of the account.
type Collector struct {
	client *godo.Client
	logger *slog.Logger

	mu   sync.RWMutex
	snap []cluster
}

// New returns a database collector backed by client. The logger records what
// the scheduler never sees: a list the endpoint served again because it
// ignored the page it was asked for. A nil logger discards it.
func New(client *godo.Client, logger *slog.Logger) *Collector {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Collector{client: client, logger: logger}
}

// Name implements collector.Collector.
func (c *Collector) Name() string { return "databases" }

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
	opts := &godo.ListOptions{Page: 1, PerPage: databasesPerPage}
	var next []cluster
	seen := make(map[string]struct{})

	for {
		page, _, err := c.client.Databases.List(ctx, opts)
		if err != nil {
			return fmt.Errorf("list databases: %w", err)
		}

		// A cluster that was already on an earlier page means the endpoint
		// served the same list again: it documents no paging, and an
		// implementation that ignores the page parameter would otherwise be
		// asked for page 2, 3 and so on until the refresh died on its
		// deadline. The repeat is where the list ends.
		repeated := false
		for i := range page {
			if _, dup := seen[page[i].ID]; dup {
				repeated = true
				break
			}
			seen[page[i].ID] = struct{}{}
			next = append(next, newCluster(&page[i]))
		}
		if repeated {
			c.logger.Debug("stopped listing databases at a repeated cluster",
				"page", opts.Page, "clusters", len(next))
			break
		}

		// godo drops the pagination links of this endpoint, so a full page is
		// the only signal that another one may follow. The cost of being wrong
		// is one empty request on an account whose clusters divide exactly.
		if len(page) < databasesPerPage {
			break
		}
		opts.Page++
	}

	c.mu.Lock()
	c.snap = next
	c.mu.Unlock()
	return nil
}

// newCluster converts one API cluster into its snapshot form.
func newCluster(db *godo.Database) cluster {
	out := cluster{
		id:      db.ID,
		name:    db.Name,
		region:  db.RegionSlug,
		size:    db.SizeSlug,
		engine:  db.EngineSlug,
		version: db.VersionSlug,
		online:  boolToFloat(strings.EqualFold(db.Status, onlineStatus)),
		nodes:   float64(db.NumNodes),
		storage: float64(db.StorageSizeMib) * bytesPerMebibyte,
	}
	if db.MaintenanceWindow != nil {
		out.maintenance = boolToFloat(db.MaintenanceWindow.Pending)
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
// and on an account with no managed database, it emits nothing.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	snap := c.snap
	c.mu.RUnlock()

	for _, db := range snap {
		full := []string{db.id, db.name, db.region, db.size, db.engine, db.version}
		short := []string{db.id, db.name, db.region}
		gauge(ch, statusDesc, db.online, full...)
		gauge(ch, nodesDesc, db.nodes, full...)
		gauge(ch, storageDesc, db.storage, short...)
		gauge(ch, maintenanceDesc, db.maintenance, short...)
	}
}

// gauge sends one gauge sample of desc with the given label values.
func gauge(ch chan<- prometheus.Metric, desc *prometheus.Desc, value float64, labels ...string) {
	ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, value, labels...)
}
