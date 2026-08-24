package account_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/panbotka/digitalocean_exporter/internal/collector/account"
	"github.com/panbotka/digitalocean_exporter/internal/doclient"
)

const accountJSON = `{"account":{"droplet_limit":25,"floating_ip_limit":3,"reserved_ip_limit":3,` +
	`"volume_limit":100,"email":"user@example.com","uuid":"uuid","email_verified":true,"status":"active"}}`

const balanceJSON = `{"month_to_date_balance":"23.44","account_balance":"12.23",` +
	`"month_to_date_usage":"11.21","generated_at":"2026-08-24T12:00:00Z"}`

// newTestCollector wires a collector to a fake DigitalOcean API.
func newTestCollector(t *testing.T, handler http.HandlerFunc) *account.Collector {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	client, err := doclient.New("token", srv.URL+"/", "test", 5*time.Second,
		doclient.NewMetrics(prometheus.NewRegistry()))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return account.New(client)
}

func okHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.HasSuffix(r.URL.Path, "/v2/account"):
		_, _ = w.Write([]byte(accountJSON))
	case strings.HasSuffix(r.URL.Path, "/balance"):
		_, _ = w.Write([]byte(balanceJSON))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestCollectAfterRefresh(t *testing.T) {
	c := newTestCollector(t, okHandler)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	expected := `
# HELP digitalocean_account_active Whether the account status is active.
# TYPE digitalocean_account_active gauge
digitalocean_account_active 1
# HELP digitalocean_account_balance Current account balance in the account currency.
# TYPE digitalocean_account_balance gauge
digitalocean_account_balance 12.23
# HELP digitalocean_account_droplet_limit Maximum number of droplets the account may have.
# TYPE digitalocean_account_droplet_limit gauge
digitalocean_account_droplet_limit 25
# HELP digitalocean_account_floating_ip_limit Maximum number of floating IPs the account may have.
# TYPE digitalocean_account_floating_ip_limit gauge
digitalocean_account_floating_ip_limit 3
# HELP digitalocean_account_reserved_ip_limit Maximum number of reserved IPs the account may have.
# TYPE digitalocean_account_reserved_ip_limit gauge
digitalocean_account_reserved_ip_limit 3
# HELP digitalocean_account_verified Whether the account email address is verified.
# TYPE digitalocean_account_verified gauge
digitalocean_account_verified 1
# HELP digitalocean_account_volume_limit Maximum number of volumes the account may have.
# TYPE digitalocean_account_volume_limit gauge
digitalocean_account_volume_limit 100
# HELP digitalocean_balance_generated_at Unix timestamp the balance figures were generated at.
# TYPE digitalocean_balance_generated_at gauge
digitalocean_balance_generated_at 1787572800
# HELP digitalocean_month_to_date_balance Month-to-date balance in the account currency.
# TYPE digitalocean_month_to_date_balance gauge
digitalocean_month_to_date_balance 23.44
# HELP digitalocean_month_to_date_usage Month-to-date usage in the account currency.
# TYPE digitalocean_month_to_date_usage gauge
digitalocean_month_to_date_usage 11.21
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected)); err != nil {
		t.Errorf("unexpected metrics: %v", err)
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

func TestRefreshRejectsUnparsableBalance(t *testing.T) {
	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/v2/account") {
			_, _ = w.Write([]byte(accountJSON))
			return
		}
		// A balance that is not a number must be an error, never a silent zero.
		_, _ = w.Write([]byte(`{"month_to_date_balance":"n/a","account_balance":"1.00",` +
			`"month_to_date_usage":"1.00","generated_at":"2026-08-24T12:00:00Z"}`))
	})

	if err := c.Refresh(context.Background()); err == nil {
		t.Fatal("expected an error for an unparsable balance")
	}
}
