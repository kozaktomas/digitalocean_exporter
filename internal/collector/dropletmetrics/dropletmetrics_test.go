package dropletmetrics_test

import (
	"context"
	"encoding/json"
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

	"github.com/kozaktomas/digitalocean_exporter/internal/collector/dropletmetrics"
	"github.com/kozaktomas/digitalocean_exporter/internal/doclient"
)

// sampledAt is the timestamp every fixture below reports, so the expected
// digitalocean_droplet_metrics_timestamp_seconds is a fixed number.
const sampledAt = 1787676960

// matrix builds a monitoring response body from a list of series, each given
// as its labels in JSON and its value.
func matrix(series ...string) string {
	return `{"status":"success","data":{"resultType":"matrix","result":[` +
		strings.Join(series, ",") + `]}}`
}

// point builds one series carrying a single sample.
func point(labels, value string) string {
	return fmt.Sprintf(`{"metric":%s,"values":[[%d,%q]]}`, labels, sampledAt, value)
}

// empty is what the API returns for a metric it has nothing for, which is a
// normal state rather than a failure.
const empty = `{"status":"success","data":{"resultType":"matrix","result":[]}}`

// bodies maps each monitoring path to what the fake API answers with. CPU is
// split by mode and the filesystem by mount, the way the real API splits them.
var bodies = map[string]string{
	"/v2/monitoring/metrics/droplet/cpu": matrix(
		point(`{"host_id":"1","mode":"idle"}`, "100.5"),
		point(`{"host_id":"1","mode":"user"}`, "20"),
	),
	"/v2/monitoring/metrics/droplet/memory_total":     matrix(point(`{"host_id":"1"}`, "8333348864")),
	"/v2/monitoring/metrics/droplet/memory_available": matrix(point(`{"host_id":"1"}`, "3876790272")),
	"/v2/monitoring/metrics/droplet/memory_free":      matrix(point(`{"host_id":"1"}`, "1073741824")),
	"/v2/monitoring/metrics/droplet/memory_cached":    matrix(point(`{"host_id":"1"}`, "2147483648")),
	"/v2/monitoring/metrics/droplet/filesystem_size": matrix(
		point(`{"host_id":"1","device":"/dev/vda1","fstype":"ext4","mountpoint":"/"}`, "168881938432"),
	),
	"/v2/monitoring/metrics/droplet/filesystem_free": matrix(
		point(`{"host_id":"1","device":"/dev/vda1","fstype":"ext4","mountpoint":"/"}`, "106837884928"),
	),
	"/v2/monitoring/metrics/droplet/load_1":  matrix(point(`{"host_id":"1"}`, "4.01")),
	"/v2/monitoring/metrics/droplet/load_5":  matrix(point(`{"host_id":"1"}`, "2.89")),
	"/v2/monitoring/metrics/droplet/load_15": matrix(point(`{"host_id":"1"}`, "1.5")),
}

const oneDropletJSON = `{"droplets":[{"id":1,"name":"web-1"}],"meta":{"total":1}}`

