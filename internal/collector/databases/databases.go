// Package databases collects the state of the account's managed database
// clusters: whether they are online, how many nodes they run, how much storage
// they hold, whether maintenance is waiting, how many users and logical
// databases they carry, and — behind the details switch — their read-only
// replicas and the age of their newest backup.
//
// What the clusters are actually doing — connections, queries, disk in use —
// is not here. DigitalOcean serves those from a separate Prometheus endpoint
// per cluster, with credentials of its own.
package databases

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/digitalocean/godo"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/kozaktomas/digitalocean_exporter/internal/filter"
	"github.com/kozaktomas/digitalocean_exporter/internal/paging"
)

// databasesPerPage is how many clusters one page request asks for.
const databasesPerPage = 200

// bytesPerMebibyte converts the storage figure the API reports.
const bytesPerMebibyte = 1024 * 1024

// onlineStatus is the cluster status that counts as up.
const onlineStatus = "online"

// knownReplicaStatuses are the replica statuses DigitalOcean documents, which
// are the cluster statuses. Every one of them is reported for every replica on
// every scrape, so an alert has a series for the status it looks for before a
// replica ever enters it: a status that only appears once something is wrong
// is a query returning no data exactly when it matters.
var knownReplicaStatuses = []string{"creating", onlineStatus, "resizing", "migrating", "forking"}

// Metric descriptors. The status and node metrics keep the names and the
// descriptive labels of the older, unmaintained exporter; its three
// maintenance-window labels are deliberately not carried, because a label that
// flips with every pending maintenance ends one series and starts another.
//
// The replica series carry the cluster's id and name and the replica's own
// name, region and status: a replica may live in a different region from its
// cluster, which is much of the point of having one.
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
	usersDesc = prometheus.NewDesc("digitalocean_database_users",
		"Number of database users on the cluster.", []string{"id", "name", "region"}, nil)
	logicalDesc = prometheus.NewDesc("digitalocean_database_databases",
		"Number of logical databases on the cluster.", []string{"id", "name", "region"}, nil)
	autoscaleDesc = prometheus.NewDesc("digitalocean_database_storage_autoscale_enabled",
		"Whether the cluster grows its own storage before it fills.",
		[]string{"id", "name", "region"}, nil)
	infoDesc = prometheus.NewDesc("digitalocean_database_cluster_info",
		"Always 1. Its labels tie the cluster to its project and its VPC.",
		[]string{"id", "name", "region", "project_id", "private_network_uuid"}, nil)
	replicasDesc = prometheus.NewDesc("digitalocean_database_replicas",
		"Number of read-only replicas of the cluster.", []string{"id", "name", "region"}, nil)
	replicaStatusDesc = prometheus.NewDesc("digitalocean_database_replica_status",
		"Always 1 for the replica's current status and 0 for every other known one.",
		[]string{"id", "name", "replica", "region", "status"}, nil)
	backupDesc = prometheus.NewDesc("digitalocean_database_last_backup_timestamp_seconds",
		"Unix time the newest backup of the cluster was taken.",
		[]string{"id", "name", "region"}, nil)
)

// descriptors lists every metric the collector can emit.
var descriptors = []*prometheus.Desc{
	statusDesc, nodesDesc, storageDesc, maintenanceDesc,
	usersDesc, logicalDesc, autoscaleDesc, infoDesc,
	replicasDesc, replicaStatusDesc, backupDesc,
}

// replica is what one refresh learned about a single read-only replica.
type replica struct {
	name   string
	region string
	status string
}

// details is what the two per-cluster lookups learned about a single cluster.
// known separates "never looked up successfully" from "looked up, found
// nothing": the first emits no detail series, the second emits zeros, which are
// answers.
type details struct {
	known    bool
	replicas []replica
	// lastBackup is the Unix time of the newest backup; hasBackup separates a
	// cluster with none — an engine without backups, or one too young to have
	// any — from one whose lookup never succeeded.
	lastBackup float64
	hasBackup  bool
}

// cluster is what one refresh learned about a single database cluster.
type cluster struct {
	id      string
	name    string
	region  string
	size    string
	engine  string
	version string

	projectID          string
	privateNetworkUUID string

	online      float64
	nodes       float64
	storage     float64
	maintenance float64
	users       float64
	logical     float64
	autoscale   float64

	details details
}

// Collector reports the managed database clusters of the account.
type Collector struct {
	client     *godo.Client
	askDetails bool
	filter     filter.Filter
	logger     *slog.Logger

	mu   sync.RWMutex
	snap []cluster
}

