// Package config turns command-line flags and environment variables into a
// validated exporter configuration.
package config

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/alecthomas/kingpin/v2"

	"github.com/kozaktomas/digitalocean_exporter/internal/version"
)

// ErrNoToken reports that no DigitalOcean API token was configured.
var ErrNoToken = errors.New("no DigitalOcean API token: set --do.token or --do.token-file")

// ErrTokenConflict reports that both token sources were configured at once.
var ErrTokenConflict = errors.New("--do.token and --do.token-file are mutually exclusive")

// ErrHelpShown reports that kingpin printed what it was asked for — usage, or
// the version — and the process should exit successfully. Parsing deliberately
// never terminates the process itself, so the caller decides; without this
// --help and --version would fall through to token validation and exit 1 with a
// confusing message.
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

// ErrNonPositiveInterval reports that a refresh interval was zero or negative.
// time.NewTicker panics on such a duration, and it would do so in a scheduler
// goroutine started after the metrics server has already bound its port, so the
// process would die with a stack trace instead of a configuration error.
var ErrNonPositiveInterval = errors.New("refresh interval must be greater than zero")

// ErrNonPositiveTimeout reports that a timeout was zero or negative. A refresh
// bounded by one is over before it starts: every refresh of every affected
// collector fails with a deadline exceeded, forever.
var ErrNonPositiveTimeout = errors.New("timeout must be greater than zero")

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
	// Concurrency caps how many buckets are measured at once.
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
	// APIBaseURL overrides the public DigitalOcean API endpoint when it is not
	// empty. Empty is the default and means the public API.
	APIBaseURL string
	// Timeout bounds a single collector refresh unless the collector sets its
	// own.
	Timeout time.Duration
	// RateLimit caps how many API requests per second the exporter makes,
	// across every collector at once. Zero or less turns the limiter off.
	RateLimit float64
	// LogLevel is the slog level name.
	LogLevel string
	// LogFormat is either logfmt or json.
	LogFormat string
	// Collectors maps a collector name to its configuration.
	Collectors map[string]CollectorConfig
	// Spaces holds the Spaces collector's own settings.
	Spaces SpacesConfig
	// DropletMetricsConcurrency caps how many droplets the droplet metrics
	// collector measures at once.
	DropletMetricsConcurrency int
	// DropletMetricsAgentOnly limits the droplet metrics collector to the
	// droplets whose listing reports DigitalOcean's monitoring agent.
	DropletMetricsAgentOnly bool
	// LoadBalancerMetricsConcurrency caps how many load balancers the load
	// balancer metrics collector measures at once.
	LoadBalancerMetricsConcurrency int
}

// collectorFlags holds the two flags every collector has.
type collectorFlags struct {
	enabled  *bool
	interval *time.Duration
}

// flags holds every bound flag before validation turns them into a Config.
type flags struct {
	listen     *string
	webConfig  *string
	token      *string
	tokenFile  *string
	apiBaseURL *string
	timeout    *time.Duration
	rateLimit  *float64
	logLevel   *string
	logFormat  *string
	// simple holds the collectors configured by nothing but an enable switch
	// and an interval, keyed by collector name.
	simple           map[string]*collectorFlags
	dropletMetrics   *bool
	dmInterval       *time.Duration
	dmTimeout        *time.Duration
	dmConcurrency    *int
	dmAgentOnly      *bool
	lbMetrics        *bool
	lbmInterval      *time.Duration
	lbmTimeout       *time.Duration
	lbmConcurrency   *int
	spaces           *bool
	spacesInterval   *time.Duration
	spacesTimeout    *time.Duration
	spacesBuckets    *[]string
	spacesConcurrent *int
	spacesKey        *string
	spacesKeyFile    *string
	spacesSecret     *string
	spacesSecretFile *string
	spacesRegion     *string
}

// simpleCollectors are the collectors that need nothing but an enable switch
// and a refresh interval. The ones that carry a timeout or a concurrency of
// their own — the two monitoring-API collectors and spaces — are bound
// separately.
//
// A collector is on by default when it costs one or two API requests per
// refresh and its metrics suit any account. firewalls and certificates are the
// exception: they cost the same as the others, but a firewall ruleset changes
// when somebody deploys and a certificate when it is renewed, so scraping them
// every five minutes is a deliberate choice rather than a default.
//
// This is a slice rather than a map so the flags appear in --help in the same
// order every run.
var simpleCollectors = []struct {
	name    string
	help    string
	enabled bool
}{
	{"account", "Enable the account collector.", true},
	{"balance", "Enable the balance collector, which needs a billing-scoped token.", true},
	{"registry", "Enable the container registry collector.", true},
	{"limits", "Enable the limits collector, which counts droplets, reserved IPs and volumes.", true},
	{"droplets", "Enable the droplets collector.", true},
	{"databases", "Enable the managed databases collector.", true},
	{"kubernetes", "Enable the managed Kubernetes collector.", true},
	{"volumes", "Enable the block storage volumes collector.", true},
	{"loadbalancers", "Enable the load balancers collector.", true},
	{"cdn", "Enable the CDN endpoints collector.", true},
	{"domains", "Enable the DNS domains collector.", true},
	{"firewalls",
		"Enable the cloud firewalls collector, which reports rule counts and how far each firewall is applied.",
		false},
	{"certificates",
		"Enable the certificates collector, which reports when each TLS certificate expires.",
		false},
}

