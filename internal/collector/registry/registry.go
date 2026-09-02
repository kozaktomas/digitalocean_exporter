// Package registry collects DigitalOcean Container Registry metrics: how much
// storage each registry uses, what the subscription tier includes, and how many
// tags and manifests each repository holds.
//
// An account can hold more than one registry. A Professional subscription may
// create several, and once it has, part of the single-registry `/v2/registry`
// surface stops answering, so the collector enumerates registries through
// `/v2/registries` and reads the single-registry endpoints only when that one
// is unavailable — which is what an older account still offers.
//
// An account without a registry is a normal state, not a failure: both
// endpoints answer 404 and the collector then reports nothing at all.
package registry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	"github.com/digitalocean/godo"
	"github.com/prometheus/client_golang/prometheus"
)

// repositoriesPerPage is how many repositories one page request asks for. The
// API caps a page at 200, and a registry rarely fills even one.
const repositoriesPerPage = 200

// centsPerDollar converts the price the API reports to the unit the metric
// promises.
const centsPerDollar = 100

// ErrNoRepositoriesListed reports that not one registry could be read past its
// name. It fails the refresh, because at that point nothing about the
// repositories is current.
var ErrNoRepositoriesListed = errors.New("no registry's repositories could be listed")

// Metric descriptors. Every one of them carries a `registry` label, which is
// what makes several registries on one account fit the same data model.
var (
	storageUsageDesc = prometheus.NewDesc("digitalocean_registry_storage_usage_bytes",
		"Storage the registry uses, as last measured by DigitalOcean.",
		[]string{"registry", "region"}, nil)
	storageUpdatedDesc = prometheus.NewDesc("digitalocean_registry_storage_usage_updated_timestamp_seconds",
		"When DigitalOcean last measured that storage.",
		[]string{"registry", "region"}, nil)
	storageIncludedDesc = prometheus.NewDesc("digitalocean_registry_storage_included_bytes",
		"Storage included in the subscription tier.",
		[]string{"registry", "region"}, nil)
	bandwidthIncludedDesc = prometheus.NewDesc("digitalocean_registry_bandwidth_included_bytes",
		"Outbound transfer included in the subscription tier each month.",
		[]string{"registry", "region"}, nil)
	priceDesc = prometheus.NewDesc("digitalocean_registry_subscription_monthly_price_usd",
		"Monthly price of the subscription tier in US dollars.",
		[]string{"registry", "tier"}, nil)
	infoDesc = prometheus.NewDesc("digitalocean_registry_info",
		"Always 1. Its labels name the registry, its region and its subscription tier.",
		[]string{"registry", "region", "tier", "tier_name"}, nil)
	upDesc = prometheus.NewDesc("digitalocean_registry_up",
		"Whether the last refresh could list the registry's repositories.",
		[]string{"registry", "region"}, nil)
	repositoriesDesc = prometheus.NewDesc("digitalocean_registry_repositories",
		"Number of repositories in the registry.",
		[]string{"registry"}, nil)
	tagsDesc = prometheus.NewDesc("digitalocean_registry_repository_tags",
		"Number of tags in the repository.",
		[]string{"registry", "repository"}, nil)
	manifestsDesc = prometheus.NewDesc("digitalocean_registry_repository_manifests",
		"Number of manifests in the repository.",
		[]string{"registry", "repository"}, nil)
	manifestSizeDesc = prometheus.NewDesc("digitalocean_registry_repository_latest_manifest_size_bytes",
		"Compressed size of the repository's newest manifest.",
		[]string{"registry", "repository"}, nil)
	lastPushDesc = prometheus.NewDesc("digitalocean_registry_repository_last_push_timestamp_seconds",
		"Unix timestamp of the last push to the repository.",
		[]string{"registry", "repository"}, nil)
)

// descriptors lists every metric the collector can emit.
var descriptors = []*prometheus.Desc{
	storageUsageDesc, storageUpdatedDesc, storageIncludedDesc, bandwidthIncludedDesc,
	priceDesc, infoDesc, upDesc,
	repositoriesDesc, tagsDesc, manifestsDesc, manifestSizeDesc, lastPushDesc,
}

