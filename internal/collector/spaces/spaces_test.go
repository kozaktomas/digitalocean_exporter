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

// twoBuckets is a small account: two buckets in two different regions.
func twoBuckets() map[string]*stubBucket {
	return map[string]*stubBucket{
		"images": {region: "fra1", objects: 5, bytes: 1500},
		"logs":   {region: "ams3", objects: 1, bytes: 1024},
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
# HELP digitalocean_spaces_bucket_size_bytes Bytes stored in the bucket, as Spaces accounts for them.
# TYPE digitalocean_spaces_bucket_size_bytes gauge
digitalocean_spaces_bucket_size_bytes{bucket="images",region="fra1"} 1500
digitalocean_spaces_bucket_size_bytes{bucket="logs",region="ams3"} 1024
# HELP digitalocean_spaces_bucket_up Whether the bucket's last measurement succeeded.
# TYPE digitalocean_spaces_bucket_up gauge
digitalocean_spaces_bucket_up{bucket="images",region="fra1"} 1
digitalocean_spaces_bucket_up{bucket="logs",region="ams3"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected)); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}

	// One request per bucket, whatever the buckets hold. That is the point.
	if got := api.headCnt.Load(); got != 2 {
		t.Errorf("HeadBucket calls = %d, want one per bucket", got)
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
# HELP digitalocean_spaces_bucket_size_bytes Bytes stored in the bucket, as Spaces accounts for them.
# TYPE digitalocean_spaces_bucket_size_bytes gauge
digitalocean_spaces_bucket_size_bytes{bucket="empty",region="fra1"} 0
# HELP digitalocean_spaces_bucket_up Whether the bucket's last measurement succeeded.
# TYPE digitalocean_spaces_bucket_up gauge
digitalocean_spaces_bucket_up{bucket="empty",region="fra1"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected)); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// A key scoped to one bucket is forbidden on the others. That must cost only
// the bucket it happened to, never the ones that measured fine.
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
# HELP digitalocean_spaces_bucket_up Whether the bucket's last measurement succeeded.
# TYPE digitalocean_spaces_bucket_up gauge
digitalocean_spaces_bucket_up{bucket="images",region="fra1"} 1
digitalocean_spaces_bucket_up{bucket="logs",region="ams3"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"digitalocean_spaces_bucket_up"); err != nil {
		t.Errorf("up metric: %v", err)
	}

	// A bucket never measured has nothing to report but its failure: no
	// size, no object count, rather than a zero that reads as an empty bucket.
	sizes := `
# HELP digitalocean_spaces_bucket_size_bytes Bytes stored in the bucket, as Spaces accounts for them.
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
# HELP digitalocean_spaces_bucket_size_bytes Bytes stored in the bucket, as Spaces accounts for them.
# TYPE digitalocean_spaces_bucket_size_bytes gauge
digitalocean_spaces_bucket_size_bytes{bucket="images",region="fra1"} 1500
digitalocean_spaces_bucket_size_bytes{bucket="logs",region="ams3"} 1024
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"digitalocean_spaces_bucket_size_bytes"); err != nil {
		t.Errorf("size after a failed measurement: %v", err)
	}

	up := `
# HELP digitalocean_spaces_bucket_up Whether the bucket's last measurement succeeded.
# TYPE digitalocean_spaces_bucket_up gauge
digitalocean_spaces_bucket_up{bucket="images",region="fra1"} 0
digitalocean_spaces_bucket_up{bucket="logs",region="ams3"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(up),
		"digitalocean_spaces_bucket_up"); err != nil {
		t.Errorf("up after a failed measurement: %v", err)
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
		t.Fatal("expected an error when no bucket could be measured")
	}
}

