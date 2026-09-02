package registry_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kozaktomas/digitalocean_exporter/internal/collector/registry"
	"github.com/kozaktomas/digitalocean_exporter/internal/doclient"
)

const registryJSON = `{"registry":{"name":"acme","created_at":"2025-07-08T10:51:46Z",` +
	`"region":"fra1","storage_usage_bytes":16228798464,` +
	`"storage_usage_bytes_updated_at":"2026-08-31T04:00:00Z","read_only":false}}`

const subscriptionJSON = `{"subscription":{"tier":{"name":"Professional","slug":"professional",` +
	`"included_repositories":0,"included_storage_bytes":107374182400,"allow_storage_overage":true,` +
	`"included_bandwidth_bytes":107374182400,"monthly_price_in_cents":2000},` +
	`"created_at":"2025-07-08T10:51:46Z","updated_at":"2026-04-20T13:06:11Z"}}`

// Three repositories: an ordinary one, one whose name contains a slash, and
// one that has never been pushed to and therefore carries no manifest.
const repositoriesJSON = `{"repositories":[` +
	`{"registry_name":"acme","name":"api","tag_count":6,"manifest_count":5,` +
	`"latest_manifest":{"digest":"sha256:aaa","compressed_size_bytes":27453550,` +
	`"size_bytes":80000000,"updated_at":"2026-08-24T12:00:00Z"}},` +
	`{"registry_name":"acme","name":"web/nginx","tag_count":3,"manifest_count":3,` +
	`"latest_manifest":{"digest":"sha256:bbb","compressed_size_bytes":26287124,` +
	`"size_bytes":70000000,"updated_at":"2026-08-20T08:30:00Z"}},` +
	`{"registry_name":"acme","name":"empty","tag_count":0,"manifest_count":0}` +
	`],"meta":{"total":3}}`

const registryMetrics = `
# HELP digitalocean_registry_bandwidth_included_bytes Outbound transfer included in the subscription tier each month.
# TYPE digitalocean_registry_bandwidth_included_bytes gauge
digitalocean_registry_bandwidth_included_bytes{region="fra1",registry="acme"} 107374182400
# HELP digitalocean_registry_info Always 1. Its labels name the registry, its region and its subscription tier.
# TYPE digitalocean_registry_info gauge
digitalocean_registry_info{region="fra1",registry="acme",tier="professional",tier_name="Professional"} 1
# HELP digitalocean_registry_repositories Number of repositories in the registry.
# TYPE digitalocean_registry_repositories gauge
digitalocean_registry_repositories{registry="acme"} 3
# HELP digitalocean_registry_repository_last_push_timestamp_seconds Unix timestamp of the last push to the repository.
# TYPE digitalocean_registry_repository_last_push_timestamp_seconds gauge
digitalocean_registry_repository_last_push_timestamp_seconds{registry="acme",repository="api"} 1787572800
digitalocean_registry_repository_last_push_timestamp_seconds{registry="acme",repository="web/nginx"} 1787214600
# HELP digitalocean_registry_repository_latest_manifest_size_bytes Compressed size of the repository's newest manifest.
# TYPE digitalocean_registry_repository_latest_manifest_size_bytes gauge
digitalocean_registry_repository_latest_manifest_size_bytes{registry="acme",repository="api"} 27453550
digitalocean_registry_repository_latest_manifest_size_bytes{registry="acme",repository="web/nginx"} 26287124
# HELP digitalocean_registry_repository_manifests Number of manifests in the repository.
# TYPE digitalocean_registry_repository_manifests gauge
digitalocean_registry_repository_manifests{registry="acme",repository="api"} 5
digitalocean_registry_repository_manifests{registry="acme",repository="empty"} 0
digitalocean_registry_repository_manifests{registry="acme",repository="web/nginx"} 3
# HELP digitalocean_registry_repository_tags Number of tags in the repository.
# TYPE digitalocean_registry_repository_tags gauge
digitalocean_registry_repository_tags{registry="acme",repository="api"} 6
digitalocean_registry_repository_tags{registry="acme",repository="empty"} 0
digitalocean_registry_repository_tags{registry="acme",repository="web/nginx"} 3
# HELP digitalocean_registry_storage_included_bytes Storage included in the subscription tier.
# TYPE digitalocean_registry_storage_included_bytes gauge
digitalocean_registry_storage_included_bytes{region="fra1",registry="acme"} 107374182400
# HELP digitalocean_registry_storage_usage_bytes Storage the registry uses, as last measured by DigitalOcean.
# TYPE digitalocean_registry_storage_usage_bytes gauge
digitalocean_registry_storage_usage_bytes{region="fra1",registry="acme"} 16228798464
# HELP digitalocean_registry_storage_usage_updated_timestamp_seconds When DigitalOcean last measured that storage.
# TYPE digitalocean_registry_storage_usage_updated_timestamp_seconds gauge
digitalocean_registry_storage_usage_updated_timestamp_seconds{region="fra1",registry="acme"} 1788148800
# HELP digitalocean_registry_subscription_monthly_price_usd Monthly price of the subscription tier in US dollars.
# TYPE digitalocean_registry_subscription_monthly_price_usd gauge
digitalocean_registry_subscription_monthly_price_usd{registry="acme",tier="professional"} 20
# HELP digitalocean_registry_up Whether the last refresh could list the registry's repositories.
# TYPE digitalocean_registry_up gauge
digitalocean_registry_up{region="fra1",registry="acme"} 1
`

