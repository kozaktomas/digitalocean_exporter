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

// ErrNoSpacesCredentials reports that the Spaces collector was enabled without
// the access key pair it authenticates with.
var ErrNoSpacesCredentials = errors.New(
	"no Spaces credentials: set --spaces.access-key and --spaces.secret-key")

// ErrSpacesCredentialConflict reports that a Spaces credential was given both
// inline and as a file.
var ErrSpacesCredentialConflict = errors.New(
	"a Spaces credential and its file are mutually exclusive")

// ErrNoSpacesRegion reports that a bucket, or discovery, has no region to talk
// to. Failing at startup beats a refresh that fails hours later.
var ErrNoSpacesRegion = errors.New("no Spaces region: set --spaces.region or use bucket@region")

// CollectorConfig holds the switches of a single collector.
type CollectorConfig struct {
	// Enabled reports whether the collector should run at all.
	Enabled bool
	// Interval is how often the collector refreshes its snapshot.
	Interval time.Duration
	// Timeout bounds one refresh of this collector. Zero means the exporter's
	// own timeout is used.
	Timeout time.Duration
}

// SpacesBucket names a bucket and the region it lives in.
type SpacesBucket struct {
	// Name is the bucket name.
	Name string
	// Region is the Spaces region serving the bucket.
	Region string
}

// SpacesConfig holds everything specific to the Spaces collector.
type SpacesConfig struct {
	// AccessKey and SecretKey authenticate against the S3-compatible API.
	// They are unrelated to the DigitalOcean API token.
	AccessKey string
	SecretKey string
	// Region is the region used for discovery and as the default for buckets
	// configured without one.
	Region string
	// Buckets lists the buckets to measure. Empty means discovery, which
	// needs a full-access key.
	Buckets []SpacesBucket
	// Concurrency caps how many buckets are listed at once.
	Concurrency int
}

// Config is the fully resolved exporter configuration.
type Config struct {
	// ListenAddress is the address the metrics server binds to.
	ListenAddress string
	// WebConfigFile optionally points at an exporter-toolkit web config.
	WebConfigFile string
	// Token is the resolved DigitalOcean API token.
	Token string
	// Timeout bounds a single collector refresh unless the collector sets its
	// own.
	Timeout time.Duration
	// LogLevel is the slog level name.
	LogLevel string
	// LogFormat is either logfmt or json.
	LogFormat string
	// Collectors maps a collector name to its configuration.
	Collectors map[string]CollectorConfig
	// Spaces holds the Spaces collector's own settings.
	Spaces SpacesConfig
}

// flags holds every bound flag before validation turns them into a Config.
type flags struct {
	listen             *string
	webConfig          *string
	token              *string
	tokenFile          *string
	timeout            *time.Duration
	logLevel           *string
	logFormat          *string
	account            *bool
	accountInterval    *time.Duration
	balance            *bool
	balanceInterval    *time.Duration
	registry           *bool
	registryInterval   *time.Duration
	limits             *bool
	limitsInterval     *time.Duration
	droplets           *bool
	dropletsInterval   *time.Duration
	databases          *bool
	databasesInterval  *time.Duration
	kubernetes         *bool
	kubernetesInterval *time.Duration
	spaces             *bool
	spacesInterval     *time.Duration
	spacesTimeout      *time.Duration
	spacesBuckets      *[]string
	spacesConcurrent   *int
	spacesKey          *string
	spacesKeyFile      *string
	spacesSecret       *string
	spacesSecretFile   *string
	spacesRegion       *string
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

	f := bind(app)

	if _, err := app.Parse(args); err != nil {
		return nil, fmt.Errorf("parse flags: %w", err)
	}
	if terminated {
		return nil, ErrHelpShown
	}
	return f.config()
}

