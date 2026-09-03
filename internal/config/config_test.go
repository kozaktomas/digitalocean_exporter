package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kozaktomas/digitalocean_exporter/internal/config"
)

func TestParseDefaults(t *testing.T) {
	cfg, err := config.Parse([]string{"--do.token", "secret"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ListenAddress != ":9212" {
		t.Errorf("listen address = %q, want :9212", cfg.ListenAddress)
	}
	if cfg.Timeout != 30*time.Second {
		t.Errorf("timeout = %v, want 30s", cfg.Timeout)
	}
	// Four a second is 240 a minute, just inside DigitalOcean's burst limit.
	if cfg.RateLimit != 4 {
		t.Errorf("rate limit = %v, want 4 requests per second", cfg.RateLimit)
	}
	account, ok := cfg.Collectors["account"]
	if !ok {
		t.Fatal("account collector missing from config")
	}
	if !account.Enabled || account.Interval != 5*time.Minute {
		t.Errorf("account = %+v, want enabled with a 5m interval", account)
	}
}

func TestParseFlagBeatsEnv(t *testing.T) {
	t.Setenv("WEB_LISTEN_ADDRESS", ":1111")
	cfg, err := config.Parse([]string{"--do.token", "secret", "--web.listen-address", ":2222"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ListenAddress != ":2222" {
		t.Errorf("listen address = %q, want the flag value :2222", cfg.ListenAddress)
	}
}

func TestParseReadsEnv(t *testing.T) {
	t.Setenv("DIGITALOCEAN_TOKEN", "from-env")
	cfg, err := config.Parse(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Token != "from-env" {
		t.Errorf("token = %q, want from-env", cfg.Token)
	}
}

func TestParseTokenFileIsTrimmed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("  file-token\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	cfg, err := config.Parse([]string{"--do.token-file", path})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Token != "file-token" {
		t.Errorf("token = %q, want the trimmed file contents", cfg.Token)
	}
}

func TestParseRejectsBothTokenSources(t *testing.T) {
	_, err := config.Parse([]string{"--do.token", "a", "--do.token-file", "/tmp/b"})
	if !errors.Is(err, config.ErrTokenConflict) {
		t.Fatalf("error = %v, want ErrTokenConflict", err)
	}
}

func TestParseRequiresAToken(t *testing.T) {
	t.Setenv("DIGITALOCEAN_TOKEN", "")
	_, err := config.Parse(nil)
	if !errors.Is(err, config.ErrNoToken) {
		t.Fatalf("error = %v, want ErrNoToken", err)
	}
}

func TestParseHelpIsReportedNotTreatedAsMissingToken(t *testing.T) {
	_, err := config.Parse([]string{"--help"})
	if !errors.Is(err, config.ErrHelpShown) {
		t.Fatalf("error = %v, want ErrHelpShown", err)
	}
}

func TestParseVersionIsReportedNotTreatedAsMissingToken(t *testing.T) {
	// --version must not fall through to token validation and exit 1, the same
	// trap --help would hit.
	t.Setenv("DIGITALOCEAN_TOKEN", "")
	_, err := config.Parse([]string{"--version"})
	if !errors.Is(err, config.ErrHelpShown) {
		t.Fatalf("error = %v, want ErrHelpShown", err)
	}
}

func TestParseRegistersTheBalanceCollector(t *testing.T) {
	cfg, err := config.Parse([]string{"--do.token", "secret"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	balance, ok := cfg.Collectors["balance"]
	if !ok {
		t.Fatal("balance collector missing from config")
	}
	if !balance.Enabled || balance.Interval != 5*time.Minute {
		t.Errorf("balance = %+v, want enabled with a 5m interval", balance)
	}
}

// A token without the billing scope is forbidden from reading the balance, so
// operators must be able to switch that collector off on its own.
func TestParseDisablesTheBalanceCollectorAlone(t *testing.T) {
	// kingpin negates a boolean flag with --no-<name>; --collector.balance=false
	// is a parse error, which is why the Helm chart renders the negated form.
	cfg, err := config.Parse([]string{"--do.token", "secret", "--no-collector.balance"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Collectors["balance"].Enabled {
		t.Error("balance collector = enabled, want disabled")
	}
	if !cfg.Collectors["account"].Enabled {
		t.Error("account collector = disabled, want it untouched")
	}
}

func TestParseSpacesDefaults(t *testing.T) {
	cfg, err := config.Parse([]string{"--do.token", "secret"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	spaces, ok := cfg.Collectors["spaces"]
	if !ok {
		t.Fatal("spaces collector missing from config")
	}
	// It needs a Spaces key pair, a credential the other collectors do not
	// take, so it stays off until asked for.
	if spaces.Enabled {
		t.Error("spaces collector = enabled by default, want disabled")
	}
	if spaces.Interval != 5*time.Minute {
		t.Errorf("spaces interval = %v, want 5m", spaces.Interval)
	}
	if spaces.Timeout != 2*time.Minute {
		t.Errorf("spaces timeout = %v, want 2m", spaces.Timeout)
	}
	if cfg.Spaces.Concurrency != 4 {
		t.Errorf("spaces concurrency = %d, want 4", cfg.Spaces.Concurrency)
	}
}

func TestParseSpacesNeedsCredentials(t *testing.T) {
	_, err := config.Parse([]string{"--do.token", "t", "--collector.spaces", "--spaces.region", "fra1"})
	if !errors.Is(err, config.ErrNoSpacesCredentials) {
		t.Fatalf("error = %v, want ErrNoSpacesCredentials", err)
	}
}

func TestParseSpacesBuckets(t *testing.T) {
	cfg, err := config.Parse([]string{
		"--do.token", "t", "--collector.spaces",
		"--spaces.access-key", "k", "--spaces.secret-key", "s",
		"--spaces.region", "fra1",
		"--collector.spaces.bucket", "images@ams3",
		"--collector.spaces.bucket", "logs",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []config.SpacesBucket{
		{Name: "images", Region: "ams3"},
		{Name: "logs", Region: "fra1"},
	}
	if len(cfg.Spaces.Buckets) != len(want) {
		t.Fatalf("buckets = %+v, want %+v", cfg.Spaces.Buckets, want)
	}
	for i, b := range cfg.Spaces.Buckets {
		if b != want[i] {
			t.Errorf("bucket %d = %+v, want %+v", i, b, want[i])
		}
	}
}

// A bucket with no region has no endpoint to talk to. Saying so at startup
// beats a refresh that fails six hours later.
func TestParseSpacesBucketWithoutRegionFails(t *testing.T) {
	_, err := config.Parse([]string{
		"--do.token", "t", "--collector.spaces",
		"--spaces.access-key", "k", "--spaces.secret-key", "s",
		"--collector.spaces.bucket", "logs",
	})
	if !errors.Is(err, config.ErrNoSpacesRegion) {
		t.Fatalf("error = %v, want ErrNoSpacesRegion", err)
	}
}

func TestParseSpacesDiscoveryNeedsARegion(t *testing.T) {
	_, err := config.Parse([]string{
		"--do.token", "t", "--collector.spaces",
		"--spaces.access-key", "k", "--spaces.secret-key", "s",
	})
	if !errors.Is(err, config.ErrNoSpacesRegion) {
		t.Fatalf("error = %v, want ErrNoSpacesRegion", err)
	}
}

func TestParseSpacesSecretFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte("  file-secret\n"), 0o600); err != nil {
		t.Fatalf("write secret file: %v", err)
	}
	cfg, err := config.Parse([]string{
		"--do.token", "t", "--collector.spaces",
		"--spaces.access-key", "k", "--spaces.secret-key-file", path,
		"--spaces.region", "fra1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Spaces.SecretKey != "file-secret" {
		t.Errorf("secret = %q, want the trimmed file contents", cfg.Spaces.SecretKey)
	}
}

func TestParseSpacesRejectsBothSecretSources(t *testing.T) {
	_, err := config.Parse([]string{
		"--do.token", "t", "--collector.spaces",
		"--spaces.access-key", "k", "--spaces.secret-key", "s", "--spaces.secret-key-file", "/tmp/x",
		"--spaces.region", "fra1",
	})
	if !errors.Is(err, config.ErrSpacesCredentialConflict) {
		t.Fatalf("error = %v, want ErrSpacesCredentialConflict", err)
	}
}

func TestParseRegistersTheRegistryCollector(t *testing.T) {
	cfg, err := config.Parse([]string{"--do.token", "secret"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	registry, ok := cfg.Collectors["registry"]
	if !ok {
		t.Fatal("registry collector missing from config")
	}
	if !registry.Enabled || registry.Interval != 5*time.Minute {
		t.Errorf("registry = %+v, want enabled with a 5m interval", registry)
	}
}

// An account with no container registry has nothing to collect, so the
// collector must be switchable on its own, and its interval must be settable
// for a registry whose storage figure DigitalOcean updates rarely.
func TestParseConfiguresTheRegistryCollectorAlone(t *testing.T) {
	cfg, err := config.Parse([]string{"--do.token", "secret", "--no-collector.registry"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Collectors["registry"].Enabled {
		t.Error("registry collector = enabled, want disabled")
	}
	if !cfg.Collectors["account"].Enabled {
		t.Error("account collector = disabled, want it untouched")
	}

	cfg, err = config.Parse([]string{"--do.token", "secret", "--collector.registry.interval", "30m"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.Collectors["registry"].Interval; got != 30*time.Minute {
		t.Errorf("registry interval = %v, want 30m", got)
	}
}

func TestParseRegistersTheLimitsCollector(t *testing.T) {
	cfg, err := config.Parse([]string{"--do.token", "secret"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	l, ok := cfg.Collectors["limits"]
	if !ok {
		t.Fatal("limits collector missing from config")
	}
	if !l.Enabled || l.Interval != 5*time.Minute {
		t.Errorf("limits = %+v, want enabled with a 5m interval", l)
	}

	cfg, err = config.Parse([]string{"--do.token", "secret", "--no-collector.limits"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Collectors["limits"].Enabled {
		t.Error("limits collector = enabled, want disabled")
	}
}

func TestParseRegistersTheDropletsCollector(t *testing.T) {
	cfg, err := config.Parse([]string{"--do.token", "secret"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	d, ok := cfg.Collectors["droplets"]
	if !ok {
		t.Fatal("droplets collector missing from config")
	}
	if !d.Enabled || d.Interval != 5*time.Minute {
		t.Errorf("droplets = %+v, want enabled with a 5m interval", d)
	}

	cfg, err = config.Parse([]string{"--do.token", "secret", "--no-collector.droplets"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Collectors["droplets"].Enabled {
		t.Error("droplets collector = enabled, want disabled")
	}
}

func TestParseRegistersTheDatabasesCollector(t *testing.T) {
	cfg, err := config.Parse([]string{"--do.token", "secret"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	d, ok := cfg.Collectors["databases"]
	if !ok {
		t.Fatal("databases collector missing from config")
	}
	if !d.Enabled || d.Interval != 5*time.Minute || d.Timeout != 2*time.Minute {
		t.Errorf("databases = %+v, want enabled with a 5m interval and a 2m timeout", d)
	}

	cfg, err = config.Parse([]string{"--do.token", "secret", "--no-collector.databases"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Collectors["databases"].Enabled {
		t.Error("databases collector = enabled, want disabled")
	}
}

func TestParseRegistersTheKubernetesCollector(t *testing.T) {
	cfg, err := config.Parse([]string{"--do.token", "secret"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	k, ok := cfg.Collectors["kubernetes"]
	if !ok {
		t.Fatal("kubernetes collector missing from config")
	}
	if !k.Enabled || k.Interval != 5*time.Minute {
		t.Errorf("kubernetes = %+v, want enabled with a 5m interval", k)
	}

	cfg, err = config.Parse([]string{"--do.token", "secret", "--no-collector.kubernetes"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Collectors["kubernetes"].Enabled {
		t.Error("kubernetes collector = enabled, want disabled")
	}
}

// The upgrades lookup is the only part of the Kubernetes collector that costs a
// request per cluster rather than one per refresh, so it has a switch of its
// own. It is on because an account has a handful of clusters, not hundreds.
func TestKubernetesUpgradesDefaultOnAndCanBeDisabled(t *testing.T) {
	cfg, err := config.Parse([]string{"--do.token", "secret"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.KubernetesUpgrades {
		t.Error("KubernetesUpgrades = false by default, want true")
	}

	cfg, err = config.Parse([]string{"--do.token", "secret", "--no-collector.kubernetes.upgrades"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.KubernetesUpgrades {
		t.Error("KubernetesUpgrades = true with the flag negated, want false")
	}
}

// The detail lookups are the only part of the databases collector that costs
// requests per cluster rather than per refresh, so they have a switch of their
// own. They are on because an account has a handful of clusters, not hundreds.
func TestDatabaseDetailsDefaultOnAndCanBeDisabled(t *testing.T) {
	cfg, err := config.Parse([]string{"--do.token", "secret"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.DatabaseDetails {
		t.Error("DatabaseDetails = false by default, want true")
	}

	cfg, err = config.Parse([]string{"--do.token", "secret", "--no-collector.databases.details"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.DatabaseDetails {
		t.Error("DatabaseDetails = true with the flag negated, want false")
	}
}

func TestVolumesCollectorDefaultsOnAndCanBeDisabled(t *testing.T) {
	cfg, err := config.Parse([]string{"--do.token", "secret"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	v, ok := cfg.Collectors["volumes"]
	if !ok {
		t.Fatal("volumes collector missing from config")
	}
	if !v.Enabled || v.Interval != 5*time.Minute {
		t.Errorf("volumes = %+v, want enabled with a 5m interval", v)
	}

	cfg, err = config.Parse([]string{"--do.token", "secret", "--no-collector.volumes"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Collectors["volumes"].Enabled {
		t.Error("volumes collector = enabled, want disabled")
	}
}

// The images collector is the one that does not take the shared five-minute
// default: a snapshot list changes when somebody takes a snapshot, which is
// hours apart, so it refreshes every ten minutes instead.
func TestImagesCollectorDefaultsOnWithATenMinuteInterval(t *testing.T) {
	cfg, err := config.Parse([]string{"--do.token", "secret"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	i, ok := cfg.Collectors["images"]
	if !ok {
		t.Fatal("images collector missing from config")
	}
	if !i.Enabled || i.Interval != 10*time.Minute {
		t.Errorf("images = %+v, want enabled with a 10m interval", i)
	}

	cfg, err = config.Parse([]string{"--do.token", "secret", "--no-collector.images"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Collectors["images"].Enabled {
		t.Error("images collector = enabled, want disabled")
	}

	cfg, err = config.Parse([]string{"--do.token", "secret", "--collector.images.interval", "1h"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.Collectors["images"].Interval; got != time.Hour {
		t.Errorf("images interval = %s, want 1h", got)
	}
}

func TestLoadBalancersCollectorDefaultsOnAndCanBeDisabled(t *testing.T) {
	cfg, err := config.Parse([]string{"--do.token", "secret"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	lb, ok := cfg.Collectors["loadbalancers"]
	if !ok {
		t.Fatal("loadbalancers collector missing from config")
	}
	if !lb.Enabled || lb.Interval != 5*time.Minute {
		t.Errorf("loadbalancers = %+v, want enabled with a 5m interval", lb)
	}

	cfg, err = config.Parse([]string{"--do.token", "secret", "--no-collector.loadbalancers"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Collectors["loadbalancers"].Enabled {
		t.Error("loadbalancers collector = enabled, want disabled")
	}
}

func TestCDNCollectorDefaultsOnAndCanBeDisabled(t *testing.T) {
	cfg, err := config.Parse([]string{"--do.token", "secret"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	c, ok := cfg.Collectors["cdn"]
	if !ok {
		t.Fatal("cdn collector missing from config")
	}
	if !c.Enabled || c.Interval != 5*time.Minute {
		t.Errorf("cdn = %+v, want enabled with a 5m interval", c)
	}

	cfg, err = config.Parse([]string{"--do.token", "secret", "--no-collector.cdn"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Collectors["cdn"].Enabled {
		t.Error("cdn collector = enabled, want disabled")
	}
}

func TestAppsCollectorDefaultsOnAndCanBeDisabled(t *testing.T) {
	cfg, err := config.Parse([]string{"--do.token", "secret"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	c, ok := cfg.Collectors["apps"]
	if !ok {
		t.Fatal("apps collector missing from config")
	}
	if !c.Enabled || c.Interval != 5*time.Minute {
		t.Errorf("apps = %+v, want enabled with a 5m interval", c)
	}

	cfg, err = config.Parse([]string{"--do.token", "secret", "--no-collector.apps"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Collectors["apps"].Enabled {
		t.Error("apps collector = enabled, want disabled")
	}
}

func TestDomainsCollectorDefaultsOnAndCanBeDisabled(t *testing.T) {
	cfg, err := config.Parse([]string{"--do.token", "secret"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	c, ok := cfg.Collectors["domains"]
	if !ok {
		t.Fatal("domains collector missing from config")
	}
	if !c.Enabled || c.Interval != 5*time.Minute {
		t.Errorf("domains = %+v, want enabled with a 5m interval", c)
	}

	cfg, err = config.Parse([]string{"--do.token", "secret", "--no-collector.domains"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Collectors["domains"].Enabled {
		t.Error("domains collector = enabled, want disabled")
	}
}

// Firewalls and certificates cost one list request each, the same as the
// inventory collectors that default on. They stay off because what they report
// changes on human timescales, not because of what they cost.
func TestSlowMovingCollectorsDefaultOffAndCanBeEnabled(t *testing.T) {
	cfg, err := config.Parse([]string{"--do.token", "secret"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, name := range []string{"firewalls", "certificates"} {
		c, ok := cfg.Collectors[name]
		if !ok {
			t.Fatalf("%s collector missing from config", name)
		}
		if c.Enabled {
			t.Errorf("%s collector = enabled by default, want disabled", name)
		}
		if c.Interval != 5*time.Minute {
			t.Errorf("%s interval = %v, want 5m", name, c.Interval)
		}
	}

	cfg, err = config.Parse([]string{
		"--do.token", "secret",
		"--collector.firewalls", "--collector.firewalls.interval", "15m",
		"--collector.certificates", "--collector.certificates.interval", "1h",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fw := cfg.Collectors["firewalls"]; !fw.Enabled || fw.Interval != 15*time.Minute {
		t.Errorf("firewalls = %+v, want enabled with a 15m interval", fw)
	}
	if cert := cfg.Collectors["certificates"]; !cert.Enabled || cert.Interval != time.Hour {
		t.Errorf("certificates = %+v, want enabled with a 1h interval", cert)
	}
}

// The droplet metrics collector costs API requests in proportion to the size
// of the account, so it must stay off until it is asked for.
func TestDropletMetricsCollectorDefaultsOff(t *testing.T) {
	cfg, err := config.Parse([]string{"--do.token", "secret"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	d, ok := cfg.Collectors["dropletmetrics"]
	if !ok {
		t.Fatal("dropletmetrics collector missing from config")
	}
	if d.Enabled {
		t.Error("dropletmetrics collector = enabled by default, want disabled")
	}
	if d.Interval != 5*time.Minute || d.Timeout != 2*time.Minute {
		t.Errorf("dropletmetrics = %+v, want a 5m interval and a 2m timeout", d)
	}
	if cfg.DropletMetricsConcurrency != 4 {
		t.Errorf("DropletMetricsConcurrency = %d, want 4", cfg.DropletMetricsConcurrency)
	}

	cfg, err = config.Parse([]string{"--do.token", "secret", "--collector.dropletmetrics",
		"--collector.dropletmetrics.concurrency", "8"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.Collectors["dropletmetrics"].Enabled {
		t.Error("dropletmetrics collector = disabled, want enabled")
	}
	if cfg.DropletMetricsConcurrency != 8 {
		t.Errorf("DropletMetricsConcurrency = %d, want 8", cfg.DropletMetricsConcurrency)
	}
}

// Skipping droplets that do not report the monitoring agent saves ten requests
// each, but the feature is only set on droplets created with the agent, so a
// droplet with one installed later would go unmeasured. That makes it opt-in.
func TestDropletMetricsAgentOnlyDefaultsOff(t *testing.T) {
	cfg, err := config.Parse([]string{"--do.token", "secret"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.DropletMetricsAgentOnly {
		t.Error("DropletMetricsAgentOnly = true by default, want false")
	}

	cfg, err = config.Parse([]string{"--do.token", "secret", "--collector.dropletmetrics",
		"--collector.dropletmetrics.agent-only"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.DropletMetricsAgentOnly {
		t.Error("DropletMetricsAgentOnly = false with the flag given, want true")
	}
}

// The load balancer metrics collector also costs API requests per resource, so
// it is off until it is asked for.
func TestLoadBalancerMetricsCollectorDefaultsOff(t *testing.T) {
	cfg, err := config.Parse([]string{"--do.token", "secret"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	lb, ok := cfg.Collectors["loadbalancermetrics"]
	if !ok {
		t.Fatal("loadbalancermetrics collector missing from config")
	}
	if lb.Enabled {
		t.Error("loadbalancermetrics collector = enabled by default, want disabled")
	}
	if lb.Interval != 5*time.Minute || lb.Timeout != 2*time.Minute {
		t.Errorf("loadbalancermetrics = %+v, want a 5m interval and a 2m timeout", lb)
	}
	if cfg.LoadBalancerMetricsConcurrency != 4 {
		t.Errorf("LoadBalancerMetricsConcurrency = %d, want 4", cfg.LoadBalancerMetricsConcurrency)
	}

	cfg, err = config.Parse([]string{"--do.token", "secret", "--collector.loadbalancermetrics"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.Collectors["loadbalancermetrics"].Enabled {
		t.Error("loadbalancermetrics collector = disabled, want enabled")
	}
}

// A zero interval reaches time.NewTicker, which panics — in a goroutine started
// after the metrics server has already bound its port. Fail at startup instead.
func TestParseRejectsAZeroInterval(t *testing.T) {
	_, err := config.Parse([]string{"--do.token", "secret", "--collector.account.interval", "0s"})
	if !errors.Is(err, config.ErrNonPositiveInterval) {
		t.Fatalf("error = %v, want ErrNonPositiveInterval", err)
	}
}

func TestParseRejectsANegativeInterval(t *testing.T) {
	_, err := config.Parse([]string{"--do.token", "secret", "--collector.droplets.interval=-1m"})
	if !errors.Is(err, config.ErrNonPositiveInterval) {
		t.Fatalf("error = %v, want ErrNonPositiveInterval", err)
	}
}

// The collectors with a timeout of their own carry an interval too, and it is
// bound separately from the simple ones.
func TestParseRejectsAZeroIntervalOnATimeoutCarryingCollector(t *testing.T) {
	_, err := config.Parse([]string{"--do.token", "secret", "--collector.spaces.interval", "0s"})
	if !errors.Is(err, config.ErrNonPositiveInterval) {
		t.Fatalf("error = %v, want ErrNonPositiveInterval", err)
	}
}

// A zero timeout is accepted quietly today and every refresh then fails with a
// deadline exceeded, forever.
func TestParseRejectsAZeroTimeout(t *testing.T) {
	_, err := config.Parse([]string{"--do.token", "secret", "--do.timeout", "0s"})
	if !errors.Is(err, config.ErrNonPositiveTimeout) {
		t.Fatalf("error = %v, want ErrNonPositiveTimeout", err)
	}
}

func TestParseRejectsANegativeTimeout(t *testing.T) {
	_, err := config.Parse([]string{"--do.token", "secret", "--do.timeout=-30s"})
	if !errors.Is(err, config.ErrNonPositiveTimeout) {
		t.Fatalf("error = %v, want ErrNonPositiveTimeout", err)
	}
}

func TestParseRejectsAZeroCollectorTimeout(t *testing.T) {
	_, err := config.Parse([]string{"--do.token", "secret", "--collector.dropletmetrics.timeout", "0s"})
	if !errors.Is(err, config.ErrNonPositiveTimeout) {
		t.Fatalf("error = %v, want ErrNonPositiveTimeout", err)
	}
}

func TestParseRejectsANegativeCollectorTimeout(t *testing.T) {
	_, err := config.Parse([]string{
		"--do.token", "secret", "--collector.loadbalancermetrics.timeout=-2m"})
	if !errors.Is(err, config.ErrNonPositiveTimeout) {
		t.Fatalf("error = %v, want ErrNonPositiveTimeout", err)
	}
}

// The message has to say which flag was wrong: an operator reading stderr gets
// nothing from a bare "interval must be greater than zero".
func TestParseNamesTheOffendingFlagAndValue(t *testing.T) {
	_, err := config.Parse([]string{"--do.token", "secret", "--collector.account.interval", "0s"})
	if err == nil {
		t.Fatal("no error for a zero interval")
	}
	msg := err.Error()
	if !strings.Contains(msg, "--collector.account.interval") || !strings.Contains(msg, "0s") {
		t.Errorf("error = %q, want it to name the flag and the value", msg)
	}
}

// A disabled collector's interval is validated too: it is one --collector.<name>
// away from being switched on, and a chart value sets both independently.
func TestParseRejectsAZeroIntervalOnADisabledCollector(t *testing.T) {
	_, err := config.Parse([]string{
		"--do.token", "secret", "--no-collector.account", "--collector.account.interval", "0s"})
	if !errors.Is(err, config.ErrNonPositiveInterval) {
		t.Fatalf("error = %v, want ErrNonPositiveInterval", err)
	}
}

// Zero is how an operator turns the client-side limit off, which is what the
// smoke run does against a stub API that has no limits of its own.
func TestParseRateLimitCanBeSet(t *testing.T) {
	cfg, err := config.Parse([]string{"--do.token", "secret", "--do.rate-limit", "0"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.RateLimit != 0 {
		t.Errorf("rate limit = %v, want 0", cfg.RateLimit)
	}

	t.Setenv("DO_RATE_LIMIT", "2.5")
	cfg, err = config.Parse([]string{"--do.token", "secret"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.RateLimit != 2.5 {
		t.Errorf("rate limit from the environment = %v, want 2.5", cfg.RateLimit)
	}
}

// The API base URL used to be read straight from the environment, outside the
// flag parser, which made an endpoint that redirects every request the one
// setting the exporter could not report on. It is a flag now — hidden, because
// it exists for development and the smoke test rather than for operators.
func TestParseAPIBaseURL(t *testing.T) {
	cfg, err := config.Parse([]string{"--do.token", "secret"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.APIBaseURL != "" {
		t.Errorf("default API base URL = %q, want the public API", cfg.APIBaseURL)
	}

	t.Setenv("DO_API_BASE_URL", "http://127.0.0.1:19213/")
	cfg, err = config.Parse([]string{"--do.token", "secret"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.APIBaseURL != "http://127.0.0.1:19213/" {
		t.Errorf("API base URL from the environment = %q", cfg.APIBaseURL)
	}

	cfg, err = config.Parse([]string{"--do.token", "secret", "--do.api-base-url", "http://stub/"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.APIBaseURL != "http://stub/" {
		t.Errorf("API base URL from the flag = %q", cfg.APIBaseURL)
	}
}

// COLLECTOR_SPACES_BUCKET is the environment form of a repeatable flag, and
// kingpin splits such a value on newlines alone. A comma-separated list is the
// obvious thing to write there, and it used to parse as one bucket with a
// comma in its name that reported itself down hours later.
func TestParseSpacesBucketsSeparatedByCommas(t *testing.T) {
	cfg, err := config.Parse([]string{
		"--do.token", "t", "--collector.spaces",
		"--spaces.access-key", "k", "--spaces.secret-key", "s",
		"--spaces.region", "fra1",
		"--collector.spaces.bucket", "images@ams3, logs",
		"--collector.spaces.bucket", "backups",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []config.SpacesBucket{
		{Name: "images", Region: "ams3"},
		{Name: "logs", Region: "fra1"},
		{Name: "backups", Region: "fra1"},
	}
	if len(cfg.Spaces.Buckets) != len(want) {
		t.Fatalf("buckets = %+v, want %+v", cfg.Spaces.Buckets, want)
	}
	for i, b := range cfg.Spaces.Buckets {
		if b != want[i] {
			t.Errorf("bucket %d = %+v, want %+v", i, b, want[i])
		}
	}
}
