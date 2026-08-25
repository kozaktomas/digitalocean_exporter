package spaces_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kozaktomas/digitalocean_exporter/internal/collector/spaces"
)

// twoBuckets is a small account: one bucket that needs three pages to list,
// one that fits in a single page.
func twoBuckets() map[string]*stubBucket {
	return map[string]*stubBucket{
		"images": {region: "fra1", pageSize: 2, objects: []object{
			{"a", 100}, {"b", 200}, {"c", 300}, {"d", 400}, {"e", 500},
		}},
		"logs": {region: "ams3", objects: []object{{"x", 1024}}},
	}
}

func TestCollectAfterRefresh(t *testing.T) {
	api, factory := newStubAPI(t, twoBuckets())
	c := newCollector(t, factory, []spaces.Bucket{
		{Name: "images", Region: "fra1"},
		{Name: "logs", Region: "ams3"},
	}, "fra1")

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	expected := `
# HELP digitalocean_spaces_bucket_objects Number of objects in the bucket.
# TYPE digitalocean_spaces_bucket_objects gauge
digitalocean_spaces_bucket_objects{bucket="images",region="fra1"} 5
digitalocean_spaces_bucket_objects{bucket="logs",region="ams3"} 1
# HELP digitalocean_spaces_bucket_size_bytes Total size of every object in the bucket.
# TYPE digitalocean_spaces_bucket_size_bytes gauge
digitalocean_spaces_bucket_size_bytes{bucket="images",region="fra1"} 1500
digitalocean_spaces_bucket_size_bytes{bucket="logs",region="ams3"} 1024
# HELP digitalocean_spaces_bucket_up Whether the bucket's last listing succeeded.
# TYPE digitalocean_spaces_bucket_up gauge
digitalocean_spaces_bucket_up{bucket="images",region="fra1"} 1
digitalocean_spaces_bucket_up{bucket="logs",region="ams3"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected)); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}

	// Three pages for images (2+2+1) and one for logs.
	if api.listObjectCnt != 4 {
		t.Errorf("ListObjectsV2 calls = %d, want 4 (three pages plus one)", api.listObjectCnt)
	}
}

func TestCollectBeforeRefreshEmitsNothing(t *testing.T) {
	_, factory := newStubAPI(t, twoBuckets())
	c := newCollector(t, factory, []spaces.Bucket{{Name: "images", Region: "fra1"}}, "fra1")

	if got := testutil.CollectAndCount(c); got != 0 {
		t.Errorf("metric count before the first refresh = %d, want 0", got)
	}
}

func TestEmptyBucketReportsZeroes(t *testing.T) {
	_, factory := newStubAPI(t, map[string]*stubBucket{"empty": {region: "fra1"}})
	c := newCollector(t, factory, []spaces.Bucket{{Name: "empty", Region: "fra1"}}, "fra1")

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	expected := `
# HELP digitalocean_spaces_bucket_objects Number of objects in the bucket.
# TYPE digitalocean_spaces_bucket_objects gauge
digitalocean_spaces_bucket_objects{bucket="empty",region="fra1"} 0
# HELP digitalocean_spaces_bucket_size_bytes Total size of every object in the bucket.
# TYPE digitalocean_spaces_bucket_size_bytes gauge
digitalocean_spaces_bucket_size_bytes{bucket="empty",region="fra1"} 0
# HELP digitalocean_spaces_bucket_up Whether the bucket's last listing succeeded.
# TYPE digitalocean_spaces_bucket_up gauge
digitalocean_spaces_bucket_up{bucket="empty",region="fra1"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected)); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// A key scoped to one bucket is forbidden on the others. That must cost only
// the bucket it happened to, never the ones that listed fine.
func TestForbiddenBucketDoesNotCostTheOthers(t *testing.T) {
	buckets := twoBuckets()
	buckets["logs"].forbidden = true
	_, factory := newStubAPI(t, buckets)
	c := newCollector(t, factory, []spaces.Bucket{
		{Name: "images", Region: "fra1"},
		{Name: "logs", Region: "ams3"},
	}, "fra1")

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh with one forbidden bucket: %v", err)
	}

	expected := `
# HELP digitalocean_spaces_bucket_up Whether the bucket's last listing succeeded.
# TYPE digitalocean_spaces_bucket_up gauge
digitalocean_spaces_bucket_up{bucket="images",region="fra1"} 1
digitalocean_spaces_bucket_up{bucket="logs",region="ams3"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"digitalocean_spaces_bucket_up"); err != nil {
		t.Errorf("up metric: %v", err)
	}

	// A bucket that never listed has nothing to report but its failure: no
	// size, no object count, rather than a zero that reads as an empty bucket.
	sizes := `