// newTestCollector wires a collector to a fake DigitalOcean API.
func newTestCollector(t *testing.T, handler http.HandlerFunc) *registry.Collector {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	client, err := doclient.New(doclient.Config{
		Token: "token", BaseURL: srv.URL + "/", UserAgent: "test", Timeout: 5 * time.Second,
		// One attempt: retrying a stubbed failure only makes this test sit
		// through the backoff, and the retries have their own test in doclient.
		MaxAttempts: 1, Metrics: doclient.NewMetrics(prometheus.NewRegistry()),
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return registry.New(client, nil)
}

// okHandler serves the single-registry endpoints. It answers 404 to anything
// else, including the multi-registry list a refresh tries first.
func okHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/v2/registry":
		_, _ = w.Write([]byte(registryJSON))
	case "/v2/registry/subscription":
		_, _ = w.Write([]byte(subscriptionJSON))
	case "/v2/registry/acme/repositoriesV2":
		_, _ = w.Write([]byte(repositoriesJSON))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestCollectAfterRefresh(t *testing.T) {
	c := newTestCollector(t, okHandler)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if err := testutil.CollectAndCompare(c, strings.NewReader(registryMetrics)); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// A registry with more repositories than fit on one page is paginated with a
// page token, and every page has to reach the snapshot.
func TestRefreshFollowsRepositoryPages(t *testing.T) {
	const firstPage = `{"repositories":[{"name":"api","tag_count":6,"manifest_count":5}],"links":{"pages":` +
		`{"next":"https://api.digitalocean.com/v2/registry/acme/repositoriesV2?page_token=second"}},` +
		`"meta":{"total":2}}`
	const secondPage = `{"repositories":[{"name":"web","tag_count":1,"manifest_count":1}],"meta":{"total":2}}`

	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/registry":
			_, _ = w.Write([]byte(registryJSON))
		case "/v2/registry/subscription":
			_, _ = w.Write([]byte(subscriptionJSON))
		case "/v2/registry/acme/repositoriesV2":
			if r.URL.Query().Get("page_token") == "second" {
				_, _ = w.Write([]byte(secondPage))
				return
			}
			_, _ = w.Write([]byte(firstPage))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	const want = `
# HELP digitalocean_registry_repositories Number of repositories in the registry.
# TYPE digitalocean_registry_repositories gauge
digitalocean_registry_repositories{registry="acme"} 2
# HELP digitalocean_registry_repository_tags Number of tags in the repository.
# TYPE digitalocean_registry_repository_tags gauge
digitalocean_registry_repository_tags{registry="acme",repository="api"} 6
digitalocean_registry_repository_tags{registry="acme",repository="web"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want),
		"digitalocean_registry_repositories", "digitalocean_registry_repository_tags"); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// An account may simply have no container registry. The API answers 404 there,
// which is a legitimate state rather than a failure: the refresh succeeds and
// the collector reports nothing, so collector_success stays 1.
func TestRefreshWithoutRegistrySucceedsAndEmitsNothing(t *testing.T) {
	c := newTestCollector(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"id":"not_found","message":"The resource you requested could not be found."}`))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh without a registry: %v", err)
	}
	if got := testutil.CollectAndCount(c); got != 0 {
		t.Errorf("metric count without a registry = %d, want 0", got)
	}
}

