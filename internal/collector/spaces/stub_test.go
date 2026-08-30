package spaces_test

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kozaktomas/digitalocean_exporter/internal/collector/spaces"
	"github.com/kozaktomas/digitalocean_exporter/internal/spacesclient"
)

// stubBucket is what the fake Spaces API knows about one bucket.
type stubBucket struct {
	region  string
	objects int64
	bytes   int64
	// forbidden makes the bucket answer 403, as a key without a grant for it
	// is told.
	forbidden bool
	// noUsage answers the HEAD without the usage headers, as an S3 endpoint
	// that is not Ceph-backed would.
	noUsage bool
}

// authRegionPattern pulls the region out of the SigV4 credential scope, which
// is the only place the region of a request survives: every regional client of
// a test points at the one stub endpoint.
var authRegionPattern = regexp.MustCompile(`Credential=[^/]+/[^/]+/([^/]+)/`)

// stubAPI is a fake S3-compatible API: enough of HeadBucket, ListBuckets and
// GetBucketLocation to drive the collector.
//
// A bucket is keyed by its name, or by "name@region" to give two buckets that
// share a name different contents in different regions.
type stubAPI struct {
	buckets     map[string]*stubBucket
	denyListAll bool
	// headCnt is written from several handler goroutines at once: the
	// collector measures buckets in parallel.
	headCnt atomic.Int64
}

// newStubAPI starts the fake API and returns it with a factory pointed at it.
func newStubAPI(t *testing.T, buckets map[string]*stubBucket) (*stubAPI, *spacesclient.Factory) {
	t.Helper()
	api := &stubAPI{buckets: buckets}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	return api, spacesclient.NewFactory("key", "secret", srv.URL)
}

func (a *stubAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := strings.Trim(r.URL.Path, "/")
	switch {
	case r.Method == http.MethodHead:
		a.headBucket(w, name, authRegion(r))
	case name == "":
		a.listBuckets(w)
	case r.URL.Query().Has("location"):
		a.bucketLocation(w, name)
	default:
		writeS3Error(w, http.StatusBadRequest, "NotImplemented", "The stub serves HEAD only.")
	}
}

// authRegion returns the region a request was signed for.
func authRegion(r *http.Request) string {
	match := authRegionPattern.FindStringSubmatch(r.Header.Get("Authorization"))
	if match == nil {
		return ""
	}
	return match[1]
}

// bucketName drops the optional region suffix from a key.
func bucketName(key string) string {
	name, _, _ := strings.Cut(key, "@")
	return name
}

// headBucket answers with the Ceph usage headers the collector reads.
func (a *stubAPI) headBucket(w http.ResponseWriter, name, region string) {
	bucket, ok := a.buckets[name+"@"+region]
	if !ok {
		bucket, ok = a.buckets[name]
	}
	if !ok {
		writeS3Error(w, http.StatusNotFound, "NoSuchBucket", "No such bucket.")
		return
	}
	if bucket.forbidden {
		writeS3Error(w, http.StatusForbidden, "AccessDenied", "Access Denied.")
		return
	}
	a.headCnt.Add(1)

	if !bucket.noUsage {
		w.Header().Set("x-rgw-object-count", strconv.FormatInt(bucket.objects, 10))
		w.Header().Set("x-rgw-bytes-used", strconv.FormatInt(bucket.bytes, 10))
	}
	w.WriteHeader(http.StatusOK)
}

func (a *stubAPI) listBuckets(w http.ResponseWriter) {
	if a.denyListAll {
		writeS3Error(w, http.StatusForbidden, "AccessDenied", "Access Denied.")
		return
	}
	var entries strings.Builder
	for key := range a.buckets {
		fmt.Fprintf(&entries, "<Bucket><Name>%s</Name>"+
			"<CreationDate>2026-01-01T00:00:00.000Z</CreationDate></Bucket>", bucketName(key))
	}
	writeXML(w, http.StatusOK, `<ListAllMyBucketsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`+
		`<Owner><ID>owner</ID></Owner><Buckets>`+entries.String()+`</Buckets></ListAllMyBucketsResult>`)
}

func (a *stubAPI) bucketLocation(w http.ResponseWriter, name string) {
	bucket, ok := a.buckets[name]
	if !ok {
		writeS3Error(w, http.StatusNotFound, "NoSuchBucket", "No such bucket.")
		return
	}
	writeXML(w, http.StatusOK, `<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`+
		bucket.region+`</LocationConstraint>`)
}

func writeXML(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	// The body is XML this file assembled itself, never anything a caller
	// supplied, and this server only ever runs inside a test.
	_, _ = w.Write([]byte(xml.Header + body)) // #nosec G705
}

func writeS3Error(w http.ResponseWriter, status int, code, message string) {
	writeXML(w, status, fmt.Sprintf("<Error><Code>%s</Code><Message>%s</Message></Error>", code, message))
}

// newCollector wires a collector to the stubbed API.
func newCollector(
	t *testing.T, factory *spacesclient.Factory, buckets []spaces.Bucket, region string,
) *spaces.Collector {
	t.Helper()
	return spaces.New(spaces.Config{
		Factory:     factory,
		Buckets:     buckets,
		Region:      region,
		Concurrency: 2,
	})
}
