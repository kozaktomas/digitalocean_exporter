package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/panbotka/digitalocean_exporter/internal/config"
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
