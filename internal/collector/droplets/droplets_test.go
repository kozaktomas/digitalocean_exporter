package droplets_test

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

	"github.com/kozaktomas/digitalocean_exporter/internal/collector/droplets"
	"github.com/kozaktomas/digitalocean_exporter/internal/doclient"
)

// Two droplets: one running an image with a slug, one powered off and running
// a custom image, which has no slug and must fall back to its name.
const dropletsJSON = `{"droplets":[` +
	`{"id":1,"name":"web-1","status":"active","vcpus":2,"memory":4096,"disk":80,` +
	`"region":{"slug":"fra1"},"size_slug":"s-2vcpu-4gb",` +
	`"size":{"slug":"s-2vcpu-4gb","price_hourly":0.02679,"price_monthly":18},` +
	`"image":{"slug":"ubuntu-24-04","distribution":"Ubuntu","name":"24.04 (LTS) x64"}},` +
	`{"id":2,"name":"db-1","status":"off","vcpus":4,"memory":8192,"disk":160,` +
	`"region":{"slug":"ams3"},"size_slug":"s-4vcpu-8gb",` +
	`"size":{"slug":"s-4vcpu-8gb","price_hourly":0.07143,"price_monthly":48},` +
	`"image":{"distribution":"Debian","name":"do-kube-1.35"}}` +
	`],"meta":{"total":2}}`

const dropletMetrics = `
# HELP digitalocean_droplet_cpus Number of virtual CPUs of the droplet.
# TYPE digitalocean_droplet_cpus gauge
digitalocean_droplet_cpus{id="1",name="web-1",region="fra1"} 2
digitalocean_droplet_cpus{id="2",name="db-1",region="ams3"} 4
# HELP digitalocean_droplet_disk_bytes Disk of the droplet.
# TYPE digitalocean_droplet_disk_bytes gauge
digitalocean_droplet_disk_bytes{id="1",name="web-1",region="fra1"} 85899345920
digitalocean_droplet_disk_bytes{id="2",name="db-1",region="ams3"} 171798691840
# HELP digitalocean_droplet_info Always 1. Its labels describe the droplet's size, status and image.
# TYPE digitalocean_droplet_info gauge
digitalocean_droplet_info{id="1",image="ubuntu-24-04",name="web-1",region="fra1",size="s-2vcpu-4gb",status="active"} 1
digitalocean_droplet_info{id="2",image="do-kube-1.35",name="db-1",region="ams3",size="s-4vcpu-8gb",status="off"} 1
# HELP digitalocean_droplet_memory_bytes Memory of the droplet.
# TYPE digitalocean_droplet_memory_bytes gauge
digitalocean_droplet_memory_bytes{id="1",name="web-1",region="fra1"} 4294967296
digitalocean_droplet_memory_bytes{id="2",name="db-1",region="ams3"} 8589934592
# HELP digitalocean_droplet_price_hourly Price of the droplet per hour in US dollars.
# TYPE digitalocean_droplet_price_hourly gauge
digitalocean_droplet_price_hourly{id="1",name="web-1",region="fra1"} 0.02679
digitalocean_droplet_price_hourly{id="2",name="db-1",region="ams3"} 0.07143
# HELP digitalocean_droplet_price_monthly Price of the droplet per month in US dollars.
# TYPE digitalocean_droplet_price_monthly gauge
digitalocean_droplet_price_monthly{id="1",name="web-1",region="fra1"} 18
digitalocean_droplet_price_monthly{id="2",name="db-1",region="ams3"} 48
# HELP digitalocean_droplet_up Whether the droplet is active.
# TYPE digitalocean_droplet_up gauge
digitalocean_droplet_up{id="1",name="web-1",region="fra1"} 1
digitalocean_droplet_up{id="2",name="db-1",region="ams3"} 0
`

// newTestCollector wires a collector to a fake DigitalOcean API.
func newTestCollector(t *testing.T, handler http.HandlerFunc) *droplets.Collector {
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
	return droplets.New(client)
}

func okHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/v2/droplets" {
		_, _ = w.Write([]byte(dropletsJSON))
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func TestCollectAfterRefresh(t *testing.T) {
	c := newTestCollector(t, okHandler)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if err := testutil.CollectAndCompare(c, strings.NewReader(dropletMetrics)); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// An account with more droplets than fit on one page is paginated by page
// number, and every page has to reach the snapshot.
func TestRefreshFollowsPages(t *testing.T) {
	page := func(id int, name string, next bool) string {
		links := `"links":{}`
		if next {
			links = `"links":{"pages":{"next":"https://api.digitalocean.com/v2/droplets?page=2"}}`
		}
		return fmt.Sprintf(`{"droplets":[{"id":%d,"name":%q,"status":"active","vcpus":1,"memory":1024,`+
			`"disk":25,"region":{"slug":"fra1"},"size":{"slug":"s-1vcpu-1gb"},"image":{"slug":"debian-12"}}],`+
			`%s,"meta":{"total":2}}`, id, name, links)
	}

	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write([]byte(page(2, "second", false)))
			return
		}
		_, _ = w.Write([]byte(page(1, "first", true)))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	const want = `
# HELP digitalocean_droplet_up Whether the droplet is active.
# TYPE digitalocean_droplet_up gauge
digitalocean_droplet_up{id="1",name="first",region="fra1"} 1
digitalocean_droplet_up{id="2",name="second",region="fra1"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want), "digitalocean_droplet_up"); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// An account with no droplets is a normal state: the refresh succeeds and
// there is simply nothing to report.
func TestRefreshWithoutDropletsSucceeds(t *testing.T) {
	c := newTestCollector(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"droplets":[],"meta":{"total":0}}`))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh without droplets: %v", err)
	}
	if got := testutil.CollectAndCount(c); got != 0 {
		t.Errorf("metric count without droplets = %d, want 0", got)
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

	if err := testutil.CollectAndCompare(c, strings.NewReader(dropletMetrics)); err != nil {
		t.Errorf("unexpected metrics after a failed refresh: %v", err)
	}
}

func TestName(t *testing.T) {
	c := newTestCollector(t, okHandler)
	if got := c.Name(); got != "droplets" {
		t.Errorf("Name() = %q, want %q", got, "droplets")
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
	if want := 7; count != want {
		t.Errorf("Describe sent %d descriptors, want %d", count, want)
	}
}
