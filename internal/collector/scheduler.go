package collector

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
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
	var wg sync.WaitGroup
	for index, e := range s.entries {
		offset := stagger(index, len(s.entries), e.interval)
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.loop(ctx, e, offset)
		}()
	}
	wg.Wait()
}

// stagger returns how long the collector at index waits before its first
// refresh: an even share of the window, which is the collector's own interval
// or maxStagger, whichever is shorter. Deriving it from the registration order
// rather than from the clock or a hash of the name makes it the same on every
// run, and gives two collectors the same offset only if there is one of them.
func stagger(index, count int, interval time.Duration) time.Duration {
	if index <= 0 || count <= 1 {
		return 0
	}
	window := min(interval, maxStagger)
	return time.Duration(int64(window) / int64(count) * int64(index))
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
func (s *Scheduler) refresh(ctx context.Context, c Collector, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	name := c.Name()
	start := time.Now()
	err := c.Refresh(ctx)
	s.duration.WithLabelValues(name).Set(time.Since(start).Seconds())

	if err != nil {
		s.success.WithLabelValues(name).Set(0)
		s.logger.Error("collector refresh failed", "collector", name, "error", err)
		return
	}

	s.success.WithLabelValues(name).Set(1)
	s.lastOK.WithLabelValues(name).Set(float64(time.Now().Unix()))
}

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