// repository is what one refresh learned about a single repository.
type repository struct {
	name      string
	tags      float64
	manifests float64
	// manifest reports whether the repository has a newest manifest at all. A
	// repository that was never pushed to has none, and reporting a zero size
	// for it would read as an image of no size.
	manifest     bool
	manifestSize float64
	// pushed reports whether that manifest carries a usable timestamp.
	pushed   bool
	pushedAt float64
}

// registryStats is what one refresh learned about a single registry.
type registryStats struct {
	name         string
	region       string
	storageUsage float64
	// measured reports whether the API said when it last measured the storage
	// figure. Without it the age of that figure is left unstated rather than
	// reported as the epoch.
	measured   bool
	measuredAt float64
	// repositories are the registry's repositories, carried over from the
	// previous refresh when this one could not list them.
	repositories []repository
	// known reports whether the repositories were ever listed successfully. A
	// registry seen for the first time whose listing failed has no count to
	// report, and a zero would read as a registry holding nothing.
	known bool
	// up reports whether this refresh listed them.
	up bool
}

// snapshot is an immutable view of the account's registries from one refresh.
type snapshot struct {
	// registries is keyed by name, which is unique within an account. An empty
	// map is an account without a registry, which is how that is told apart
	// from figures that are merely stale: the collector then emits nothing.
	registries map[string]*registryStats
	// tier reports whether the subscription tier could be read. Without it the
	// quota metrics would claim an allowance of zero. The subscription covers
	// the account, however many registries it holds.
	tier              bool
	tierSlug          string
	tierName          string
	storageIncluded   float64
	bandwidthIncluded float64
	monthlyPrice      float64
}

// Collector reports the size, subscription and repositories of the account's
// container registries.
type Collector struct {
	client *godo.Client
	logger *slog.Logger

	mu   sync.RWMutex
	snap *snapshot
}

// New returns a registry collector backed by client. The logger records what
// the scheduler never sees: an account without a registry, and a single
// registry that could not be read while the others could. A nil logger
// discards both.
func New(client *godo.Client, logger *slog.Logger) *Collector {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Collector{client: client, logger: logger}
}

// Name implements collector.Collector.
func (c *Collector) Name() string { return "registry" }

// Describe implements collector.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range descriptors {
		ch <- d
	}
}

// Refresh implements collector.Collector. It enumerates the registries, reads
// the account's subscription and every registry's repositories, and swaps the
// snapshot in only once all of it has been fetched, so a partial failure
// changes nothing. A registry whose repositories cannot be listed keeps the
// ones it last reported and is marked down; only the failure of every registry
// fails the refresh. Finding no registry at all is not an error: the account
// has none, and the snapshot becomes an empty one.
func (c *Collector) Refresh(ctx context.Context) error {
	registries, multi, err := c.list(ctx)
	if err != nil {
		return err
	}
	if len(registries) == 0 {
		c.markAbsent()
		return nil
	}

	sub, err := c.subscription(ctx, multi)
	if err != nil {
		return err
	}

	results := c.readAll(ctx, registries, multi)

	next := &snapshot{}
	applySubscription(next, sub)
	c.swap(next, results)

	return c.reportFailures(results)
}

// list enumerates the account's registries. It asks the multi-registry
// endpoint first, because an account that has created a second registry can no
// longer be read through the single-registry one. An account whose API does
// not offer `/v2/registries` — or offers it and names nothing — is read
// through `/v2/registry` instead, so an older account keeps working. Nothing
// from either means the account has no registry. The bool reports which path
// answered, because every later request has to stay on it.
func (c *Collector) list(ctx context.Context) ([]*godo.Registry, bool, error) {
	registries, _, err := c.client.Registries.List(ctx)
	if err == nil && len(registries) > 0 {
		return registries, true, nil
	}
	if err != nil {
		c.logger.Debug("listing registries failed, falling back to the single-registry endpoint",
			"error", err)
	}

	reg, _, singleErr := c.client.Registry.Get(ctx)
	switch {
	case singleErr == nil && reg != nil:
		return []*godo.Registry{reg}, false, nil
	case singleErr == nil, isNotFound(singleErr):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("get registry: %w (after list registries: %w)", singleErr, err)
	default:
		return nil, false, fmt.Errorf("get registry: %w", singleErr)
	}
}

