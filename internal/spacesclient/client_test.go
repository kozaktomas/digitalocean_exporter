package spacesclient_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/kozaktomas/digitalocean_exporter/internal/spacesclient"
)

func TestClientUsesTheRegionalEndpoint(t *testing.T) {
	client := spacesclient.NewFactory("key", "secret", "").Client("fra1")

	if got := aws.ToString(client.Options().BaseEndpoint); got != "https://fra1.digitaloceanspaces.com" {
		t.Errorf("endpoint = %q, want the fra1 Spaces endpoint", got)
	}
	if got := client.Options().Region; got != "fra1" {
		t.Errorf("region = %q, want fra1", got)
	}
	// Path style needs no wildcard DNS, which is what lets a test or the smoke
	// test point a real client at a local stub.
	if !client.Options().UsePathStyle {
		t.Error("UsePathStyle = false, want path-style addressing")
	}
}

func TestClientHonoursAnEndpointOverride(t *testing.T) {
	client := spacesclient.NewFactory("key", "secret", "http://127.0.0.1:19213").Client("fra1")

	if got := aws.ToString(client.Options().BaseEndpoint); got != "http://127.0.0.1:19213" {
		t.Errorf("endpoint = %q, want the override", got)
	}
}

// usageServer answers every HEAD with the given headers and status.
func usageServer(t *testing.T, status int, headers map[string]string) *spacesclient.Factory {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for name, value := range headers {
			w.Header().Set(name, value)
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return spacesclient.NewFactory("key", "secret", srv.URL)
}

func TestBucketUsageReadsTheCephHeaders(t *testing.T) {
	factory := usageServer(t, http.StatusOK, map[string]string{
		"x-rgw-object-count": "93879",
		"x-rgw-bytes-used":   "10324594821",
	})

	usage, err := spacesclient.BucketUsage(context.Background(), factory.Client("fra1"), "images")
	if err != nil {
		t.Fatalf("bucket usage: %v", err)
	}
	if usage.Objects != 93879 {
		t.Errorf("objects = %d, want 93879", usage.Objects)
	}
	if usage.Bytes != 10324594821 {
		t.Errorf("bytes = %d, want 10324594821", usage.Bytes)
	}
}

// The headers are a Ceph extension rather than S3, so an endpoint may answer
// the HEAD perfectly well and still not report usage. That is not a bucket of
// zero bytes and must not be reported as one.
func TestBucketUsageWithoutTheHeaders(t *testing.T) {
	factory := usageServer(t, http.StatusOK, nil)

	_, err := spacesclient.BucketUsage(context.Background(), factory.Client("fra1"), "images")
	if !errors.Is(err, spacesclient.ErrNoUsage) {
		t.Fatalf("error = %v, want ErrNoUsage", err)
	}
	if !strings.Contains(err.Error(), "x-rgw-object-count") {
		t.Errorf("error = %q, want it to name the missing header", err)
	}
}

func TestBucketUsageWithAMalformedHeader(t *testing.T) {
	factory := usageServer(t, http.StatusOK, map[string]string{
		"x-rgw-object-count": "264",
		"x-rgw-bytes-used":   "not a number",
	})

	_, err := spacesclient.BucketUsage(context.Background(), factory.Client("fra1"), "images")
	if err == nil {
		t.Fatal("expected an unparsable header to fail")
	}
	if errors.Is(err, spacesclient.ErrNoUsage) {
		t.Errorf("error = %v, want a parse failure rather than a missing header", err)
	}
}

// A HEAD has no body, so the S3 error code never arrives and a bare 403 is all
// the operator would otherwise see.
func TestBucketUsageExplainsAForbiddenBucket(t *testing.T) {
	factory := usageServer(t, http.StatusForbidden, nil)

	_, err := spacesclient.BucketUsage(context.Background(), factory.Client("fra1"), "images")
	if err == nil {
		t.Fatal("expected a 403 to fail")
	}
	if !strings.Contains(err.Error(), "no read grant") {
		t.Errorf("error = %q, want it to explain what a 403 means", err)
	}
}
