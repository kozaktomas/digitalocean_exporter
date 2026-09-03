package uptime_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kozaktomas/digitalocean_exporter/internal/collector/uptime"
	"github.com/kozaktomas/digitalocean_exporter/internal/doclient"
)

// Two checks: an enabled https check probed from two regions, one of which
// sees it down, and a disabled ping check whose state lookup fails in the
// tests that ask it to.
const checksJSON = `{"checks":[` +
	`{"id":"c1","name":"web","type":"https","target":"https://example.com",` +
	`"regions":["us_east","eu_west"],"enabled":true},` +
	`{"id":"c2","name":"gateway","type":"ping","target":"gw.example.com",` +
	`"regions":["eu_west"],"enabled":false}` +
	`],"links":{"pages":{}},"meta":{"total":2}}`

// The state of check c1: eu_west sees it down and reports the previous
// outage, which began 2026-09-01T10:00:00Z — 1788256800 in Unix time.
const state1JSON = `{"state":{"regions":{` +
	`"us_east":{"status":"UP","status_changed_at":"2026-09-01T10:02:00Z",` +
	`"thirty_day_uptime_percentage":99.5},` +
	`"eu_west":{"status":"DOWN","status_changed_at":"2026-09-01T10:00:00Z",` +
	`"thirty_day_uptime_percentage":97}},` +
	`"previous_outage":{"region":"eu_west","started_at":"2026-09-01T10:00:00Z",` +
	`"ended_at":"2026-09-01T10:02:00Z","duration_seconds":120}}}`

// The state of check c2: one region, up, no outage yet.
const state2JSON = `{"state":{"regions":{` +
	`"eu_west":{"status":"UP","status_changed_at":"2026-08-01T00:00:00Z",` +
	`"thirty_day_uptime_percentage":100}},` +
	`"previous_outage":{}}}`

// uptimeMetrics is what the two checks expose when both state lookups
// succeed.
const uptimeMetrics = `
# HELP digitalocean_uptime_check_info Always 1. Its labels describe the check: what it probes and whether it is enabled.
# TYPE digitalocean_uptime_check_info gauge
digitalocean_uptime_check_info{enabled="true",id="c1",name="web",target="https://example.com",type="https"} 1
digitalocean_uptime_check_info{enabled="false",id="c2",name="gateway",target="gw.example.com",type="ping"} 1
# HELP digitalocean_uptime_check_previous_outage_duration_seconds How long the check's previous outage lasted.
# TYPE digitalocean_uptime_check_previous_outage_duration_seconds gauge
digitalocean_uptime_check_previous_outage_duration_seconds{id="c1",name="web",region="eu_west"} 120
# HELP digitalocean_uptime_check_previous_outage_start_timestamp_seconds Unix time the check's previous outage began, as seen from the region that reported it.
# TYPE digitalocean_uptime_check_previous_outage_start_timestamp_seconds gauge
digitalocean_uptime_check_previous_outage_start_timestamp_seconds{id="c1",name="web",region="eu_west"} 1788256800
# HELP digitalocean_uptime_check_region_status Always 1 for the region's current status and 0 for every other known one.
# TYPE digitalocean_uptime_check_region_status gauge
digitalocean_uptime_check_region_status{id="c1",name="web",region="eu_west",status="CHECKING"} 0
digitalocean_uptime_check_region_status{id="c1",name="web",region="eu_west",status="DOWN"} 1
digitalocean_uptime_check_region_status{id="c1",name="web",region="eu_west",status="UP"} 0
digitalocean_uptime_check_region_status{id="c1",name="web",region="us_east",status="CHECKING"} 0
digitalocean_uptime_check_region_status{id="c1",name="web",region="us_east",status="DOWN"} 0
digitalocean_uptime_check_region_status{id="c1",name="web",region="us_east",status="UP"} 1
digitalocean_uptime_check_region_status{id="c2",name="gateway",region="eu_west",status="CHECKING"} 0
digitalocean_uptime_check_region_status{id="c2",name="gateway",region="eu_west",status="DOWN"} 0
digitalocean_uptime_check_region_status{id="c2",name="gateway",region="eu_west",status="UP"} 1
# HELP digitalocean_uptime_check_up Whether the check's last state lookup succeeded.
# TYPE digitalocean_uptime_check_up gauge
digitalocean_uptime_check_up{id="c1",name="web"} 1
digitalocean_uptime_check_up{id="c2",name="gateway"} 1
# HELP digitalocean_uptime_check_uptime_ratio Thirty-day uptime of the check as measured from the region, as a ratio between 0 and 1.
# TYPE digitalocean_uptime_check_uptime_ratio gauge
digitalocean_uptime_check_uptime_ratio{id="c1",name="web",region="eu_west"} 0.97
digitalocean_uptime_check_uptime_ratio{id="c1",name="web",region="us_east"} 0.995
digitalocean_uptime_check_uptime_ratio{id="c2",name="gateway",region="eu_west"} 1
`