// A registry that is deleted while the exporter runs stops being reported,
// rather than freezing on its last known size.
func TestRefreshDropsMetricsWhenRegistryDisappears(t *testing.T) {
	var gone bool
	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		if gone {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"id":"not_found","message":"not found"}`))
			return
		}
		okHandler(w, r)
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	gone = true
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh after the registry was deleted: %v", err)
	}

	if got := testutil.CollectAndCount(c); got != 0 {
		t.Errorf("metric count after the registry was deleted = %d, want 0", got)
	}
}

// A token without the registry scope gets 403, which is a real failure and
// must not be mistaken for an account without a registry.
func TestRefreshFailsOnForbidden(t *testing.T) {
	c := newTestCollector(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"id":"forbidden","message":"You are not authorized to perform this operation."}`))
	})

	if err := c.Refresh(context.Background()); err == nil {
		t.Fatal("expected a forbidden registry endpoint to fail the refresh")
	}
}

func TestCollectBeforeRefreshEmitsNothing(t *testing.T) {
	c := newTestCollector(t, okHandler)
	if got := testutil.CollectAndCount(c); got != 0 {
		t.Errorf("metric count before the first refresh = %d, want 0", got)
	}
}

func TestFailedRefreshKeepsPreviousSnapshot(t *testing.T) {
	var fail bool
	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		okHandler(w, r)
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}

	fail = true
	if err := c.Refresh(context.Background()); err == nil {
		t.Fatal("expected the second refresh to fail")
	}

	if err := testutil.CollectAndCompare(c, strings.NewReader(registryMetrics)); err != nil {
		t.Errorf("unexpected metrics after a failed refresh: %v", err)
	}
}

func TestName(t *testing.T) {
	c := newTestCollector(t, okHandler)
	if got := c.Name(); got != "registry" {
		t.Errorf("Name() = %q, want %q", got, "registry")
	}
}

func TestDescribeCoversEveryMetric(t *testing.T) {
	c := newTestCollector(t, okHandler)

	ch := make(chan *prometheus.Desc, 32)
	c.Describe(ch)
	close(ch)

	var count int
	for range ch {
		count++
	}
	if want := 12; count != want {
		t.Errorf("Describe sent %d descriptors, want %d", count, want)
	}
}

// registriesJSON is what the multi-registry endpoint answers for an account
// holding two of them.
const registriesJSON = `{"registries":[` +
	`{"name":"acme","region":"fra1","storage_usage_bytes":16228798464,` +
	`"storage_usage_bytes_updated_at":"2026-08-31T04:00:00Z","created_at":"2025-07-08T10:51:46Z"},` +
	`{"name":"backup","region":"nyc3","storage_usage_bytes":4096,` +
	`"storage_usage_bytes_updated_at":"2026-08-31T05:00:00Z","created_at":"2026-01-02T09:00:00Z"}` +
	`],"total_storage_usage_bytes":16228802560}`

const backupRepositoriesJSON = `{"repositories":[` +
	`{"registry_name":"backup","name":"db","tag_count":2,"manifest_count":2}` +
	`],"meta":{"total":1}}`

