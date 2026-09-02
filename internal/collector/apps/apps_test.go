package apps_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kozaktomas/digitalocean_exporter/internal/collector/apps"
	"github.com/kozaktomas/digitalocean_exporter/internal/doclient"
)

// Three apps across two pages, each in a state the metrics exist to tell apart:
// a build that failed, a rollout under way over a deployment that is still
// serving, and an app created a moment ago that has never had one.
//
// storefront also carries a static site beside its service, which App Platform
// runs no instances for; brand-new declares its region only in its spec, which
// is what an app looks like before DigitalOcean has resolved it.
const appsPage1 = `{"apps":[` +
	`{"id":"app-1","tier_slug":"basic","region":{"slug":"fra1"},` +
	`"default_ingress":"https://storefront.ondigitalocean.app",` +
	`"created_at":"2026-01-01T00:00:00Z","last_deployment_active_at":"2026-08-30T12:00:00Z",` +
	`"active_deployment":{"id":"dep-1","phase":"ERROR"},` +
	`"spec":{"name":"storefront","region":"fra",` +
	`"services":[{"name":"web","instance_count":2,"instance_size_slug":"apps-s-1vcpu-1gb"}],` +
	`"static_sites":[{"name":"docs"}]}},` +
	`{"id":"app-2","tier_slug":"professional","region":{"slug":"ams3"},` +
	`"default_ingress":"https://pipeline.ondigitalocean.app",` +
	`"created_at":"2026-02-14T09:30:00Z","last_deployment_active_at":"2026-08-31T18:45:00Z",` +
	`"active_deployment":{"id":"dep-2","phase":"ACTIVE"},` +
	`"in_progress_deployment":{"id":"dep-3","phase":"DEPLOYING"},` +
	`"spec":{"name":"pipeline","region":"ams",` +
	`"workers":[{"name":"queue","instance_count":3,"instance_size_slug":"apps-d-1vcpu-1gb"}],` +
	`"jobs":[{"name":"migrate","instance_count":1,"instance_size_slug":"apps-s-1vcpu-0.5gb"}]}}` +
	`],"links":{"pages":{"next":"https://api.digitalocean.com/v2/apps?page=2"}},"meta":{"total":3}}`

const appsPage2 = `{"apps":[` +
	`{"id":"app-3","tier_slug":"basic","default_ingress":"",` +
	`"created_at":"2026-09-01T06:00:00Z",` +
	`"in_progress_deployment":{"id":"dep-4","phase":"PENDING_BUILD"},` +
	`"spec":{"name":"brand-new","region":"nyc",` +
	`"static_sites":[{"name":"landing"}]}}` +
	`],"links":{},"meta":{"total":3}}`

