package config_test

import (
	"errors"
	"os"
	"path/filepath"
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
	// It needs credentials the other collectors do not and its refresh costs
	// minutes, so it stays off until asked for.
	if spaces.Enabled {
		t.Error("spaces collector = enabled by default, want disabled")
	}
	if spaces.Interval != 6*time.Hour {
		t.Errorf("spaces interval = %v, want 6h", spaces.Interval)
	}
	if spaces.Timeout != 15*time.Minute {
		t.Errorf("spaces timeout = %v, want 15m", spaces.Timeout)
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
	if !d.Enabled || d.Interval != 5*time.Minute {
		t.Errorf("databases = %+v, want enabled with a 5m interval", d)
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