# HELP digitalocean_spaces_bucket_size_bytes Total size of every object in the bucket.
# TYPE digitalocean_spaces_bucket_size_bytes gauge
digitalocean_spaces_bucket_size_bytes{bucket="images",region="fra1"} 1500
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(sizes),
		"digitalocean_spaces_bucket_size_bytes"); err != nil {
		t.Errorf("size metric: %v", err)
	}
}

func TestFailedBucketKeepsItsPreviousValues(t *testing.T) {
	buckets := twoBuckets()
	_, factory := newStubAPI(t, buckets)
	c := newCollector(t, factory, []spaces.Bucket{
		{Name: "images", Region: "fra1"},
		{Name: "logs", Region: "ams3"},
	}, "fra1")

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}

	buckets["images"].forbidden = true
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("second refresh: %v", err)
	}

	expected := `
# HELP digitalocean_spaces_bucket_size_bytes Total size of every object in the bucket.
# TYPE digitalocean_spaces_bucket_size_bytes gauge
digitalocean_spaces_bucket_size_bytes{bucket="images",region="fra1"} 1500
digitalocean_spaces_bucket_size_bytes{bucket="logs",region="ams3"} 1024
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"digitalocean_spaces_bucket_size_bytes"); err != nil {
		t.Errorf("size after a failed listing: %v", err)
	}

	up := `
# HELP digitalocean_spaces_bucket_up Whether the bucket's last listing succeeded.
# TYPE digitalocean_spaces_bucket_up gauge
digitalocean_spaces_bucket_up{bucket="images",region="fra1"} 0
digitalocean_spaces_bucket_up{bucket="logs",region="ams3"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(up),
		"digitalocean_spaces_bucket_up"); err != nil {
		t.Errorf("up after a failed listing: %v", err)
	}
}

func TestEveryBucketFailingIsARefreshFailure(t *testing.T) {
	buckets := twoBuckets()
	buckets["images"].forbidden = true
	buckets["logs"].forbidden = true
	_, factory := newStubAPI(t, buckets)
	c := newCollector(t, factory, []spaces.Bucket{
		{Name: "images", Region: "fra1"},
		{Name: "logs", Region: "ams3"},
	}, "fra1")

	if err := c.Refresh(context.Background()); err == nil {
		t.Fatal("expected an error when no bucket could be listed")
	}
}

func TestDiscoveryFindsBucketsAndTheirRegions(t *testing.T) {
	_, factory := newStubAPI(t, twoBuckets())
	c := newCollector(t, factory, nil, "fra1")

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh in discovery mode: %v", err)
	}

	expected := `
# HELP digitalocean_spaces_bucket_size_bytes Total size of every object in the bucket.
# TYPE digitalocean_spaces_bucket_size_bytes gauge
digitalocean_spaces_bucket_size_bytes{bucket="images",region="fra1"} 1500
digitalocean_spaces_bucket_size_bytes{bucket="logs",region="ams3"} 1024
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"digitalocean_spaces_bucket_size_bytes"); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// Listing all buckets is a full-access capability. A limited key gets 403 and
// the operator needs to be told what to do about it, not handed a bare S3
// error.
func TestDiscoveryDeniedExplainsTheBucketList(t *testing.T) {
	api, factory := newStubAPI(t, twoBuckets())
	api.denyListAll = true
	c := newCollector(t, factory, nil, "fra1")

	err := c.Refresh(context.Background())
	if err == nil {
		t.Fatal("expected discovery to fail with a limited key")
	}
	if !strings.Contains(err.Error(), "--collector.spaces.bucket") {
		t.Errorf("error = %q, want it to point at the explicit bucket list", err)
	}
}

// A bucket that fails is not a refresh failure, so the scheduler never logs
// it. The collector has to say what went wrong itself, or an operator sees
// up 0 with no reason anywhere.
func TestFailedBucketIsLogged(t *testing.T) {
	buckets := twoBuckets()
	buckets["logs"].forbidden = true
	_, factory := newStubAPI(t, buckets)

	var logged bytes.Buffer
	c := spaces.New(spaces.Config{
		Factory:     factory,
		Buckets:     []spaces.Bucket{{Name: "images", Region: "fra1"}, {Name: "logs", Region: "ams3"}},
		Region:      "fra1",
		Concurrency: 2,
		Logger:      slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	out := logged.String()
	if !strings.Contains(out, "logs") {
		t.Errorf("log = %q, want it to name the bucket that failed", out)
	}
	if !strings.Contains(out, "AccessDenied") {
		t.Errorf("log = %q, want it to carry the underlying error", out)
	}
	if strings.Contains(out, "images") {
		t.Errorf("log = %q, want nothing logged about the bucket that succeeded", out)
	}
}