//nolint:lll // golden exposition text: one series per line, unwrappable.
const appMetrics = `
# HELP digitalocean_app_component_instances Number of instances the app's spec asks for of this component.
# TYPE digitalocean_app_component_instances gauge
digitalocean_app_component_instances{component="docs",id="app-1",instance_size="",kind="static_site",name="storefront"} 0
digitalocean_app_component_instances{component="landing",id="app-3",instance_size="",kind="static_site",name="brand-new"} 0
digitalocean_app_component_instances{component="migrate",id="app-2",instance_size="apps-s-1vcpu-0.5gb",kind="job",name="pipeline"} 1
digitalocean_app_component_instances{component="queue",id="app-2",instance_size="apps-d-1vcpu-1gb",kind="worker",name="pipeline"} 3
digitalocean_app_component_instances{component="web",id="app-1",instance_size="apps-s-1vcpu-1gb",kind="service",name="storefront"} 2
# HELP digitalocean_app_created_timestamp_seconds Creation time of the app as a Unix timestamp.
# TYPE digitalocean_app_created_timestamp_seconds gauge
digitalocean_app_created_timestamp_seconds{id="app-1",name="storefront"} 1.7672256e+09
digitalocean_app_created_timestamp_seconds{id="app-2",name="pipeline"} 1.7710614e+09
digitalocean_app_created_timestamp_seconds{id="app-3",name="brand-new"} 1.7882424e+09
# HELP digitalocean_app_deployment_in_progress Whether a deployment of the app is currently in progress.
# TYPE digitalocean_app_deployment_in_progress gauge
digitalocean_app_deployment_in_progress{id="app-1",name="storefront"} 0
digitalocean_app_deployment_in_progress{id="app-2",name="pipeline"} 1
digitalocean_app_deployment_in_progress{id="app-3",name="brand-new"} 1
# HELP digitalocean_app_deployment_phase Always 1 for the phase the app's active deployment is in and 0 for every other known one.
# TYPE digitalocean_app_deployment_phase gauge
digitalocean_app_deployment_phase{id="app-1",name="storefront",phase="ACTIVE"} 0
digitalocean_app_deployment_phase{id="app-1",name="storefront",phase="BUILDING"} 0
digitalocean_app_deployment_phase{id="app-1",name="storefront",phase="CANCELED"} 0
digitalocean_app_deployment_phase{id="app-1",name="storefront",phase="DEPLOYING"} 0
digitalocean_app_deployment_phase{id="app-1",name="storefront",phase="ERROR"} 1
digitalocean_app_deployment_phase{id="app-1",name="storefront",phase="PENDING_BUILD"} 0
digitalocean_app_deployment_phase{id="app-1",name="storefront",phase="PENDING_DEPLOY"} 0
digitalocean_app_deployment_phase{id="app-1",name="storefront",phase="SUPERSEDED"} 0
digitalocean_app_deployment_phase{id="app-2",name="pipeline",phase="ACTIVE"} 1
digitalocean_app_deployment_phase{id="app-2",name="pipeline",phase="BUILDING"} 0
digitalocean_app_deployment_phase{id="app-2",name="pipeline",phase="CANCELED"} 0
digitalocean_app_deployment_phase{id="app-2",name="pipeline",phase="DEPLOYING"} 0
digitalocean_app_deployment_phase{id="app-2",name="pipeline",phase="ERROR"} 0
digitalocean_app_deployment_phase{id="app-2",name="pipeline",phase="PENDING_BUILD"} 0
digitalocean_app_deployment_phase{id="app-2",name="pipeline",phase="PENDING_DEPLOY"} 0
digitalocean_app_deployment_phase{id="app-2",name="pipeline",phase="SUPERSEDED"} 0
# HELP digitalocean_app_info Always 1. Its labels describe the app's tier, region and default ingress.
# TYPE digitalocean_app_info gauge
digitalocean_app_info{default_ingress="",id="app-3",name="brand-new",region="nyc",tier="basic"} 1
digitalocean_app_info{default_ingress="https://pipeline.ondigitalocean.app",id="app-2",name="pipeline",region="ams3",tier="professional"} 1
digitalocean_app_info{default_ingress="https://storefront.ondigitalocean.app",id="app-1",name="storefront",region="fra1",tier="basic"} 1
# HELP digitalocean_app_last_deployment_active_timestamp_seconds When the app's most recent deployment went active, as a Unix timestamp.
# TYPE digitalocean_app_last_deployment_active_timestamp_seconds gauge
digitalocean_app_last_deployment_active_timestamp_seconds{id="app-1",name="storefront"} 1.7880912e+09
digitalocean_app_last_deployment_active_timestamp_seconds{id="app-2",name="pipeline"} 1.7882019e+09
`

// newTestCollector wires a collector to a fake DigitalOcean API.
func newTestCollector(t *testing.T, handler http.HandlerFunc) *apps.Collector {
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
	return apps.New(client, nil)
}

// okHandler serves the three-app account across its two pages.
func okHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path != "/v2/apps" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if r.URL.Query().Get("page") == "2" {
		_, _ = w.Write([]byte(appsPage2))
		return
	}
	_, _ = w.Write([]byte(appsPage1))
}

