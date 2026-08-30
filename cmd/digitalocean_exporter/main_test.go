package main

import (
	"io"
	"log/slog"
	"sort"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/kozaktomas/digitalocean_exporter/internal/collector"
	"github.com/kozaktomas/digitalocean_exporter/internal/config"
	"github.com/kozaktomas/digitalocean_exporter/internal/doclient"
)

// Every collector is configured in one place and registered in another, so a
// new one can be given flags and a chart value and still never run. Nothing
// else notices: the exporter starts, serves /metrics and simply lacks those
// metrics. This pins the two lists together.
func TestRegisterCollectorsCoversEveryConfiguredCollector(t *testing.T) {
	cfg, err := config.Parse([]string{
		"--do.token", "secret",
		"--collector.dropletmetrics",
		"--collector.loadbalancermetrics",
		"--collector.firewalls",
		"--collector.certificates",
		"--collector.spaces",
		"--spaces.access-key", "key",
		"--spaces.secret-key", "secret",
		"--spaces.region", "fra1",
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	for name, c := range cfg.Collectors {
		if !c.Enabled {
			t.Fatalf("collector %q is not enabled, so this test would not check it", name)
		}
	}

	reg := prometheus.NewRegistry()
	client, err := doclient.New(doclient.Config{
		Token: "secret", UserAgent: "test", Timeout: time.Second,
		Metrics: doclient.NewMetrics(reg),
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	scheduler := collector.NewScheduler(time.Second, logger, reg)
	registerCollectors(scheduler, cfg, client, logger)

	registered := scheduler.Names()
	sort.Strings(registered)

	configured := make([]string, 0, len(cfg.Collectors))
	for name := range cfg.Collectors {
		configured = append(configured, name)
	}
	sort.Strings(configured)

	if len(registered) != len(configured) {
		t.Fatalf("registered %v, configured %v", registered, configured)
	}
	for i := range configured {
		if registered[i] != configured[i] {
			t.Errorf("registered[%d] = %q, configured[%d] = %q", i, registered[i], i, configured[i])
		}
	}
}