// uptimeMetricsC2Failed is the exposition when check c2's state lookup has
// never succeeded: its info series and its _up 0 appear, its state series do
// not.
const uptimeMetricsC2Failed = `
# HELP digitalocean_uptime_check_info Always 1. Its labels describe the check: what it probes and whether it is enabled.
# TYPE digitalocean_uptime_check_info gauge
digitalocean_uptime_check_info{enabled="true",id="c1",name="web",target="https://example.com",type="https"} 1
digitalocean_uptime_check_info{enabled="false",id="c2",name="gateway",target="gw.example.com",type="ping"} 1
# HELP digitalocean_uptime_check_previous_outage_duration_seconds How long the check's previous outage lasted.
# TYPE digitalocean_uptime_check_previous_outage_duration_seconds gauge
digitalocean_uptime_check_previous_outage_duration_seconds{id="c1",name="web",region="eu_west"} 120
# HELP digitalocean_uptime_check_previous_outage_start_timestamp_seconds Unix time the check's previous outage began, as seen from the region that reported it.
# TYPE digitalocean_uptime_check_previous_outage_start_timestamp_seconds gauge
digitalocean_uptime_check_previous_outage_start_timestamp_seconds{id="c1",name="web",region="eu_west"} 1788256800
# HELP digitalocean_uptime_check_region_status Always 1 for the region's current status and 0 for every other known one.
# TYPE digitalocean_uptime_check_region_status gauge
digitalocean_uptime_check_region_status{id="c1",name="web",region="eu_west",status="CHECKING"} 0
digitalocean_uptime_check_region_status{id="c1",name="web",region="eu_west",status="DOWN"} 1
digitalocean_uptime_check_region_status{id="c1",name="web",region="eu_west",status="UP"} 0
digitalocean_uptime_check_region_status{id="c1",name="web",region="us_east",status="CHECKING"} 0
digitalocean_uptime_check_region_status{id="c1",name="web",region="us_east",status="DOWN"} 0
digitalocean_uptime_check_region_status{id="c1",name="web",region="us_east",status="UP"} 1
# HELP digitalocean_uptime_check_up Whether the check's last state lookup succeeded.
# TYPE digitalocean_uptime_check_up gauge
digitalocean_uptime_check_up{id="c1",name="web"} 1
digitalocean_uptime_check_up{id="c2",name="gateway"} 0
# HELP digitalocean_uptime_check_uptime_ratio Thirty-day uptime of the check as measured from the region, as a ratio between 0 and 1.
# TYPE digitalocean_uptime_check_uptime_ratio gauge
digitalocean_uptime_check_uptime_ratio{id="c1",name="web",region="eu_west"} 0.97
digitalocean_uptime_check_uptime_ratio{id="c1",name="web",region="us_east"} 0.995
`

