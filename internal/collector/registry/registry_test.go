package registry_test

import (
	"context"
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
	`"region":"fra1","storage_usage_bytes":16228798464,"read_only":false}}`

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
# HELP digitalocean_registry_subscription_monthly_price_usd Monthly price of the subscription tier in US dollars.
# TYPE digitalocean_registry_subscription_monthly_price_usd gauge
digitalocean_registry_subscription_monthly_price_usd{registry="acme",tier="professional"} 20
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

// okHandler serves the three endpoints one refresh reads.
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
	if want := 10; count != want {
		t.Errorf("Describe sent %d descriptors, want %d", count, want)
	}
}