const wantMetrics = `
# HELP digitalocean_droplet_cpu_seconds_total Cumulative CPU time of the droplet in seconds, by mode.
# TYPE digitalocean_droplet_cpu_seconds_total counter
digitalocean_droplet_cpu_seconds_total{id="1",mode="idle",name="web-1"} 100.5
digitalocean_droplet_cpu_seconds_total{id="1",mode="user",name="web-1"} 20
# HELP digitalocean_droplet_filesystem_free_bytes Free space on the filesystem.
# TYPE digitalocean_droplet_filesystem_free_bytes gauge
` +
	`digitalocean_droplet_filesystem_free_bytes{device="/dev/vda1",fstype="ext4",id="1",` +
	`mountpoint="/",name="web-1"} 1.06837884928e+11` + "\n" +
	`# HELP digitalocean_droplet_filesystem_size_bytes Size of the filesystem.
# TYPE digitalocean_droplet_filesystem_size_bytes gauge
` +
	`digitalocean_droplet_filesystem_size_bytes{device="/dev/vda1",fstype="ext4",id="1",` +
	`mountpoint="/",name="web-1"} 1.68881938432e+11` + "\n" +
	`# HELP digitalocean_droplet_load1 Load average over the last minute.
# TYPE digitalocean_droplet_load1 gauge
digitalocean_droplet_load1{id="1",name="web-1"} 4.01
# HELP digitalocean_droplet_load15 Load average over the last fifteen minutes.
# TYPE digitalocean_droplet_load15 gauge
digitalocean_droplet_load15{id="1",name="web-1"} 1.5
# HELP digitalocean_droplet_load5 Load average over the last five minutes.
# TYPE digitalocean_droplet_load5 gauge
digitalocean_droplet_load5{id="1",name="web-1"} 2.89
# HELP digitalocean_droplet_memory_available_bytes Memory available for starting new applications without swapping.
# TYPE digitalocean_droplet_memory_available_bytes gauge
digitalocean_droplet_memory_available_bytes{id="1",name="web-1"} 3.876790272e+09
# HELP digitalocean_droplet_memory_cached_bytes Memory used by the page cache.
# TYPE digitalocean_droplet_memory_cached_bytes gauge
digitalocean_droplet_memory_cached_bytes{id="1",name="web-1"} 2.147483648e+09
# HELP digitalocean_droplet_memory_free_bytes Memory not used for anything at all.
# TYPE digitalocean_droplet_memory_free_bytes gauge
digitalocean_droplet_memory_free_bytes{id="1",name="web-1"} 1.073741824e+09
# HELP digitalocean_droplet_memory_total_bytes Total memory of the droplet as its operating system reports it.
# TYPE digitalocean_droplet_memory_total_bytes gauge
digitalocean_droplet_memory_total_bytes{id="1",name="web-1"} 8.333348864e+09
# HELP digitalocean_droplet_metrics_timestamp_seconds Unix time of the newest sample returned for the droplet.
# TYPE digitalocean_droplet_metrics_timestamp_seconds gauge
digitalocean_droplet_metrics_timestamp_seconds{id="1",name="web-1"} 1.78767696e+09
# HELP digitalocean_droplet_metrics_up Whether the droplet's last metrics fetch succeeded.
# TYPE digitalocean_droplet_metrics_up gauge
digitalocean_droplet_metrics_up{id="1",name="web-1"} 1
`

// newTestCollector wires a collector to a fake DigitalOcean API.
func newTestCollector(t *testing.T, concurrency int, handler http.HandlerFunc) *dropletmetrics.Collector {
	t.Helper()
	return newConfiguredCollector(t, dropletmetrics.Config{Concurrency: concurrency}, handler)
}

// newConfiguredCollector is newTestCollector for a test that sets more of the
// collector's configuration than its concurrency.
func newConfiguredCollector(
	t *testing.T, cfg dropletmetrics.Config, handler http.HandlerFunc,
) *dropletmetrics.Collector {
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
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	return dropletmetrics.New(client, cfg)
}

// okHandler serves one droplet and a full set of readings for it.
func okHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/v2/droplets" {
		_, _ = w.Write([]byte(oneDropletJSON))
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

// The window sent to the API has to reach back far enough to catch a sample,
// and must not ask for the future.
func TestRefreshAsksForARecentWindow(t *testing.T) {
	var (
		mu    sync.Mutex
		start string
		end   string
	)
	c := newTestCollector(t, 1, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v2/monitoring/") {
			mu.Lock()
			start, end = r.URL.Query().Get("start"), r.URL.Query().Get("end")
			mu.Unlock()
		}
		okHandler(w, r)
	})

	before := time.Now().Add(-time.Minute).Unix()
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	after := time.Now().Add(time.Minute).Unix()

	mu.Lock()
	defer mu.Unlock()
	var from, to int64
	if _, err := fmt.Sscan(start, &from); err != nil {
		t.Fatalf("start %q is not a timestamp: %v", start, err)
	}
	if _, err := fmt.Sscan(end, &to); err != nil {
		t.Fatalf("end %q is not a timestamp: %v", end, err)
	}
	if to < before || to > after {
		t.Errorf("end = %d, want a time around now (%d..%d)", to, before, after)
	}
	if span := to - from; span < 120 {
		t.Errorf("window = %ds, want at least one sampling interval of 120s", span)
	}
}

