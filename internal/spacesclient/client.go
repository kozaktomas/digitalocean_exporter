// Package spacesclient builds the S3-compatible clients the Spaces collector
// talks to.
//
// Spaces is addressed per region, so a client is scoped to one region and the
// collector asks for the one its bucket lives in. Path-style addressing is used
// throughout: DigitalOcean serves both styles, and path style needs no wildcard
// DNS, which is what lets the tests point a real client at a local stub.
//
// The package also reads a bucket's size and object count, which Spaces answers
// in headers outside the S3 model and the SDK therefore does not surface.
package spacesclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// Factory hands out an S3 client per Spaces region.
type Factory struct {
	accessKey string
	secretKey string
	endpoint  string
}

// NewFactory returns a factory authenticating with the given Spaces key pair.
// A non-empty endpoint overrides the public regional endpoints, which the
// tests and the smoke test rely on.
func NewFactory(accessKey, secretKey, endpoint string) *Factory {
	return &Factory{accessKey: accessKey, secretKey: secretKey, endpoint: endpoint}
}

// Client returns a client for region.
func (f *Factory) Client(region string) *s3.Client {
	return s3.New(s3.Options{
		Region:       region,
		BaseEndpoint: aws.String(f.endpointFor(region)),
		Credentials: credentials.NewStaticCredentialsProvider(
			f.accessKey, f.secretKey, ""),
		UsePathStyle: true,
	})
}

// endpointFor resolves the endpoint a region's buckets are served from.
func (f *Factory) endpointFor(region string) string {
	if f.endpoint != "" {
		return f.endpoint
	}
	return fmt.Sprintf("https://%s.digitaloceanspaces.com", region)
}

// Usage is what Spaces reports about the contents of a bucket.
type Usage struct {
	// Objects is the number of objects the bucket holds.
	Objects int64
	// Bytes is the number of bytes those objects occupy.
	Bytes int64
}

// ErrNoUsage reports that a bucket's HEAD response carried no usage headers,
// which means the endpoint is not the Ceph-backed Spaces service this reads.
var ErrNoUsage = errors.New("no usage headers on the bucket response")

// Names of the Ceph RADOS Gateway headers carrying a bucket's usage.
const (
	objectCountHeader = "x-rgw-object-count"
	bytesUsedHeader   = "x-rgw-bytes-used"
)

// BucketUsage returns the size and object count of a bucket, from the single
// HEAD request that asks for them.
//
// Spaces runs on the Ceph RADOS Gateway, which answers a HEAD of a bucket with
// its own accounting in x-rgw-object-count and x-rgw-bytes-used. Neither header
// is part of S3, so aws-sdk-go-v2 does not model it and they have to be taken
// off the raw response by a middleware. Both figures were verified against a
// full listing of three live buckets and matched byte for byte.
func BucketUsage(ctx context.Context, client *s3.Client, bucket string) (Usage, error) {
	var header http.Header

	_, err := client.HeadBucket(ctx,
		&s3.HeadBucketInput{Bucket: aws.String(bucket)},
		s3.WithAPIOptions(captureHeader(&header)))
	if err != nil {
		// A HEAD carries no body, so the S3 error code that a GET would have
		// spelled out never arrives and the operator is left with a bare 403.
		// Say what the two causes are instead.
		var response interface{ HTTPStatusCode() int }
		if errors.As(err, &response) && response.HTTPStatusCode() == http.StatusForbidden {
			return Usage{}, fmt.Errorf(
				"head bucket %q: %w (no read grant for the bucket, or an invalid key pair)", bucket, err)
		}
		return Usage{}, fmt.Errorf("head bucket %q: %w", bucket, err)
	}

	objects, err := usageHeader(header, objectCountHeader)
	if err != nil {
		return Usage{}, err
	}
	bytes, err := usageHeader(header, bytesUsedHeader)
	if err != nil {
		return Usage{}, err
	}
	return Usage{Objects: objects, Bytes: bytes}, nil
}

// captureHeader builds an API option copying the response header of one
// operation into dst, since the modelled output carries only what S3 defines.
func captureHeader(dst *http.Header) func(*middleware.Stack) error {
	return func(stack *middleware.Stack) error {
		return stack.Deserialize.Add(middleware.DeserializeMiddlewareFunc("captureSpacesUsage",
			func(ctx context.Context, in middleware.DeserializeInput, next middleware.DeserializeHandler) (
				middleware.DeserializeOutput, middleware.Metadata, error,
			) {
				out, metadata, err := next.HandleDeserialize(ctx, in)
				if response, ok := out.RawResponse.(*smithyhttp.Response); ok {
					*dst = response.Header
				}
				return out, metadata, err
			}), middleware.After)
	}
}

// usageHeader reads one usage header as a count. A missing header is not a
// malformed one: it says the endpoint does not report usage at all, which is a
// different thing to tell an operator.
func usageHeader(header http.Header, name string) (int64, error) {
	raw := header.Get(name)
	if raw == "" {
		return 0, fmt.Errorf("%w: %s is missing", ErrNoUsage, name)
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s %q: %w", name, raw, err)
	}
	return value, nil
}
