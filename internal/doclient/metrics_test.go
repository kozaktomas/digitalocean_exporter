package doclient

import (
	"context"
	"net/http"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// The three rate-limit gauges are the whole picture of the budget, and each of
// them has to survive a response that does not carry its header: the last
// known value is right until the API says otherwise, while a zero written over
// it reads as a budget that has just run out.
func TestObserveRateLimit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                                string
		header                              http.Header
		remaining, limit, reset             float64
		wantRemaining, wantLimit, wantReset float64
	}{
		{
			name: "all three headers are recorded",
			header: http.Header{
				"Ratelimit-Remaining": {"4711"},
				"Ratelimit-Limit":     {"5000"},
				"Ratelimit-Reset":     {"1756742400"},
			},
			wantRemaining: 4711, wantLimit: 5000, wantReset: 1756742400,
		},
		{
			name:          "a response without any of them changes nothing",
			header:        http.Header{},
			remaining:     100,
			limit:         5000,
			reset:         1756742400,
			wantRemaining: 100, wantLimit: 5000, wantReset: 1756742400,
		},
		{
			name:          "only the header that is present is updated",
			header:        http.Header{"Ratelimit-Remaining": {"42"}},
			remaining:     100,
			limit:         5000,
			reset:         1756742400,
			wantRemaining: 42, wantLimit: 5000, wantReset: 1756742400,
		},
		{
			name: "an unparsable value leaves its gauge alone",
			header: http.Header{
				"Ratelimit-Remaining": {"plenty"},
				"Ratelimit-Limit":     {""},
				"Ratelimit-Reset":     {"never"},
			},
			remaining:     100,
			limit:         5000,
			reset:         1756742400,
			wantRemaining: 100, wantLimit: 5000, wantReset: 1756742400,
		},
		{
			name: "a larger allowance is followed",
			header: http.Header{
				"Ratelimit-Limit":     {"10000"},
				"Ratelimit-Remaining": {"9999"},
			},
			limit:         5000,
			wantRemaining: 9999, wantLimit: 10000, wantReset: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			metrics := NewMetrics(prometheus.NewRegistry())
			metrics.Remaining.Set(tt.remaining)
			metrics.Limit.Set(tt.limit)
			metrics.Reset.Set(tt.reset)

			metrics.observeRateLimit(tt.header)

			for _, gauge := range []struct {
				name  string
				value prometheus.Gauge
				want  float64
			}{
				{"remaining", metrics.Remaining, tt.wantRemaining},
				{"limit", metrics.Limit, tt.wantLimit},
				{"reset", metrics.Reset, tt.wantReset},
			} {
				if got := testutil.ToFloat64(gauge.value); got != gauge.want {
					t.Errorf("%s = %v, want %v", gauge.name, got, gauge.want)
				}
			}
		})
	}
}

// The resource label is what keeps the request counter's cardinality bounded,
// and every path the exporter calls has to reduce to something readable rather
// than to an object identifier.
func TestResource(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
		want string
	}{
		{"an account path", "/v2/account", "account"},
		{"a path without the leading slash", "v2/droplets", "droplets"},
		{"a nested path keeps only its first segment", "/v2/customers/my/balance", "customers"},
		{"an identifier never reaches the label", "/v2/droplets/123456/actions", "droplets"},
		{"a monitoring query", "/v2/monitoring/metrics/droplet/cpu", "monitoring"},
		{"a two-segment resource keeps the first", "/v2/kubernetes/clusters", "kubernetes"},
		{"an unversioned path is taken as it is", "/healthz", "healthz"},
		{"the version prefix alone is unknown", "/v2/", "unknown"},
		{"the root is unknown", "/", "unknown"},
		{"an empty path is unknown", "", "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := resource(tt.path); got != tt.want {
				t.Errorf("resource(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

// A request made outside a refresh still has to carry a collector label, or
// the series it lands on cannot be summed with the rest.
func TestCollectorName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ctx  context.Context
		want string
	}{
		{"a refresh names its collector", WithCollector(context.Background(), "droplets"), "droplets"},
		{"a bare context is nobody's", context.Background(), "none"},
		{"an empty name is not recorded", WithCollector(context.Background(), ""), "none"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := collectorName(tt.ctx); got != tt.want {
				t.Errorf("collectorName() = %q, want %q", got, tt.want)
			}
		})
	}
}
