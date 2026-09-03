package projects_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kozaktomas/digitalocean_exporter/internal/collector/projects"
	"github.com/kozaktomas/digitalocean_exporter/internal/doclient"
)

// Two projects: the default one owning a mixed bag of resources — including a
// URN of a shape the exporter does not know, which must land under "unknown"
// rather than vanish — and a staging one owning a single droplet.
const projectsJSON = `{"projects":[` +
	`{"id":"p-1","name":"prod","purpose":"Web app","environment":"Production","is_default":true},` +
	`{"id":"p-2","name":"staging","purpose":"Testing","environment":"Staging","is_default":false}` +
	`],"meta":{"total":2}}`

const prodResourcesJSON = `{"resources":[` +
	`{"urn":"do:droplet:1"},{"urn":"do:droplet:2"},` +
	`{"urn":"do:volume:aa"},{"urn":"do:loadbalancer:bb"},{"urn":"do:domain:example.com"},` +
	`{"urn":"something-else"}` +
	`],"meta":{"total":6}}`

const stagingResourcesJSON = `{"resources":[{"urn":"do:droplet:3"}],"meta":{"total":1}}`

const projectMetrics = `
# HELP digitalocean_project_info Always 1. Its labels describe the project.
# TYPE digitalocean_project_info gauge
digitalocean_project_info{environment="Production",id="p-1",is_default="true",name="prod",purpose="Web app"} 1
digitalocean_project_info{environment="Staging",id="p-2",is_default="false",name="staging",purpose="Testing"} 1
# HELP digitalocean_project_resources Number of resources of the given type the project owns, by URN type.
# TYPE digitalocean_project_resources gauge
digitalocean_project_resources{id="p-1",name="prod",type="domain"} 1
digitalocean_project_resources{id="p-1",name="prod",type="droplet"} 2
digitalocean_project_resources{id="p-1",name="prod",type="loadbalancer"} 1
digitalocean_project_resources{id="p-1",name="prod",type="unknown"} 1
digitalocean_project_resources{id="p-1",name="prod",type="volume"} 1
digitalocean_project_resources{id="p-2",name="staging",type="droplet"} 1
`

// newTestCollector wires a collector to a fake DigitalOcean API.
func newTestCollector(t *testing.T, handler http.HandlerFunc) *projects.Collector {
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
	return projects.New(client, nil)
}

// okHandler serves the two-project account.
func okHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/v2/projects":
		_, _ = w.Write([]byte(projectsJSON))
	case "/v2/projects/p-1/resources":
		_, _ = w.Write([]byte(prodResourcesJSON))
	case "/v2/projects/p-2/resources":
		_, _ = w.Write([]byte(stagingResourcesJSON))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestCollectAfterRefresh(t *testing.T) {
	c := newTestCollector(t, okHandler)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if err := testutil.CollectAndCompare(c, strings.NewReader(projectMetrics)); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// An account with more projects than fit on one page is paginated by page
// number, and every page has to reach the snapshot. The resources list of a
// single project pages the same way, and its pages have to be added up.
func TestRefreshFollowsPages(t *testing.T) {
	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/projects":
			if r.URL.Query().Get("page") == "2" {
				_, _ = w.Write([]byte(`{"projects":[{"id":"p-2","name":"staging",` +
					`"purpose":"Testing","environment":"Staging","is_default":false}],` +
					`"links":{},"meta":{"total":2}}`))
				return
			}
			_, _ = w.Write([]byte(`{"projects":[{"id":"p-1","name":"prod",` +
				`"purpose":"Web app","environment":"Production","is_default":true}],` +
				`"links":{"pages":{"next":"https://api.digitalocean.com/v2/projects?page=2"}},` +
				`"meta":{"total":2}}`))
		case "/v2/projects/p-1/resources":
			if r.URL.Query().Get("page") == "2" {
				_, _ = w.Write([]byte(`{"resources":[{"urn":"do:volume:aa"}],"links":{},"meta":{"total":2}}`))
				return
			}
			_, _ = w.Write([]byte(`{"resources":[{"urn":"do:droplet:1"}],` +
				`"links":{"pages":{"next":"https://api.digitalocean.com/v2/projects/p-1/resources?page=2"}},` +
				`"meta":{"total":2}}`))
		case "/v2/projects/p-2/resources":
			_, _ = w.Write([]byte(stagingResourcesJSON))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	const want = `
# HELP digitalocean_project_resources Number of resources of the given type the project owns, by URN type.
# TYPE digitalocean_project_resources gauge
digitalocean_project_resources{id="p-1",name="prod",type="droplet"} 1
digitalocean_project_resources{id="p-1",name="prod",type="volume"} 1
digitalocean_project_resources{id="p-2",name="staging",type="droplet"} 1
`
	const metric = "digitalocean_project_resources"
	if err := testutil.CollectAndCompare(c, strings.NewReader(want), metric); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// One project's resources lookup failing must not cost the others: the failed
// project keeps the counts of its last successful lookup, the rest refresh,
// and the refresh as a whole still succeeds.
func TestOneFailingProjectKeepsItsPreviousCounts(t *testing.T) {
	var failProd bool
	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		if failProd && r.URL.Path == "/v2/projects/p-1/resources" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if !failProd && r.URL.Path == "/v2/projects/p-2/resources" {
			// The first refresh sees one droplet in staging; the second sees
			// two, which proves the healthy project did refresh.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(stagingResourcesJSON))
			return
		}
		if r.URL.Path == "/v2/projects/p-2/resources" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"resources":[{"urn":"do:droplet:3"},{"urn":"do:droplet:4"}],` +
				`"meta":{"total":2}}`))
			return
		}
		okHandler(w, r)
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}

	failProd = true
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("second refresh with one failing project: %v", err)
	}

	const want = `
# HELP digitalocean_project_resources Number of resources of the given type the project owns, by URN type.
# TYPE digitalocean_project_resources gauge
digitalocean_project_resources{id="p-1",name="prod",type="domain"} 1
digitalocean_project_resources{id="p-1",name="prod",type="droplet"} 2
digitalocean_project_resources{id="p-1",name="prod",type="loadbalancer"} 1
digitalocean_project_resources{id="p-1",name="prod",type="unknown"} 1
digitalocean_project_resources{id="p-1",name="prod",type="volume"} 1
digitalocean_project_resources{id="p-2",name="staging",type="droplet"} 2
`
	const metric = "digitalocean_project_resources"
	if err := testutil.CollectAndCompare(c, strings.NewReader(want), metric); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// A project whose resources have never been listed successfully reports its
// info and no counts: a fabricated zero would be indistinguishable from an
// empty project.
func TestNeverMeasuredProjectEmitsInfoOnly(t *testing.T) {
	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/projects/p-1/resources" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		okHandler(w, r)
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	const want = `
# HELP digitalocean_project_resources Number of resources of the given type the project owns, by URN type.
# TYPE digitalocean_project_resources gauge
digitalocean_project_resources{id="p-2",name="staging",type="droplet"} 1
`
	const metric = "digitalocean_project_resources"
	if err := testutil.CollectAndCompare(c, strings.NewReader(want), metric); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}

	const info = `
# HELP digitalocean_project_info Always 1. Its labels describe the project.
# TYPE digitalocean_project_info gauge
digitalocean_project_info{environment="Production",id="p-1",is_default="true",name="prod",purpose="Web app"} 1
digitalocean_project_info{environment="Staging",id="p-2",is_default="false",name="staging",purpose="Testing"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(info), "digitalocean_project_info"); err != nil {
		t.Errorf("unexpected info metrics: %v", err)
	}
}

// Every project failing at once is a different incident from one project
// failing: nothing was measured, so the refresh fails and the previous
// snapshot survives untouched.
func TestAllProjectsFailingFailsTheRefresh(t *testing.T) {
	var fail bool
	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		if fail && strings.HasSuffix(r.URL.Path, "/resources") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		okHandler(w, r)
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}

	fail = true
	err := c.Refresh(context.Background())
	if !errors.Is(err, projects.ErrNoProjectMeasured) {
		t.Fatalf("second refresh error = %v, want ErrNoProjectMeasured", err)
	}

	if err := testutil.CollectAndCompare(c, strings.NewReader(projectMetrics)); err != nil {
		t.Errorf("unexpected metrics after a failed refresh: %v", err)
	}
}