// Parse builds a Config from args, falling back to environment variables.
// It never terminates the process: every failure is returned as an error.
func Parse(args []string) (*Config, error) {
	app := kingpin.New("digitalocean_exporter", "Prometheus exporter for DigitalOcean.")
	app.Version(version.String())

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
	// Hidden because pointing the exporter at something other than the public
	// API is a development and smoke-test affair, not an operational one. It is
	// a flag rather than a bare os.Getenv so that it is parsed, validated and
	// logged like every other setting: an endpoint that quietly redirects every
	// request is otherwise invisible in a running exporter.
	f.apiBaseURL = app.Flag("do.api-base-url",
		"DigitalOcean API base URL. Empty means the public API.").
		Envar("DO_API_BASE_URL").Default("").Hidden().String()
	f.timeout = app.Flag("do.timeout", "Timeout of a single collector refresh.").
		Envar("DO_TIMEOUT").Default("30s").Duration()
	f.rateLimit = app.Flag("do.rate-limit",
		"Client-side limit on API requests per second, shared by every collector. 0 disables it.").
		Envar("DO_RATE_LIMIT").Default("4").Float64()
	f.logLevel = app.Flag("log.level", "Log level: debug, info, warn or error.").
		Envar("LOG_LEVEL").Default("info").Enum("debug", "info", "warn", "error")
	f.logFormat = app.Flag("log.format", "Log format: logfmt or json.").
		Envar("LOG_FORMAT").Default("logfmt").Enum("logfmt", "json")
	f.simple = bindSimple(app)
	bindMonitoring(app, f)
	bindSpaces(app, f)
	return f
}

// bindSimple declares the enable switch and the refresh interval of every
// collector in simpleCollectors. The pair has the same shape for all of them,
// so it is spelled out once here rather than thirteen times.
//
// The environment variable follows from the name: COLLECTOR_<NAME> and
// COLLECTOR_<NAME>_INTERVAL, which is the convention every collector already
// used.
func bindSimple(app *kingpin.Application) map[string]*collectorFlags {
	bound := make(map[string]*collectorFlags, len(simpleCollectors))
	for _, c := range simpleCollectors {
		envar := "COLLECTOR_" + strings.ToUpper(c.name)
		bound[c.name] = &collectorFlags{
			enabled: app.Flag("collector."+c.name, c.help).
				Envar(envar).Default(strconv.FormatBool(c.enabled)).Bool(),
			interval: app.Flag("collector."+c.name+".interval",
				"Refresh interval of the "+c.name+" collector.").
				Envar(envar + "_INTERVAL").Default("5m").Duration(),
		}
	}
	return bound
}

// bindMonitoring declares the flags of the collectors that read the monitoring
// API. They are off by default: the API answers one metric of one resource per
// request, so their cost grows with the size of the account and no upgrade
// should quietly multiply somebody's API usage.
func bindMonitoring(app *kingpin.Application, f *flags) {
	f.dropletMetrics = app.Flag("collector.dropletmetrics",
		"Enable the droplet metrics collector. Costs 10 API requests per droplet per refresh.").
		Envar("COLLECTOR_DROPLETMETRICS").Default("false").Bool()
	f.dmInterval = app.Flag("collector.dropletmetrics.interval",
		"Refresh interval of the droplet metrics collector. The API samples every 2m.").
		Envar("COLLECTOR_DROPLETMETRICS_INTERVAL").Default("5m").Duration()
	f.dmTimeout = app.Flag("collector.dropletmetrics.timeout",
		"Timeout of one full droplet metrics refresh.").
		Envar("COLLECTOR_DROPLETMETRICS_TIMEOUT").Default("2m").Duration()
	f.dmConcurrency = app.Flag("collector.dropletmetrics.concurrency",
		"How many droplets to measure at once.").
		Envar("COLLECTOR_DROPLETMETRICS_CONCURRENCY").Default("4").Int()
	f.dmAgentOnly = app.Flag("collector.dropletmetrics.agent-only",
		"Measure only droplets whose listing reports the monitoring agent. "+
			"An agent installed after the droplet was created does not set that feature.").
		Envar("COLLECTOR_DROPLETMETRICS_AGENT_ONLY").Default("false").Bool()
	f.lbMetrics = app.Flag("collector.loadbalancermetrics",
		"Enable the load balancer metrics collector. Costs 7 API requests per load balancer.").
		Envar("COLLECTOR_LOADBALANCERMETRICS").Default("false").Bool()
	f.lbmInterval = app.Flag("collector.loadbalancermetrics.interval",
		"Refresh interval of the load balancer metrics collector. The API samples every 2m.").
		Envar("COLLECTOR_LOADBALANCERMETRICS_INTERVAL").Default("5m").Duration()
	f.lbmTimeout = app.Flag("collector.loadbalancermetrics.timeout",
		"Timeout of one full load balancer metrics refresh.").
		Envar("COLLECTOR_LOADBALANCERMETRICS_TIMEOUT").Default("2m").Duration()
	f.lbmConcurrency = app.Flag("collector.loadbalancermetrics.concurrency",
		"How many load balancers to measure at once.").
		Envar("COLLECTOR_LOADBALANCERMETRICS_CONCURRENCY").Default("4").Int()
}

