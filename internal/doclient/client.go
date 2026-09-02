// Package doclient builds the DigitalOcean API client used by the collectors
// and instruments every request it makes.
package doclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/digitalocean/godo"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/time/rate"
)

const (
	// defaultMaxAttempts is how many times one request is tried in total: the
	// first attempt and two retries. DigitalOcean's burst window is a minute
	// wide, so a third retry would mostly sit waiting inside a refresh that has
	// a timeout of its own.
	defaultMaxAttempts = 3
	// baseBackoff is the wait before the first retry when the response carries
	// no Retry-After of its own. It doubles with every further attempt.
	baseBackoff = time.Second
	// maxBackoff caps that fallback wait. It does not cap a Retry-After: when
	// the API says when its window reopens, waiting less is an attempt that is
	// certain to be rejected, and a rejected attempt still spends from the
	// hourly budget. A wait that does not fit the request's deadline is not
	// shortened either, it is not made at all.
	maxBackoff = 10 * time.Second
	// maxDrain bounds how much of a discarded body is read back before the
	// connection is reused. An API error body is a few hundred bytes.
	maxDrain = 64 << 10
)

// Metrics tracks the exporter's use of the DigitalOcean API.
type Metrics struct {
	// Requests counts API calls by resource and response status.
	Requests *prometheus.CounterVec
	// RateLimit holds the remaining hourly request budget reported by the API.
	RateLimit prometheus.Gauge
}

// NewMetrics creates the API metrics and registers them with reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "digitalocean_exporter_api_requests_total",
			Help: "Total DigitalOcean API requests by resource and response status.",
		}, []string{"resource", "status"}),
		RateLimit: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "digitalocean_exporter_api_rate_limit_remaining",
			Help: "Requests left in the current DigitalOcean API rate-limit window.",
		}),
	}
	reg.MustRegister(m.Requests, m.RateLimit)
	return m
}

// Config describes the API client to build.
type Config struct {
	// Token authenticates every request. A read-only token is enough.
	Token string
	// BaseURL overrides the public API endpoint when it is non-empty, which
	// the tests and the smoke run rely on.
	BaseURL string
	// UserAgent identifies the exporter to the API.
	UserAgent string
	// Timeout bounds one request, including its retries and any time spent
	// waiting for the rate limiter.
	Timeout time.Duration
	// RateLimit caps how many requests per second the exporter makes, across
	// every collector at once. Zero or less turns the limiter off.
	RateLimit float64
	// MaxAttempts bounds how many times a single request is tried. Zero means
	// defaultMaxAttempts; one disables retries.
	MaxAttempts int
	// Metrics counts the requests the client makes. It is required.
	Metrics *Metrics
}

// New returns a godo client built from cfg.
//
// The rate limiter and the retries live in the exporter's own transport rather
// than in godo's SetStaticRateLimit and WithRetryAndBackoffs. Both of godo's
// sit above the transport: its limiter would not pace the retries, and its
// retry option replaces the HTTP client outright, which would drop the
// instrumentation that counts every request. Here each attempt is paced and
// counted alike.
func New(cfg Config) (*godo.Client, error) {
	// Every request records itself, so a client without metrics would panic on
	// the first one. Saying so here beats a nil dereference minutes later.
	if cfg.Metrics == nil {
		return nil, errors.New("api metrics are required")
	}

	attempts := cfg.MaxAttempts
	if attempts <= 0 {
		attempts = defaultMaxAttempts
	}

	var limiter *rate.Limiter
	if cfg.RateLimit > 0 {
		// A burst of one spreads the requests evenly instead of letting a
		// refresh spend a whole second's worth at once, which is the shape of
		// traffic that trips the per-minute limit in the first place.
		limiter = rate.NewLimiter(rate.Limit(cfg.RateLimit), 1)
	}

	httpClient := &http.Client{
		Timeout: cfg.Timeout,
		Transport: &transport{
			base:        http.DefaultTransport,
			token:       cfg.Token,
			metrics:     cfg.Metrics,
			limiter:     limiter,
			maxAttempts: attempts,
		},
	}

	opts := []godo.ClientOpt{godo.SetUserAgent(cfg.UserAgent)}
	if cfg.BaseURL != "" {
		opts = append(opts, godo.SetBaseURL(cfg.BaseURL))
	}
	return godo.New(httpClient, opts...)
}

// transport adds the bearer token to every request, paces the requests and
// records the outcome of each attempt. Authenticating here rather than through
// oauth2 keeps the token in one place and lets the same wrapper observe the
// response headers.
type transport struct {
	base        http.RoundTripper
	token       string
	metrics     *Metrics
	limiter     *rate.Limiter
	maxAttempts int
}

// RoundTrip implements http.RoundTripper.
//
// A rejected or failed request is tried again up to the attempt budget, as
// long as the wait the API asks for fits inside the caller's deadline. When it
// does not, the response is handed back as it is: the collector fails on the
// API's own answer rather than on a deadline, and the attempts it would have
// spent stay in the hourly budget.
func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	attempts := t.maxAttempts
	if req.Body != nil {
		// Only a request that can be replayed is retried, and a body read by
		// the first attempt is gone. Every call the exporter makes is a GET,
		// so this is a guard rather than a limitation.
		attempts = 1
	}

	for attempt := 1; ; attempt++ {
		resp, err := t.attempt(req)
		if attempt >= attempts {
			return resp, err
		}

		wait, retry := nextWait(resp, err, attempt)
		if !retry || !fits(req.Context(), wait) {
			return resp, err
		}

		if resp != nil {
			// The body has to be read and closed before the connection can
			// serve the retry; leaving it open leaks one per rejected request.
			drain(resp)
		}
		if sleepErr := sleep(req.Context(), wait); sleepErr != nil {
			return nil, sleepErr
		}
	}
}