// A failed project list fails the refresh before any resources are asked for.
func TestFailedProjectListKeepsPreviousSnapshot(t *testing.T) {
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

	if err := testutil.CollectAndCompare(c, strings.NewReader(projectMetrics)); err != nil {
		t.Errorf("unexpected metrics after a failed refresh: %v", err)
	}
}

// An account with no projects is a normal state: the refresh succeeds and
// there is simply nothing to report.
func TestRefreshWithoutProjectsSucceeds(t *testing.T) {
	c := newTestCollector(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"projects":[],"meta":{"total":0}}`))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh without projects: %v", err)
	}
	if got := testutil.CollectAndCount(c); got != 0 {
		t.Errorf("metric count without projects = %d, want 0", got)
	}
}

func TestCollectBeforeRefreshEmitsNothing(t *testing.T) {
	c := newTestCollector(t, okHandler)
	if got := testutil.CollectAndCount(c); got != 0 {
		t.Errorf("metric count before the first refresh = %d, want 0", got)
	}
}

func TestName(t *testing.T) {
	c := newTestCollector(t, okHandler)
	if got := c.Name(); got != "projects" {
		t.Errorf("Name() = %q, want %q", got, "projects")
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
	if want := 2; count != want {
		t.Errorf("Describe sent %d descriptors, want %d", count, want)
	}
}

// The list can shift between two page requests and the same project then
// arrives on both pages. It has to reach the snapshot once.
func TestRefreshDropsADuplicateProjectOnTwoPages(t *testing.T) {
	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/projects":
			links := `"links":{"pages":{"next":"https://api.digitalocean.com/v2/projects?page=2"}}`
			if r.URL.Query().Get("page") == "2" {
				links = `"links":{}`
			}
			_, _ = fmt.Fprintf(w, `{"projects":[{"id":"p-1","name":"prod",`+
				`"purpose":"Web app","environment":"Production","is_default":true}],%s,`+
				`"meta":{"total":1}}`, links)
		case "/v2/projects/p-1/resources":
			_, _ = w.Write([]byte(`{"resources":[{"urn":"do:droplet:1"}],"meta":{"total":1}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	const want = `
# HELP digitalocean_project_info Always 1. Its labels describe the project.
# TYPE digitalocean_project_info gauge
digitalocean_project_info{environment="Production",id="p-1",is_default="true",name="prod",purpose="Web app"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want), "digitalocean_project_info"); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}
