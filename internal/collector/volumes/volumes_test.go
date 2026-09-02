package volumes_test

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

	"github.com/kozaktomas/digitalocean_exporter/internal/collector/volumes"
	"github.com/kozaktomas/digitalocean_exporter/internal/doclient"
)

// Two volumes: one attached to a droplet, one orphaned. The orphan also has no
// filesystem, which is what an unformatted volume looks like.
const volumesJSON = `{"volumes":[` +
	`{"id":"vol-1","name":"data","region":{"slug":"fra1"},"size_gigabytes":100,` +
	`"filesystem_type":"ext4","filesystem_label":"data","droplet_ids":[42]},` +
	`{"id":"vol-2","name":"orphan","region":{"slug":"ams3"},"size_gigabytes":10,` +
	`"filesystem_type":"","filesystem_label":"","droplet_ids":[]}` +
	`],"meta":{"total":2}}`

const volumeMetrics = `
# HELP digitalocean_volume_droplets Number of droplets the volume is attached to. Zero means it is billed but unused.
# TYPE digitalocean_volume_droplets gauge
digitalocean_volume_droplets{id="vol-1",name="data",region="fra1"} 1
digitalocean_volume_droplets{id="vol-2",name="orphan",region="ams3"} 0
# HELP digitalocean_volume_info Always 1. Its labels describe the volume's filesystem.
# TYPE digitalocean_volume_info gauge
digitalocean_volume_info{filesystem_label="data",filesystem_type="ext4",id="vol-1",name="data",region="fra1"} 1
digitalocean_volume_info{filesystem_label="",filesystem_type="",id="vol-2",name="orphan",region="ams3"} 1
# HELP digitalocean_volume_size_bytes Size of the volume in bytes.
# TYPE digitalocean_volume_size_bytes gauge
digitalocean_volume_size_bytes{id="vol-1",name="data",region="fra1"} 1.073741824e+11
digitalocean_volume_size_bytes{id="vol-2",name="orphan",region="ams3"} 1.073741824e+10
`

// newTestCollector wires a collector to a fake DigitalOcean API.
func newTestCollector(t *testing.T, handler http.HandlerFunc) *volumes.Collector {
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
	return volumes.New(client, nil)
}

// okHandler serves the two-volume account.
func okHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/v2/volumes" {
		_, _ = w.Write([]byte(volumesJSON))
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func TestCollectAfterRefresh(t *testing.T) {
	c := newTestCollector(t, okHandler)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if err := testutil.CollectAndCompare(c, strings.NewReader(volumeMetrics)); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// An account with more volumes than fit on one page is paginated by page
// number, and every page has to reach the snapshot.
func TestRefreshFollowsPages(t *testing.T) {
	page := func(id, name string, next bool) string {
		links := `"links":{}`
		if next {
			links = `"links":{"pages":{"next":"https://api.digitalocean.com/v2/volumes?page=2"}}`
		}
		return fmt.Sprintf(`{"volumes":[{"id":%q,"name":%q,"region":{"slug":"fra1"},`+
			`"size_gigabytes":1,"droplet_ids":[]}],%s,"meta":{"total":2}}`, id, name, links)
	}

	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write([]byte(page("vol-2", "second", false)))
			return
		}
		_, _ = w.Write([]byte(page("vol-1", "first", true)))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	const want = `
# HELP digitalocean_volume_size_bytes Size of the volume in bytes.
# TYPE digitalocean_volume_size_bytes gauge
digitalocean_volume_size_bytes{id="vol-1",name="first",region="fra1"} 1.073741824e+09
digitalocean_volume_size_bytes{id="vol-2",name="second",region="fra1"} 1.073741824e+09
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want), "digitalocean_volume_size_bytes"); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// An account with no volumes is a normal state: the refresh succeeds and there
// is simply nothing to report.
func TestRefreshWithoutVolumesSucceeds(t *testing.T) {
	c := newTestCollector(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"volumes":[],"meta":{"total":0}}`))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh without volumes: %v", err)
	}
	if got := testutil.CollectAndCount(c); got != 0 {
		t.Errorf("metric count without volumes = %d, want 0", got)
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

	if err := testutil.CollectAndCompare(c, strings.NewReader(volumeMetrics)); err != nil {
		t.Errorf("unexpected metrics after a failed refresh: %v", err)
	}
}

func TestName(t *testing.T) {
	c := newTestCollector(t, okHandler)
	if got := c.Name(); got != "volumes" {
		t.Errorf("Name() = %q, want %q", got, "volumes")
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
	if want := 3; count != want {
		t.Errorf("Describe sent %d descriptors, want %d", count, want)
	}
}

// The list can shift between two page requests — a resource created or
// destroyed while the pages are being read — and the same volume then arrives
// on both. It has to reach the snapshot once: two entries would be two series
// with identical labels, which fails the whole scrape rather than one metric.
func TestRefreshDropsADuplicateVolumeOnTwoPages(t *testing.T) {
	page := func(next bool) string {
		links := `"links":{}`
		if next {
			links = `"links":{"pages":{"next":"https://api.digitalocean.com/v2/volumes?page=2"}}`
		}
		return fmt.Sprintf(`{"volumes":[{"id":"vol-1","name":"first","region":{"slug":"fra1"},`+
			`"size_gigabytes":1,"droplet_ids":[]}],%s,"meta":{"total":1}}`, links)
	}

	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(page(r.URL.Query().Get("page") != "2")))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	const want = `
# HELP digitalocean_volume_size_bytes Size of the volume in bytes.
# TYPE digitalocean_volume_size_bytes gauge
digitalocean_volume_size_bytes{id="vol-1",name="first",region="fra1"} 1.073741824e+09
`
	const metric = "digitalocean_volume_size_bytes"
	if err := testutil.CollectAndCompare(c, strings.NewReader(want), metric); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}