func TestDiscoveryFindsBucketsAndTheirRegions(t *testing.T) {
	_, factory := newStubAPI(t, twoBuckets())
	c := newCollector(t, factory, nil, "fra1")

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh in discovery mode: %v", err)
	}

	expected := `
# HELP digitalocean_spaces_bucket_size_bytes Bytes stored in the bucket, as Spaces accounts for them.
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
	// A HEAD has no body to carry an S3 error code, so a lost grant shows up
	// as a bare 403 and the message has to say what that means.
	if !strings.Contains(out, "403") || !strings.Contains(out, "no read grant") {
		t.Errorf("log = %q, want the status and what a 403 means", out)
	}
	if strings.Contains(out, "images") {
		t.Errorf("log = %q, want nothing logged about the bucket that succeeded", out)
	}
}

// The usage headers are a Ceph extension, not S3. An endpoint that answers a
// HEAD without them cannot be measured at all, and that has to surface as the
// bucket being down with a reason, not as a bucket of zero bytes.
func TestBucketWithoutUsageHeadersFails(t *testing.T) {
	buckets := twoBuckets()
	buckets["logs"].noUsage = true
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

	expected := `
# HELP digitalocean_spaces_bucket_up Whether the bucket's last measurement succeeded.
# TYPE digitalocean_spaces_bucket_up gauge
digitalocean_spaces_bucket_up{bucket="images",region="fra1"} 1
digitalocean_spaces_bucket_up{bucket="logs",region="ams3"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"digitalocean_spaces_bucket_up"); err != nil {
		t.Errorf("up metric: %v", err)
	}
	if out := logged.String(); !strings.Contains(out, "x-rgw-object-count") {
		t.Errorf("log = %q, want it to name the header that was missing", out)
	}
}

