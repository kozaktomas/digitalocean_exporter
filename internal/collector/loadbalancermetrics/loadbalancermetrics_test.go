package loadbalancermetrics_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kozaktomas/digitalocean_exporter/internal/collector/loadbalancermetrics"
	"github.com/kozaktomas/digitalocean_exporter/internal/doclient"
)

// sampledAt is the timestamp every fixture below reports.
const sampledAt = 1787677800

// matrix builds a monitoring response body from a list of series.
func matrix(series ...string) string {
	return `{"status":"success","data":{"resultType":"matrix","result":[` +
		strings.Join(series, ",") + `]}}`
}

// point builds one series carrying a single sample.
func point(labels, value string) string {
	return fmt.Sprintf(`{"metric":%s,"values":[[%d,%q]]}`, labels, sampledAt, value)
}

// empty is what the API returns for a metric it has nothing for. A load
// balancer with no traffic answers its HTTP metrics this way.
const empty = `{"status":"success","data":{"resultType":"matrix","result":[]}}`

// bodies mirrors the shapes the real API returns, which differ per metric:
// the current connection count carries no labels at all, the response rate is
// split by code and the backend metrics by server.
var bodies = map[string]string{
	"/v2/monitoring/metrics/load_balancer/frontend_connections_current": matrix(
		point(`{}`, "132"),
	),
	"/v2/monitoring/metrics/load_balancer/frontend_connections_limit": matrix(
		point(`{"lb_id":"lb-1","region":"fra1"}`, "10000"),
	),
	"/v2/monitoring/metrics/load_balancer/frontend_cpu_utilization": matrix(
		point(`{"lb_id":"lb-1","region":"fra1"}`, "4.9"),
	),
	"/v2/monitoring/metrics/load_balancer/frontend_http_responses": matrix(
		point(`{"code":"2xx","lb_id":"lb-1","region":"fra1"}`, "12.5"),
		point(`{"code":"5xx","lb_id":"lb-1","region":"fra1"}`, "0.25"),
	),
	"/v2/monitoring/metrics/load_balancer/droplets_health_checks": matrix(
		point(`{"lb_id":"lb-1","region":"fra1","server":"node-1"}`, "100"),
		point(`{"lb_id":"lb-1","region":"fra1","server":"node-2"}`, "0"),
	),
	"/v2/monitoring/metrics/load_balancer/droplets_downtime": matrix(
		point(`{"lb_id":"lb-1","region":"fra1","server":"node-1"}`, "0"),
		point(`{"lb_id":"lb-1","region":"fra1","server":"node-2"}`, "42"),
	),
	"/v2/monitoring/metrics/load_balancer/droplets_http_response_time_95p": matrix(
		point(`{"lb_id":"lb-1","region":"fra1"}`, "0.0153"),
	),
}

const oneLoadBalancerJSON = `{"load_balancers":[{"id":"lb-1","name":"public"}],"meta":{"total":1}}`

const wantMetrics = `
# HELP digitalocean_loadbalancer_droplets_downtime Downtime status of a backend droplet; 0 when it is up.
# TYPE digitalocean_loadbalancer_droplets_downtime gauge
digitalocean_loadbalancer_droplets_downtime{id="lb-1",name="public",server="node-1"} 0
digitalocean_loadbalancer_droplets_downtime{id="lb-1",name="public",server="node-2"} 42
# HELP digitalocean_loadbalancer_droplets_health_checks Health check status of a backend droplet; 100 when healthy.
# TYPE digitalocean_loadbalancer_droplets_health_checks gauge
digitalocean_loadbalancer_droplets_health_checks{id="lb-1",name="public",server="node-1"} 100
digitalocean_loadbalancer_droplets_health_checks{id="lb-1",name="public",server="node-2"} 0
# HELP digitalocean_loadbalancer_droplets_http_response_time_p95_seconds 95th percentile backend response time.
# TYPE digitalocean_loadbalancer_droplets_http_response_time_p95_seconds gauge
digitalocean_loadbalancer_droplets_http_response_time_p95_seconds{id="lb-1",name="public"} 0.0153
# HELP digitalocean_loadbalancer_frontend_connections_current Active connections to the load balancer's frontend.
# TYPE digitalocean_loadbalancer_frontend_connections_current gauge
digitalocean_loadbalancer_frontend_connections_current{id="lb-1",name="public"} 132
# HELP digitalocean_loadbalancer_frontend_connections_limit Maximum active connections the frontend allows.
# TYPE digitalocean_loadbalancer_frontend_connections_limit gauge
digitalocean_loadbalancer_frontend_connections_limit{id="lb-1",name="public"} 10000
# HELP digitalocean_loadbalancer_frontend_cpu_utilization_percent Average CPU utilization of the frontend, in percent.
# TYPE digitalocean_loadbalancer_frontend_cpu_utilization_percent gauge
digitalocean_loadbalancer_frontend_cpu_utilization_percent{id="lb-1",name="public"} 4.9
# HELP digitalocean_loadbalancer_frontend_http_responses_per_second Rate of HTTP responses served, by code class.
# TYPE digitalocean_loadbalancer_frontend_http_responses_per_second gauge
digitalocean_loadbalancer_frontend_http_responses_per_second{code="2xx",id="lb-1",name="public"} 12.5
digitalocean_loadbalancer_frontend_http_responses_per_second{code="5xx",id="lb-1",name="public"} 0.25
# HELP digitalocean_loadbalancer_metrics_timestamp_seconds Unix time of the newest sample returned.
# TYPE digitalocean_loadbalancer_metrics_timestamp_seconds gauge
digitalocean_loadbalancer_metrics_timestamp_seconds{id="lb-1",name="public"} 1.7876778e+09
# HELP digitalocean_loadbalancer_metrics_up Whether the load balancer's last metrics fetch succeeded.
# TYPE digitalocean_loadbalancer_metrics_up gauge
digitalocean_loadbalancer_metrics_up{id="lb-1",name="public"} 1
`

