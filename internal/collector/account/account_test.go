package account_test

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

	"github.com/kozaktomas/digitalocean_exporter/internal/collector/account"
	"github.com/kozaktomas/digitalocean_exporter/internal/doclient"
)

const accountJSON = `{"account":{"droplet_limit":25,"floating_ip_limit":3,"reserved_ip_limit":3,` +
	`"volume_limit":100,"email":"user@example.com","uuid":"uuid","email_verified":true,"status":"active"}}`

const accountMetrics = `
# HELP digitalocean_account_active Whether the account status is active.
# TYPE digitalocean_account_active gauge
digitalocean_account_active 1
# HELP digitalocean_account_droplet_limit Maximum number of droplets the account may have.
# TYPE digitalocean_account_droplet_limit gauge
digitalocean_account_droplet_limit 25
# HELP digitalocean_account_floating_ip_limit Maximum number of floating IPs the account may have.
# TYPE digitalocean_account_floating_ip_limit gauge
digitalocean_account_floating_ip_limit 3
# HELP digitalocean_account_reserved_ip_limit Maximum number of reserved IPs the account may have.
# TYPE digitalocean_account_reserved_ip_limit gauge
digitalocean_account_reserved_ip_limit 3
# HELP digitalocean_account_status Always 1 for the account's current status and 0 for every other known one.
# TYPE digitalocean_account_status gauge
digitalocean_account_status{status="active"} 1
digitalocean_account_status{status="locked"} 0
digitalocean_account_status{status="warning"} 0
# HELP digitalocean_account_verified Whether the account email address is verified.
# TYPE digitalocean_account_verified gauge
digitalocean_account_verified 1
# HELP digitalocean_account_volume_limit Maximum number of volumes the account may have.
# TYPE digitalocean_account_volume_limit gauge
digitalocean_account_volume_limit 100
`

// newTestCollector wires a collector to a fake DigitalOcean API.
func newTestCollector(t *testing.T, handler http.HandlerFunc) *account.Collector {
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
	return account.New(client)
}

func okHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if strings.HasSuffix(r.URL.Path, "/v2/account") {
		_, _ = w.Write([]byte(accountJSON))
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func TestCollectAfterRefresh(t *testing.T) {
	c := newTestCollector(t, okHandler)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if err := testutil.CollectAndCompare(c, strings.NewReader(accountMetrics)); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// A token without the billing scope is forbidden from reading the balance.
// That must not cost the account metrics, which is why balance lives in its
// own collector: nothing here may touch the billing endpoint at all.
func TestRefreshIgnoresForbiddenBalanceEndpoint(t *testing.T) {
	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/v2/account") {
			_, _ = w.Write([]byte(accountJSON))
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"id":"Forbidden","message":"You are not authorized to perform this operation"}`))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh with a forbidden balance endpoint: %v", err)
	}
	if err := testutil.CollectAndCompare(c, strings.NewReader(accountMetrics)); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// digitalocean_account_active collapses every status that is not active into
// one 0. The status metric is what tells warning — the billing-trouble state
// worth paging on — apart from locked.
func TestCollectReportsTheCurrentStatus(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   string
	}{
		{
			status: "warning",
			want: `
# HELP digitalocean_account_status Always 1 for the account's current status and 0 for every other known one.
# TYPE digitalocean_account_status gauge
digitalocean_account_status{status="active"} 0
digitalocean_account_status{status="locked"} 0
digitalocean_account_status{status="warning"} 1
`,
		},
		{
			status: "locked",
			want: `
# HELP digitalocean_account_status Always 1 for the account's current status and 0 for every other known one.
# TYPE digitalocean_account_status gauge
digitalocean_account_status{status="active"} 0
digitalocean_account_status{status="locked"} 1
digitalocean_account_status{status="warning"} 0
`,
		},
		{
			// A status this exporter has never heard of still gets a series
			// of its own, so that the account is not reported as being in no
			// status at all.
			status: "suspended",
			want: `
# HELP digitalocean_account_status Always 1 for the account's current status and 0 for every other known one.
# TYPE digitalocean_account_status gauge
digitalocean_account_status{status="active"} 0
digitalocean_account_status{status="locked"} 0
digitalocean_account_status{status="suspended"} 1
digitalocean_account_status{status="warning"} 0
`,
		},
	} {
		t.Run(tc.status, func(t *testing.T) {
			body := strings.Replace(accountJSON, `"status":"active"`,
				fmt.Sprintf(`"status":%q`, tc.status), 1)
			c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/v2/account") {
					_, _ = w.Write([]byte(body))
					return
				}
				w.WriteHeader(http.StatusNotFound)
			})

			if err := c.Refresh(context.Background()); err != nil {
				t.Fatalf("refresh: %v", err)
			}

			const metric = "digitalocean_account_status"
			if err := testutil.CollectAndCompare(c, strings.NewReader(tc.want), metric); err != nil {
				t.Errorf("unexpected metrics: %v", err)
			}
			// digitalocean_account_active keeps its meaning: 0 for every
			// status but active, whichever one it is.
			const active = `
# HELP digitalocean_account_active Whether the account status is active.
# TYPE digitalocean_account_active gauge
digitalocean_account_active 0
`
			if err := testutil.CollectAndCompare(c, strings.NewReader(active),
				"digitalocean_account_active"); err != nil {
				t.Errorf("unexpected digitalocean_account_active: %v", err)
			}
		})
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
	before := testutil.CollectAndCount(c)

	fail = true
	if err := c.Refresh(context.Background()); err == nil {
		t.Fatal("expected the second refresh to fail")
	}

	if after := testutil.CollectAndCount(c); after != before {
		t.Errorf("metric count after a failed refresh = %d, want the previous %d", after, before)
	}
}
