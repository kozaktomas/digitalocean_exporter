// Package reservedips collects the reserved IP addresses of the account and
// reports which of them are assigned to a droplet.
//
// DigitalOcean bills a reserved IP that is assigned to nothing. It is the same
// cost signal an unattached volume is — something reserved, billed and serving
// no purpose — and nothing in the control panel brings it up, which is why the
// assignment is a metric of its own rather than a label on an info metric.
//
// Both address families are read: /v2/reserved_ips for IPv4 and
// /v2/reserved_ipv6 for IPv6. They are one collector rather than two because
// they answer one question, and the version label keeps them apart. The IPv6
// listing carries no project, so project_id is empty there.
package reservedips

import (
	"context"
	"log/slog"
	"strconv"
	"sync"

	"github.com/digitalocean/godo"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/kozaktomas/digitalocean_exporter/internal/paging"
)

// The values of the version label, which is the IP version of the address.
const (
	versionIPv4 = "4"
	versionIPv6 = "6"
)

// Metric descriptors.
var (
	assignedDesc = prometheus.NewDesc("digitalocean_reserved_ip_assigned",
		"1 when the reserved IP is assigned to a droplet, 0 when it is idle.",
		[]string{"ip", "region", "version"}, nil)
	infoDesc = prometheus.NewDesc("digitalocean_reserved_ip_info",
		"Always 1. Its labels name the droplet the address serves and its project.",
		[]string{"ip", "region", "version", "droplet_id", "droplet_name", "project_id"}, nil)
	countDesc = prometheus.NewDesc("digitalocean_reserved_ips",
		"Number of reserved IP addresses in this region of this IP version.",
		[]string{"region", "version"}, nil)
)

// descriptors lists every metric the collector can emit.
var descriptors = []*prometheus.Desc{assignedDesc, infoDesc, countDesc}

// reservedIP is what one refresh learned about a single reserved IP address.
type reservedIP struct {
	ip      string
	region  string
	version string

	// dropletID and dropletName are empty when the address is assigned to
	// nothing, which is the state that costs money for no service.
	dropletID   string
	dropletName string
	projectID   string

	assigned float64
}

// group is the pair the count metric is broken down by.
type group struct {
	region  string
	version string
}

// snapshot is an immutable view of the reserved IPs from one successful
// refresh, with the counts already worked out.
type snapshot struct {
	ips    []reservedIP
	counts map[group]float64
}

// Collector reports the reserved IP addresses of the account.
type Collector struct {
	client *godo.Client
	logger *slog.Logger

	mu   sync.RWMutex
	snap *snapshot
}

// New returns a reserved IP collector backed by client. The logger records what
// the scheduler never sees: a duplicate address dropped from a list that
// shifted between two page requests. A nil logger discards it.
func New(client *godo.Client, logger *slog.Logger) *Collector {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Collector{client: client, logger: logger}
}

// Name implements collector.Collector.
func (c *Collector) Name() string { return "reservedips" }

// Describe implements collector.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range descriptors {
		ch <- d
	}
}

// Refresh implements collector.Collector. Both listings are read in full
// before the snapshot is replaced, so a failure in either one leaves the
// previous addresses in place rather than reporting half of them.
func (c *Collector) Refresh(ctx context.Context) error {
	v4, err := paging.All(ctx, c.logger, "reserved IPs",
		func(ip godo.ReservedIP) string { return ip.IP }, c.client.ReservedIPs.List)
	if err != nil {
		return err
	}

	v6, err := paging.All(ctx, c.logger, "reserved IPv6 addresses",
		func(ip godo.ReservedIPV6) string { return ip.IP }, c.client.ReservedIPV6s.List)
	if err != nil {
		return err
	}

	next := &snapshot{
		ips:    make([]reservedIP, 0, len(v4)+len(v6)),
		counts: make(map[group]float64, len(v4)+len(v6)),
	}
	for i := range v4 {
		next.add(newIPv4(&v4[i]))
	}
	for i := range v6 {
		next.add(newIPv6(&v6[i]))
	}

	c.mu.Lock()
	c.snap = next
	c.mu.Unlock()
	return nil
}

// add records one address and counts it against its region and version.
func (s *snapshot) add(ip reservedIP) {
	s.ips = append(s.ips, ip)
	s.counts[group{region: ip.region, version: ip.version}]++
}

// newIPv4 converts one API IPv4 reserved IP into its snapshot form.
func newIPv4(ip *godo.ReservedIP) reservedIP {
	out := reservedIP{ip: ip.IP, version: versionIPv4, projectID: ip.ProjectID}
	if ip.Region != nil {
		out.region = ip.Region.Slug
	}
	assign(&out, ip.Droplet)
	return out
}

// newIPv6 converts one API IPv6 reserved IP into its snapshot form. The IPv6
// listing reports its region as a slug rather than an object, and carries no
// project at all.
func newIPv6(ip *godo.ReservedIPV6) reservedIP {
	out := reservedIP{ip: ip.IP, region: ip.RegionSlug, version: versionIPv6}
	assign(&out, ip.Droplet)
	return out
}

// assign fills in the droplet an address is attached to. A nil droplet is the
// idle address the collector exists for, and leaves both labels empty.
func assign(out *reservedIP, droplet *godo.Droplet) {
	if droplet == nil {
		return
	}
	out.assigned = 1
	out.dropletID = strconv.Itoa(droplet.ID)
	out.dropletName = droplet.Name
}

// Collect implements collector.Collector. Before the first successful refresh
// it emits nothing, and an account with no reserved IPs emits nothing either:
// there is no region to count them under.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	snap := c.snap
	c.mu.RUnlock()

	if snap == nil {
		return
	}

	for _, ip := range snap.ips {
		gauge(ch, assignedDesc, ip.assigned, ip.ip, ip.region, ip.version)
		gauge(ch, infoDesc, 1,
			ip.ip, ip.region, ip.version, ip.dropletID, ip.dropletName, ip.projectID)
	}
	for g, count := range snap.counts {
		gauge(ch, countDesc, count, g.region, g.version)
	}
}

// gauge sends one gauge sample of desc with the given label values.
func gauge(ch chan<- prometheus.Metric, desc *prometheus.Desc, value float64, labels ...string) {
	ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, value, labels...)
}