// A bucket name is unique within a region, not across the account, and the
// metrics say so with a region label. Keyed by name alone the two collapsed
// into one snapshot entry and only whichever measured last was reported.
func TestSameNameInTwoRegionsStaysTwoBuckets(t *testing.T) {
	_, factory := newStubAPI(t, map[string]*stubBucket{
		"backups@fra1": {region: "fra1", objects: 5, bytes: 1500},
		"backups@ams3": {region: "ams3", objects: 1, bytes: 1024},
	})
	c := newCollector(t, factory, []spaces.Bucket{
		{Name: "backups", Region: "fra1"},
		{Name: "backups", Region: "ams3"},
	}, "fra1")

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	expected := `
# HELP digitalocean_spaces_bucket_objects Number of objects in the bucket.
# TYPE digitalocean_spaces_bucket_objects gauge
digitalocean_spaces_bucket_objects{bucket="backups",region="ams3"} 1
digitalocean_spaces_bucket_objects{bucket="backups",region="fra1"} 5
# HELP digitalocean_spaces_bucket_size_bytes Bytes stored in the bucket, as Spaces accounts for them.
# TYPE digitalocean_spaces_bucket_size_bytes gauge
digitalocean_spaces_bucket_size_bytes{bucket="backups",region="ams3"} 1024
digitalocean_spaces_bucket_size_bytes{bucket="backups",region="fra1"} 1500
# HELP digitalocean_spaces_bucket_up Whether the bucket's last measurement succeeded.
# TYPE digitalocean_spaces_bucket_up gauge
digitalocean_spaces_bucket_up{bucket="backups",region="ams3"} 1
digitalocean_spaces_bucket_up{bucket="backups",region="fra1"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected)); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// One of the two failing keeps its own previous figures and marks only itself
// down, which needs the previous snapshot to be found under the same key.
func TestSameNameInTwoRegionsFailsIndependently(t *testing.T) {
	buckets := map[string]*stubBucket{
		"backups@fra1": {region: "fra1", objects: 5, bytes: 1500},
		"backups@ams3": {region: "ams3", objects: 1, bytes: 1024},
	}
	_, factory := newStubAPI(t, buckets)
	c := newCollector(t, factory, []spaces.Bucket{
		{Name: "backups", Region: "fra1"},
		{Name: "backups", Region: "ams3"},
	}, "fra1")

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}

	buckets["backups@ams3"].forbidden = true
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("second refresh: %v", err)
	}

	expected := `
# HELP digitalocean_spaces_bucket_size_bytes Bytes stored in the bucket, as Spaces accounts for them.
# TYPE digitalocean_spaces_bucket_size_bytes gauge
digitalocean_spaces_bucket_size_bytes{bucket="backups",region="ams3"} 1024
digitalocean_spaces_bucket_size_bytes{bucket="backups",region="fra1"} 1500
# HELP digitalocean_spaces_bucket_up Whether the bucket's last measurement succeeded.
# TYPE digitalocean_spaces_bucket_up gauge
digitalocean_spaces_bucket_up{bucket="backups",region="ams3"} 0
digitalocean_spaces_bucket_up{bucket="backups",region="fra1"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"digitalocean_spaces_bucket_size_bytes", "digitalocean_spaces_bucket_up"); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// Discovery locates one bucket at a time, and a bucket can refuse to be
// located: the key may have lost its grant on it, or it may have been created
// or destroyed between the listing and the location request. That must cost
// only that bucket. Failing the refresh blanked every other bucket of the
// account over one of them.
func TestDiscoveryToleratesAnUnlocatableBucket(t *testing.T) {
	buckets := map[string]*stubBucket{
		"images@fra1": {region: "fra1", objects: 5, bytes: 1500},
		"logs@ams3":   {region: "ams3", objects: 1, bytes: 1024, unlocatable: true},
	}
	_, factory := newStubAPI(t, buckets)

	var logged bytes.Buffer
	c := spaces.New(spaces.Config{
		Factory:     factory,
		Region:      "fra1",
		Concurrency: 2,
		Logger:      slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh with one unlocatable bucket: %v", err)
	}

	// The bucket that could not be located is assumed to be in the default
	// region, measured there, and marked down because it is not.
	expected := `
# HELP digitalocean_spaces_bucket_size_bytes Bytes stored in the bucket, as Spaces accounts for them.
# TYPE digitalocean_spaces_bucket_size_bytes gauge
digitalocean_spaces_bucket_size_bytes{bucket="images",region="fra1"} 1500
# HELP digitalocean_spaces_bucket_up Whether the bucket's last measurement succeeded.
# TYPE digitalocean_spaces_bucket_up gauge
digitalocean_spaces_bucket_up{bucket="images",region="fra1"} 1
digitalocean_spaces_bucket_up{bucket="logs",region="fra1"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"digitalocean_spaces_bucket_size_bytes", "digitalocean_spaces_bucket_up"); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}

	if out := logged.String(); !strings.Contains(out, "logs") ||
		!strings.Contains(out, "assuming the default region") {
		t.Errorf("log = %q, want the bucket that could not be located and what was assumed", out)
	}
}

// A bucket never changes region, so locating one is worth doing once. Doing it
// on every refresh cost a request per bucket, forever, to learn what the
// previous refresh already knew.
func TestDiscoveryCachesTheRegionItLocated(t *testing.T) {
	buckets := map[string]*stubBucket{
		"images@fra1": {region: "fra1", objects: 5, bytes: 1500},
		"logs@ams3":   {region: "ams3", objects: 1, bytes: 1024},
	}
	api, factory := newStubAPI(t, buckets)
	c := newCollector(t, factory, nil, "fra1")

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if got := api.locationCnt.Load(); got != 2 {
		t.Fatalf("GetBucketLocation calls on the first refresh = %d, want one per bucket", got)
	}

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	if got := api.locationCnt.Load(); got != 2 {
		t.Errorf("GetBucketLocation calls after the second refresh = %d, want the cache used", got)
	}

	// A measurement that fails is the one thing that can mean the remembered
	// region is wrong, so that bucket — and only that bucket — is located
	// again on the refresh after it.
	buckets["logs@ams3"].forbidden = true
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("third refresh: %v", err)
	}
	if got := api.locationCnt.Load(); got != 2 {
		t.Errorf("GetBucketLocation calls after the third refresh = %d, want the cache used", got)
	}

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("fourth refresh: %v", err)
	}
	if got := api.locationCnt.Load(); got != 3 {
		t.Errorf("GetBucketLocation calls after the fourth refresh = %d, "+
			"want the failed bucket located again and the measured one not", got)
	}
}
