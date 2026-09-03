// Package tags collects how many resources carry each tag of the account.
//
// The tag list itself answers the question: every tag arrives with per-type
// resource counts already on it, so one paged walk of /v2/tags covers however
// many droplets, volumes and databases the tags are spread across. Nothing
// here fans out per tag.
//
// A type the API reports no count for is skipped rather than exported as
// zero. The API omits a type it did not count, and a fabricated zero for
// every tag would read as "this tag covers nothing of that type", which is a
// claim the response never made.
package tags

import (
	"context"
	"log/slog"
	"sync"

	"github.com/digitalocean/godo"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/kozaktomas/digitalocean_exporter/internal/paging"
)

// resourcesDesc is the one metric the collector emits.
var resourcesDesc = prometheus.NewDesc("digitalocean_tag_resources",
	"Number of resources of the given type carrying the tag.",
	[]string{"tag", "type"}, nil)

// typeCount is one per-type count of a tag: how many resources of one type
// carry it.
type typeCount struct {
	resourceType string
	count        float64
}

// tag is what one refresh learned about a single tag.
type tag struct {
	name   string
	counts []typeCount
}

// Collector reports the per-type resource counts of every tag.
type Collector struct {
	client *godo.Client
	logger *slog.Logger

	mu   sync.RWMutex
	snap []tag
}

// New returns a tags collector backed by client. The logger records what the
// scheduler never sees: a duplicate tag dropped from a list that shifted
// between two page requests. A nil logger discards it.
func New(client *godo.Client, logger *slog.Logger) *Collector {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Collector{client: client, logger: logger}
}

// Name implements collector.Collector.
func (c *Collector) Name() string { return "tags" }

// Describe implements collector.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- resourcesDesc
}

// Refresh implements collector.Collector. Every page is read before the
// snapshot is replaced, so a failure halfway through the list leaves the
// previous tags in place rather than reporting half an account.
func (c *Collector) Refresh(ctx context.Context) error {
	list, err := paging.All(ctx, c.logger, "tags",
		func(t godo.Tag) string { return t.Name }, c.client.Tags.List)
	if err != nil {
		return err
	}

	next := make([]tag, 0, len(list))
	for i := range list {
		next = append(next, newTag(&list[i]))
	}

	c.mu.Lock()
	c.snap = next
	c.mu.Unlock()
	return nil
}

// newTag converts one API tag into its snapshot form, keeping only the types
// the API reported a count for.
func newTag(t *godo.Tag) tag {
	out := tag{name: t.Name}
	if t.Resources == nil {
		return out
	}
	if r := t.Resources.Droplets; r != nil {
		out.counts = append(out.counts, typeCount{"droplet", float64(r.Count)})
	}
	if r := t.Resources.Images; r != nil {
		out.counts = append(out.counts, typeCount{"image", float64(r.Count)})
	}
	if r := t.Resources.Volumes; r != nil {
		out.counts = append(out.counts, typeCount{"volume", float64(r.Count)})
	}
	if r := t.Resources.VolumeSnapshots; r != nil {
		out.counts = append(out.counts, typeCount{"volume_snapshot", float64(r.Count)})
	}
	if r := t.Resources.Databases; r != nil {
		out.counts = append(out.counts, typeCount{"database", float64(r.Count)})
	}
	return out
}

// Collect implements collector.Collector. Before the first successful refresh,
// and on an account with no tags, it emits nothing.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	snap := c.snap
	c.mu.RUnlock()

	for _, t := range snap {
		for _, tc := range t.counts {
			ch <- prometheus.MustNewConstMetric(resourcesDesc, prometheus.GaugeValue,
				tc.count, t.name, tc.resourceType)
		}
	}
}
