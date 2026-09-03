// Package droplets collects one set of metrics per droplet: whether it is up,
// what it is made of and what it costs.
//
// The metric names match the older, widely deployed DigitalOcean exporter, so
// dashboards survive a migration. Its disk figure does not: this collector
// reads DigitalOcean's gigabytes as binary, the same way it reads the memory
// figure, which makes digitalocean_droplet_disk_bytes about 7% larger here.
//
// Backups, the monitoring agent, the creation time, the VPC and the tags all
// arrive in the same list response as the rest, so reporting them costs no
// extra request. The VPC is a label rather than a metric because it is what
// joins a droplet to the load balancer in front of it, which exports the same
// vpc_uuid.
package droplets

import (
	"context"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/digitalocean/godo"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/kozaktomas/digitalocean_exporter/internal/filter"
	"github.com/kozaktomas/digitalocean_exporter/internal/paging"
)

// The API reports memory in mebibytes and disk in gibibytes.
const (
	bytesPerMebibyte = 1024 * 1024
	bytesPerGibibyte = 1024 * 1024 * 1024
)

// activeStatus is the droplet status that counts as up.
const activeStatus = "active"

// The droplet features that are worth a metric of their own. They arrive in
// the same list request as everything else here, so reading them costs nothing.
const (
	backupsFeature    = "backups"
	monitoringFeature = "monitoring"
)

// Metric descriptors. Their label sets match the older exporter exactly, so
// the descriptive labels live on an info metric of their own rather than
// widening these.
var (
	upDesc = prometheus.NewDesc("digitalocean_droplet_up",
		"Whether the droplet is active.", []string{"id", "name", "region"}, nil)
	cpusDesc = prometheus.NewDesc("digitalocean_droplet_cpus",
		"Number of virtual CPUs of the droplet.", []string{"id", "name", "region"}, nil)
	memoryDesc = prometheus.NewDesc("digitalocean_droplet_memory_bytes",
		"Memory of the droplet.", []string{"id", "name", "region"}, nil)
	diskDesc = prometheus.NewDesc("digitalocean_droplet_disk_bytes",
		"Disk of the droplet.", []string{"id", "name", "region"}, nil)
	priceHourlyDesc = prometheus.NewDesc("digitalocean_droplet_price_hourly",
		"Price of the droplet per hour in US dollars.", []string{"id", "name", "region"}, nil)
	priceMonthlyDesc = prometheus.NewDesc("digitalocean_droplet_price_monthly",
		"Price of the droplet per month in US dollars.", []string{"id", "name", "region"}, nil)
	backupsDesc = prometheus.NewDesc("digitalocean_droplet_backups_enabled",
		"Whether DigitalOcean's automatic backups are enabled for the droplet.",
		[]string{"id", "name", "region"}, nil)
	monitoringAgentDesc = prometheus.NewDesc("digitalocean_droplet_monitoring_agent",
		"Whether the droplet carries DigitalOcean's monitoring agent.",
		[]string{"id", "name", "region"}, nil)
	createdDesc = prometheus.NewDesc("digitalocean_droplet_created_timestamp_seconds",
		"When the droplet was created, as a Unix timestamp.",
		[]string{"id", "name", "region"}, nil)
	infoDesc = prometheus.NewDesc("digitalocean_droplet_info",
		"Always 1. Its labels describe the droplet's size, status, image, VPC and tags.",
		[]string{"id", "name", "region", "size", "status", "image", "vpc_uuid", "tags"}, nil)
)

// descriptors lists every metric the collector can emit.
var descriptors = []*prometheus.Desc{
	upDesc, cpusDesc, memoryDesc, diskDesc, priceHourlyDesc, priceMonthlyDesc,
	backupsDesc, monitoringAgentDesc, createdDesc, infoDesc,
}

// droplet is what one refresh learned about a single droplet.
type droplet struct {
	id      string
	name    string
	region  string
	size    string
	status  string
	image   string
	vpcUUID string
	tags    string

	up              float64
	cpus            float64
	memory          float64
	disk            float64
	priceHourly     float64
	priceMonthly    float64
	backups         float64
	monitoringAgent float64

	// created is false for a droplet whose creation time the API did not
	// return or returned unparseably. Such a droplet emits no timestamp at
	// all rather than a zero, which would place it in 1970 and make its age
	// pass every threshold.
	created   bool
	createdAt float64
}

// Collector reports the droplets of the account, or, with a filter set, only
// the droplets that pass it.
type Collector struct {
	client *godo.Client
	filter filter.Filter
	logger *slog.Logger

	mu   sync.RWMutex
	snap []droplet
}

// New returns a droplet collector backed by client, reporting only the
// droplets f matches. The logger records what the scheduler never sees: a
// duplicate droplet dropped from a list that shifted between two page
// requests. A nil logger discards it.
func New(client *godo.Client, f filter.Filter, logger *slog.Logger) *Collector {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Collector{client: client, filter: f, logger: logger}
}

// Name implements collector.Collector.
func (c *Collector) Name() string { return "droplets" }

// Describe implements collector.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range descriptors {
		ch <- d
	}
}