func TestCollectAfterRefresh(t *testing.T) {
	c := newTestCollector(t, okHandler)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if err := testutil.CollectAndCompare(c, strings.NewReader(appMetrics)); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// An app between creation and its first successful build has no active
// deployment. Reporting every phase at zero for it would read as a deployment
// that is in none of them, so it reports no phase series at all.
func TestAppWithoutActiveDeploymentReportsNoPhase(t *testing.T) {
	c := newTestCollector(t, okHandler)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	const metrics = "digitalocean_app_deployment_phase"
	if got, want := testutil.CollectAndCount(c, metrics), 2*len(knownPhaseNames); got != want {
		t.Errorf("phase samples = %d, want %d — the app without an active deployment reported some",
			got, want)
	}
}

// knownPhaseNames mirrors the phases the collector reports, so the count above
// says what it is counting rather than repeating a bare 16.
var knownPhaseNames = []string{
	"PENDING_BUILD", "BUILDING", "PENDING_DEPLOY", "DEPLOYING",
	"ACTIVE", "SUPERSEDED", "ERROR", "CANCELED",
}

// A phase DigitalOcean invents after this was written is reported beside the
// documented ones. Dropping it would leave every phase series of that app at 0,
// which is what an app with no deployment at all looks like.
func TestUnknownPhaseIsReportedBesideTheKnownOnes(t *testing.T) {
	c := newTestCollector(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"apps":[{"id":"app-1","tier_slug":"basic",` +
			`"active_deployment":{"id":"dep-1","phase":"REWINDING"},` +
			`"spec":{"name":"odd"}}],"links":{},"meta":{"total":1}}`))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	//nolint:lll // golden exposition text: one series per line, unwrappable.
	const want = `
# HELP digitalocean_app_deployment_phase Always 1 for the phase the app's active deployment is in and 0 for every other known one.
# TYPE digitalocean_app_deployment_phase gauge
digitalocean_app_deployment_phase{id="app-1",name="odd",phase="ACTIVE"} 0
digitalocean_app_deployment_phase{id="app-1",name="odd",phase="BUILDING"} 0
digitalocean_app_deployment_phase{id="app-1",name="odd",phase="CANCELED"} 0
digitalocean_app_deployment_phase{id="app-1",name="odd",phase="DEPLOYING"} 0
digitalocean_app_deployment_phase{id="app-1",name="odd",phase="ERROR"} 0
digitalocean_app_deployment_phase{id="app-1",name="odd",phase="PENDING_BUILD"} 0
digitalocean_app_deployment_phase{id="app-1",name="odd",phase="PENDING_DEPLOY"} 0
digitalocean_app_deployment_phase{id="app-1",name="odd",phase="REWINDING"} 1
digitalocean_app_deployment_phase{id="app-1",name="odd",phase="SUPERSEDED"} 0
`
	const metric = "digitalocean_app_deployment_phase"
	if err := testutil.CollectAndCompare(c, strings.NewReader(want), metric); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// An app whose deployment has never gone active keeps its other metrics rather
// than reporting a timestamp at the epoch, which every "how long since it
// deployed" query would read as fifty-six years.
func TestAppWithoutADeploymentTimestampKeepsItsOtherMetrics(t *testing.T) {
	c := newTestCollector(t, okHandler)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	const metric = "digitalocean_app_last_deployment_active_timestamp_seconds"
	if got, want := testutil.CollectAndCount(c, metric), 2; got != want {
		t.Errorf("last deployment samples = %d, want %d", got, want)
	}

	const info = `
# HELP digitalocean_app_deployment_in_progress Whether a deployment of the app is currently in progress.
# TYPE digitalocean_app_deployment_in_progress gauge
digitalocean_app_deployment_in_progress{id="app-1",name="storefront"} 0
digitalocean_app_deployment_in_progress{id="app-2",name="pipeline"} 1
digitalocean_app_deployment_in_progress{id="app-3",name="brand-new"} 1
`
	const inProgress = "digitalocean_app_deployment_in_progress"
	if err := testutil.CollectAndCompare(c, strings.NewReader(info), inProgress); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// An account running no apps is a normal state: the refresh succeeds and there
// is simply nothing to report.
func TestRefreshWithoutAppsSucceeds(t *testing.T) {
	c := newTestCollector(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"apps":[],"links":{},"meta":{"total":0}}`))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh without apps: %v", err)
	}
	if got := testutil.CollectAndCount(c); got != 0 {
		t.Errorf("metric count without apps = %d, want 0", got)
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

	if err := testutil.CollectAndCompare(c, strings.NewReader(appMetrics)); err != nil {
		t.Errorf("unexpected metrics after a failed refresh: %v", err)
	}
}

// A failure on the second page must not leave half the account in the snapshot:
// the apps the first page carried would look like the whole of it.
func TestFailureOnASecondPageKeepsPreviousSnapshot(t *testing.T) {
	var fail bool
	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		if fail && r.URL.Query().Get("page") == "2" {
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
		t.Fatal("expected the refresh to fail on the second page")
	}

	if err := testutil.CollectAndCompare(c, strings.NewReader(appMetrics)); err != nil {
		t.Errorf("unexpected metrics after a failed second page: %v", err)
	}
}

func TestName(t *testing.T) {
	c := newTestCollector(t, okHandler)
	if got := c.Name(); got != "apps" {
		t.Errorf("Name() = %q, want %q", got, "apps")
	}
}

func TestDescribeCoversEveryMetric(t *testing.T) {
	c := newTestCollector(t, okHandler)

	ch := make(chan *prometheus.Desc, 16)
	c.Describe(ch)
	close(ch)

	var count int
	for range ch {
		count++
	}
	if want := 6; count != want {
		t.Errorf("Describe sent %d descriptors, want %d", count, want)
	}
}

// The list can shift between two page requests — an app created or destroyed
// while the pages are being read — and the same app then arrives on both. It
// has to reach the snapshot once: two entries would be two series with
// identical labels, which fails the whole scrape rather than one metric.
func TestRefreshDropsADuplicateAppOnTwoPages(t *testing.T) {
	page := func(next bool) string {
		links := `"links":{}`
		if next {
			links = `"links":{"pages":{"next":"https://api.digitalocean.com/v2/apps?page=2"}}`
		}
		return fmt.Sprintf(`{"apps":[{"id":"app-1","tier_slug":"basic",`+
			`"spec":{"name":"only","region":"fra"}}],%s,"meta":{"total":1}}`, links)
	}

	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(page(r.URL.Query().Get("page") != "2")))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	const want = `
# HELP digitalocean_app_info Always 1. Its labels describe the app's tier, region and default ingress.
# TYPE digitalocean_app_info gauge
digitalocean_app_info{default_ingress="",id="app-1",name="only",region="fra",tier="basic"} 1
`
	const metric = "digitalocean_app_info"
	if err := testutil.CollectAndCompare(c, strings.NewReader(want), metric); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}
