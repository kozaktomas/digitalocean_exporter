// Command digitalocean_exporter exports DigitalOcean metrics for Prometheus.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/digitalocean/godo"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/promslog"
	"github.com/prometheus/exporter-toolkit/web"

	"github.com/kozaktomas/digitalocean_exporter/internal/collector"
	"github.com/kozaktomas/digitalocean_exporter/internal/collector/account"
	"github.com/kozaktomas/digitalocean_exporter/internal/collector/apps"
	"github.com/kozaktomas/digitalocean_exporter/internal/collector/balance"
	"github.com/kozaktomas/digitalocean_exporter/internal/collector/cdn"
	"github.com/kozaktomas/digitalocean_exporter/internal/collector/certificates"
	"github.com/kozaktomas/digitalocean_exporter/internal/collector/databases"
	"github.com/kozaktomas/digitalocean_exporter/internal/collector/domains"
	"github.com/kozaktomas/digitalocean_exporter/internal/collector/dropletautoscale"
	"github.com/kozaktomas/digitalocean_exporter/internal/collector/dropletmetrics"
	"github.com/kozaktomas/digitalocean_exporter/internal/collector/droplets"
	"github.com/kozaktomas/digitalocean_exporter/internal/collector/firewalls"
	"github.com/kozaktomas/digitalocean_exporter/internal/collector/images"
	"github.com/kozaktomas/digitalocean_exporter/internal/collector/kubernetes"
	"github.com/kozaktomas/digitalocean_exporter/internal/collector/limits"
	"github.com/kozaktomas/digitalocean_exporter/internal/collector/loadbalancermetrics"
	"github.com/kozaktomas/digitalocean_exporter/internal/collector/loadbalancers"
	"github.com/kozaktomas/digitalocean_exporter/internal/collector/projects"
	"github.com/kozaktomas/digitalocean_exporter/internal/collector/registry"
	"github.com/kozaktomas/digitalocean_exporter/internal/collector/reservedips"
	"github.com/kozaktomas/digitalocean_exporter/internal/collector/spaces"
	"github.com/kozaktomas/digitalocean_exporter/internal/collector/tags"
	"github.com/kozaktomas/digitalocean_exporter/internal/collector/volumes"
	"github.com/kozaktomas/digitalocean_exporter/internal/config"
	"github.com/kozaktomas/digitalocean_exporter/internal/doclient"
	"github.com/kozaktomas/digitalocean_exporter/internal/filter"
	"github.com/kozaktomas/digitalocean_exporter/internal/spacesclient"
	"github.com/kozaktomas/digitalocean_exporter/internal/version"
)