// bind declares every flag on app.
func bind(app *kingpin.Application) *flags {
	f := &flags{}
	f.listen = app.Flag("web.listen-address", "Address to expose metrics on.").
		Envar("WEB_LISTEN_ADDRESS").Default(":9212").String()
	f.webConfig = app.Flag("web.config.file", "Path to an exporter-toolkit web config (TLS, basic auth).").
		Envar("WEB_CONFIG_FILE").Default("").String()
	f.token = app.Flag("do.token", "DigitalOcean API token. A read-only token is enough.").
		Envar("DIGITALOCEAN_TOKEN").Default("").String()
	f.tokenFile = app.Flag("do.token-file", "File holding the DigitalOcean API token.").
		Envar("DIGITALOCEAN_TOKEN_FILE").Default("").String()
	f.timeout = app.Flag("do.timeout", "Timeout of a single collector refresh.").
		Envar("DO_TIMEOUT").Default("30s").Duration()
	f.logLevel = app.Flag("log.level", "Log level: debug, info, warn or error.").
		Envar("LOG_LEVEL").Default("info").Enum("debug", "info", "warn", "error")
	f.logFormat = app.Flag("log.format", "Log format: logfmt or json.").
		Envar("LOG_FORMAT").Default("logfmt").Enum("logfmt", "json")
	f.account = app.Flag("collector.account", "Enable the account collector.").
		Envar("COLLECTOR_ACCOUNT").Default("true").Bool()
	f.accountInterval = app.Flag("collector.account.interval", "Refresh interval of the account collector.").
		Envar("COLLECTOR_ACCOUNT_INTERVAL").Default("5m").Duration()
	f.balance = app.Flag("collector.balance", "Enable the balance collector, which needs a billing-scoped token.").
		Envar("COLLECTOR_BALANCE").Default("true").Bool()
	f.balanceInterval = app.Flag("collector.balance.interval", "Refresh interval of the balance collector.").
		Envar("COLLECTOR_BALANCE_INTERVAL").Default("5m").Duration()
	f.registry = app.Flag("collector.registry", "Enable the container registry collector.").
		Envar("COLLECTOR_REGISTRY").Default("true").Bool()
	f.registryInterval = app.Flag("collector.registry.interval", "Refresh interval of the registry collector.").
		Envar("COLLECTOR_REGISTRY_INTERVAL").Default("5m").Duration()
	f.limits = app.Flag("collector.limits",
		"Enable the limits collector, which counts droplets, reserved IPs and volumes.").
		Envar("COLLECTOR_LIMITS").Default("true").Bool()
	f.limitsInterval = app.Flag("collector.limits.interval", "Refresh interval of the limits collector.").
		Envar("COLLECTOR_LIMITS_INTERVAL").Default("5m").Duration()
	f.droplets = app.Flag("collector.droplets", "Enable the droplets collector.").
		Envar("COLLECTOR_DROPLETS").Default("true").Bool()
	f.dropletsInterval = app.Flag("collector.droplets.interval", "Refresh interval of the droplets collector.").
		Envar("COLLECTOR_DROPLETS_INTERVAL").Default("5m").Duration()
	f.databases = app.Flag("collector.databases", "Enable the managed databases collector.").
		Envar("COLLECTOR_DATABASES").Default("true").Bool()
	f.databasesInterval = app.Flag("collector.databases.interval", "Refresh interval of the databases collector.").
		Envar("COLLECTOR_DATABASES_INTERVAL").Default("5m").Duration()
	f.kubernetes = app.Flag("collector.kubernetes", "Enable the managed Kubernetes collector.").
		Envar("COLLECTOR_KUBERNETES").Default("true").Bool()
	f.kubernetesInterval = app.Flag("collector.kubernetes.interval",
		"Refresh interval of the Kubernetes collector.").
		Envar("COLLECTOR_KUBERNETES_INTERVAL").Default("5m").Duration()
	bindSpaces(app, f)
	return f
}

// bindSpaces declares the flags of the Spaces collector, which brings its own
// credentials, its own timeout and a list of buckets.
func bindSpaces(app *kingpin.Application, f *flags) {
	f.spaces = app.Flag("collector.spaces", "Enable the Spaces collector. It lists every object.").
		Envar("COLLECTOR_SPACES").Default("false").Bool()
	f.spacesInterval = app.Flag("collector.spaces.interval", "Refresh interval of the Spaces collector.").
		Envar("COLLECTOR_SPACES_INTERVAL").Default("6h").Duration()
	f.spacesTimeout = app.Flag("collector.spaces.timeout", "Timeout of one full Spaces refresh.").
		Envar("COLLECTOR_SPACES_TIMEOUT").Default("15m").Duration()
	f.spacesBuckets = app.Flag("collector.spaces.bucket",
		"Bucket to measure, as name or name@region. Repeatable. Empty means discovery.").
		Envar("COLLECTOR_SPACES_BUCKET").Strings()
	f.spacesConcurrent = app.Flag("collector.spaces.concurrency", "How many buckets to list at once.").
		Envar("COLLECTOR_SPACES_CONCURRENCY").Default("4").Int()
	f.spacesKey = app.Flag("spaces.access-key", "Spaces access key.").
		Envar("DIGITALOCEAN_SPACES_KEY").Default("").String()
	f.spacesKeyFile = app.Flag("spaces.access-key-file", "File holding the Spaces access key.").
		Envar("DIGITALOCEAN_SPACES_KEY_FILE").Default("").String()
	f.spacesSecret = app.Flag("spaces.secret-key", "Spaces secret key.").
		Envar("DIGITALOCEAN_SPACES_SECRET").Default("").String()
	f.spacesSecretFile = app.Flag("spaces.secret-key-file", "File holding the Spaces secret key.").
		Envar("DIGITALOCEAN_SPACES_SECRET_FILE").Default("").String()
	f.spacesRegion = app.Flag("spaces.region", "Spaces region for discovery and for buckets without one.").
		Envar("SPACES_REGION").Default("").String()
}

