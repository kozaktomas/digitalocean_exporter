// Package projects collects the projects of the account and what each one
// owns.
//
// The project list is one paged walk, but the counts cost one more paged walk
// per project: /v2/projects/{id}/resources is the only place the API says
// what a project holds. Each project is isolated the way the Spaces collector
// isolates buckets — a resources lookup that fails keeps that project's
// previous counts and is logged, rather than failing the refresh and taking
// the other projects' fresh counts with it.
//
// The resource type is derived from the URN the resources endpoint returns,
// `do:<type>:<id>`, because the endpoint reports nothing else about a
// resource. A URN of any other shape is counted as type "unknown" rather than
// dropped, so the total across types stays the number of resources the
// project owns.
package projects

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/digitalocean/godo"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/kozaktomas/digitalocean_exporter/internal/paging"
)

// Metric descriptors.
var (
	infoDesc = prometheus.NewDesc("digitalocean_project_info",
		"Always 1. Its labels describe the project.",
		[]string{"id", "name", "purpose", "environment", "is_default"}, nil)
	resourcesDesc = prometheus.NewDesc("digitalocean_project_resources",
		"Number of resources of the given type the project owns, by URN type.",
		[]string{"id", "name", "type"}, nil)
)

// ErrNoProjectMeasured reports that not a single project's resources could be
// listed.
var ErrNoProjectMeasured = errors.New("no project's resources could be listed")

// typeCount is one per-type count of a project: how many resources of one
// URN type it owns.
type typeCount struct {
	resourceType string
	count        float64
}

// project is what one refresh learned about a single project.
type project struct {
	id          string
	name        string
	purpose     string
	environment string
	isDefault   string

	counts []typeCount
	// known reports whether counts come from a successful resources lookup,
	// this refresh or an earlier one. A project whose resources were never
	// listed reports its info and nothing else, because a fabricated zero is
	// indistinguishable from an empty project.
	known bool
}

// Collector reports the projects of the account and their resource counts.
type Collector struct {
	client *godo.Client
	logger *slog.Logger

	mu   sync.RWMutex
	snap map[string]project
}

// New returns a projects collector backed by client. The logger records what
// the scheduler never sees: the per-project resources lookups that failed,
// and any duplicate dropped from a list that shifted between two page
// requests. A nil logger discards both.
func New(client *godo.Client, logger *slog.Logger) *Collector {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Collector{client: client, logger: logger}
}

// Name implements collector.Collector.
func (c *Collector) Name() string { return "projects" }

// Describe implements collector.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- infoDesc
	ch <- resourcesDesc
}

// Refresh implements collector.Collector. The whole new snapshot is built
// before it is swapped in: a failure to list the projects, or of every
// per-project resources lookup at once, leaves the previous snapshot
// untouched. One project failing costs only that project its fresh counts.
func (c *Collector) Refresh(ctx context.Context) error {
	list, err := paging.All(ctx, c.logger, "projects",
		func(p godo.Project) string { return p.ID }, c.client.Projects.List)
	if err != nil {
		return err
	}

	c.mu.RLock()
	previous := c.snap
	c.mu.RUnlock()

	next := make(map[string]project, len(list))
	failed := 0
	var lastErr error
	for i := range list {
		entry, err := c.measure(ctx, &list[i], previous)
		if err != nil {
			failed++
			lastErr = err
			c.logger.Warn("listing a project's resources failed",
				"project", list[i].Name, "id", list[i].ID, "error", err)
		}
		next[entry.id] = entry
	}
	if failed > 0 && failed == len(list) {
		return fmt.Errorf("%w: %d attempted, last error: %w", ErrNoProjectMeasured, failed, lastErr)
	}

	c.mu.Lock()
	c.snap = next
	c.mu.Unlock()
	return nil
}

// measure builds the snapshot entry of one project: its labels from the list,
// its counts from the resources endpoint. On a failed lookup it returns the
// entry carrying whatever counts previous held for the project, and the
// error for the caller to log and tally.
func (c *Collector) measure(
	ctx context.Context, p *godo.Project, previous map[string]project,
) (project, error) {
	entry := project{
		id:          p.ID,
		name:        p.Name,
		purpose:     p.Purpose,
		environment: p.Environment,
		isDefault:   strconv.FormatBool(p.IsDefault),
	}

	counts, err := c.countResources(ctx, p.ID)
	if err != nil {
		if old, ok := previous[p.ID]; ok && old.known {
			entry.counts, entry.known = old.counts, true
		}
		return entry, err
	}
	entry.counts, entry.known = counts, true
	return entry, nil
}

// countResources walks the project's resources and counts them by URN type.
func (c *Collector) countResources(ctx context.Context, id string) ([]typeCount, error) {
	resources, err := paging.All(ctx, c.logger, "project resources",
		func(r godo.ProjectResource) string { return r.URN },
		func(ctx context.Context, opts *godo.ListOptions) ([]godo.ProjectResource, *godo.Response, error) {
			return c.client.Projects.ListResources(ctx, id, opts)
		})
	if err != nil {
		return nil, err
	}

	byType := make(map[string]float64)
	for _, r := range resources {
		byType[urnType(r.URN)]++
	}
	counts := make([]typeCount, 0, len(byType))
	for resourceType, count := range byType {
		counts = append(counts, typeCount{resourceType, count})
	}
	// The map iterates in a random order; sorting keeps the snapshot, and with
	// it anything that ranges over it, the same between two identical refreshes.
	sort.Slice(counts, func(i, j int) bool { return counts[i].resourceType < counts[j].resourceType })
	return counts, nil
}

// urnType extracts the resource type from a DigitalOcean URN, `do:<type>:<id>`.
// Anything of another shape is "unknown", so it is still counted.
func urnType(urn string) string {
	parts := strings.SplitN(urn, ":", 3)
	if len(parts) == 3 && parts[0] == "do" && parts[1] != "" {
		return parts[1]
	}
	return "unknown"
}

// Collect implements collector.Collector. Before the first successful refresh
// it emits nothing; a project whose resources have never been listed emits its
// info and no counts.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	snap := c.snap
	c.mu.RUnlock()

	for _, p := range snap {
		ch <- prometheus.MustNewConstMetric(infoDesc, prometheus.GaugeValue, 1,
			p.id, p.name, p.purpose, p.environment, p.isDefault)
		for _, tc := range p.counts {
			ch <- prometheus.MustNewConstMetric(resourcesDesc, prometheus.GaugeValue,
				tc.count, p.id, p.name, tc.resourceType)
		}
	}
}
