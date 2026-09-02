package collector_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kozaktomas/digitalocean_exporter/internal/collector"
)

// fake is a Collector whose refresh behaviour the test controls.
type fake struct {
	mu       sync.Mutex
	name     string
	calls    atomic.Int64
	err      error
	desc     *prometheus.Desc
	snapshot float64
	budget   atomic.Int64
	first    atomic.Int64

	panicking  atomic.Bool  // whether Refresh panics instead of returning.
	work       atomic.Int64 // how long one refresh takes.
	blocking   atomic.Bool  // whether Refresh waits for its context instead of returning.
	inFlight   atomic.Int64
	overlapped atomic.Bool // whether two refreshes were ever in flight at once.
}

func newFake() *fake { return newNamedFake("fake") }

func newNamedFake(name string) *fake {
	return &fake{name: name, desc: prometheus.NewDesc("fake_metric", "Fake.", nil, nil)}
}

func (f *fake) Name() string { return f.name }

func (f *fake) Describe(ch chan<- *prometheus.Desc) { ch <- f.desc }

func (f *fake) Refresh(ctx context.Context) error {
	if f.inFlight.Add(1) > 1 {
		f.overlapped.Store(true)
	}
	defer f.inFlight.Add(-1)

	if f.calls.Add(1) == 1 {
		f.first.Store(time.Now().UnixNano())
	}
	if deadline, ok := ctx.Deadline(); ok {
		f.budget.Store(int64(time.Until(deadline)))
	}
	if f.panicking.Load() {
		panic("nil pointer in an unexpected API response")
	}
	if work := time.Duration(f.work.Load()); work > 0 {
		// Interruptible, so that a refresh still running when the test ends
		// gives its goroutine back instead of deadlocking the bubble.
		timer := time.NewTimer(work)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	if f.blocking.Load() {
		<-ctx.Done()
		return ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.snapshot++
	return nil
}

func (f *fake) Collect(ch chan<- prometheus.Metric) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch <- prometheus.MustNewConstMetric(f.desc, prometheus.GaugeValue, f.snapshot)
}

// firstRefresh reports when the collector was first refreshed.
func (f *fake) firstRefresh() time.Time {
	return time.Unix(0, f.first.Load())
}

func (f *fake) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestSchedulerRefreshesImmediatelyAndOnInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reg := prometheus.NewRegistry()
		scheduler := collector.NewScheduler(time.Second, discardLogger(), reg)
		f := newFake()
		scheduler.Register(f, time.Minute, 0)
		reg.MustRegister(scheduler)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go scheduler.Run(ctx)

		synctest.Wait()
		if got := f.calls.Load(); got != 1 {
			t.Fatalf("refresh calls after start = %d, want 1 immediate refresh", got)
		}

		time.Sleep(3 * time.Minute)
		synctest.Wait()
		if got := f.calls.Load(); got != 4 {
			t.Fatalf("refresh calls after 3 intervals = %d, want 4", got)
		}
	})
}

func TestSchedulerKeepsSnapshotAfterFailedRefresh(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reg := prometheus.NewRegistry()
		scheduler := collector.NewScheduler(time.Second, discardLogger(), reg)
		f := newFake()
		scheduler.Register(f, time.Minute, 0)
		reg.MustRegister(scheduler)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go scheduler.Run(ctx)
		synctest.Wait()

		// One good refresh, then the API starts failing.
		f.setErr(errors.New("api is down"))
		time.Sleep(time.Minute)
		synctest.Wait()

		if got := testutil.ToFloat64(f); got != 1 {
			t.Errorf("fake_metric = %v, want the snapshot from the last good refresh (1)", got)
		}

		success := `
# HELP digitalocean_exporter_collector_success Whether the collector's last refresh succeeded.
# TYPE digitalocean_exporter_collector_success gauge
digitalocean_exporter_collector_success{collector="fake"} 0
`
		if err := testutil.GatherAndCompare(reg, strings.NewReader(success),
			"digitalocean_exporter_collector_success"); err != nil {
			t.Errorf("success metric: %v", err)
		}
	})
}

func TestSchedulerBoundsRefreshByItsOwnTimeoutByDefault(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reg := prometheus.NewRegistry()
		scheduler := collector.NewScheduler(30*time.Second, discardLogger(), reg)
		f := newFake()
		scheduler.Register(f, time.Minute, 0)
		reg.MustRegister(scheduler)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go scheduler.Run(ctx)
		synctest.Wait()

		if got := time.Duration(f.budget.Load()); got != 30*time.Second {
			t.Errorf("refresh budget = %v, want the scheduler timeout of 30s", got)
		}
	})
}

