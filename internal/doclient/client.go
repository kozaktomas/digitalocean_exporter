// Package doclient builds the DigitalOcean API client used by the collectors
// and instruments every request it makes.
package doclient

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/digitalocean/godo"
	"github.com/prometheus/client_golang/prometheus"
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

// New returns a godo client authenticated with token and instrumented with m.
// A non-empty baseURL overrides the public API endpoint, which tests rely on.
func New(token, baseURL, userAgent string, timeout time.Duration, m *Metrics) (*godo.Client, error) {
	httpClient := &http.Client{
		Timeout: timeout,
		Transport: &transport{
			base:    http.DefaultTransport,
			token:   token,
			metrics: m,
		},
	}

	opts := []godo.ClientOpt{godo.SetUserAgent(userAgent)}
	if baseURL != "" {
		opts = append(opts, godo.SetBaseURL(baseURL))
	}
	return godo.New(httpClient, opts...)
}

// transport adds the bearer token to every request and records the outcome.
// Authenticating here rather than through oauth2 keeps the token in one place
// and lets the same wrapper observe the response headers.
type transport struct {
	base    http.RoundTripper
	token   string
	metrics *Metrics
}

// RoundTrip implements http.RoundTripper.
func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
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
