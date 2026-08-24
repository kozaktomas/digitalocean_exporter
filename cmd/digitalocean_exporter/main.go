// Command digitalocean_exporter exports DigitalOcean metrics for Prometheus.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/promslog"
	"github.com/prometheus/exporter-toolkit/web"

	"github.com/kozaktomas/digitalocean_exporter/internal/collector"
	"github.com/kozaktomas/digitalocean_exporter/internal/collector/account"
	"github.com/kozaktomas/digitalocean_exporter/internal/collector/balance"
	"github.com/kozaktomas/digitalocean_exporter/internal/config"
	"github.com/kozaktomas/digitalocean_exporter/internal/doclient"
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

	apiMetrics := doclient.NewMetrics(reg)
	userAgent := "digitalocean_exporter/" + version.Version

	// DO_API_BASE_URL points the client at a stub API. It exists so the smoke
	// test can run offline; in production it is unset and the public endpoint
	// is used.
	client, err := doclient.New(cfg.Token, os.Getenv("DO_API_BASE_URL"), userAgent, cfg.Timeout, apiMetrics)
	if err != nil {
		return fmt.Errorf("build API client: %w", err)
	}

	scheduler := collector.NewScheduler(cfg.Timeout, logger, reg)
	if cfg.Collectors["account"].Enabled {
		scheduler.Register(account.New(client), cfg.Collectors["account"].Interval)
	}
	if cfg.Collectors["balance"].Enabled {
		scheduler.Register(balance.New(client), cfg.Collectors["balance"].Interval)
	}
	reg.MustRegister(scheduler)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go scheduler.Run(ctx)

	return serve(ctx, cfg, reg, logger)
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

// serve runs the metrics HTTP server until ctx is cancelled.
func serve(ctx context.Context, cfg *config.Config, reg *prometheus.Registry, logger *slog.Logger) error {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	landing, err := web.NewLandingPage(web.LandingConfig{
		Name:        "DigitalOcean Exporter",
		Description: "Prometheus exporter for DigitalOcean.",
		Version:     version.Version,
		Links:       []web.LandingLinks{{Address: "/metrics", Text: "Metrics"}},
	})
	if err != nil {
		return fmt.Errorf("build landing page: %w", err)
	}
	mux.Handle("/", landing)

	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}

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