// config validates the parsed flags and assembles the configuration.
func (f *flags) config() (*Config, error) {
	token, err := resolveSecret(*f.token, *f.tokenFile, ErrTokenConflict, ErrNoToken)
	if err != nil {
		return nil, err
	}

	spaces, err := f.spacesConfig()
	if err != nil {
		return nil, err
	}

	return &Config{
		ListenAddress: *f.listen,
		WebConfigFile: *f.webConfig,
		Token:         token,
		Timeout:       *f.timeout,
		LogLevel:      *f.logLevel,
		LogFormat:     *f.logFormat,
		Collectors: map[string]CollectorConfig{
			"account":    {Enabled: *f.account, Interval: *f.accountInterval},
			"balance":    {Enabled: *f.balance, Interval: *f.balanceInterval},
			"registry":   {Enabled: *f.registry, Interval: *f.registryInterval},
			"limits":     {Enabled: *f.limits, Interval: *f.limitsInterval},
			"droplets":   {Enabled: *f.droplets, Interval: *f.dropletsInterval},
			"databases":  {Enabled: *f.databases, Interval: *f.databasesInterval},
			"kubernetes": {Enabled: *f.kubernetes, Interval: *f.kubernetesInterval},
			"spaces": {
				Enabled:  *f.spaces,
				Interval: *f.spacesInterval,
				Timeout:  *f.spacesTimeout,
			},
		},
		Spaces: spaces,
	}, nil
}

// spacesConfig resolves the Spaces settings. Credentials and regions are only
// demanded when the collector is actually enabled.
func (f *flags) spacesConfig() (SpacesConfig, error) {
	cfg := SpacesConfig{Region: *f.spacesRegion, Concurrency: *f.spacesConcurrent}

	key, err := resolveSecret(*f.spacesKey, *f.spacesKeyFile,
		ErrSpacesCredentialConflict, ErrNoSpacesCredentials)
	if err != nil && *f.spaces {
		return SpacesConfig{}, err
	}
	secret, err := resolveSecret(*f.spacesSecret, *f.spacesSecretFile,
		ErrSpacesCredentialConflict, ErrNoSpacesCredentials)
	if err != nil && *f.spaces {
		return SpacesConfig{}, err
	}
	cfg.AccessKey, cfg.SecretKey = key, secret

	buckets, err := parseBuckets(*f.spacesBuckets, cfg.Region)
	if err != nil && *f.spaces {
		return SpacesConfig{}, err
	}
	cfg.Buckets = buckets

	if *f.spaces && len(buckets) == 0 && cfg.Region == "" {
		return SpacesConfig{}, fmt.Errorf("discovery needs a region: %w", ErrNoSpacesRegion)
	}
	return cfg, nil
}

// parseBuckets turns the repeatable bucket flag into names and regions.
func parseBuckets(raw []string, defaultRegion string) ([]SpacesBucket, error) {
	buckets := make([]SpacesBucket, 0, len(raw))
	for _, entry := range raw {
		name, region, found := strings.Cut(strings.TrimSpace(entry), "@")
		if !found || region == "" {
			region = defaultRegion
		}
		if name == "" {
			return nil, fmt.Errorf("empty bucket name in %q", entry)
		}
		if region == "" {
			return nil, fmt.Errorf("bucket %q has no region: %w", name, ErrNoSpacesRegion)
		}
		buckets = append(buckets, SpacesBucket{Name: name, Region: region})
	}
	return buckets, nil
}

// resolveSecret picks a secret from whichever source was configured. Exactly
// one source may be set; which errors describe the two failures is up to the
// caller, so the token and the Spaces keys keep their own wording.
func resolveSecret(inline, file string, conflict, missing error) (string, error) {
	switch {
	case inline != "" && file != "":
		return "", conflict
	case inline != "":
		return inline, nil
	case file != "":
		// The path is supplied by the operator running the exporter, which is
		// the whole point of the flag; there is no untrusted input here.
		raw, err := os.ReadFile(file) // #nosec G304
		if err != nil {
			return "", fmt.Errorf("read %q: %w", file, err)
		}
		trimmed := strings.TrimSpace(string(raw))
		if trimmed == "" {
			return "", fmt.Errorf("file %q is empty: %w", file, missing)
		}
		return trimmed, nil
	default:
		return "", missing
	}
}
