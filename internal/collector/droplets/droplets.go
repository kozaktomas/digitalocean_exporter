// Package droplets collects one set of metrics per droplet: whether it is up,
// what it is made of and what it costs.
//
// The metric names match the older, widely deployed DigitalOcean exporter, so
// dashboards survive a migration. Its disk figure does not: this collector
// reads DigitalOcean's gigabytes as binary, the same way it reads the memory
// figure, which makes digitalocean_droplet_disk_bytes about 7% larger here.
package droplets

import (
	"context"
	"fmt"
	"strconv"
	"sync"

	"github.com/digitalocean/godo"
	"github.com/prometheus/client_golang/prometheus"
)

// dropletsPerPage is how many droplets one page request asks for, which is the
// most the API allows.
const dropletsPerPage = 200

// The API reports memory in mebibytes and disk in gibibytes.
const (
	bytesPerMebibyte = 1024 * 1024
	bytesPerGibibyte = 1024 * 1024 * 1024
)

// activeStatus is the droplet status that counts as up.
const activeStatus = "active"

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
	infoDesc = prometheus.NewDesc("digitalocean_droplet_info",
		"Always 1. Its labels describe the droplet's size, status and image.",
		[]string{"id", "name", "region", "size", "status", "image"}, nil)
)

// descriptors lists every metric the collector can emit.
var descriptors = []*prometheus.Desc{
	upDesc, cpusDesc, memoryDesc, diskDesc, priceHourlyDesc, priceMonthlyDesc, infoDesc,
}

// droplet is what one refresh learned about a single droplet.
type droplet struct {
	id     string
	name   string
	region string
	size   string
	status string
	image  string

	up           float64
	cpus         float64
	memory       float64
	disk         float64
	priceHourly  float64
	priceMonthly float64
}

// Collector reports the droplets of the account.
type Collector struct {
	client *godo.Client

	mu   sync.RWMutex
	snap []droplet
}

// New returns a droplet collector backed by client.
func New(client *godo.Client) *Collector {
	return &Collector{client: client}
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
	opts := &godo.ListOptions{PerPage: dropletsPerPage}
	var next []droplet

	for {
		page, resp, err := c.client.Droplets.List(ctx, opts)
		if err != nil {
			return fmt.Errorf("list droplets: %w", err)
		}
		for i := range page {
			next = append(next, newDroplet(&page[i]))
		}

		if resp == nil || resp.Links == nil || resp.Links.IsLastPage() {
			break
		}
		current, err := resp.Links.CurrentPage()
		if err != nil {
			return fmt.Errorf("next page of droplets: %w", err)
		}
		opts.Page = current + 1
	}

	c.mu.Lock()
	c.snap = next
	c.mu.Unlock()
	return nil
}

// newDroplet converts one API droplet into its snapshot form.
func newDroplet(d *godo.Droplet) droplet {
	out := droplet{
		id:           strconv.Itoa(d.ID),
		name:         d.Name,
		status:       d.Status,
		image:        imageName(d.Image),
		up:           boolToFloat(d.Status == activeStatus),
		cpus:         float64(d.Vcpus),
		memory:       float64(d.Memory) * bytesPerMebibyte,
		disk:         float64(d.Disk) * bytesPerGibibyte,
		priceHourly:  0,
		priceMonthly: 0,
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
		gauge(ch, infoDesc, 1, d.id, d.name, d.region, d.size, d.status, d.image)
	}
}

// gauge sends one gauge sample of desc with the given label values.
func gauge(ch chan<- prometheus.Metric, desc *prometheus.Desc, value float64, labels ...string) {
	ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, value, labels...)
}