// New returns a database collector backed by client, reporting only the
// clusters f matches; a filtered-out cluster is also spared its detail
// lookups. With details set the refresh also asks each cluster for its
// replicas and its backups, which costs two requests per cluster. The logger
// records what the scheduler never sees: a list the endpoint served again
// because it ignored the page it was asked for, and a detail lookup that
// failed for one cluster. A nil logger discards it.
func New(client *godo.Client, details bool, f filter.Filter, logger *slog.Logger) *Collector {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Collector{client: client, askDetails: details, filter: f, logger: logger}
}

// Name implements collector.Collector.
func (c *Collector) Name() string { return "databases" }

// Describe implements collector.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range descriptors {
		ch <- d
	}
}

// Refresh implements collector.Collector. Every page and every detail lookup
// is read before the snapshot is replaced, so a failure halfway through leaves
// the previous clusters in place.
func (c *Collector) Refresh(ctx context.Context) error {
	next, err := c.listClusters(ctx)
	if err != nil {
		return err
	}

	previous := c.previousDetails()
	for i := range next {
		if err := c.refreshDetails(ctx, &next[i], previous); err != nil {
			return err
		}
	}

	c.mu.Lock()
	c.snap = next
	c.mu.Unlock()
	return nil
}

// listClusters reads every page of the cluster list.
func (c *Collector) listClusters(ctx context.Context) ([]cluster, error) {
	opts := &godo.ListOptions{Page: 1, PerPage: databasesPerPage}
	var next []cluster
	seen := make(map[string]struct{})

	for {
		page, _, err := c.client.Databases.List(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("list databases: %w", err)
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
			// The filter sits after the duplicate bookkeeping on purpose: a
			// filtered-out cluster still marks the point where the list
			// starts repeating.
			if !c.filter.Match(page[i].Tags, page[i].RegionSlug) {
				continue
			}
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
	return next, nil
}

// previousDetails returns what the last refresh knew about each cluster's
// replicas and backups, keyed by cluster id, so a lookup that fails this time
// can keep reporting it.
func (c *Collector) previousDetails() map[string]details {
	c.mu.RLock()
	defer c.mu.RUnlock()

	previous := make(map[string]details, len(c.snap))
	for _, cl := range c.snap {
		previous[cl.id] = cl.details
	}
	return previous
}

// refreshDetails fills in the replicas and the newest backup of cl.
//
// One cluster's lookup failing is not the refresh failing: the cluster keeps
// the details the last successful lookup found and the failure is logged,
// because nothing else reports it. Running out of time is the other case — the
// deadline belongs to the whole refresh, so there is no point asking about the
// clusters that are left, and the error is returned as any other.
func (c *Collector) refreshDetails(ctx context.Context, cl *cluster, previous map[string]details) error {
	if !c.askDetails {
		return nil
	}

	d, err := c.lookupDetails(ctx, cl.id)
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		cl.details = previous[cl.id]
		c.logger.Warn("database detail lookup failed",
			"cluster", cl.name, "cluster_id", cl.id, "err", err)
		return nil
	}
	cl.details = d
	return nil
}

// lookupDetails asks one cluster for its replicas and its backups. The two are
// looked up together and fail together, so a cluster's details are always one
// refresh's answer rather than half of one and half of another.
func (c *Collector) lookupDetails(ctx context.Context, id string) (details, error) {
	replicas, err := c.listReplicas(ctx, id)
	if err != nil {
		return details{}, err
	}

	d := details{known: true, replicas: replicas}
	backups, err := c.listBackups(ctx, id)
	if err != nil {
		return details{}, err
	}
	for _, b := range backups {
		if at := float64(b.CreatedAt.Unix()); !d.hasBackup || at > d.lastBackup {
			d.lastBackup, d.hasBackup = at, true
		}
	}
	return d, nil
}

// listReplicas reads every replica of one cluster. An engine that does not
// offer replicas answers with a client error, which is an answer — the cluster
// has none — not a failure.
func (c *Collector) listReplicas(ctx context.Context, id string) ([]replica, error) {
	page, err := paging.All(ctx, c.logger, "database replicas",
		func(r godo.DatabaseReplica) string { return r.ID },
		func(ctx context.Context, opts *godo.ListOptions) ([]godo.DatabaseReplica, *godo.Response, error) {
			return c.client.Databases.ListReplicas(ctx, id, opts)
		})
	if err != nil {
		if engineDoesNotOffer(err) {
			return nil, nil
		}
		return nil, err
	}

	replicas := make([]replica, 0, len(page))
	for _, r := range page {
		replicas = append(replicas, replica{name: r.Name, region: r.Region, status: r.Status})
	}
	return replicas, nil
}

// listBackups reads every backup of one cluster. As with replicas, an engine
// without backups answers with a client error and that reads as "no backups".
func (c *Collector) listBackups(ctx context.Context, id string) ([]godo.DatabaseBackup, error) {
	backups, err := paging.All(ctx, c.logger, "database backups",
		func(b godo.DatabaseBackup) string { return b.CreatedAt.String() },
		func(ctx context.Context, opts *godo.ListOptions) ([]godo.DatabaseBackup, *godo.Response, error) {
			return c.client.Databases.ListBackups(ctx, id, opts)
		})
	if err != nil {
		if engineDoesNotOffer(err) {
			return nil, nil
		}
		return nil, err
	}
	return backups, nil
}

// engineDoesNotOffer reports whether err is the API answering definitively
// that the cluster does not have the endpoint — a caching cluster asked for
// backups, say. Only the codes that mean "this resource has no such thing" are
// read that way; an expired token or a rate limit answers with a client error
// too, and treating those as "no backups" would report every backup fine while
// nothing was being asked at all.
func engineDoesNotOffer(err error) bool {
	var respErr *godo.ErrorResponse
	if !errors.As(err, &respErr) || respErr.Response == nil {
		return false
	}
	switch respErr.Response.StatusCode {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusPreconditionFailed:
		return true
	default:
		return false
	}
}

// newCluster converts one API cluster into its snapshot form.
func newCluster(db *godo.Database) cluster {
	out := cluster{
		id:                 db.ID,
		name:               db.Name,
		region:             db.RegionSlug,
		size:               db.SizeSlug,
		engine:             db.EngineSlug,
		version:            db.VersionSlug,
		projectID:          db.ProjectID,
		privateNetworkUUID: db.PrivateNetworkUUID,
		online:             boolToFloat(strings.EqualFold(db.Status, onlineStatus)),
		nodes:              float64(db.NumNodes),
		storage:            float64(db.StorageSizeMib) * bytesPerMebibyte,
		users:              float64(len(db.Users)),
		logical:            float64(len(db.DBNames)),
	}
	if db.MaintenanceWindow != nil {
		out.maintenance = boolToFloat(db.MaintenanceWindow.Pending)
	}
	if db.StorageAutoscale != nil {
		out.autoscale = boolToFloat(db.StorageAutoscale.Enabled)
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
		gauge(ch, usersDesc, db.users, short...)
		gauge(ch, logicalDesc, db.logical, short...)
		gauge(ch, autoscaleDesc, db.autoscale, short...)
		gauge(ch, infoDesc, 1, db.id, db.name, db.region, db.projectID, db.privateNetworkUUID)
		collectDetails(ch, db)
	}
}

// collectDetails emits the replica and backup metrics of one cluster. A
// cluster whose detail lookup has never succeeded — because the collector was
// started with details switched off, or because every attempt so far failed —
// emits nothing rather than zeros, which would read as "no replicas and no
// backups" while nothing had been asked.
func collectDetails(ch chan<- prometheus.Metric, db cluster) {
	if !db.details.known {
		return
	}

	gauge(ch, replicasDesc, float64(len(db.details.replicas)), db.id, db.name, db.region)
	for _, r := range db.details.replicas {
		collectReplica(ch, db, r)
	}
	if db.details.hasBackup {
		gauge(ch, backupDesc, db.details.lastBackup, db.id, db.name, db.region)
	}
}

// collectReplica emits the status of a single replica.
//
// A status DigitalOcean has invented since this was written is reported beside
// the documented ones: left out, it would make every series of that replica
// read 0, which is indistinguishable from the replica having disappeared.
func collectReplica(ch chan<- prometheus.Metric, db cluster, r replica) {
	statuses := knownReplicaStatuses
	if r.status != "" && !slices.Contains(statuses, r.status) {
		statuses = append(slices.Clone(statuses), r.status)
	}
	for _, status := range statuses {
		gauge(ch, replicaStatusDesc, boolToFloat(status == r.status),
			db.id, db.name, r.name, r.region, status)
	}
}

// gauge sends one gauge sample of desc with the given label values.
func gauge(ch chan<- prometheus.Metric, desc *prometheus.Desc, value float64, labels ...string) {
	ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, value, labels...)
}
