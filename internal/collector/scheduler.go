package collector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/kozaktomas/digitalocean_exporter/internal/doclient"
)

// defaultInterval is what a collector registered with a non-positive interval
// falls back to. It matches the default of every --collector.<name>.interval
// flag, so the fallback behaves like an unconfigured collector.
const defaultInterval = 5 * time.Minute

// maxStagger caps how long the last collector waits for its first refresh.
//
// Collectors are otherwise all registered with the same interval and all
// started at once, so every interval the whole set fires within milliseconds of
// itself — the shape of traffic that trips DigitalOcean's 250-requests-a-minute
// burst limit while the hourly budget is barely touched. Spreading the first
// refresh spreads every later one with it, because each collector keeps the
// phase its first refresh gave it.
//
// The ceiling is what keeps /metrics useful moments after startup: a few
// seconds of spread is enough to matter to the burst limit and short enough
// that nobody waits for it.
const maxStagger = 3 * time.Second

// Scheduler refreshes each registered collector on its own interval and
// exposes them, plus the exporter's own health metrics, to Prometheus.
type Scheduler struct {
	timeout time.Duration
	logger  *slog.Logger
	entries []entry

	success  *prometheus.GaugeVec
	duration *prometheus.GaugeVec
	lastOK   *prometheus.GaugeVec

	// mu guards ready, which is written from every collector's loop and read
	// from the readiness handler on the HTTP server's goroutines.
	mu sync.Mutex
	// ready holds the collectors that have completed at least one successful
	// refresh, by name. Names are unique: a collector is registered once.
	ready map[string]bool
}

// entry pairs a collector with the interval it refreshes on and the timeout
// that bounds one refresh.
type entry struct {
	collector Collector
	interval  time.Duration
	timeout   time.Duration
}

// NewScheduler creates a scheduler whose refreshes are bounded by timeout and
// whose self-metrics are registered with reg.
func NewScheduler(timeout time.Duration, logger *slog.Logger, reg prometheus.Registerer) *Scheduler {
	s := &Scheduler{
		timeout: timeout,
		logger:  logger,
		ready:   make(map[string]bool),
		success: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "digitalocean_exporter_collector_success",
			Help: "Whether the collector's last refresh succeeded.",
		}, []string{"collector"}),
		duration: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "digitalocean_exporter_collector_duration_seconds",
			Help: "Duration of the collector's last refresh.",
		}, []string{"collector"}),
		lastOK: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "digitalocean_exporter_collector_last_success_timestamp_seconds",
			Help: "Unix timestamp of the collector's last successful refresh.",
		}, []string{"collector"}),
	}
	reg.MustRegister(s.success, s.duration, s.lastOK)
	return s
}

// Register adds a collector to be refreshed every interval, bounding one
// refresh by timeout. A timeout of zero means the scheduler's own: a collector
// whose refresh is a single API call needs nothing special, while one that fans
// out over every droplet has to say so. It must be called before Run.
//
// A non-positive interval falls back to defaultInterval. Config rejects such a
// value at startup, so this is only the second line of defence — but it is
// worth having, because time.NewTicker panics on a non-positive duration and it
// would do so in a goroutine raised long after the metrics server bound its
// port, killing the process with a stack trace.
func (s *Scheduler) Register(c Collector, interval, timeout time.Duration) {
	if timeout <= 0 {
		timeout = s.timeout
	}
	if interval <= 0 {
		s.logger.Warn("collector registered with a non-positive interval",
			"collector", c.Name(), "interval", interval, "using", defaultInterval)
		interval = defaultInterval
	}
	s.entries = append(s.entries, entry{collector: c, interval: interval, timeout: timeout})
}

// Pending returns the registered collectors that have not yet completed a
// successful refresh, in registration order. It is empty once every one of
// them holds a snapshot, and empty from the start when nothing is registered.
//
// This is what readiness is made of. A collector emits nothing at all before
// its first success — not zeros — so a scrape taken earlier is silently
// missing whole metrics rather than reporting them as unknown, and a pod that
// answers it is not yet serving what it was rolled out for.
//
// Register is called before Run, so entries is only read here.
func (s *Scheduler) Pending() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	pending := make([]string, 0, len(s.entries))
	for _, e := range s.entries {
		if name := e.collector.Name(); !s.ready[name] {
			pending = append(pending, name)
		}
	}
	return pending
}

// markReady records that a collector has produced its first snapshot. Later
// successes cost a map write that changes nothing, which is cheaper than the
// bookkeeping needed to avoid it.
func (s *Scheduler) markReady(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ready[name] = true
}

// Names returns the names of the registered collectors, in registration order.
func (s *Scheduler) Names() []string {
	names := make([]string, 0, len(s.entries))
	for _, e := range s.entries {
		names = append(names, e.collector.Name())
	}
	return names
}

// Run refreshes every registered collector — the first one straight away, the
// rest staggered — and then on its own ticker. It returns once ctx is cancelled
// and all loops have stopped.
func (s *Scheduler) Run(ctx context.Context) {
	window := staggerWindow(s.entries)

	var wg sync.WaitGroup
	for index, e := range s.entries {
		offset := stagger(index, len(s.entries), window)
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.loop(ctx, e, offset)
		}()
	}
	wg.Wait()
}