// subscription reads the account's subscription tier. It is account-wide
// however many registries it covers, so it stays a single request; it is only
// read from the same surface the registries were listed from, because the
// other one may not answer.
func (c *Collector) subscription(ctx context.Context, multi bool) (*godo.RegistrySubscription, error) {
	get := c.client.Registry.GetSubscription
	if multi {
		get = c.client.Registries.GetSubscription
	}

	sub, _, err := get(ctx)
	if err != nil {
		return nil, fmt.Errorf("get registry subscription: %w", err)
	}
	return sub, nil
}

// result is the outcome of reading one registry's repositories.
type result struct {
	registry     *godo.Registry
	repositories []repository
	err          error
}

// readAll lists the repositories of every registry. They are read one after
// another rather than at once: an account holds a handful of registries at
// most, and the shared transport paces the requests anyway.
func (c *Collector) readAll(ctx context.Context, registries []*godo.Registry, multi bool) []result {
	results := make([]result, 0, len(registries))
	for _, reg := range registries {
		repositories, err := c.repositories(ctx, reg.Name, multi)
		results = append(results, result{registry: reg, repositories: repositories, err: err})
	}
	return results
}

// repositories reads every page of one registry's repository list. The list is
// paginated with a page token rather than a page number.
func (c *Collector) repositories(ctx context.Context, name string, multi bool) ([]repository, error) {
	list := c.client.Registry.ListRepositoriesV2
	if multi {
		list = c.client.Registries.ListRepositoriesV2
	}

	opts := &godo.TokenListOptions{PerPage: repositoriesPerPage}
	var out []repository

	for {
		page, resp, err := list(ctx, name, opts)
		if err != nil {
			return nil, fmt.Errorf("list repositories: %w", err)
		}
		for _, repo := range page {
			out = append(out, newRepository(repo))
		}

		if resp == nil || resp.Links == nil || resp.Links.IsLastPage() {
			return out, nil
		}
		token, err := resp.Links.NextPageToken()
		if err != nil {
			return nil, fmt.Errorf("next page of repositories: %w", err)
		}
		if token == "" {
			return out, nil
		}
		opts.Token = token
	}
}

// newRepository converts one API repository into its snapshot form.
func newRepository(repo *godo.RepositoryV2) repository {
	out := repository{
		name:      repo.Name,
		tags:      float64(repo.TagCount),
		manifests: float64(repo.ManifestCount),
	}
	if repo.LatestManifest == nil {
		return out
	}

	out.manifest = true
	out.manifestSize = float64(repo.LatestManifest.CompressedSizeBytes)
	if !repo.LatestManifest.UpdatedAt.IsZero() {
		out.pushed = true
		out.pushedAt = float64(repo.LatestManifest.UpdatedAt.Unix())
	}
	return out
}

// applySubscription copies the subscription tier into the snapshot. A missing
// tier leaves the quota metrics out rather than reporting them as zero.
func applySubscription(snap *snapshot, sub *godo.RegistrySubscription) {
	if sub == nil || sub.Tier == nil {
		return
	}
	snap.tier = true
	snap.tierSlug = sub.Tier.Slug
	snap.tierName = sub.Tier.Name
	snap.storageIncluded = float64(sub.Tier.IncludedStorageBytes)
	snap.bandwidthIncluded = float64(sub.Tier.IncludedBandwidthBytes)
	snap.monthlyPrice = float64(sub.Tier.MonthlyPriceInCents) / centsPerDollar
}

// swap fills next with the results and installs it. A registry whose
// repositories could not be listed carries the previous ones forward and is
// marked down: its size and region were read from the listing and are current,
// and one unreadable registry must not blank the others.
func (c *Collector) swap(next *snapshot, results []result) {
	c.mu.Lock()
	defer c.mu.Unlock()

	next.registries = make(map[string]*registryStats, len(results))
	for _, r := range results {
		stats := &registryStats{
			name:         r.registry.Name,
			region:       r.registry.Region,
			storageUsage: float64(r.registry.StorageUsageBytes),
		}
		if !r.registry.StorageUsageBytesUpdatedAt.IsZero() {
			stats.measured = true
			stats.measuredAt = float64(r.registry.StorageUsageBytesUpdatedAt.Unix())
		}

		switch previous, ok := c.previous(stats.name); {
		case r.err == nil:
			stats.repositories = r.repositories
			stats.known = true
			stats.up = true
		case ok:
			stats.repositories = previous.repositories
			stats.known = previous.known
		}
		next.registries[stats.name] = stats
	}
	c.snap = next
}

