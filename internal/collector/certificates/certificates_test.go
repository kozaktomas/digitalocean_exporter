package certificates_test

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

	"github.com/kozaktomas/digitalocean_exporter/internal/collector/certificates"
	"github.com/kozaktomas/digitalocean_exporter/internal/doclient"
)

// Two certificates: a managed one covering two names, and a custom one whose
// renewal has failed, which is the state the expiry metric exists to catch.
const certificatesJSON = `{"certificates":[` +
	`{"id":"cert-1","name":"cdn","type":"lets_encrypt","state":"verified",` +
	`"not_after":"2026-11-05T07:18:56Z","sha1_fingerprint":"a4b4e231",` +
	`"dns_names":["cdn.example.com","example.com"]},` +
	`{"id":"cert-2","name":"legacy","type":"custom","state":"error",` +
	`"not_after":"2026-09-01T00:00:00Z","sha1_fingerprint":"773e2bda",` +
	`"dns_names":["legacy.example.com"]}` +
	`],"meta":{"total":2}}`

const certificateMetrics = `
# HELP digitalocean_certificate_dns_names Number of DNS names the certificate covers.
# TYPE digitalocean_certificate_dns_names gauge
digitalocean_certificate_dns_names{id="cert-1",name="cdn"} 2
digitalocean_certificate_dns_names{id="cert-2",name="legacy"} 1
# HELP digitalocean_certificate_expiry_timestamp_seconds Expiry of the certificate as a Unix timestamp.
# TYPE digitalocean_certificate_expiry_timestamp_seconds gauge
digitalocean_certificate_expiry_timestamp_seconds{id="cert-1",name="cdn",type="lets_encrypt"} 1.793863136e+09
digitalocean_certificate_expiry_timestamp_seconds{id="cert-2",name="legacy",type="custom"} 1.7882208e+09
# HELP digitalocean_certificate_info Always 1. Its labels describe the certificate's type, state and fingerprint.
# TYPE digitalocean_certificate_info gauge
digitalocean_certificate_info{id="cert-1",name="cdn",sha1_fingerprint="a4b4e231",state="verified",type="lets_encrypt"} 1
digitalocean_certificate_info{id="cert-2",name="legacy",sha1_fingerprint="773e2bda",state="error",type="custom"} 1
`

// newTestCollector wires a collector to a fake DigitalOcean API.
func newTestCollector(t *testing.T, handler http.HandlerFunc) *certificates.Collector {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	client, err := doclient.New("token", srv.URL+"/", "test", 5*time.Second,
		doclient.NewMetrics(prometheus.NewRegistry()))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return certificates.New(client)
}

// okHandler serves the two-certificate account.
func okHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/v2/certificates" {
		_, _ = w.Write([]byte(certificatesJSON))
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func TestCollectAfterRefresh(t *testing.T) {
	c := newTestCollector(t, okHandler)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if err := testutil.CollectAndCompare(c, strings.NewReader(certificateMetrics)); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// A certificate whose not_after the API leaves out keeps its other metrics
// rather than reporting an expiry at the epoch, which would fire every alert
// written against it.
func TestCertificateWithoutExpiryKeepsItsOtherMetrics(t *testing.T) {
	c := newTestCollector(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"certificates":[{"id":"cert-1","name":"broken",` +
			`"type":"custom","state":"error","dns_names":["example.com"]}],"meta":{"total":1}}`))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	const want = `
# HELP digitalocean_certificate_dns_names Number of DNS names the certificate covers.
# TYPE digitalocean_certificate_dns_names gauge
digitalocean_certificate_dns_names{id="cert-1",name="broken"} 1
`
	const metric = "digitalocean_certificate_dns_names"
	if err := testutil.CollectAndCompare(c, strings.NewReader(want), metric); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}

	const expiry = "digitalocean_certificate_expiry_timestamp_seconds"
	if got := testutil.CollectAndCount(c, expiry); got != 0 {
		t.Errorf("expiry samples for a certificate without not_after = %d, want 0", got)
	}
}

// An account with more certificates than fit on one page is paginated by page
// number, and every page has to reach the snapshot.
func TestRefreshFollowsPages(t *testing.T) {
	page := func(id, name string, next bool) string {
		links := `"links":{}`
		if next {
			links = `"links":{"pages":{"next":"https://api.digitalocean.com/v2/certificates?page=2"}}`
		}
		return fmt.Sprintf(`{"certificates":[{"id":%q,"name":%q,"type":"custom","state":"verified",`+
			`"dns_names":["example.com"]}],%s,"meta":{"total":2}}`, id, name, links)
	}

	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write([]byte(page("cert-2", "second", false)))
			return
		}
		_, _ = w.Write([]byte(page("cert-1", "first", true)))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	const want = `
# HELP digitalocean_certificate_dns_names Number of DNS names the certificate covers.
# TYPE digitalocean_certificate_dns_names gauge
digitalocean_certificate_dns_names{id="cert-1",name="first"} 1
digitalocean_certificate_dns_names{id="cert-2",name="second"} 1
`
	const metric = "digitalocean_certificate_dns_names"
	if err := testutil.CollectAndCompare(c, strings.NewReader(want), metric); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// An account holding no certificates is a normal state: the refresh succeeds
// and there is simply nothing to report.
func TestRefreshWithoutCertificatesSucceeds(t *testing.T) {
	c := newTestCollector(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"certificates":[],"meta":{"total":0}}`))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh without certificates: %v", err)
	}
	if got := testutil.CollectAndCount(c); got != 0 {
		t.Errorf("metric count without certificates = %d, want 0", got)
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

	if err := testutil.CollectAndCompare(c, strings.NewReader(certificateMetrics)); err != nil {
		t.Errorf("unexpected metrics after a failed refresh: %v", err)
	}
}

func TestName(t *testing.T) {
	c := newTestCollector(t, okHandler)
	if got := c.Name(); got != "certificates" {
		t.Errorf("Name() = %q, want %q", got, "certificates")
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
	if want := 3; count != want {
		t.Errorf("Describe sent %d descriptors, want %d", count, want)
	}
}