// A collector that fans out over every droplet runs far past a timeout sized
// for a single API call, so a collector may bring its own.
func TestSchedulerBoundsRefreshByTheCollectorTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reg := prometheus.NewRegistry()
		scheduler := collector.NewScheduler(30*time.Second, discardLogger(), reg)
		f := newFake()
		scheduler.Register(f, time.Hour, 15*time.Minute)
		reg.MustRegister(scheduler)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go scheduler.Run(ctx)
		synctest.Wait()

		if got := time.Duration(f.budget.Load()); got != 15*time.Minute {
			t.Errorf("refresh budget = %v, want the collector timeout of 15m", got)
		}
	})
}

// Config rejects a non-positive interval, but the scheduler must survive one
// anyway: time.NewTicker panics on it, inside a goroutine raised after the
// metrics server has already bound its port.
func TestSchedulerFallsBackToADefaultIntervalInsteadOfPanicking(t *testing.T) {
	for name, interval := range map[string]time.Duration{"zero": 0, "negative": -time.Minute} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				reg := prometheus.NewRegistry()
				scheduler := collector.NewScheduler(time.Second, discardLogger(), reg)
				f := newFake()
				scheduler.Register(f, interval, 0)
				reg.MustRegister(scheduler)

				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				go scheduler.Run(ctx)

				synctest.Wait()
				if got := f.calls.Load(); got != 1 {
					t.Fatalf("refresh calls after start = %d, want 1 immediate refresh", got)
				}

				// The fallback is five minutes, so ten of them are two ticks.
				time.Sleep(10 * time.Minute)
				synctest.Wait()
				if got := f.calls.Load(); got != 3 {
					t.Fatalf("refresh calls after 10m = %d, want 3 on the 5m fallback", got)
				}
			})
		})
	}
}

// Every collector shares the same interval by default, so without a stagger the
// whole set fires at once, every interval — the burst DigitalOcean's
// 250-a-minute limit objects to.
func TestSchedulerStaggersTheFirstRefreshOfEachCollector(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reg := prometheus.NewRegistry()
		scheduler := collector.NewScheduler(time.Second, discardLogger(), reg)

		fakes := make([]*fake, 5)
		for i := range fakes {
			fakes[i] = newNamedFake("fake-" + strconv.Itoa(i))
			scheduler.Register(fakes[i], time.Minute, 0)
		}

		start := time.Now()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go scheduler.Run(ctx)

		// The ceiling is three seconds, so by then every one of them has run
		// once and none has ticked again on its one-minute interval.
		time.Sleep(3 * time.Second)
		synctest.Wait()

		seen := make(map[time.Duration]string, len(fakes))
		for i, f := range fakes {
			if got := f.calls.Load(); got != 1 {
				t.Fatalf("%s refreshed %d times in the first 3s, want 1", f.Name(), got)
			}
			offset := f.firstRefresh().Sub(start)
			if other, clash := seen[offset]; clash {
				t.Errorf("%s and %s both refreshed at %v", f.Name(), other, offset)
			}
			seen[offset] = f.Name()

			// Five collectors sharing a three-second window: one every 600ms,
			// the first of them straight away.
			if want := time.Duration(i) * 600 * time.Millisecond; offset != want {
				t.Errorf("%s first refreshed after %v, want %v", f.Name(), offset, want)
			}
		}
	})
}

// The spread is bounded so that /metrics is worth scraping moments after
// startup, however long the collectors' intervals are.
func TestSchedulerStaggerStaysUnderItsCeiling(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reg := prometheus.NewRegistry()
		scheduler := collector.NewScheduler(time.Second, discardLogger(), reg)

		fakes := make([]*fake, 20)
		for i := range fakes {
			fakes[i] = newNamedFake("fake-" + strconv.Itoa(i))
			scheduler.Register(fakes[i], time.Hour, 0)
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go scheduler.Run(ctx)

		time.Sleep(3 * time.Second)
		synctest.Wait()

		for _, f := range fakes {
			if got := f.calls.Load(); got != 1 {
				t.Errorf("%s refreshed %d times within the 3s ceiling, want 1", f.Name(), got)
			}
		}
	})
}

