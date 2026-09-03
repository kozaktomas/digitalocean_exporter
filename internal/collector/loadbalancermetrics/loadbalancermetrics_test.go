package loadbalancermetrics_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kozaktomas/digitalocean_exporter/internal/collector/loadbalancermetrics"
	"github.com/kozaktomas/digitalocean_exporter/internal/doclient"
	"github.com/kozaktomas/digitalocean_exporter/internal/filter"
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

// newTestCollector wires an unfiltered collector to a fake DigitalOcean API.
func newTestCollector(
	t *testing.T, concurrency int, handler http.HandlerFunc,
) *loadbalancermetrics.Collector {
	t.Helper()
	return newFilteredCollector(t, concurrency, filter.Filter{}, handler)
}

// newFilteredCollector is newTestCollector with a filter set.
func newFilteredCollector(
	t *testing.T, concurrency int, f filter.Filter, handler http.HandlerFunc,
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
	return loadbalancermetrics.New(client, concurrency, f, logger)
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
// A filtered-out load balancer is not measured at all: the handler serves
// nothing but the listing, so had it been measured, every fetch would have
// failed and the refresh with it. It carries no tags, so a tag filter
// rejects it.
func TestFilteredOutLoadBalancerIsNotMeasured(t *testing.T) {
	c := newFilteredCollector(t, 1, filter.New([]string{"prod"}, nil),
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/v2/load_balancers" {
				_, _ = w.Write([]byte(oneLoadBalancerJSON))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh with everything filtered out: %v", err)
	}
	if got := testutil.CollectAndCount(c); got != 0 {
		t.Errorf("metric count with everything filtered out = %d, want 0", got)
	}
}

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

// fleetJSON lists count load balancers named lb-N, so a test can build a set
// larger than one refresh can measure.
func fleetJSON(count int) string {
	entries := make([]string, 0, count)
	for i := 1; i <= count; i++ {
		entries = append(entries, fmt.Sprintf(`{"id":"lb-%d","name":"lb-%d"}`, i, i))
	}
	return fmt.Sprintf(`{"load_balancers":[%s],"meta":{"total":%d}}`,
		strings.Join(entries, ","), count)
}

// cutShortHandler serves a set of load balancers and cancels the refresh once
// measure requests for `after` of them have been answered, which is what a
// timeout does to a set too large to fit in it. The cancellation lands on the
// first request of the next one, so those before it are measured in full.
//
// It records the load balancer each measure request was for, in the order the
// requests were made, which is the order the fan-out worked through.
type cutShortHandler struct {
	fleet  string
	after  int
	cancel context.CancelFunc

	mu       sync.Mutex
	requests int
	asked    []string
}

func (h *cutShortHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/v2/load_balancers" {
		_, _ = w.Write([]byte(h.fleet))
		return
	}

	// Parsed and rebuilt rather than echoed: the reply is derived from the
	// number in the request, not from the request's own text.
	number, err := strconv.Atoi(strings.TrimPrefix(r.URL.Query().Get("lb_id"), "lb-"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	id := fmt.Sprintf("lb-%d", number)

	h.mu.Lock()
	h.requests++
	over := h.requests > h.after*len(bodies)
	if len(h.asked) == 0 || h.asked[len(h.asked)-1] != id {
		h.asked = append(h.asked, id)
	}
	h.mu.Unlock()

	if over {
		// Cancel and then hold the response until the client has torn the
		// request down, so this request fails with the context's error rather
		// than racing the cancellation to a reply the collector would count as
		// a measurement.
		h.cancel()
		<-r.Context().Done()
		return
	}
	if body, ok := bodies[r.URL.Path]; ok {
		_, _ = w.Write([]byte(strings.ReplaceAll(body, `"lb_id":"lb-1"`,
			fmt.Sprintf(`"lb_id":"lb-%d"`, number))))
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

// measured returns the load balancers the fan-out asked about, in order.
func (h *cutShortHandler) measured() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.asked...)
}

// A refresh the context cuts short is a failed refresh even though the first
// load balancers answered: the ones still queued were never measured, and
// reporting success would claim a snapshot of the whole account. The ones that
// did answer keep their fresh readings all the same.
func TestCutShortRefreshFailsAndKeepsThePartialMerge(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handler := &cutShortHandler{fleet: fleetJSON(3), after: 1, cancel: cancel}
	c := newTestCollector(t, 1, handler.ServeHTTP)

	err := c.Refresh(ctx)
	if !errors.Is(err, loadbalancermetrics.ErrRefreshCutShort) {
		t.Fatalf("refresh error = %v, want ErrRefreshCutShort", err)
	}
	if !strings.Contains(err.Error(), "measured 1 of 3 load balancers") {
		t.Errorf("refresh error = %q, want it to count the load balancers measured", err)
	}

	const want = `
# HELP digitalocean_loadbalancer_frontend_connections_current Active connections to the load balancer's frontend.
# TYPE digitalocean_loadbalancer_frontend_connections_current gauge
digitalocean_loadbalancer_frontend_connections_current{id="lb-1",name="lb-1"} 132
# HELP digitalocean_loadbalancer_metrics_up Whether the load balancer's last metrics fetch succeeded.
# TYPE digitalocean_loadbalancer_metrics_up gauge
digitalocean_loadbalancer_metrics_up{id="lb-1",name="lb-1"} 1
digitalocean_loadbalancer_metrics_up{id="lb-2",name="lb-2"} 0
digitalocean_loadbalancer_metrics_up{id="lb-3",name="lb-3"} 0
`
	err = testutil.CollectAndCompare(c, strings.NewReader(want),
		"digitalocean_loadbalancer_frontend_connections_current",
		"digitalocean_loadbalancer_metrics_up")
	if err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// A set that never fits in one refresh must not measure the same head of the
// list forever: each refresh starts where the last one stopped, so every load
// balancer is measured within a few of them.
func TestRotationCoversEveryLoadBalancerAcrossRefreshes(t *testing.T) {
	const fleet = 4
	first := make([]string, 0, fleet)

	var (
		mu      sync.Mutex
		cancel  context.CancelFunc
		handler *cutShortHandler
	)
	c := newTestCollector(t, 1, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		h := handler
		mu.Unlock()
		h.ServeHTTP(w, r)
	})

	for refresh := range fleet {
		var ctx context.Context
		ctx, cancel = context.WithCancel(context.Background())
		mu.Lock()
		handler = &cutShortHandler{fleet: fleetJSON(fleet), after: 1, cancel: cancel}
		mu.Unlock()

		if err := c.Refresh(ctx); !errors.Is(err, loadbalancermetrics.ErrRefreshCutShort) {
			t.Fatalf("refresh %d error = %v, want ErrRefreshCutShort", refresh, err)
		}
		cancel()

		asked := handler.measured()
		if len(asked) == 0 {
			t.Fatalf("refresh %d measured nothing at all", refresh)
		}
		first = append(first, asked[0])
	}

	want := []string{"lb-1", "lb-2", "lb-3", "lb-4"}
	if !slices.Equal(first, want) {
		t.Errorf("first load balancer of each refresh = %v, want %v", first, want)
	}
}

// A refresh that gets through the whole set leaves the order alone: rotation
// is what a cut-short refresh causes, not a cost every refresh pays.
func TestAFullRefreshKeepsTheListingOrder(t *testing.T) {
	var handler *cutShortHandler
	c := newTestCollector(t, 1, func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
	})

	for refresh := range 2 {
		handler = &cutShortHandler{fleet: fleetJSON(3), after: 3, cancel: func() {}}
		if err := c.Refresh(context.Background()); err != nil {
			t.Fatalf("refresh %d: %v", refresh, err)
		}
		want := []string{"lb-1", "lb-2", "lb-3"}
		if got := handler.measured(); !slices.Equal(got, want) {
			t.Errorf("refresh %d measured %v, want %v", refresh, got, want)
		}
	}
}

// The load balancer list can shift between two page requests, and the same
// load balancer then arrives on both. Measuring it twice would report every one
// of its readings twice, under identical labels, and fail the whole scrape.
func TestListDropsADuplicateLoadBalancerOnTwoPages(t *testing.T) {
	page := func(next bool) string {
		links := `"links":{}`
		if next {
			links = `"links":{"pages":{"next":"https://api.digitalocean.com/v2/load_balancers?page=2"}}`
		}
		return fmt.Sprintf(`{"load_balancers":[{"id":"lb-1","name":"public"}],%s,"meta":{"total":1}}`, links)
	}

	c := newTestCollector(t, 4, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/load_balancers" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(page(r.URL.Query().Get("page") != "2")))
			return
		}
		okHandler(w, r)
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if err := testutil.CollectAndCompare(c, strings.NewReader(wantMetrics)); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}
