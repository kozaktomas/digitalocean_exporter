package images_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kozaktomas/digitalocean_exporter/internal/collector/images"
	"github.com/kozaktomas/digitalocean_exporter/internal/doclient"
)

// The account holds one of each type, spread over two pages: a droplet
// snapshot, an automatic droplet backup and an uploaded custom image. The
// custom image is available in two regions written out of order, which is what
// the sorted regions label is for.
const firstPage = `{"images":[` +
	`{"id":1,"name":"snap1","type":"snapshot","distribution":"Ubuntu",` +
	`"status":"available","regions":["fra1"],"min_disk_size":25,"size_gigabytes":2.5,` +
	`"created_at":"2026-08-01T10:00:00Z"},` +
	`{"id":2,"name":"bk1","type":"backup","distribution":"Ubuntu",` +
	`"status":"available","regions":["fra1"],"min_disk_size":25,"size_gigabytes":1.25,` +
	`"created_at":"2026-08-20T03:00:00Z"}` +
	`],"links":{"pages":{"next":"https://api.digitalocean.com/v2/images?page=2&private=true"}},` +
	`"meta":{"total":3}}`

const secondPage = `{"images":[` +
	`{"id":3,"name":"gold","type":"custom","distribution":"Debian",` +
	`"status":"available","regions":["nyc3","ams3"],"min_disk_size":10,"size_gigabytes":0.5,` +
	`"created_at":"2026-01-15T12:30:00Z"}` +
	`],"links":{},"meta":{"total":3}}`

const imageMetrics = `
# HELP digitalocean_image_created_timestamp_seconds When the image was created, as a Unix timestamp.
# TYPE digitalocean_image_created_timestamp_seconds gauge
digitalocean_image_created_timestamp_seconds{id="1",name="snap1",type="snapshot"} 1.7855784e+09
digitalocean_image_created_timestamp_seconds{id="2",name="bk1",type="backup"} 1.7871948e+09
digitalocean_image_created_timestamp_seconds{id="3",name="gold",type="custom"} 1.7684802e+09
# HELP digitalocean_image_info Always 1. Its labels describe the image.
# TYPE digitalocean_image_info gauge
digitalocean_image_info{distribution="Ubuntu",id="1",name="snap1",regions="fra1",status="available",type="snapshot"} 1
digitalocean_image_info{distribution="Ubuntu",id="2",name="bk1",regions="fra1",status="available",type="backup"} 1
digitalocean_image_info{distribution="Debian",id="3",name="gold",regions="ams3,nyc3",status="available",type="custom"} 1
# HELP digitalocean_image_min_disk_size_bytes Smallest disk in bytes a droplet must have to boot this image.
# TYPE digitalocean_image_min_disk_size_bytes gauge
digitalocean_image_min_disk_size_bytes{id="1",name="snap1",type="snapshot"} 2.68435456e+10
digitalocean_image_min_disk_size_bytes{id="2",name="bk1",type="backup"} 2.68435456e+10
digitalocean_image_min_disk_size_bytes{id="3",name="gold",type="custom"} 1.073741824e+10
# HELP digitalocean_image_size_bytes Size of the stored image in bytes.
# TYPE digitalocean_image_size_bytes gauge
digitalocean_image_size_bytes{id="1",name="snap1",type="snapshot"} 2.68435456e+09
digitalocean_image_size_bytes{id="2",name="bk1",type="backup"} 1.34217728e+09
digitalocean_image_size_bytes{id="3",name="gold",type="custom"} 5.36870912e+08
# HELP digitalocean_images Number of private images of this type on the account.
# TYPE digitalocean_images gauge
digitalocean_images{type="backup"} 1
digitalocean_images{type="custom"} 1
digitalocean_images{type="snapshot"} 1
`

// newTestCollector wires a collector to a fake DigitalOcean API.
func newTestCollector(t *testing.T, handler http.HandlerFunc) *images.Collector {
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
	return images.New(client, nil)
}

// okHandler serves the three-image account over two pages.
func okHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path != "/v2/images" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if r.URL.Query().Get("page") == "2" {
		_, _ = w.Write([]byte(secondPage))
		return
	}
	_, _ = w.Write([]byte(firstPage))
}