// attempt makes one request, waiting for the rate limiter first and counting
// the outcome afterwards. Retries come back through here, so every one of them
// is paced and counted like a first attempt.
func (t *transport) attempt(req *http.Request) (*http.Response, error) {
	if t.limiter != nil {
		if err := t.limiter.Wait(req.Context()); err != nil {
			return nil, fmt.Errorf("wait for the API rate limiter: %w", err)
		}
	}

	// A RoundTripper must not modify the request it is given.
	authed := req.Clone(req.Context())
	authed.Header.Set("Authorization", "Bearer "+t.token)

	res := resource(req.URL.Path)
	resp, err := t.base.RoundTrip(authed)
	if err != nil {
		t.metrics.Requests.WithLabelValues(res, "error").Inc()
		return nil, err
	}

	t.metrics.Requests.WithLabelValues(res, strconv.Itoa(resp.StatusCode)).Inc()
	if remaining := resp.Header.Get("RateLimit-Remaining"); remaining != "" {
		if value, convErr := strconv.ParseFloat(remaining, 64); convErr == nil {
			t.metrics.RateLimit.Set(value)
		}
	}
	return resp, nil
}

// nextWait returns how long to wait before trying the request again, and
// whether trying it again is worth anything at all.
func nextWait(resp *http.Response, err error, attempt int) (time.Duration, bool) {
	if err != nil {
		if !retryableError(err) {
			return 0, false
		}
		// A connection that failed says nothing about when it will work, so
		// the doubling fallback is all there is to go on.
		return fallbackBackoff(attempt), true
	}
	return retryDelay(resp, attempt)
}

// retryDelay returns how long to wait before retrying resp, and whether resp
// is worth retrying.
//
// The API asks for a wait in two different ways, and one of them is a refusal.
// A Retry-After — which DigitalOcean sends for the burst limit — names the
// moment its window reopens, and is honoured in full. A 429 without one, but
// with the hourly budget reported as spent, is the hourly limit: nothing frees
// that up before the hour turns, so it is returned rather than retried. What
// is left, a 5xx or a 429 carrying neither signal, gets the doubling fallback.
func retryDelay(resp *http.Response, attempt int) (time.Duration, bool) {
	if !retryable(resp.StatusCode) {
		return 0, false
	}
	if wait, ok := retryAfter(resp.Header.Get("Retry-After")); ok {
		return wait, true
	}
	if resp.StatusCode == http.StatusTooManyRequests && budgetSpent(resp.Header) {
		return 0, false
	}
	return fallbackBackoff(attempt), true
}

// retryable reports whether a status is worth another attempt: the burst
// rejection itself, and the server-side failures that are usually transient.
// 501 is excluded, because an endpoint that is not implemented will not become
// implemented a second later.
func retryable(status int) bool {
	return status == http.StatusTooManyRequests ||
		(status >= http.StatusInternalServerError && status != http.StatusNotImplemented)
}

// retryableError reports whether a transport-level failure is worth another
// attempt. A reset connection, an unexpected EOF, a DNS hiccup or a refused
// connect often is, and Go's transport replays none of them but the idle one.
// A context that is done never is: the caller has already given up, and the
// next attempt would fail on the same context without reaching the API.
func retryableError(err error) bool {
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

// budgetSpent reports whether the response says the hourly allowance is gone.
// DigitalOcean sends RateLimit-Remaining on every response; a 429 that carries
// a zero is the hourly limit rather than the burst one.
func budgetSpent(header http.Header) bool {
	remaining, err := strconv.ParseFloat(header.Get("RateLimit-Remaining"), 64)
	return err == nil && remaining <= 0
}

// fallbackBackoff returns the wait to use when the response names none itself:
// one second, then two, then four, capped.
func fallbackBackoff(attempt int) time.Duration {
	return min(baseBackoff<<(attempt-1), maxBackoff)
}

// retryAfter parses a Retry-After header, which is either a number of seconds
// or an HTTP date. A value in the past means "now".
func retryAfter(value string) (time.Duration, bool) {
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		return max(time.Duration(seconds)*time.Second, 0), true
	}
	if when, err := http.ParseTime(value); err == nil {
		return max(time.Until(when), 0), true
	}
	return 0, false
}

// fits reports whether waiting for d still leaves the request's context alive
// to make the retry with. A context without a deadline always has the time.
func fits(ctx context.Context, d time.Duration) bool {
	deadline, ok := ctx.Deadline()
	if !ok {
		return true
	}
	return time.Now().Add(d).Before(deadline)
}

// sleep waits for d, or until ctx is done.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// drain empties and closes a response body that is about to be discarded, so
// the underlying connection can be reused for the retry.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrain))
	_ = resp.Body.Close()
}

// resource reduces an API path to a low-cardinality label: the first segment
// after the version prefix. "/v2/customers/my/balance" becomes "customers".
// Object identifiers must never reach a label, or the metric explodes.
func resource(path string) string {
	trimmed := strings.TrimPrefix(strings.TrimPrefix(path, "/"), "v2/")
	if index := strings.IndexByte(trimmed, '/'); index >= 0 {
		trimmed = trimmed[:index]
	}
	if trimmed == "" {
		return "unknown"
	}
	return trimmed
}