// A droplet without the monitoring agent answers every metric with an empty
// result. That is data, not a failure: it is up, with no readings and no
// timestamp.
func TestDropletWithoutAgentIsUpWithoutReadings(t *testing.T) {
	c := newTestCollector(t, 1, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v2/droplets" {
			_, _ = w.Write([]byte(oneDropletJSON))
			return
		}
		_, _ = w.Write([]byte(empty))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	const want = `
# HELP digitalocean_droplet_metrics_up Whether the droplet's last metrics fetch succeeded.
# TYPE digitalocean_droplet_metrics_up gauge
digitalocean_droplet_metrics_up{id="1",name="web-1"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want)); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

const twoDropletsJSON = `{"droplets":[{"id":1,"name":"web-1"},{"id":2,"name":"web-2"}],` +
	`"meta":{"total":2}}`

// twoDropletHandler serves two droplets and fails every metric request for the
// droplet named in failFor.
func twoDropletHandler(failFor string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v2/droplets" {
			_, _ = w.Write([]byte(twoDropletsJSON))
			return
		}
		if r.URL.Query().Get("host_id") == failFor {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		body, ok := bodies[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// The fixtures carry host_id 1; rewrite it so the second droplet's
		// series are its own.
		_, _ = w.Write([]byte(strings.ReplaceAll(body, `"host_id":"1"`, `"host_id":"2"`)))
	}
}

// One droplet failing must not cost the droplets that succeeded, and the
// refresh as a whole still succeeds. Run under the race detector this also
// covers the fan-out writing into the shared results slice.
func TestOneFailingDropletDoesNotCostTheOthers(t *testing.T) {
	c := newTestCollector(t, 4, twoDropletHandler("1"))

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh with one failing droplet: %v", err)
	}

	const want = `
# HELP digitalocean_droplet_metrics_up Whether the droplet's last metrics fetch succeeded.
# TYPE digitalocean_droplet_metrics_up gauge
digitalocean_droplet_metrics_up{id="1",name="web-1"} 0
digitalocean_droplet_metrics_up{id="2",name="web-2"} 1
`
	const metric = "digitalocean_droplet_metrics_up"
	if err := testutil.CollectAndCompare(c, strings.NewReader(want), metric); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}

	// The droplet that succeeded still reports its readings.
	const wantLoad = `
# HELP digitalocean_droplet_load1 Load average over the last minute.
# TYPE digitalocean_droplet_load1 gauge
digitalocean_droplet_load1{id="2",name="web-2"} 4.01
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(wantLoad), "digitalocean_droplet_load1"); err != nil {
		t.Errorf("unexpected load metrics: %v", err)
	}
}

// A droplet whose fetch fails keeps the readings it last reported, so a graph
// shows a stale value rather than a gap, and says so with up 0.
func TestFailingDropletKeepsItsPreviousReadings(t *testing.T) {
	var failing string
	c := newTestCollector(t, 1, func(w http.ResponseWriter, r *http.Request) {
		twoDropletHandler(failing)(w, r)
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}

	failing = "2"
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("second refresh: %v", err)
	}

	const want = `
# HELP digitalocean_droplet_load1 Load average over the last minute.
# TYPE digitalocean_droplet_load1 gauge
digitalocean_droplet_load1{id="1",name="web-1"} 4.01
digitalocean_droplet_load1{id="2",name="web-2"} 4.01
# HELP digitalocean_droplet_metrics_up Whether the droplet's last metrics fetch succeeded.
# TYPE digitalocean_droplet_metrics_up gauge
digitalocean_droplet_metrics_up{id="1",name="web-1"} 1
digitalocean_droplet_metrics_up{id="2",name="web-2"} 0
`
	err := testutil.CollectAndCompare(c, strings.NewReader(want),
		"digitalocean_droplet_load1", "digitalocean_droplet_metrics_up")
	if err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// Every droplet failing points at the API rather than at any one droplet, so
// the refresh itself fails and collector_success goes to 0.
func TestEveryDropletFailingFailsTheRefresh(t *testing.T) {
	c := newTestCollector(t, 4, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v2/droplets" {
			_, _ = w.Write([]byte(twoDropletsJSON))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	})

	if err := c.Refresh(context.Background()); err == nil {
		t.Fatal("expected the refresh to fail when no droplet could be measured")
	}
}