func TestCollectAfterRefresh(t *testing.T) {
	c := newTestCollector(t, okHandler)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if err := testutil.CollectAndCompare(c, strings.NewReader(imageMetrics)); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// The public distribution and application images are DigitalOcean's, cost
// nothing and number in the hundreds. Only the account's own are asked for.
func TestRefreshAsksForPrivateImagesOnly(t *testing.T) {
	private := make(chan string, 4)
	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case private <- r.URL.Query().Get("private"):
		default:
		}
		okHandler(w, r)
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	close(private)

	for got := range private {
		if got != "true" {
			t.Errorf("private = %q, want %q", got, "true")
		}
	}
}

// An account with no images at all is a normal state. The per-type counts are
// still reported, because a backup policy that stopped running has to show as
// a zero rather than as a series that quietly went away.
func TestRefreshWithoutImagesStillCounts(t *testing.T) {
	c := newTestCollector(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"images":[],"links":{},"meta":{"total":0}}`))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh without images: %v", err)
	}

	const want = `
# HELP digitalocean_images Number of private images of this type on the account.
# TYPE digitalocean_images gauge
digitalocean_images{type="backup"} 0
digitalocean_images{type="custom"} 0
digitalocean_images{type="snapshot"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want)); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// An image whose creation time the API left out, or wrote in a format this
// cannot read, keeps its other metrics. Reporting the epoch instead would read
// as an image created in 1970 and age past every threshold there is.
func TestImageWithoutACreationTimeOmitsTheTimestamp(t *testing.T) {
	c := newTestCollector(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"images":[{"id":9,"name":"odd","type":"custom",` +
			`"status":"available","min_disk_size":10,"size_gigabytes":1,` +
			`"created_at":"whenever"}],"links":{},"meta":{"total":1}}`))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	const metric = "digitalocean_image_created_timestamp_seconds"
	if got := testutil.CollectAndCount(c, metric); got != 0 {
		t.Errorf("%s samples = %d, want 0", metric, got)
	}
	if got := testutil.CollectAndCount(c, "digitalocean_image_size_bytes"); got != 1 {
		t.Errorf("size samples = %d, want 1", got)
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

	if err := testutil.CollectAndCompare(c, strings.NewReader(imageMetrics)); err != nil {
		t.Errorf("unexpected metrics after a failed refresh: %v", err)
	}
}

// A deleted image has to leave the snapshot, or the account's stored bytes
// only ever grow.
func TestRefreshDropsImagesThatAreGone(t *testing.T) {
	var second bool
	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		if !second {
			okHandler(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"images":[{"id":3,"name":"gold","type":"custom",` +
			`"distribution":"Debian","status":"available","regions":["nyc3","ams3"],` +
			`"min_disk_size":10,"size_gigabytes":0.5,` +
			`"created_at":"2026-01-15T12:30:00Z"}],"links":{},"meta":{"total":1}}`))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	second = true
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("second refresh: %v", err)
	}

	const want = `
# HELP digitalocean_image_size_bytes Size of the stored image in bytes.
# TYPE digitalocean_image_size_bytes gauge
digitalocean_image_size_bytes{id="3",name="gold",type="custom"} 5.36870912e+08
# HELP digitalocean_images Number of private images of this type on the account.
# TYPE digitalocean_images gauge
digitalocean_images{type="backup"} 0
digitalocean_images{type="custom"} 1
digitalocean_images{type="snapshot"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want),
		"digitalocean_image_size_bytes", "digitalocean_images"); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// The list can shift between two page requests — an image created or destroyed
// while the pages are being read — and the same image then arrives on both. It
// has to reach the snapshot once: two entries would be two series with
// identical labels, which fails the whole scrape rather than one metric.
func TestRefreshDropsADuplicateImageOnTwoPages(t *testing.T) {
	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write([]byte(secondPage))
			return
		}
		_, _ = w.Write([]byte(strings.Replace(firstPage,
			`{"id":2,"name":"bk1","type":"backup"`,
			`{"id":3,"name":"gold","type":"custom"`, 1)))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	const want = `
# HELP digitalocean_images Number of private images of this type on the account.
# TYPE digitalocean_images gauge
digitalocean_images{type="backup"} 0
digitalocean_images{type="custom"} 1
digitalocean_images{type="snapshot"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want), "digitalocean_images"); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

func TestName(t *testing.T) {
	c := newTestCollector(t, okHandler)
	if got := c.Name(); got != "images" {
		t.Errorf("Name() = %q, want %q", got, "images")
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
	if want := 5; count != want {
		t.Errorf("Describe sent %d descriptors, want %d", count, want)
	}
}