// newTestCollector wires a collector to a fake DigitalOcean API.
func newTestCollector(
	t *testing.T, concurrency int, handler http.HandlerFunc,
) *loadbalancermetrics.Collector {
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
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return loadbalancermetrics.New(client, concurrency, logger)
}

// okHandler serves one load balancer and a full set of readings for it.
func okHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/v2/load_balancers" {
		_, _ = w.Write([]byte(oneLoadBalancerJSON))
		return
	}
	if body, ok := bodies[r.URL.Path]; ok {
		_, _ = w.Write([]byte(body))
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func TestCollectAfterRefresh(t *testing.T) {
	c := newTestCollector(t, 4, okHandler)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if err := testutil.CollectAndCompare(c, strings.NewReader(wantMetrics)); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// A load balancer with no traffic returns empty results for its HTTP metrics.
// That is the normal state of an idle one, not a failure, so it stays up and
// simply reports fewer series.
func TestIdleLoadBalancerIsUpWithFewerSeries(t *testing.T) {
	c := newTestCollector(t, 1, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v2/load_balancers" {
			_, _ = w.Write([]byte(oneLoadBalancerJSON))
			return
		}
		if strings.HasSuffix(r.URL.Path, "frontend_connections_current") {
			_, _ = w.Write([]byte(bodies[r.URL.Path]))
			return
		}
		_, _ = w.Write([]byte(empty))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	const want = `
# HELP digitalocean_loadbalancer_frontend_connections_current Active connections to the load balancer's frontend.
# TYPE digitalocean_loadbalancer_frontend_connections_current gauge
digitalocean_loadbalancer_frontend_connections_current{id="lb-1",name="public"} 132
# HELP digitalocean_loadbalancer_metrics_up Whether the load balancer's last metrics fetch succeeded.
# TYPE digitalocean_loadbalancer_metrics_up gauge
digitalocean_loadbalancer_metrics_up{id="lb-1",name="public"} 1
`
	err := testutil.CollectAndCompare(c, strings.NewReader(want),
		"digitalocean_loadbalancer_frontend_connections_current",
		"digitalocean_loadbalancer_metrics_up")
	if err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

const twoLoadBalancersJSON = `{"load_balancers":[{"id":"lb-1","name":"public"},` +
	`{"id":"lb-2","name":"internal"}],"meta":{"total":2}}`

// twoHandler serves two load balancers and fails every metric request for the
// one named in failFor.
func twoHandler(failFor string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v2/load_balancers" {
			_, _ = w.Write([]byte(twoLoadBalancersJSON))
			return
		}
		if r.URL.Query().Get("lb_id") == failFor {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if body, ok := bodies[r.URL.Path]; ok {
			_, _ = w.Write([]byte(body))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}
}

// One load balancer failing must not cost the ones that succeeded. Under the
// race detector this also covers the fan-out writing into the shared results.
func TestOneFailingLoadBalancerDoesNotCostTheOthers(t *testing.T) {
	c := newTestCollector(t, 4, twoHandler("lb-1"))

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh with one failing load balancer: %v", err)
	}

	const want = `
# HELP digitalocean_loadbalancer_metrics_up Whether the load balancer's last metrics fetch succeeded.
# TYPE digitalocean_loadbalancer_metrics_up gauge
digitalocean_loadbalancer_metrics_up{id="lb-1",name="public"} 0
digitalocean_loadbalancer_metrics_up{id="lb-2",name="internal"} 1
`
	const metric = "digitalocean_loadbalancer_metrics_up"
	if err := testutil.CollectAndCompare(c, strings.NewReader(want), metric); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// A load balancer whose fetch fails keeps the readings it last reported.
func TestFailingLoadBalancerKeepsItsPreviousReadings(t *testing.T) {
	var failing string
	c := newTestCollector(t, 1, func(w http.ResponseWriter, r *http.Request) {
		twoHandler(failing)(w, r)
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}

	failing = "lb-2"
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("second refresh: %v", err)
	}

	const want = `
# HELP digitalocean_loadbalancer_frontend_connections_current Active connections to the load balancer's frontend.
# TYPE digitalocean_loadbalancer_frontend_connections_current gauge
digitalocean_loadbalancer_frontend_connections_current{id="lb-1",name="public"} 132
digitalocean_loadbalancer_frontend_connections_current{id="lb-2",name="internal"} 132
# HELP digitalocean_loadbalancer_metrics_up Whether the load balancer's last metrics fetch succeeded.
# TYPE digitalocean_loadbalancer_metrics_up gauge
digitalocean_loadbalancer_metrics_up{id="lb-1",name="public"} 1
digitalocean_loadbalancer_metrics_up{id="lb-2",name="internal"} 0
`
	err := testutil.CollectAndCompare(c, strings.NewReader(want),
		"digitalocean_loadbalancer_frontend_connections_current",
		"digitalocean_loadbalancer_metrics_up")
	if err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// Every load balancer failing points at the API, so the refresh itself fails.
func TestEveryLoadBalancerFailingFailsTheRefresh(t *testing.T) {
	c := newTestCollector(t, 4, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v2/load_balancers" {
			_, _ = w.Write([]byte(twoLoadBalancersJSON))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	})

	if err := c.Refresh(context.Background()); err == nil {
		t.Fatal("expected the refresh to fail when no load balancer could be measured")
	}
}

// Failing to list the load balancers fails the refresh outright.
func TestFailedListingFailsTheRefresh(t *testing.T) {
	c := newTestCollector(t, 1, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	if err := c.Refresh(context.Background()); err == nil {
		t.Fatal("expected the refresh to fail when load balancers cannot be listed")
	}
}

// An account with no load balancers is a normal state.
func TestRefreshWithoutLoadBalancersSucceeds(t *testing.T) {
	c := newTestCollector(t, 1, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"load_balancers":[],"meta":{"total":0}}`))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh without load balancers: %v", err)
	}
	if got := testutil.CollectAndCount(c); got != 0 {
		t.Errorf("metric count without load balancers = %d, want 0", got)
	}
}

// A destroyed load balancer drops out of the snapshot.
func TestDestroyedLoadBalancerLeavesTheSnapshot(t *testing.T) {
	list := twoLoadBalancersJSON
	c := newTestCollector(t, 1, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v2/load_balancers" {
			_, _ = w.Write([]byte(list))
			return
		}
		if body, ok := bodies[r.URL.Path]; ok {
			_, _ = w.Write([]byte(body))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}

	list = oneLoadBalancerJSON
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("second refresh: %v", err)
	}

	const want = `
# HELP digitalocean_loadbalancer_metrics_up Whether the load balancer's last metrics fetch succeeded.
# TYPE digitalocean_loadbalancer_metrics_up gauge
digitalocean_loadbalancer_metrics_up{id="lb-1",name="public"} 1
`
	const metric = "digitalocean_loadbalancer_metrics_up"
	if err := testutil.CollectAndCompare(c, strings.NewReader(want), metric); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

func TestCollectBeforeRefreshEmitsNothing(t *testing.T) {
	c := newTestCollector(t, 1, okHandler)
	if got := testutil.CollectAndCount(c); got != 0 {
		t.Errorf("metric count before the first refresh = %d, want 0", got)
	}
}

func TestName(t *testing.T) {
	c := newTestCollector(t, 1, okHandler)
	if got := c.Name(); got != "loadbalancermetrics" {
		t.Errorf("Name() = %q, want %q", got, "loadbalancermetrics")
	}
}

// A concurrency below one would deadlock the fan-out, so it is raised to one.
func TestZeroConcurrencyStillMeasures(t *testing.T) {
	c := newTestCollector(t, 0, okHandler)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh with zero concurrency: %v", err)
	}
	if got := testutil.CollectAndCount(c); got == 0 {
		t.Error("metric count with zero concurrency = 0, want the readings of one load balancer")
	}
}

func TestDescribeCoversEveryMetric(t *testing.T) {
	c := newTestCollector(t, 1, okHandler)

	ch := make(chan *prometheus.Desc, 32)
	c.Describe(ch)
	close(ch)

	var count int
	for range ch {
		count++
	}
	if want := 9; count != want {
		t.Errorf("Describe sent %d descriptors, want %d", count, want)
	}
}