func main() {
	err := run(os.Args[1:])
	switch {
	case err == nil:
		return
	case errors.Is(err, config.ErrHelpShown):
		// Usage has already been printed; asking for help is not a failure.
		return
	default:
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// run wires the exporter together and serves until the process is signalled.
func run(args []string) error {
	cfg, err := config.Parse(args)
	if err != nil {
		return err
	}

	logger := newLogger(cfg)
	logger.Info("starting digitalocean_exporter",
		"version", version.Version, "commit", version.Commit, "address", cfg.ListenAddress)

	reg := prometheus.NewRegistry()
	reg.MustRegister(
		version.NewCollector(),
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	// A redirected API endpoint exists so the smoke test can run offline, and
	// in production it is unset. Say so when it is not: every metric then
	// describes something other than DigitalOcean, and nothing else in the
	// exporter's output gives that away.
	if cfg.APIBaseURL != "" {
		logger.Info("using a non-default DigitalOcean API base URL", "url", cfg.APIBaseURL)
	}

	apiMetrics := doclient.NewMetrics(reg)
	client, err := doclient.New(doclient.Config{
		Token:     cfg.Token,
		BaseURL:   cfg.APIBaseURL,
		UserAgent: "digitalocean_exporter/" + version.Version,
		Timeout:   cfg.Timeout,
		RateLimit: cfg.RateLimit,
		Metrics:   apiMetrics,
	})
	if err != nil {
		return fmt.Errorf("build API client: %w", err)
	}

	scheduler := collector.NewScheduler(cfg.Timeout, logger, reg)
	registerCollectors(scheduler, cfg, client, logger)
	reg.MustRegister(scheduler)

	handler, err := newHandler(reg, scheduler, logger)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go scheduler.Run(ctx)

	return serve(ctx, cfg, handler, logger)
}

// registerCollectors enables the collectors the configuration asks for.
//
// The table is ordered rather than a map so registration order, and with it the
// order collectors appear in the scheduler, stays the same between runs. A
// collector is built only when it is enabled, so a disabled one costs nothing.
func registerCollectors(
	scheduler *collector.Scheduler, cfg *config.Config, client *godo.Client, logger *slog.Logger,
) {
	// One filter shared by every resource collector that honours it, so
	// "matches the filter" means the same thing across the whole exposition.
	// The account-wide collectors never see it.
	flt := filter.New(cfg.FilterTags, cfg.FilterRegions)

	available := []struct {
		name  string
		build func() collector.Collector
	}{
		{"account", func() collector.Collector { return account.New(client) }},
		{"balance", func() collector.Collector { return balance.New(client) }},
		{"databases", func() collector.Collector {
			return databases.New(client, cfg.DatabaseDetails, flt, logger)
		}},
		{"droplets", func() collector.Collector { return droplets.New(client, flt, logger) }},
		{"dropletautoscale", func() collector.Collector { return dropletautoscale.New(client, logger) }},
		{"images", func() collector.Collector { return images.New(client, logger) }},
		{"kubernetes", func() collector.Collector {
			return kubernetes.New(client, cfg.KubernetesUpgrades, flt, logger)
		}},
		{"limits", func() collector.Collector { return limits.New(client) }},
		{"registry", func() collector.Collector { return registry.New(client, logger) }},
		{"reservedips", func() collector.Collector { return reservedips.New(client, logger) }},
		{"spaces", func() collector.Collector { return newSpaces(cfg.Spaces, logger) }},
		{"volumes", func() collector.Collector { return volumes.New(client, flt, logger) }},
		{"loadbalancers", func() collector.Collector { return loadbalancers.New(client, flt, logger) }},
		{"cdn", func() collector.Collector { return cdn.New(client, logger) }},
		{"apps", func() collector.Collector { return apps.New(client, logger) }},
		{"domains", func() collector.Collector { return domains.New(client, logger) }},
		{"firewalls", func() collector.Collector { return firewalls.New(client, flt, logger) }},
		{"certificates", func() collector.Collector { return certificates.New(client, logger) }},
		{"tags", func() collector.Collector { return tags.New(client, logger) }},
		{"projects", func() collector.Collector { return projects.New(client, logger) }},
		{"dropletmetrics", func() collector.Collector {
			return dropletmetrics.New(client, dropletmetrics.Config{
				Concurrency: cfg.DropletMetricsConcurrency,
				AgentOnly:   cfg.DropletMetricsAgentOnly,
				Filter:      flt,
				Logger:      logger,
			})
		}},
		{"loadbalancermetrics", func() collector.Collector {
			return loadbalancermetrics.New(client, cfg.LoadBalancerMetricsConcurrency, flt, logger)
		}},
	}

	for _, a := range available {
		if c := cfg.Collectors[a.name]; c.Enabled {
			scheduler.Register(a.build(), c.Interval, c.Timeout)
		}
	}
}

// newSpaces builds the Spaces collector from its own configuration.
//
// DO_SPACES_ENDPOINT points the S3 client at a stub, mirroring DO_API_BASE_URL;
// in production it is unset and the regional Spaces endpoints are used.
func newSpaces(cfg config.SpacesConfig, logger *slog.Logger) *spaces.Collector {
	buckets := make([]spaces.Bucket, 0, len(cfg.Buckets))
	for _, b := range cfg.Buckets {
		buckets = append(buckets, spaces.Bucket{Name: b.Name, Region: b.Region})
	}
	return spaces.New(spaces.Config{
		Factory:     spacesclient.NewFactory(cfg.AccessKey, cfg.SecretKey, os.Getenv("DO_SPACES_ENDPOINT")),
		Buckets:     buckets,
		Region:      cfg.Region,
		Concurrency: cfg.Concurrency,
		Logger:      logger,
	})
}

// newLogger builds the structured logger from the configuration. Both values
// were validated by kingpin's Enum, so Set cannot fail here.
func newLogger(cfg *config.Config) *slog.Logger {
	level := promslog.NewLevel()
	_ = level.Set(cfg.LogLevel)
	format := promslog.NewFormat()
	_ = format.Set(cfg.LogFormat)
	return promslog.New(&promslog.Config{Level: level, Format: format})
}

// readinessReporter names the collectors that have not yet produced a
// snapshot. The scheduler is the only implementation; the interface is here so
// the HTTP routes can be tested without one.
type readinessReporter interface {
	Pending() []string
}

// newHandler builds the exporter's HTTP routes: metrics, the two probes and
// the landing page that lists them.
func newHandler(reg *prometheus.Registry, ready readinessReporter, logger *slog.Logger) (http.Handler, error) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		Registry: reg,
		// One collector emitting a duplicate label set or a metric it never
		// described must not take the scrape down with it. The default is a
		// 500 with no body at all, which drops every other collector's metrics
		// too and reads exactly like the exporter going away — the gap that
		// looks like an outage. Continuing serves what could be gathered, logs
		// what could not, and still counts the failure in
		// promhttp_metric_handler_errors_total.
		ErrorHandling: promhttp.ContinueOnError,
		ErrorLog:      slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}))
	mux.HandleFunc("/healthz", handleHealthz)
	mux.HandleFunc("/readyz", handleReadyz(ready))

	landing, err := web.NewLandingPage(web.LandingConfig{
		Name:        "DigitalOcean Exporter",
		Description: "Prometheus exporter for DigitalOcean.",
		Version:     version.Version,
		Links: []web.LandingLinks{
			{Address: "/metrics", Text: "Metrics", Description: "The exposition the collectors feed."},
			{Address: "/healthz", Text: "Health", Description: "Liveness: the process is running."},
			{Address: "/readyz", Text: "Readiness",
				Description: "Readiness: every enabled collector has a snapshot."},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("build landing page: %w", err)
	}
	mux.Handle("/", landing)

	return mux, nil
}

// handleHealthz answers the liveness probe, and answers it unconditionally on
// purpose. Liveness asks whether the process is worth killing, and a collector
// that cannot reach DigitalOcean is not a question a restart answers: the next
// refresh happens anyway, while a restart throws away the snapshots every
// other collector does hold. That question belongs to readiness.
func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writePlain(w, http.StatusOK, "ok\n")
}