// bindSpaces declares the flags of the Spaces collector, which brings its own
// credentials, its own timeout and a list of buckets.
func bindSpaces(app *kingpin.Application, f *flags) {
	f.spaces = app.Flag("collector.spaces", "Enable the Spaces collector. It needs a Spaces key pair.").
		Envar("COLLECTOR_SPACES").Default("false").Bool()
	f.spacesInterval = app.Flag("collector.spaces.interval", "Refresh interval of the Spaces collector.").
		Envar("COLLECTOR_SPACES_INTERVAL").Default("5m").Duration()
	f.spacesTimeout = app.Flag("collector.spaces.timeout", "Timeout of one full Spaces refresh.").
		Envar("COLLECTOR_SPACES_TIMEOUT").Default("2m").Duration()
	f.spacesBuckets = app.Flag("collector.spaces.bucket",
		"Bucket to measure, as name or name@region. Repeatable. Empty means discovery.").
		Envar("COLLECTOR_SPACES_BUCKET").Strings()
	f.spacesConcurrent = app.Flag("collector.spaces.concurrency", "How many buckets to measure at once.").
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
	// Durations first: the check is pure flag validation, and a value that
	// would panic the scheduler or stall every refresh is worth reporting even
	// when the token is missing too.
	if err := f.validateDurations(); err != nil {
		return nil, err
	}

	token, err := resolveSecret(*f.token, *f.tokenFile, ErrTokenConflict, ErrNoToken)
	if err != nil {
		return nil, err
	}

	spaces, err := f.spacesConfig()
	if err != nil {
		return nil, err
	}

	collectors := make(map[string]CollectorConfig, len(f.simple)+3)
	for name, bound := range f.simple {
		collectors[name] = CollectorConfig{Enabled: *bound.enabled, Interval: *bound.interval}
	}
	collectors["dropletmetrics"] = CollectorConfig{
		Enabled: *f.dropletMetrics, Interval: *f.dmInterval, Timeout: *f.dmTimeout,
	}
	collectors["loadbalancermetrics"] = CollectorConfig{
		Enabled: *f.lbMetrics, Interval: *f.lbmInterval, Timeout: *f.lbmTimeout,
	}
	collectors["spaces"] = CollectorConfig{
		Enabled: *f.spaces, Interval: *f.spacesInterval, Timeout: *f.spacesTimeout,
	}

	return &Config{
		ListenAddress:                  *f.listen,
		WebConfigFile:                  *f.webConfig,
		Token:                          token,
		APIBaseURL:                     *f.apiBaseURL,
		Timeout:                        *f.timeout,
		RateLimit:                      *f.rateLimit,
		LogLevel:                       *f.logLevel,
		LogFormat:                      *f.logFormat,
		Collectors:                     collectors,
		Spaces:                         spaces,
		DropletMetricsConcurrency:      *f.dmConcurrency,
		DropletMetricsAgentOnly:        *f.dmAgentOnly,
		LoadBalancerMetricsConcurrency: *f.lbmConcurrency,
	}, nil
}

// validateDurations rejects a non-positive interval or timeout, naming the flag
// that carries it. Both are unrecoverable at runtime and both are reachable
// from a Helm value, so they fail the process at startup instead.
func (f *flags) validateDurations() error {
	if err := requirePositive("do.timeout", *f.timeout, ErrNonPositiveTimeout); err != nil {
		return err
	}

	// The map iterates in a random order, so sort the names: two bad intervals
	// must not report a different one on every run.
	names := make([]string, 0, len(f.simple))
	for name := range f.simple {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		flag := "collector." + name + ".interval"
		if err := requirePositive(flag, *f.simple[name].interval, ErrNonPositiveInterval); err != nil {
			return err
		}
	}

	// The collectors that carry a timeout of their own. A simple collector has
	// none, and its zero means "use --do.timeout" rather than "no time at all".
	for _, c := range []struct {
		name              string
		interval, timeout time.Duration
	}{
		{"dropletmetrics", *f.dmInterval, *f.dmTimeout},
		{"loadbalancermetrics", *f.lbmInterval, *f.lbmTimeout},
		{"spaces", *f.spacesInterval, *f.spacesTimeout},
	} {
		if err := requirePositive("collector."+c.name+".interval",
			c.interval, ErrNonPositiveInterval); err != nil {
			return err
		}
		if err := requirePositive("collector."+c.name+".timeout",
			c.timeout, ErrNonPositiveTimeout); err != nil {
			return err
		}
	}
	return nil
}

// requirePositive wraps sentinel with the flag and the value it was given.
func requirePositive(flag string, d time.Duration, sentinel error) error {
	if d > 0 {
		return nil
	}
	return fmt.Errorf("--%s is %s: %w", flag, d, sentinel)
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
