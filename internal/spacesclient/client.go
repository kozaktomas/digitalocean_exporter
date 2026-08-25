// Package spacesclient builds the S3-compatible clients the Spaces collector
// talks to.
//
// Spaces is addressed per region, so a client is scoped to one region and the
// collector asks for the one its bucket lives in. Path-style addressing is used
// throughout: DigitalOcean serves both styles, and path style needs no wildcard
// DNS, which is what lets the tests point a real client at a local stub.
package spacesclient

import (
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
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