// newTestCollector wires a collector to a fake DigitalOcean API.
func newTestCollector(t *testing.T, handler http.HandlerFunc) *uptime.Collector {
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
	return uptime.New(client, nil)
}

// okHandler serves the check list and both states, unless fail2 is set, in
// which case the state of check c2 answers with a server error.
func okHandler(fail2 *atomic.Bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/uptime/checks":
			_, _ = w.Write([]byte(checksJSON))
		case "/v2/uptime/checks/c1/state":
			_, _ = w.Write([]byte(state1JSON))
		case "/v2/uptime/checks/c2/state":
			if fail2 != nil && fail2.Load() {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte(state2JSON))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func TestCollectAfterRefresh(t *testing.T) {
	c := newTestCollector(t, okHandler(nil))
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if err := testutil.CollectAndCompare(c, strings.NewReader(uptimeMetrics)); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// One check's state lookup failing must not fail the refresh or cost the
// checks whose lookups succeeded. With nothing from an earlier refresh to fall
// back on, the failed check reports _up 0 and no state series at all.
func TestStateLookupFailureCostsOnlyThatCheck(t *testing.T) {
	var fail2 atomic.Bool
	fail2.Store(true)
	c := newTestCollector(t, okHandler(&fail2))

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if err := testutil.CollectAndCompare(c, strings.NewReader(uptimeMetricsC2Failed)); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// A state lookup that fails after having succeeded keeps reporting what the
// last successful lookup found, marked down, exactly as a failed refresh keeps
// the previous snapshot.
func TestStateLookupFailureKeepsPreviousState(t *testing.T) {
	var fail2 atomic.Bool
	c := newTestCollector(t, okHandler(&fail2))

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}

	fail2.Store(true)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh with a failing state lookup: %v", err)
	}

	const want = `
# HELP digitalocean_uptime_check_region_status Always 1 for the region's current status and 0 for every other known one.
# TYPE digitalocean_uptime_check_region_status gauge
digitalocean_uptime_check_region_status{id="c2",name="gateway",region="eu_west",status="CHECKING"} 0
digitalocean_uptime_check_region_status{id="c2",name="gateway",region="eu_west",status="DOWN"} 0
digitalocean_uptime_check_region_status{id="c2",name="gateway",region="eu_west",status="UP"} 1
# HELP digitalocean_uptime_check_up Whether the check's last state lookup succeeded.
# TYPE digitalocean_uptime_check_up gauge
digitalocean_uptime_check_up{id="c1",name="web"} 1
digitalocean_uptime_check_up{id="c2",name="gateway"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want),
		"digitalocean_uptime_check_up", "digitalocean_uptime_check_region_status"); err != nil {
		t.Errorf("unexpected metrics after a failed state lookup: %v", err)
	}

	// The region series of check c1 must still be there in full.
	if got := testutil.CollectAndCount(c, "digitalocean_uptime_check_region_status"); got != 9 {
		t.Errorf("region status series = %d, want 9", got)
	}
}

// A status DigitalOcean has invented since the collector was written is
// reported beside the documented ones rather than silently zeroing them all.
func TestUnknownRegionStatusIsReported(t *testing.T) {
	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/uptime/checks":
			_, _ = w.Write([]byte(`{"checks":[{"id":"c1","name":"web","type":"https",` +
				`"target":"https://example.com","regions":["us_east"],"enabled":true}],` +
				`"links":{"pages":{}},"meta":{"total":1}}`))
		case "/v2/uptime/checks/c1/state":
			_, _ = w.Write([]byte(`{"state":{"regions":{"us_east":{"status":"DEGRADED",` +
				`"thirty_day_uptime_percentage":99}},"previous_outage":{}}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	const want = `
# HELP digitalocean_uptime_check_region_status Always 1 for the region's current status and 0 for every other known one.
# TYPE digitalocean_uptime_check_region_status gauge
digitalocean_uptime_check_region_status{id="c1",name="web",region="us_east",status="CHECKING"} 0
digitalocean_uptime_check_region_status{id="c1",name="web",region="us_east",status="DEGRADED"} 1
digitalocean_uptime_check_region_status{id="c1",name="web",region="us_east",status="DOWN"} 0
digitalocean_uptime_check_region_status{id="c1",name="web",region="us_east",status="UP"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want),
		"digitalocean_uptime_check_region_status"); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// An account with more checks than fit on a page must be reported whole.
func TestRefreshFollowsPages(t *testing.T) {
	const perPage = 200
	page := func(from, count int, next bool) string {
		checks := make([]string, 0, count)
		for i := from; i < from+count; i++ {
			checks = append(checks, fmt.Sprintf(`{"id":"%d","name":"check-%d","type":"ping",`+
				`"target":"t%d.example.com","regions":["eu_west"],"enabled":true}`, i, i, i))
		}
		links := `{"pages":{}}`
		if next {
			links = `{"pages":{"next":"https://api.digitalocean.com/v2/uptime/checks?page=2"}}`
		}
		return `{"checks":[` + strings.Join(checks, ",") + `],"links":` + links + `,"meta":{"total":201}}`
	}

	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/v2/uptime/checks" {
			// Every state lookup fails, which must not fail the refresh.
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write([]byte(page(perPage+1, 1, false)))
			return
		}
		_, _ = w.Write([]byte(page(1, perPage, true)))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if got := testutil.CollectAndCount(c, "digitalocean_uptime_check_info"); got != perPage+1 {
		t.Errorf("checks collected = %d, want %d", got, perPage+1)
	}
}

// An account with no Uptime checks is a normal state.
func TestRefreshWithoutChecksSucceeds(t *testing.T) {
	c := newTestCollector(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"checks":[],"links":{"pages":{}},"meta":{"total":0}}`))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh without checks: %v", err)
	}
	if got := testutil.CollectAndCount(c); got != 0 {
		t.Errorf("metric count without checks = %d, want 0", got)
	}
}