// Failing to list the droplets fails the refresh outright: there is nothing to
// measure and no way to tell which droplets still exist.
func TestFailedListingFailsTheRefresh(t *testing.T) {
	c := newTestCollector(t, 1, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	if err := c.Refresh(context.Background()); err == nil {
		t.Fatal("expected the refresh to fail when the droplets cannot be listed")
	}
}

// An account with no droplets is a normal state: nothing to measure, nothing
// to report, and no failure.
func TestRefreshWithoutDropletsSucceeds(t *testing.T) {
	c := newTestCollector(t, 1, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"droplets":[],"meta":{"total":0}}`))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh without droplets: %v", err)
	}
	if got := testutil.CollectAndCount(c); got != 0 {
		t.Errorf("metric count without droplets = %d, want 0", got)
	}
}

// A droplet that has been destroyed drops out of the snapshot rather than
// lingering with its last readings forever.
func TestDestroyedDropletLeavesTheSnapshot(t *testing.T) {
	droplets := twoDropletsJSON
	c := newTestCollector(t, 1, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v2/droplets" {
			_, _ = w.Write([]byte(droplets))
			return
		}
		if body, ok := bodies[r.URL.Path]; ok {
			_, _ = w.Write([]byte(strings.ReplaceAll(body, `"host_id":"1"`, `"host_id":"2"`)))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}

	droplets = oneDropletJSON
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("second refresh: %v", err)
	}

	const want = `
# HELP digitalocean_droplet_metrics_up Whether the droplet's last metrics fetch succeeded.
# TYPE digitalocean_droplet_metrics_up gauge
digitalocean_droplet_metrics_up{id="1",name="web-1"} 1
`
	const metric = "digitalocean_droplet_metrics_up"
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
	if got := c.Name(); got != "dropletmetrics" {
		t.Errorf("Name() = %q, want %q", got, "dropletmetrics")
	}
}

// A concurrency below one would deadlock the fan-out, so it is raised to one.
func TestZeroConcurrencyStillMeasures(t *testing.T) {
	c := newTestCollector(t, 0, okHandler)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh with zero concurrency: %v", err)
	}
	if got := testutil.CollectAndCount(c); got == 0 {
		t.Error("metric count with zero concurrency = 0, want the readings of one droplet")
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
	if want := 12; count != want {
		t.Errorf("Describe sent %d descriptors, want %d", count, want)
	}
}

// fleetJSON lists count droplets named web-N, each carrying the features
// given, so a test can build a fleet larger than one refresh can measure.
func fleetJSON(count int, features ...string) string {
	feature, _ := json.Marshal(features)
	entries := make([]string, 0, count)
	for i := 1; i <= count; i++ {
		entries = append(entries, fmt.Sprintf(`{"id":%d,"name":"web-%d","features":%s}`,
			i, i, feature))
	}
	return fmt.Sprintf(`{"droplets":[%s],"meta":{"total":%d}}`,
		strings.Join(entries, ","), count)
}

// cutShortHandler serves a fleet and cancels the refresh once measure requests
// for `after` droplets have been answered, which is what a timeout does to a
// fleet too large to fit in it. The cancellation lands on the first request of
// the next droplet, so the droplets before it are measured in full.
//
// It records the droplet each measure request was for, in the order the
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
	if r.URL.Path == "/v2/droplets" {
		_, _ = w.Write([]byte(h.fleet))
		return
	}

	// Parsed and rebuilt rather than echoed: the reply is derived from the
	// number in the request, not from the request's own text.
	number, err := strconv.Atoi(r.URL.Query().Get("host_id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	host := strconv.Itoa(number)

	h.mu.Lock()
	h.requests++
	over := h.requests > h.after*len(bodies)
	if len(h.asked) == 0 || h.asked[len(h.asked)-1] != host {
		h.asked = append(h.asked, host)
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
		_, _ = w.Write([]byte(strings.ReplaceAll(body, `"host_id":"1"`,
			fmt.Sprintf(`"host_id":"%d"`, number))))
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

// measured returns the droplets the fan-out asked about, in order.
func (h *cutShortHandler) measured() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.asked...)
}

// A refresh the context cuts short is a failed refresh even though most of the
// fleet answered: the droplets still queued were never measured, and reporting
// success would claim a snapshot of the whole account. The droplets that did
// answer keep their fresh readings all the same.
func TestCutShortRefreshFailsAndKeepsThePartialMerge(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handler := &cutShortHandler{fleet: fleetJSON(3), after: 1, cancel: cancel}
	c := newTestCollector(t, 1, handler.ServeHTTP)

	err := c.Refresh(ctx)
	if !errors.Is(err, dropletmetrics.ErrRefreshCutShort) {
		t.Fatalf("refresh error = %v, want ErrRefreshCutShort", err)
	}
	if !strings.Contains(err.Error(), "measured 1 of 3 droplets") {
		t.Errorf("refresh error = %q, want it to count the droplets measured", err)
	}

	// The droplet that answered keeps its reading; the two that never had
	// their turn are reported down with none.
	const want = `
# HELP digitalocean_droplet_load1 Load average over the last minute.
# TYPE digitalocean_droplet_load1 gauge
digitalocean_droplet_load1{id="1",name="web-1"} 4.01
# HELP digitalocean_droplet_metrics_up Whether the droplet's last metrics fetch succeeded.
# TYPE digitalocean_droplet_metrics_up gauge
digitalocean_droplet_metrics_up{id="1",name="web-1"} 1
digitalocean_droplet_metrics_up{id="2",name="web-2"} 0
digitalocean_droplet_metrics_up{id="3",name="web-3"} 0
`
	err = testutil.CollectAndCompare(c, strings.NewReader(want),
		"digitalocean_droplet_load1", "digitalocean_droplet_metrics_up")
	if err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// A fleet that never fits in one refresh must not measure the same head of the
// list forever: each refresh starts where the last one stopped, so every
// droplet is measured within a few of them.
func TestRotationCoversEveryDropletAcrossRefreshes(t *testing.T) {
	const fleet = 4
	first := make([]string, 0, fleet)
	covered := make(map[string]bool, fleet)

	// One collector across every refresh — a new one would start from the head
	// of the list each time — against a stub that cuts each refresh short
	// after a single droplet.
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

		if err := c.Refresh(ctx); !errors.Is(err, dropletmetrics.ErrRefreshCutShort) {
			t.Fatalf("refresh %d error = %v, want ErrRefreshCutShort", refresh, err)
		}
		cancel()

		asked := handler.measured()
		if len(asked) == 0 {
			t.Fatalf("refresh %d measured no droplet at all", refresh)
		}
		first = append(first, asked[0])
		covered[asked[0]] = true
	}

	if len(covered) != fleet {
		t.Errorf("droplets measured first across %d refreshes = %v, want all %d of them",
			fleet, first, fleet)
	}
	if want := []string{"1", "2", "3", "4"}; !slices.Equal(first, want) {
		t.Errorf("first droplet of each refresh = %v, want %v", first, want)
	}
}

// A refresh that gets through the whole fleet leaves the order alone: rotation
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
		if got, want := handler.measured(), []string{"1", "2", "3"}; !slices.Equal(got, want) {
			t.Errorf("refresh %d measured %v, want %v", refresh, got, want)
		}
	}
}

// With agent-only set, a droplet whose listing does not report the monitoring
// agent is not queried at all: it costs no requests and reports nothing, since
// there is no reading to report and no failure to describe.
func TestAgentOnlySkipsDropletsWithoutTheFeature(t *testing.T) {
	const mixed = `{"droplets":[` +
		`{"id":1,"name":"web-1","features":["monitoring","private_networking"]},` +
		`{"id":2,"name":"web-2","features":["private_networking"]}],"meta":{"total":2}}`

	var asked []string
	var mu sync.Mutex
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v2/droplets" {
			_, _ = w.Write([]byte(mixed))
			return
		}
		mu.Lock()
		asked = append(asked, r.URL.Query().Get("host_id"))
		mu.Unlock()
		_, _ = w.Write([]byte(bodies[r.URL.Path]))
	}

	c := newConfiguredCollector(t, dropletmetrics.Config{Concurrency: 1, AgentOnly: true}, handler)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	const want = `
# HELP digitalocean_droplet_metrics_up Whether the droplet's last metrics fetch succeeded.
# TYPE digitalocean_droplet_metrics_up gauge
digitalocean_droplet_metrics_up{id="1",name="web-1"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want),
		"digitalocean_droplet_metrics_up"); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, host := range asked {
		if host != "1" {
			t.Fatalf("measured droplet %q, want only the one reporting the agent", host)
		}
	}
	if len(asked) != len(bodies) {
		t.Errorf("measure requests = %d, want %d, one per metric of the one droplet",
			len(asked), len(bodies))
	}
}

