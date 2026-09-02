// Package volumes collects the block storage volumes of the account: how big
// each one is and how many droplets it is attached to.
//
// digitalocean_volume_size_bytes matches the older, widely deployed
// DigitalOcean exporter exactly, down to reading the API's gigabytes as binary,
// so dashboards survive a migration.
//
// Attachment is reported as a count rather than a boolean because a volume can
// in principle be attached to more than one droplet, which would make a single
// droplet_id label ambiguous. An unattached volume — one that is billed while
// serving nothing — is therefore digitalocean_volume_droplets == 0.
package volumes

import (
	"context"
	"log/slog"
	"sync"

	"github.com/digitalocean/godo"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/kozaktomas/digitalocean_exporter/internal/paging"
)

// The API reports volume size in gigabytes, read here as binary.
const bytesPerGibibyte = 1024 * 1024 * 1024

// Metric descriptors. The label set of sizeDesc matches the older exporter, so
// the descriptive labels live on an info metric of its own rather than
// widening it.
var (
	sizeDesc = prometheus.NewDesc("digitalocean_volume_size_bytes",
		"Size of the volume in bytes.", []string{"id", "name", "region"}, nil)
	dropletsDesc = prometheus.NewDesc("digitalocean_volume_droplets",
		"Number of droplets the volume is attached to. Zero means it is billed but unused.",
		[]string{"id", "name", "region"}, nil)
	infoDesc = prometheus.NewDesc("digitalocean_volume_info",
		"Always 1. Its labels describe the volume's filesystem.",
		[]string{"id", "name", "region", "filesystem_type", "filesystem_label"}, nil)
)

// descriptors lists every metric the collector can emit.
var descriptors = []*prometheus.Desc{sizeDesc, dropletsDesc, infoDesc}

// volume is what one refresh learned about a single block storage volume.
type volume struct {
	id              string
	name            string
	region          string
	filesystemType  string
	filesystemLabel string

	size     float64
	droplets float64
}

// Collector reports the block storage volumes of the account.
type Collector struct {
	client *godo.Client
	logger *slog.Logger

	mu   sync.RWMutex
	snap []volume
}

// New returns a volume collector backed by client. The logger records what the
// scheduler never sees: a duplicate volume dropped from a list that shifted
// between two page requests. A nil logger discards it.
func New(client *godo.Client, logger *slog.Logger) *Collector {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Collector{client: client, logger: logger}
}

// Name implements collector.Collector.
func (c *Collector) Name() string { return "volumes" }

// Describe implements collector.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range descriptors {
		ch <- d
	}
}

// Refresh implements collector.Collector. Every page is read before the
// snapshot is replaced, so a failure halfway through the list leaves the
// previous volumes in place rather than reporting half an account.
func (c *Collector) Refresh(ctx context.Context) error {
	// The volume list takes its paging inside a struct of its own, which the
	// closure fills in from the options the helper walks with.
	list := func(ctx context.Context, opts *godo.ListOptions) ([]godo.Volume, *godo.Response, error) {
		return c.client.Storage.ListVolumes(ctx, &godo.ListVolumeParams{ListOptions: opts})
	}

	volumes, err := paging.All(ctx, c.logger, "volumes", func(v godo.Volume) string { return v.ID }, list)
	if err != nil {
		return err
	}

	next := make([]volume, 0, len(volumes))
	for i := range volumes {
		next = append(next, newVolume(&volumes[i]))
	}

	c.mu.Lock()
	c.snap = next
	c.mu.Unlock()
	return nil
}

// newVolume converts one API volume into its snapshot form.
func newVolume(v *godo.Volume) volume {
	out := volume{
		id:              v.ID,
		name:            v.Name,
		filesystemType:  v.FilesystemType,
		filesystemLabel: v.FilesystemLabel,
		size:            float64(v.SizeGigaBytes) * bytesPerGibibyte,
		droplets:        float64(len(v.DropletIDs)),
	}
	if v.Region != nil {
		out.region = v.Region.Slug
	}
	return out
}

// Collect implements collector.Collector. Before the first successful refresh,
// and on an account with no volumes, it emits nothing.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	snap := c.snap
	c.mu.RUnlock()

	for _, v := range snap {
		labels := []string{v.id, v.name, v.region}
		gauge(ch, sizeDesc, v.size, labels...)
		gauge(ch, dropletsDesc, v.droplets, labels...)
		gauge(ch, infoDesc, 1, v.id, v.name, v.region, v.filesystemType, v.filesystemLabel)
	}
}

// gauge sends one gauge sample of desc with the given label values.
func gauge(ch chan<- prometheus.Metric, desc *prometheus.Desc, value float64, labels ...string) {
	ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, value, labels...)
}