// An account on a Professional plan can hold several registries, and once it
// does the single-registry endpoints stop answering. Every one of them is
// measured, and a registry that cannot be read keeps what it last reported
// instead of costing the ones that could.
func TestRefreshCollectsEveryRegistry(t *testing.T) {
	var failBackup bool
	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/registries":
			_, _ = w.Write([]byte(registriesJSON))
		case "/v2/registries/subscription":
			_, _ = w.Write([]byte(subscriptionJSON))
		case "/v2/registries/acme/repositoriesV2":
			_, _ = w.Write([]byte(repositoriesJSON))
		case "/v2/registries/backup/repositoriesV2":
			if failBackup {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte(backupRepositoriesJSON))
		default:
			t.Errorf("unexpected request for %s: the single-registry endpoints must not be used here", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	const both = `
# HELP digitalocean_registry_repositories Number of repositories in the registry.
# TYPE digitalocean_registry_repositories gauge
digitalocean_registry_repositories{registry="acme"} 3
digitalocean_registry_repositories{registry="backup"} 1
# HELP digitalocean_registry_repository_tags Number of tags in the repository.
# TYPE digitalocean_registry_repository_tags gauge
digitalocean_registry_repository_tags{registry="acme",repository="api"} 6
digitalocean_registry_repository_tags{registry="acme",repository="empty"} 0
digitalocean_registry_repository_tags{registry="acme",repository="web/nginx"} 3
digitalocean_registry_repository_tags{registry="backup",repository="db"} 2
# HELP digitalocean_registry_storage_usage_bytes Storage the registry uses, as last measured by DigitalOcean.
# TYPE digitalocean_registry_storage_usage_bytes gauge
digitalocean_registry_storage_usage_bytes{region="fra1",registry="acme"} 16228798464
digitalocean_registry_storage_usage_bytes{region="nyc3",registry="backup"} 4096
# HELP digitalocean_registry_storage_usage_updated_timestamp_seconds When DigitalOcean last measured that storage.
# TYPE digitalocean_registry_storage_usage_updated_timestamp_seconds gauge
digitalocean_registry_storage_usage_updated_timestamp_seconds{region="fra1",registry="acme"} 1788148800
digitalocean_registry_storage_usage_updated_timestamp_seconds{region="nyc3",registry="backup"} 1788152400
# HELP digitalocean_registry_subscription_monthly_price_usd Monthly price of the subscription tier in US dollars.
# TYPE digitalocean_registry_subscription_monthly_price_usd gauge
digitalocean_registry_subscription_monthly_price_usd{registry="acme",tier="professional"} 20
digitalocean_registry_subscription_monthly_price_usd{registry="backup",tier="professional"} 20
# HELP digitalocean_registry_up Whether the last refresh could list the registry's repositories.
# TYPE digitalocean_registry_up gauge
digitalocean_registry_up{region="fra1",registry="acme"} 1
digitalocean_registry_up{region="nyc3",registry="backup"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(both), multiRegistryMetricNames...); err != nil {
		t.Errorf("unexpected metrics for two registries: %v", err)
	}

	// One registry now fails to list its repositories. The refresh still
	// succeeds, the other registry stays current, and the failing one keeps
	// the repositories it last reported while saying it is down.
	failBackup = true
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh with one failing registry: %v", err)
	}

	const afterFailure = `
# HELP digitalocean_registry_repositories Number of repositories in the registry.
# TYPE digitalocean_registry_repositories gauge
digitalocean_registry_repositories{registry="acme"} 3
digitalocean_registry_repositories{registry="backup"} 1
# HELP digitalocean_registry_repository_tags Number of tags in the repository.
# TYPE digitalocean_registry_repository_tags gauge
digitalocean_registry_repository_tags{registry="acme",repository="api"} 6
digitalocean_registry_repository_tags{registry="acme",repository="empty"} 0
digitalocean_registry_repository_tags{registry="acme",repository="web/nginx"} 3
digitalocean_registry_repository_tags{registry="backup",repository="db"} 2
# HELP digitalocean_registry_storage_usage_bytes Storage the registry uses, as last measured by DigitalOcean.
# TYPE digitalocean_registry_storage_usage_bytes gauge
digitalocean_registry_storage_usage_bytes{region="fra1",registry="acme"} 16228798464
digitalocean_registry_storage_usage_bytes{region="nyc3",registry="backup"} 4096
# HELP digitalocean_registry_storage_usage_updated_timestamp_seconds When DigitalOcean last measured that storage.
# TYPE digitalocean_registry_storage_usage_updated_timestamp_seconds gauge
digitalocean_registry_storage_usage_updated_timestamp_seconds{region="fra1",registry="acme"} 1788148800
digitalocean_registry_storage_usage_updated_timestamp_seconds{region="nyc3",registry="backup"} 1788152400
# HELP digitalocean_registry_subscription_monthly_price_usd Monthly price of the subscription tier in US dollars.
# TYPE digitalocean_registry_subscription_monthly_price_usd gauge
digitalocean_registry_subscription_monthly_price_usd{registry="acme",tier="professional"} 20
digitalocean_registry_subscription_monthly_price_usd{registry="backup",tier="professional"} 20
# HELP digitalocean_registry_up Whether the last refresh could list the registry's repositories.
# TYPE digitalocean_registry_up gauge
digitalocean_registry_up{region="fra1",registry="acme"} 1
digitalocean_registry_up{region="nyc3",registry="backup"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(afterFailure), multiRegistryMetricNames...); err != nil {
		t.Errorf("unexpected metrics after one registry failed: %v", err)
	}
}

// multiRegistryMetricNames are the metrics the multi-registry test compares.
var multiRegistryMetricNames = []string{
	"digitalocean_registry_repositories",
	"digitalocean_registry_repository_tags",
	"digitalocean_registry_storage_usage_bytes",
	"digitalocean_registry_storage_usage_updated_timestamp_seconds",
	"digitalocean_registry_subscription_monthly_price_usd",
	"digitalocean_registry_up",
}

// A registry seen for the first time whose repositories cannot be listed has
// no count to carry forward, and reporting zero would read as a registry
// holding nothing. It reports its size and that it is down, and nothing else.
func TestRefreshOmitsRepositoriesOfARegistryNeverListed(t *testing.T) {
	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/registries":
			_, _ = w.Write([]byte(registriesJSON))
		case "/v2/registries/subscription":
			_, _ = w.Write([]byte(subscriptionJSON))
		case "/v2/registries/acme/repositoriesV2":
			_, _ = w.Write([]byte(repositoriesJSON))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	const want = `
# HELP digitalocean_registry_repositories Number of repositories in the registry.
# TYPE digitalocean_registry_repositories gauge
digitalocean_registry_repositories{registry="acme"} 3
# HELP digitalocean_registry_up Whether the last refresh could list the registry's repositories.
# TYPE digitalocean_registry_up gauge
digitalocean_registry_up{region="fra1",registry="acme"} 1
digitalocean_registry_up{region="nyc3",registry="backup"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want),
		"digitalocean_registry_repositories", "digitalocean_registry_up"); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// Every registry failing leaves nothing current to report, and that is a
// failed refresh rather than an isolated one.
func TestRefreshFailsWhenNoRegistryCanBeListed(t *testing.T) {
	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/registries":
			_, _ = w.Write([]byte(registriesJSON))
		case "/v2/registries/subscription":
			_, _ = w.Write([]byte(subscriptionJSON))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})

	err := c.Refresh(context.Background())
	if !errors.Is(err, registry.ErrNoRepositoriesListed) {
		t.Fatalf("refresh error = %v, want %v", err, registry.ErrNoRepositoriesListed)
	}
}

// An account whose API does not offer the multi-registry endpoint, or offers
// it and names nothing, is read through the single-registry endpoints, so it
// keeps working exactly as before.
func TestRefreshFallsBackToTheSingleRegistry(t *testing.T) {
	for name, registries := range map[string]http.HandlerFunc{
		"not found": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"id":"not_found","message":"The resource you requested could not be found."}`))
		},
		"unavailable": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		},
		"empty": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"registries":[]}`))
		},
	} {
		t.Run(name, func(t *testing.T) {
			var asked bool
			c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/v2/registries" {
					asked = true
					registries(w, r)
					return
				}
				okHandler(w, r)
			})

			if err := c.Refresh(context.Background()); err != nil {
				t.Fatalf("refresh: %v", err)
			}
			if !asked {
				t.Error("the multi-registry endpoint was never asked")
			}
			if err := testutil.CollectAndCompare(c, strings.NewReader(registryMetrics)); err != nil {
				t.Errorf("unexpected metrics from the single-registry path: %v", err)
			}
		})
	}
}
