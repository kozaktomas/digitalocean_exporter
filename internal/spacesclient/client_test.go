package spacesclient_test

import (
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
