package collector_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
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
	calls    atomic.Int64
	err      error
	desc     *prometheus.Desc
	snapshot float64
	budget   atomic.Int64
}

func newFake() *fake {
	return &fake{desc: prometheus.NewDesc("fake_metric", "Fake.", nil, nil)}
}

func (f *fake) Name() string { return "fake" }

func (f *fake) Describe(ch chan<- *prometheus.Desc) { ch <- f.desc }

func (f *fake) Refresh(ctx context.Context) error {
	f.calls.Add(1)
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

// Listing every object of a Spaces bucket takes minutes, far past a timeout
// sized for a single API call, so a collector may bring its own.
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