// Refresh implements collector.Collector. Every page is read before the
// snapshot is replaced, so a failure halfway through the list leaves the
// previous droplets in place rather than reporting half an account.
func (c *Collector) Refresh(ctx context.Context) error {
	droplets, err := paging.All(ctx, c.logger, "droplets",
		func(d godo.Droplet) int { return d.ID }, listing(c.client, c.filter))
	if err != nil {
		return err
	}

	next := make([]droplet, 0, len(droplets))
	for i := range droplets {
		if !c.filter.Match(droplets[i].Tags, regionSlug(&droplets[i])) {
			continue
		}
		next = append(next, newDroplet(&droplets[i]))
	}

	c.mu.Lock()
	c.snap = next
	c.mu.Unlock()
	return nil
}

// listing picks how the droplets are listed. A filter of exactly one tag and
// no region is the one shape the API applies server-side, through the
// tag-scoped droplet listing, so that shape lets the API do the filtering and
// the pages arrive pre-narrowed; every other shape lists everything and
// filters client-side. The client-side match still runs after the tag-scoped
// listing, where it is a no-op, so the two paths cannot disagree.
func listing(
	client *godo.Client, f filter.Filter,
) func(context.Context, *godo.ListOptions) ([]godo.Droplet, *godo.Response, error) {
	tag, ok := f.SingleTag()
	if !ok {
		return client.Droplets.List
	}
	return func(ctx context.Context, opt *godo.ListOptions) ([]godo.Droplet, *godo.Response, error) {
		return client.Droplets.ListByTag(ctx, tag, opt)
	}
}

// regionSlug names the region a droplet lies in, or "" when the API reported
// none.
func regionSlug(d *godo.Droplet) string {
	if d.Region == nil {
		return ""
	}
	return d.Region.Slug
}

// newDroplet converts one API droplet into its snapshot form.
func newDroplet(d *godo.Droplet) droplet {
	out := droplet{
		id:              strconv.Itoa(d.ID),
		name:            d.Name,
		status:          d.Status,
		image:           imageName(d.Image),
		vpcUUID:         d.VPCUUID,
		tags:            joinTags(d.Tags),
		up:              boolToFloat(d.Status == activeStatus),
		cpus:            float64(d.Vcpus),
		memory:          float64(d.Memory) * bytesPerMebibyte,
		disk:            float64(d.Disk) * bytesPerGibibyte,
		priceHourly:     0,
		priceMonthly:    0,
		backups:         boolToFloat(slices.Contains(d.Features, backupsFeature)),
		monitoringAgent: boolToFloat(slices.Contains(d.Features, monitoringFeature)),
	}
	if created, err := time.Parse(time.RFC3339, d.Created); err == nil {
		out.created = true
		out.createdAt = float64(created.Unix())
	}
	if d.Region != nil {
		out.region = d.Region.Slug
	}
	if d.Size != nil {
		out.size = d.Size.Slug
		out.priceHourly = float64(d.Size.PriceHourly)
		out.priceMonthly = float64(d.Size.PriceMonthly)
	}
	return out
}

// joinTags renders a droplet's tags as one label. They are sorted first: the
// API's order is not documented as stable, and a label that reorders itself
// between two refreshes churns the series for no reason. One label holding
// them all is what keeps a droplet to one info series however many tags it
// carries.
func joinTags(tags []string) string {
	sorted := make([]string, len(tags))
	copy(sorted, tags)
	sort.Strings(sorted)
	return strings.Join(sorted, ",")
}

// imageName names the image a droplet runs. Custom images, which is what
// managed Kubernetes nodes run, carry no slug; their name says more than the
// distribution alone, so it stands in.
func imageName(image *godo.Image) string {
	switch {
	case image == nil:
		return ""
	case image.Slug != "":
		return image.Slug
	default:
		return image.Name
	}
}

// boolToFloat maps a boolean to the 1/0 convention Prometheus expects.
func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// Collect implements collector.Collector. Before the first successful refresh,
// and on an account with no droplets, it emits nothing.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	snap := c.snap
	c.mu.RUnlock()

	for _, d := range snap {
		labels := []string{d.id, d.name, d.region}
		gauge(ch, upDesc, d.up, labels...)
		gauge(ch, cpusDesc, d.cpus, labels...)
		gauge(ch, memoryDesc, d.memory, labels...)
		gauge(ch, diskDesc, d.disk, labels...)
		gauge(ch, priceHourlyDesc, d.priceHourly, labels...)
		gauge(ch, priceMonthlyDesc, d.priceMonthly, labels...)
		gauge(ch, backupsDesc, d.backups, labels...)
		gauge(ch, monitoringAgentDesc, d.monitoringAgent, labels...)
		if d.created {
			gauge(ch, createdDesc, d.createdAt, labels...)
		}
		gauge(ch, infoDesc, 1, d.id, d.name, d.region, d.size, d.status, d.image,
			d.vpcUUID, d.tags)
	}
}

// gauge sends one gauge sample of desc with the given label values.
func gauge(ch chan<- prometheus.Metric, desc *prometheus.Desc, value float64, labels ...string) {
	ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, value, labels...)
}
