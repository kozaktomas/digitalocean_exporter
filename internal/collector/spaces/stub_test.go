package spaces_test

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kozaktomas/digitalocean_exporter/internal/collector/spaces"
	"github.com/kozaktomas/digitalocean_exporter/internal/spacesclient"
)

// object is one entry of a stubbed bucket listing.
type object struct {
	key  string
	size int64
}

// stubBucket is what the fake Spaces API knows about one bucket.
type stubBucket struct {
	region    string
	objects   []object
	pageSize  int
	forbidden bool
}

// stubAPI is a fake S3-compatible API: enough of ListObjectsV2, ListBuckets
// and GetBucketLocation to drive the collector, including pagination.
type stubAPI struct {
	buckets       map[string]*stubBucket
	denyListAll   bool
	listObjectCnt int
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
	case name == "":
		a.listBuckets(w)
	case r.URL.Query().Has("location"):
		a.bucketLocation(w, name)
	default:
		a.listObjects(w, r, name)
	}
}

func (a *stubAPI) listBuckets(w http.ResponseWriter) {
	if a.denyListAll {
		writeS3Error(w, http.StatusForbidden, "AccessDenied", "Access Denied.")
		return
	}
	var entries strings.Builder
	for name := range a.buckets {
		fmt.Fprintf(&entries, "<Bucket><Name>%s</Name>"+
			"<CreationDate>2026-01-01T00:00:00.000Z</CreationDate></Bucket>", name)
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

// listObjects serves one page, honouring the continuation token so the
// collector's paging is exercised for real.
func (a *stubAPI) listObjects(w http.ResponseWriter, r *http.Request, name string) {
	bucket, ok := a.buckets[name]
	if !ok {
		writeS3Error(w, http.StatusNotFound, "NoSuchBucket", "No such bucket.")
		return
	}
	if bucket.forbidden {
		writeS3Error(w, http.StatusForbidden, "AccessDenied", "Access Denied.")
		return
	}
	a.listObjectCnt++

	start := 0
	if token := r.URL.Query().Get("continuation-token"); token != "" {
		if _, err := fmt.Sscanf(token, "offset-%d", &start); err != nil {
			writeS3Error(w, http.StatusBadRequest, "InvalidToken", "Bad continuation token.")
			return
		}
	}
	size := bucket.pageSize
	if size <= 0 {
		size = 1000
	}
	end := min(start+size, len(bucket.objects))

	var contents strings.Builder
	for _, o := range bucket.objects[start:end] {
		fmt.Fprintf(&contents, "<Contents><Key>%s</Key><Size>%d</Size>"+
			"<LastModified>2026-08-24T12:00:00.000Z</LastModified>"+
			"<StorageClass>STANDARD</StorageClass></Contents>", o.key, o.size)
	}

	truncated := end < len(bucket.objects)
	next := ""
	if truncated {
		next = fmt.Sprintf("<NextContinuationToken>offset-%d</NextContinuationToken>", end)
	}
	writeXML(w, http.StatusOK, fmt.Sprintf(
		`<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`+
			`<Name>%s</Name><KeyCount>%d</KeyCount><MaxKeys>%d</MaxKeys>`+
			`<IsTruncated>%t</IsTruncated>%s%s</ListBucketResult>`,
		name, end-start, size, truncated, next, contents.String()))
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
