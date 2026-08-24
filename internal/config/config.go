// Package config turns command-line flags and environment variables into a
// validated exporter configuration.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/alecthomas/kingpin/v2"
)

// ErrNoToken reports that no DigitalOcean API token was configured.
var ErrNoToken = errors.New("no DigitalOcean API token: set --do.token or --do.token-file")

// ErrTokenConflict reports that both token sources were configured at once.
var ErrTokenConflict = errors.New("--do.token and --do.token-file are mutually exclusive")

// ErrHelpShown reports that usage was printed and the process should exit
// successfully. Parsing deliberately never terminates the process itself, so
// the caller decides; without this the help flag would fall through to token
// validation and exit 1 with a confusing message.
var ErrHelpShown = errors.New("help shown")

// CollectorConfig holds the switches of a single collector.
type CollectorConfig struct {
	// Enabled reports whether the collector should run at all.
	Enabled bool
	// Interval is how often the collector refreshes its snapshot.
	Interval time.Duration
}

// Config is the fully resolved exporter configuration.
type Config struct {
	// ListenAddress is the address the metrics server binds to.
	ListenAddress string
	// WebConfigFile optionally points at an exporter-toolkit web config.
	WebConfigFile string
	// Token is the resolved DigitalOcean API token.
	Token string
	// Timeout bounds a single collector refresh.
	Timeout time.Duration
	// LogLevel is the slog level name.
	LogLevel string
	// LogFormat is either logfmt or json.
	LogFormat string
	// Collectors maps a collector name to its configuration.
	Collectors map[string]CollectorConfig
}

// Parse builds a Config from args, falling back to environment variables.
// It never terminates the process: every failure is returned as an error.
func Parse(args []string) (*Config, error) {
	app := kingpin.New("digitalocean_exporter", "Prometheus exporter for DigitalOcean.")

	// Capture the exit kingpin would have made instead of killing the process,
	// so Parse stays callable from tests.
	terminated := false
	app.Terminate(func(int) { terminated = true })
	app.Writer(os.Stdout)

	listen := app.Flag("web.listen-address", "Address to expose metrics on.").
		Envar("WEB_LISTEN_ADDRESS").Default(":9212").String()
	webConfig := app.Flag("web.config.file", "Path to an exporter-toolkit web config (TLS, basic auth).").
		Envar("WEB_CONFIG_FILE").Default("").String()
	token := app.Flag("do.token", "DigitalOcean API token. A read-only token is enough.").
		Envar("DIGITALOCEAN_TOKEN").Default("").String()
	tokenFile := app.Flag("do.token-file", "File holding the DigitalOcean API token.").
		Envar("DIGITALOCEAN_TOKEN_FILE").Default("").String()
	timeout := app.Flag("do.timeout", "Timeout of a single collector refresh.").
		Envar("DO_TIMEOUT").Default("30s").Duration()
	logLevel := app.Flag("log.level", "Log level: debug, info, warn or error.").
		Envar("LOG_LEVEL").Default("info").Enum("debug", "info", "warn", "error")
	logFormat := app.Flag("log.format", "Log format: logfmt or json.").
		Envar("LOG_FORMAT").Default("logfmt").Enum("logfmt", "json")
	accountEnabled := app.Flag("collector.account", "Enable the account collector.").
		Envar("COLLECTOR_ACCOUNT").Default("true").Bool()
	accountInterval := app.Flag("collector.account.interval", "Refresh interval of the account collector.").
		Envar("COLLECTOR_ACCOUNT_INTERVAL").Default("5m").Duration()

	if _, err := app.Parse(args); err != nil {
		return nil, fmt.Errorf("parse flags: %w", err)
	}
	if terminated {
		return nil, ErrHelpShown
	}

	resolved, err := resolveToken(*token, *tokenFile)
	if err != nil {
		return nil, err
	}

	return &Config{
		ListenAddress: *listen,
		WebConfigFile: *webConfig,
		Token:         resolved,
		Timeout:       *timeout,
		LogLevel:      *logLevel,
		LogFormat:     *logFormat,
		Collectors: map[string]CollectorConfig{
			"account": {Enabled: *accountEnabled, Interval: *accountInterval},
		},
	}, nil
}

// resolveToken picks the token from whichever source was configured. Exactly
// one source may be set.
func resolveToken(token, tokenFile string) (string, error) {
	switch {
	case token != "" && tokenFile != "":
		return "", ErrTokenConflict
	case token != "":
		return token, nil
	case tokenFile != "":
		// The path is supplied by the operator running the exporter, which is
		// the whole point of the flag; there is no untrusted input here.
		raw, err := os.ReadFile(tokenFile) // #nosec G304
		if err != nil {
			return "", fmt.Errorf("read token file: %w", err)
		}
		trimmed := strings.TrimSpace(string(raw))
		if trimmed == "" {
			return "", fmt.Errorf("token file %q is empty: %w", tokenFile, ErrNoToken)
		}
		return trimmed, nil
	default:
		return "", ErrNoToken
	}
}