// previous returns what the last snapshot held for a registry. The caller
// holds the lock.
func (c *Collector) previous(name string) (*registryStats, bool) {
	if c.snap == nil {
		return nil, false
	}
	stats, ok := c.snap.registries[name]
	return stats, ok
}

// reportFailures logs every registry that could not be listed and fails the
// refresh when none of them could. Those failures reach nobody else: a refresh
// that read some of the registries succeeded.
func (c *Collector) reportFailures(results []result) error {
	failed := 0
	for _, r := range results {
		if r.err != nil {
			failed++
			c.logger.Warn("listing the repositories of a container registry failed",
				"registry", r.registry.Name, "region", r.registry.Region, "error", r.err)
		}
	}
	if failed > 0 && failed == len(results) {
		return fmt.Errorf("%w: %d attempted, last error: %w",
			ErrNoRepositoriesListed, failed, results[len(results)-1].err)
	}
	return nil
}

// markAbsent records that the account has no registry. The transition is
// logged once, because it never reaches the scheduler: the refresh succeeded.
func (c *Collector) markAbsent() {
	c.mu.Lock()
	changed := c.snap == nil || len(c.snap.registries) > 0
	c.snap = &snapshot{}
	c.mu.Unlock()

	if changed {
		c.logger.Info("no container registry on this account, the registry collector reports nothing")
	}
}

// isNotFound reports whether err is the API answering 404.
func isNotFound(err error) bool {
	var apiErr *godo.ErrorResponse
	if !errors.As(err, &apiErr) || apiErr.Response == nil {
		return false
	}
	return apiErr.Response.StatusCode == http.StatusNotFound
}

// Collect implements collector.Collector. Before the first successful refresh,
// and on an account without a registry, it emits nothing.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	snap := c.snap
	c.mu.RUnlock()

	if snap == nil {
		return
	}
	for _, reg := range snap.registries {
		collectRegistry(ch, snap, reg)
		collectRepositories(ch, reg)
	}
}

// collectRegistry emits the metrics of one registry.
func collectRegistry(ch chan<- prometheus.Metric, snap *snapshot, reg *registryStats) {
	gauge(ch, storageUsageDesc, reg.storageUsage, reg.name, reg.region)
	if reg.measured {
		gauge(ch, storageUpdatedDesc, reg.measuredAt, reg.name, reg.region)
	}
	gauge(ch, upDesc, boolValue(reg.up), reg.name, reg.region)
	if reg.known {
		gauge(ch, repositoriesDesc, float64(len(reg.repositories)), reg.name)
	}

	if !snap.tier {
		return
	}
	gauge(ch, storageIncludedDesc, snap.storageIncluded, reg.name, reg.region)
	gauge(ch, bandwidthIncludedDesc, snap.bandwidthIncluded, reg.name, reg.region)
	gauge(ch, priceDesc, snap.monthlyPrice, reg.name, snap.tierSlug)
	gauge(ch, infoDesc, 1, reg.name, reg.region, snap.tierSlug, snap.tierName)
}

// collectRepositories emits the per-repository metrics of one registry.
func collectRepositories(ch chan<- prometheus.Metric, reg *registryStats) {
	for _, repo := range reg.repositories {
		gauge(ch, tagsDesc, repo.tags, reg.name, repo.name)
		gauge(ch, manifestsDesc, repo.manifests, reg.name, repo.name)
		if repo.manifest {
			gauge(ch, manifestSizeDesc, repo.manifestSize, reg.name, repo.name)
		}
		if repo.pushed {
			gauge(ch, lastPushDesc, repo.pushedAt, reg.name, repo.name)
		}
	}
}

// gauge sends one gauge sample of desc with the given label values.
func gauge(ch chan<- prometheus.Metric, desc *prometheus.Desc, value float64, labels ...string) {
	ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, value, labels...)
}

// boolValue is the sample a boolean metric carries.
func boolValue(ok bool) float64 {
	if ok {
		return 1
	}
	return 0
}
