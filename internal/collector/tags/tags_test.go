package tags_test

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

	"github.com/kozaktomas/digitalocean_exporter/internal/collector/tags"
	"github.com/kozaktomas/digitalocean_exporter/internal/doclient"
)

// Three tags covering the cases that change the output: one carried by several
// resource types, one whose response omits some types entirely — the absent
// ones must be skipped, not exported as zero — and one attached to nothing,
// which emits no series at all.
const tagsJSON = `{"tags":[` +
	`{"name":"prod","resources":{"count":4,` +
	`"droplets":{"count":2},"volumes":{"count":1},"databases":{"count":1}}},` +
	`{"name":"backup","resources":{"count":3,` +
	`"volume_snapshots":{"count":2},"images":{"count":1}}},` +
	`{"name":"unused","resources":{"count":0}}` +
	`],"meta":{"total":3}}`

const tagMetrics = `
# HELP digitalocean_tag_resources Number of resources of the given type carrying the tag.
# TYPE digitalocean_tag_resources gauge
digitalocean_tag_resources{tag="backup",type="image"} 1
digitalocean_tag_resources{tag="backup",type="volume_snapshot"} 2
digitalocean_tag_resources{tag="prod",type="database"} 1
digitalocean_tag_resources{tag="prod",type="droplet"} 2
digitalocean_tag_resources{tag="prod",type="volume"} 1
`

// newTestCollector wires a collector to a fake DigitalOcean API.
func newTestCollector(t *testing.T, handler http.HandlerFunc) *tags.Collector {
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
	return tags.New(client, nil)
}

// okHandler serves the three-tag account.
func okHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/v2/tags" {
		_, _ = w.Write([]byte(tagsJSON))
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func TestCollectAfterRefresh(t *testing.T) {
	c := newTestCollector(t, okHandler)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if err := testutil.CollectAndCompare(c, strings.NewReader(tagMetrics)); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// An account with more tags than fit on one page is paginated by page number,
// and every page has to reach the snapshot.
func TestRefreshFollowsPages(t *testing.T) {
	page := func(name string, count int, next bool) string {
		links := `"links":{}`
		if next {
			links = `"links":{"pages":{"next":"https://api.digitalocean.com/v2/tags?page=2"}}`
		}
		return fmt.Sprintf(`{"tags":[{"name":%q,"resources":{"count":%d,`+
			`"droplets":{"count":%d}}}],%s,"meta":{"total":2}}`, name, count, count, links)
	}

	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write([]byte(page("web", 3, false)))
			return
		}
		_, _ = w.Write([]byte(page("db", 1, true)))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	const want = `
# HELP digitalocean_tag_resources Number of resources of the given type carrying the tag.
# TYPE digitalocean_tag_resources gauge
digitalocean_tag_resources{tag="db",type="droplet"} 1
digitalocean_tag_resources{tag="web",type="droplet"} 3
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want)); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// The list can shift between two page requests and the same tag then arrives
// on both pages. It has to reach the snapshot once: two entries would be two
// series with identical labels, which fails the whole scrape.
func TestRefreshDropsADuplicateTagOnTwoPages(t *testing.T) {
	page := func(next bool) string {
		links := `"links":{}`
		if next {
			links = `"links":{"pages":{"next":"https://api.digitalocean.com/v2/tags?page=2"}}`
		}
		return fmt.Sprintf(`{"tags":[{"name":"web","resources":{"count":1,`+
			`"droplets":{"count":1}}}],%s,"meta":{"total":1}}`, links)
	}

	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(page(r.URL.Query().Get("page") != "2")))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	const want = `
# HELP digitalocean_tag_resources Number of resources of the given type carrying the tag.
# TYPE digitalocean_tag_resources gauge
digitalocean_tag_resources{tag="web",type="droplet"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want)); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// An account with no tags is a normal state: the refresh succeeds and there is
// simply nothing to report.
func TestRefreshWithoutTagsSucceeds(t *testing.T) {
	c := newTestCollector(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tags":[],"meta":{"total":0}}`))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh without tags: %v", err)
	}
	if got := testutil.CollectAndCount(c); got != 0 {
		t.Errorf("metric count without tags = %d, want 0", got)
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

	if err := testutil.CollectAndCompare(c, strings.NewReader(tagMetrics)); err != nil {
		t.Errorf("unexpected metrics after a failed refresh: %v", err)
	}
}

func TestName(t *testing.T) {
	c := newTestCollector(t, okHandler)
	if got := c.Name(); got != "tags" {
		t.Errorf("Name() = %q, want %q", got, "tags")
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
	if want := 1; count != want {
		t.Errorf("Describe sent %d descriptors, want %d", count, want)
	}
}