// handleReadyz answers the readiness probe: 200 once every enabled collector
// has completed one successful refresh, and 503 naming the ones still waiting
// until then. With no collectors enabled there is nothing to wait for and it
// is 200 from the start.
//
// It stays 200 afterwards even while a collector is failing, because by then
// the pod has metrics to serve and its previous values are the best available
// account of DigitalOcean. Taking it out of the Service at that point would
// stop the scrape that reports the failure.
func handleReadyz(ready readinessReporter) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		pending := ready.Pending()
		if len(pending) == 0 {
			writePlain(w, http.StatusOK, "ok\n")
			return
		}
		writePlain(w, http.StatusServiceUnavailable,
			"waiting for the first successful refresh of:\n"+strings.Join(pending, "\n")+"\n")
	}
}

// writePlain writes a plain-text body under the given status. The probes are
// read by kubelet and by whoever is curling them during an incident, so the
// body is a few lines meant for a person and the content type says as much.
func writePlain(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	// The body is assembled here from constants and the exporter's own
	// collector names, and is served as text/plain.
	_, _ = io.WriteString(w, body) //nolint:gosec // no request data reaches the response.
}

// serve runs the metrics HTTP server until ctx is cancelled.
func serve(ctx context.Context, cfg *config.Config, handler http.Handler, logger *slog.Logger) error {
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}

	go func() {
		<-ctx.Done()
		// ctx is already cancelled here, so the shutdown deadline is derived
		// from a copy with the cancellation stripped: reusing ctx directly
		// would abort the graceful shutdown instantly.
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	addresses := []string{cfg.ListenAddress}
	systemdSocket := false
	flags := &web.FlagConfig{
		WebListenAddresses: &addresses,
		WebSystemdSocket:   &systemdSocket,
		WebConfigFile:      &cfg.WebConfigFile,
	}

	if err := web.ListenAndServe(server, flags, logger); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}