// syncBuffer collects log output from the scheduler's goroutines so a test can
// read it while they are still running.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// recordingLogger logs everything, down to debug level, into buf.
func recordingLogger(buf *syncBuffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// Sixteen collectors share one process, so a panic in any of them — one nil
// field in one API response — otherwise takes the exporter down, and takes it
// down again after every restart.
func TestSchedulerRecoversFromAPanickingCollector(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var logs syncBuffer
		reg := prometheus.NewRegistry()
		scheduler := collector.NewScheduler(time.Second, recordingLogger(&logs), reg)
		f := newFake()
		healthy := newNamedFake("healthy")
		scheduler.Register(f, time.Minute, 0)
		scheduler.Register(healthy, time.Minute, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go scheduler.Run(ctx)

		// Past the stagger ceiling, so both have refreshed once, successfully.
		time.Sleep(3 * time.Second)
		synctest.Wait()

		f.panicking.Store(true)
		time.Sleep(time.Minute)
		synctest.Wait()

		if got := f.calls.Load(); got != 2 {
			t.Fatalf("fake refresh calls = %d, want the panicking second refresh", got)
		}
		if got := testutil.ToFloat64(f); got != 1 {
			t.Errorf("fake_metric = %v, want the snapshot from before the panic (1)", got)
		}

		// A panic is recorded exactly like any other failed refresh, and the
		// collector beside it never notices.
		success := `
# HELP digitalocean_exporter_collector_success Whether the collector's last refresh succeeded.
# TYPE digitalocean_exporter_collector_success gauge
digitalocean_exporter_collector_success{collector="fake"} 0
digitalocean_exporter_collector_success{collector="healthy"} 1
`
		if err := testutil.GatherAndCompare(reg, strings.NewReader(success),
			"digitalocean_exporter_collector_success"); err != nil {
			t.Errorf("success metric: %v", err)
		}
		if got := healthy.calls.Load(); got != 2 {
			t.Errorf("healthy refresh calls = %d, want 2 — the panic must not stop it", got)
		}

		out := logs.String()
		for _, want := range []string{"collector refresh panicked", "collector=fake",
			"nil pointer in an unexpected API response", "goroutine"} {
			if !strings.Contains(out, want) {
				t.Errorf("log output does not mention %q:\n%s", want, out)
			}
		}

		// The next refresh of the same collector still happens.
		f.panicking.Store(false)
		time.Sleep(time.Minute)
		synctest.Wait()

		if got := f.calls.Load(); got != 3 {
			t.Fatalf("fake refresh calls = %d, want a third refresh after the panic", got)
		}
		if got := testutil.ToFloat64(f); got != 2 {
			t.Errorf("fake_metric = %v, want the refresh after the panic to have landed (2)", got)
		}
		recovered := `
# HELP digitalocean_exporter_collector_success Whether the collector's last refresh succeeded.
# TYPE digitalocean_exporter_collector_success gauge
digitalocean_exporter_collector_success{collector="fake"} 1
digitalocean_exporter_collector_success{collector="healthy"} 1
`
		if err := testutil.GatherAndCompare(reg, strings.NewReader(recovered),
			"digitalocean_exporter_collector_success"); err != nil {
			t.Errorf("success metric after recovery: %v", err)
		}
	})
}

// A refresh cut short by shutdown is not an incident: recorded as one, the last
// lines an operator reads after a restart look like a DigitalOcean outage.
func TestSchedulerDoesNotRecordAFailureOnShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var logs syncBuffer
		reg := prometheus.NewRegistry()
		scheduler := collector.NewScheduler(time.Hour, recordingLogger(&logs), reg)
		f := newFake()
		scheduler.Register(f, time.Minute, 0)

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			scheduler.Run(ctx)
		}()
		synctest.Wait()

		// The second refresh is still in flight when the run context is
		// cancelled, and returns that context's error.
		f.blocking.Store(true)
		time.Sleep(time.Minute)
		synctest.Wait()
		if got := f.calls.Load(); got != 2 {
			t.Fatalf("refresh calls = %d, want a second refresh in flight", got)
		}

		cancel()
		<-done

		success := `
# HELP digitalocean_exporter_collector_success Whether the collector's last refresh succeeded.
# TYPE digitalocean_exporter_collector_success gauge
digitalocean_exporter_collector_success{collector="fake"} 1
`
		if err := testutil.GatherAndCompare(reg, strings.NewReader(success),
			"digitalocean_exporter_collector_success"); err != nil {
			t.Errorf("success metric: %v", err)
		}

		out := logs.String()
		if strings.Contains(out, "level=ERROR") {
			t.Errorf("shutdown logged an error:\n%s", out)
		}
		if !strings.Contains(out, "collector refresh interrupted by shutdown") {
			t.Errorf("shutdown was not logged at all:\n%s", out)
		}
	})
}

