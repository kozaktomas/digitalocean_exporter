// Package registry collects DigitalOcean Container Registry metrics: how much
// storage the registry uses, what its subscription tier includes, and how many
// tags and manifests each repository holds.
//
// An account without a registry is a normal state, not a failure: the API
// answers 404 and the collector then reports nothing at all.
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

// Metric descriptors.
var (
	storageUsageDesc = prometheus.NewDesc("digitalocean_registry_storage_usage_bytes",
		"Storage the registry uses, as last measured by DigitalOcean.",
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
	storageUsageDesc, storageIncludedDesc, bandwidthIncludedDesc, priceDesc, infoDesc,
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

// snapshot is an immutable view of the registry from one successful refresh.
type snapshot struct {
	// present reports whether the account has a registry. When it is false the
	// collector emits nothing, which is how an account without a registry is
	// told apart from one whose figures are merely stale.
	present bool
	name    string
	region  string
	// tier reports whether the subscription tier could be read. Without it the
	// quota metrics would claim an allowance of zero.
	tier              bool
	tierSlug          string
	tierName          string
	storageUsage      float64
	storageIncluded   float64
	bandwidthIncluded float64
	monthlyPrice      float64
	repositories      []repository
}

// Collector reports the size, subscription and repositories of the account's
// container registry.
type Collector struct {
	client *godo.Client
	logger *slog.Logger

	mu   sync.RWMutex
	snap *snapshot
}

// New returns a registry collector backed by client. The logger records the
// one event the scheduler never sees, an account without a registry; a nil
// logger discards it.
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

// Refresh implements collector.Collector. It reads the registry, its
// subscription and every repository, and swaps the snapshot in only once all
// three have been fetched, so a partial failure changes nothing. A 404 on the
// registry itself is not an error: the account has no registry, and the
// snapshot becomes an empty one.
func (c *Collector) Refresh(ctx context.Context) error {
	reg, _, err := c.client.Registry.Get(ctx)
	if err != nil {
		if isNotFound(err) {
			c.markAbsent()
			return nil
		}
		return fmt.Errorf("get registry: %w", err)
	}

	sub, _, err := c.client.Registry.GetSubscription(ctx)
	if err != nil {
		return fmt.Errorf("get registry subscription: %w", err)
	}

	repositories, err := c.repositories(ctx, reg.Name)
	if err != nil {
		return err
	}

	next := &snapshot{
		present:      true,
		name:         reg.Name,
		region:       reg.Region,
		storageUsage: float64(reg.StorageUsageBytes),
		repositories: repositories,
	}
	applySubscription(next, sub)

	c.mu.Lock()
	c.snap = next
	c.mu.Unlock()
	return nil
}

// repositories reads every page of the registry's repository list. The list is
// paginated with a page token rather than a page number.
func (c *Collector) repositories(ctx context.Context, name string) ([]repository, error) {
	opts := &godo.TokenListOptions{PerPage: repositoriesPerPage}
	var out []repository

	for {
		page, resp, err := c.client.Registry.ListRepositoriesV2(ctx, name, opts)
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

// markAbsent records that the account has no registry. The transition is
// logged once, because it never reaches the scheduler: the refresh succeeded.
func (c *Collector) markAbsent() {
	c.mu.Lock()
	changed := c.snap == nil || c.snap.present
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

	if snap == nil || !snap.present {
		return
	}
	collectRegistry(ch, snap)
	collectRepositories(ch, snap)
}

// collectRegistry emits the registry-wide metrics.
func collectRegistry(ch chan<- prometheus.Metric, snap *snapshot) {
	gauge(ch, storageUsageDesc, snap.storageUsage, snap.name, snap.region)
	gauge(ch, repositoriesDesc, float64(len(snap.repositories)), snap.name)

	if !snap.tier {
		return
	}
	gauge(ch, storageIncludedDesc, snap.storageIncluded, snap.name, snap.region)
	gauge(ch, bandwidthIncludedDesc, snap.bandwidthIncluded, snap.name, snap.region)
	gauge(ch, priceDesc, snap.monthlyPrice, snap.name, snap.tierSlug)
	gauge(ch, infoDesc, 1, snap.name, snap.region, snap.tierSlug, snap.tierName)
}

// collectRepositories emits the per-repository metrics.
func collectRepositories(ch chan<- prometheus.Metric, snap *snapshot) {
	for _, repo := range snap.repositories {
		gauge(ch, tagsDesc, repo.tags, snap.name, repo.name)
		gauge(ch, manifestsDesc, repo.manifests, snap.name, repo.name)
		if repo.manifest {
			gauge(ch, manifestSizeDesc, repo.manifestSize, snap.name, repo.name)
		}
		if repo.pushed {
			gauge(ch, lastPushDesc, repo.pushedAt, snap.name, repo.name)
		}
	}
}

// gauge sends one gauge sample of desc with the given label values.
func gauge(ch chan<- prometheus.Metric, desc *prometheus.Desc, value float64, labels ...string) {
	ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, value, labels...)
}