// staggerWindow returns the span the first refreshes are spread across: the
// shortest interval among the registered collectors, capped at maxStagger.
//
// It has to be the shortest of all of them rather than each collector's own,
// because a per-collector window makes the offsets share a scale only when the
// intervals do: a collector on a one-second interval and one on an hour would
// both be offered a share of their own window and could land on the same
// moment. One window for the whole set keeps every share distinct, and taking
// the smallest interval keeps the spread inside the interval of the collector
// that refreshes most often, so no collector's first tick arrives before its
// first refresh.
func staggerWindow(entries []entry) time.Duration {
	window := maxStagger
	for _, e := range entries {
		window = min(window, e.interval)
	}
	return window
}

// stagger returns how long the collector at index waits before its first
// refresh: an even share of the window shared by the whole set. Deriving it
// from the registration order rather than from the clock or a hash of the name
// makes it the same on every run, and gives every collector a different offset
// as long as the window is wide enough to divide — a share of a nanosecond
// rounds to nothing, and then nothing is staggered.
func stagger(index, count int, window time.Duration) time.Duration {
	if index <= 0 || count <= 1 {
		return 0
	}
	return window / time.Duration(count) * time.Duration(index)
}

// loop drives a single collector until ctx is cancelled, holding its first
// refresh back by offset.
func (s *Scheduler) loop(ctx context.Context, e entry, offset time.Duration) {
	if offset > 0 {
		timer := time.NewTimer(offset)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}

	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()

	// Refresh straight away so /metrics is useful before the first tick.
	s.refresh(ctx, e.collector, e.timeout)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.refresh(ctx, e.collector, e.timeout)
		}
	}
}

// refresh runs one bounded refresh and records how it went. A failure leaves
// the collector's snapshot untouched, so the metrics go stale rather than
// disappearing — a gap in a graph reads as an outage of DigitalOcean itself.
//
// runCtx is the scheduler's own context, not the one the refresh runs under:
// telling "the API is unreachable" from "we are shutting down" needs both.
//
// The refresh runs under a context naming the collector, which is what lets the
// API client attribute a request to whoever asked for it. The scheduler is the
// only place that knows: by the time a request reaches the transport, nothing
// about it says which collector built it.
func (s *Scheduler) refresh(runCtx context.Context, c Collector, timeout time.Duration) {
	name := c.Name()
	ctx, cancel := context.WithTimeout(doclient.WithCollector(runCtx, name), timeout)
	defer cancel()

	start := time.Now()
	err := guardedRefresh(ctx, c)
	s.duration.WithLabelValues(name).Set(time.Since(start).Seconds())

	if err != nil {
		var panicked *panicError
		switch {
		case errors.As(err, &panicked):
			// A bug, and one to record whatever else is going on: the
			// collector is down until someone fixes it.
			s.logger.Error("collector refresh panicked", "collector", name,
				"panic", fmt.Sprint(panicked.value), "stack", string(panicked.stack))
		case runCtx.Err() != nil:
			// The refresh was cut short by shutdown, not by DigitalOcean.
			// Recording it as a failure would leave the last lines an
			// operator reads after a restart looking like an API outage.
			s.logger.Debug("collector refresh interrupted by shutdown",
				"collector", name, "error", err)
			return
		default:
			s.logger.Error("collector refresh failed", "collector", name, "error", err)
		}
		s.success.WithLabelValues(name).Set(0)
		return
	}

	s.success.WithLabelValues(name).Set(1)
	s.lastOK.WithLabelValues(name).Set(float64(time.Now().Unix()))
	s.markReady(name)
}

// guardedRefresh calls the collector's Refresh and turns a panic into an error.
//
// Seventeen collectors share one process, and any of them can meet an API
// response shaped in a way it did not expect. Without this, one nil field in
// one response takes the exporter down — and takes it down again after every
// restart, since the response has not changed. Recovered, the collector is
// simply a failed one: its previous snapshot survives, its next refresh still
// happens, and the other fifteen never notice.
func guardedRefresh(ctx context.Context, c Collector) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &panicError{value: r, stack: debug.Stack()}
		}
	}()
	return c.Refresh(ctx)
}

// panicError carries what a collector's Refresh panicked with, and the stack it
// panicked on. It is a distinct type so that a panic is still recorded when it
// happens during shutdown, where an ordinary error is not.
type panicError struct {
	value any
	stack []byte
}

// Error implements error.
func (e *panicError) Error() string { return fmt.Sprintf("panic: %v", e.value) }

// Describe implements prometheus.Collector.
func (s *Scheduler) Describe(ch chan<- *prometheus.Desc) {
	for _, e := range s.entries {
		e.collector.Describe(ch)
	}
}

// Collect implements prometheus.Collector.
func (s *Scheduler) Collect(ch chan<- prometheus.Metric) {
	for _, e := range s.entries {
		e.collector.Collect(ch)
	}
}