// Without the flag every droplet is measured, agent or not: the feature only
// says the droplet was created with the agent, and one installed afterwards
// reports readings without ever setting it.
func TestWithoutAgentOnlyEveryDropletIsMeasured(t *testing.T) {
	handler := &cutShortHandler{
		fleet: fleetJSON(2, "private_networking"), after: 2, cancel: func() {},
	}
	c := newTestCollector(t, 2, handler.ServeHTTP)

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	const want = `
# HELP digitalocean_droplet_metrics_up Whether the droplet's last metrics fetch succeeded.
# TYPE digitalocean_droplet_metrics_up gauge
digitalocean_droplet_metrics_up{id="1",name="web-1"} 1
digitalocean_droplet_metrics_up{id="2",name="web-2"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want),
		"digitalocean_droplet_metrics_up"); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// The droplet list can shift between two page requests, and the same droplet
// then arrives on both. Measuring it twice would report every one of its
// readings twice, under identical labels, and fail the whole scrape.
func TestListDropsADuplicateDropletOnTwoPages(t *testing.T) {
	page := func(next bool) string {
		links := `"links":{}`
		if next {
			links = `"links":{"pages":{"next":"https://api.digitalocean.com/v2/droplets?page=2"}}`
		}
		return fmt.Sprintf(`{"droplets":[{"id":1,"name":"web-1"}],%s,"meta":{"total":1}}`, links)
	}

	c := newTestCollector(t, 4, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/droplets" {
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
