package collector_test

import (
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
}

func newFake() *fake { return newNamedFake("fake") }

func newNamedFake(name string) *fake {
	return &fake{name: name, desc: prometheus.NewDesc("fake_metric", "Fake.", nil, nil)}
}

func (f *fake) Name() string { return f.name }

func (f *fake) Describe(ch chan<- *prometheus.Desc) { ch <- f.desc }

func (f *fake) Refresh(ctx context.Context) error {
	if f.calls.Add(1) == 1 {
		f.first.Store(time.Now().UnixNano())
	}
	if deadline, ok := ctx.Deadline(); ok {
		f.budget.Store(int64(time.Until(deadline)))
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