func TestCollectBeforeRefreshEmitsNothing(t *testing.T) {
	c := newTestCollector(t, okHandler(nil))
	if got := testutil.CollectAndCount(c); got != 0 {
		t.Errorf("metric count before the first refresh = %d, want 0", got)
	}
}

func TestFailedRefreshKeepsPreviousSnapshot(t *testing.T) {
	var fail atomic.Bool
	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		okHandler(nil)(w, r)
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}

	fail.Store(true)
	if err := c.Refresh(context.Background()); err == nil {
		t.Fatal("expected the second refresh to fail")
	}

	if err := testutil.CollectAndCompare(c, strings.NewReader(uptimeMetrics)); err != nil {
		t.Errorf("unexpected metrics after a failed refresh: %v", err)
	}
}

// A refresh cut short by its deadline mid-fan-out is a failed refresh, not a
// snapshot with half the checks marked down.
func TestCancelledRefreshFails(t *testing.T) {
	c := newTestCollector(t, okHandler(nil))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := c.Refresh(ctx); err == nil {
		t.Fatal("expected the cancelled refresh to fail")
	}
	if got := testutil.CollectAndCount(c); got != 0 {
		t.Errorf("metric count after a cancelled first refresh = %d, want 0", got)
	}
}

func TestName(t *testing.T) {
	c := newTestCollector(t, okHandler(nil))
	if got := c.Name(); got != "uptime" {
		t.Errorf("Name() = %q, want %q", got, "uptime")
	}
}

func TestDescribeCoversEveryMetric(t *testing.T) {
	c := newTestCollector(t, okHandler(nil))

	ch := make(chan *prometheus.Desc, 16)
	c.Describe(ch)
	close(ch)

	var count int
	for range ch {
		count++
	}
	if want := 6; count != want {
		t.Errorf("Describe sent %d descriptors, want %d", count, want)
	}
}