// The window every first refresh takes a share of comes from the whole set, not
// from one collector's interval: shares of different windows collide.
func TestSchedulerStaggersDistinctlyAcrossMixedIntervals(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reg := prometheus.NewRegistry()
		scheduler := collector.NewScheduler(time.Second, discardLogger(), reg)

		// A one-second interval among five-minute ones: a share of a
		// one-second window and a share of the three-second ceiling land on
		// the same moment as soon as the indices happen to line up.
		intervals := []time.Duration{5 * time.Minute, 5 * time.Minute, 5 * time.Minute, time.Second, 5 * time.Minute}
		fakes := make([]*fake, len(intervals))
		for i, interval := range intervals {
			fakes[i] = newNamedFake("fake-" + strconv.Itoa(i))
			scheduler.Register(fakes[i], interval, 0)
		}

		start := time.Now()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go scheduler.Run(ctx)

		time.Sleep(3 * time.Second)
		synctest.Wait()

		seen := make(map[time.Duration]string, len(fakes))
		for i, f := range fakes {
			if got := f.calls.Load(); got < 1 {
				t.Fatalf("%s never refreshed", f.Name())
			}
			offset := f.firstRefresh().Sub(start)
			if other, clash := seen[offset]; clash {
				t.Errorf("%s and %s both first refreshed at %v", f.Name(), other, offset)
			}
			seen[offset] = f.Name()

			// The window is the shortest interval of the set, one second,
			// shared five ways.
			if want := time.Duration(i) * 200 * time.Millisecond; offset != want {
				t.Errorf("%s first refreshed after %v, want %v", f.Name(), offset, want)
			}
		}
	})
}

// Shutdown has to be quick and complete: a leftover goroutine or a timer still
// holding the process is what turns a restart into a kill.
func TestSchedulerRunReturnsWhenItsContextIsCancelled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reg := prometheus.NewRegistry()
		scheduler := collector.NewScheduler(time.Second, discardLogger(), reg)

		fakes := make([]*fake, 5)
		for i := range fakes {
			fakes[i] = newNamedFake("fake-" + strconv.Itoa(i))
			scheduler.Register(fakes[i], time.Hour, 0)
		}

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			scheduler.Run(ctx)
		}()

		// Only the first collector has refreshed; the rest are still holding
		// their first refresh back, so both waits are exercised.
		synctest.Wait()

		cancel()
		cancelled := time.Now()
		select {
		case <-done:
		case <-time.After(time.Minute):
			t.Fatal("Run did not return within a minute of its context being cancelled")
		}
		if took := time.Since(cancelled); took != 0 {
			t.Errorf("Run returned %v after cancellation, want immediately", took)
		}

		// Nothing is left to fire: an hour later the counts are what they were,
		// and synctest fails the test if a goroutine of the bubble outlives it.
		before := make([]int64, len(fakes))
		for i, f := range fakes {
			before[i] = f.calls.Load()
		}
		time.Sleep(time.Hour)
		synctest.Wait()
		for i, f := range fakes {
			if got := f.calls.Load(); got != before[i] {
				t.Errorf("%s refreshed %d times after Run returned, want %d", f.Name(), got, before[i])
			}
		}
	})
}

// The DigitalOcean API is slow enough that a collector fanning out over every
// droplet can outrun its own interval. Two refreshes of one collector at once
// would double its share of the rate limit and race over its snapshot.
func TestSchedulerDoesNotOverlapRefreshesOfTheSameCollector(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reg := prometheus.NewRegistry()
		scheduler := collector.NewScheduler(time.Hour, discardLogger(), reg)
		f := newFake()
		f.work.Store(int64(90 * time.Second))
		scheduler.Register(f, time.Minute, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go scheduler.Run(ctx)

		time.Sleep(5 * time.Minute)
		synctest.Wait()

		if f.overlapped.Load() {
			t.Error("two refreshes of the same collector were in flight at once")
		}
		// A 90-second refresh on a one-minute interval starts every 90 seconds:
		// the tick that arrives mid-refresh is taken as soon as it ends, and
		// the ticks behind it are dropped. Four starts in five minutes.
		if got := f.calls.Load(); got != 4 {
			t.Errorf("refresh calls in 5m = %d, want 4 back-to-back 90s refreshes", got)
		}
	})
}
