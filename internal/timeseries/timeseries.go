// Package timeseries reduces a DigitalOcean monitoring response to the newest
// sample of each series it contains.
//
// The monitoring API is a Prometheus range-query API: one request returns a
// matrix of series, each with its own labels and a list of timestamped values
// covering the requested window. An exporter wants none of that history — it
// wants the current value — so this package takes the last point of every
// series and hands back the labels that identify it.
//
// The API samples every two minutes, so the newest point in a window of
// several of them is the current value — one that is between zero and one
// sampling interval old, depending on where the request lands in the cycle.
package timeseries

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/digitalocean/godo"
	"github.com/digitalocean/godo/metrics"
)

// ErrNoResponse reports that there was no response to read at all, which means
// the caller ignored an error from the API client.
var ErrNoResponse = errors.New("no metrics response")

// ErrNotSuccess reports that the API answered with a status other than
// success. The body then carries no usable series.
var ErrNotSuccess = errors.New("metrics response was not successful")

// successStatus is the status a usable metrics response carries.
const successStatus = "success"

// Sample is the newest point of a single series.
type Sample struct {
	// Labels are the labels the API returned for the series. Which ones
	// appear depends on the metric: a droplet filesystem series is split by
	// device and mountpoint, while a load balancer connection count carries
	// no labels at all.
	Labels map[string]string
	// Value is the value of the newest point.
	Value float64
	// Time is when that point was sampled.
	Time time.Time
}

// Label returns the value of the named label, or the empty string when the
// series does not carry it. Callers use it rather than indexing Labels so that
// a metric which unexpectedly lacks a label produces an empty label value
// instead of panicking or being silently dropped.
func (s Sample) Label(name string) string {
	return s.Labels[name]
}

// Latest returns the newest sample of every series in resp.
//
// An empty result is not an error: a load balancer with no traffic genuinely
// has no series for its HTTP responses, and a droplet reports nothing until
// its monitoring agent has sent something. Callers distinguish "no data" from
// "the request failed" by the error being nil and the slice being empty.
//
// Series carrying no points, and points whose value is NaN, are skipped: the
// API uses both to say it has nothing for that window, and forwarding them
// would publish a reading that was never taken.
func Latest(resp *godo.MetricsResponse) ([]Sample, error) {
	if resp == nil {
		return nil, ErrNoResponse
	}
	if resp.Status != successStatus {
		return nil, fmt.Errorf("%w: status %q", ErrNotSuccess, resp.Status)
	}

	samples := make([]Sample, 0, len(resp.Data.Result))
	for _, series := range resp.Data.Result {
		point, ok := newest(series.Values)
		if !ok {
			continue
		}
		samples = append(samples, Sample{
			Labels: labelsOf(series),
			Value:  float64(point.Value),
			Time:   point.Timestamp.Time(),
		})
	}
	return samples, nil
}

// newest returns the point with the highest timestamp, and whether there was a
// usable one at all. The API returns points in ascending order, but scanning
// for the maximum costs nothing and does not depend on that.
func newest(values []metrics.SamplePair) (metrics.SamplePair, bool) {
	var (
		best  metrics.SamplePair
		found bool
	)
	for _, point := range values {
		if math.IsNaN(float64(point.Value)) {
			continue
		}
		if !found || point.Timestamp.After(best.Timestamp) {
			best, found = point, true
		}
	}
	return best, found
}

// labelsOf copies a series' labels into a plain map, so that nothing outside
// this package has to know the API client's label types.
func labelsOf(series metrics.SampleStream) map[string]string {
	labels := make(map[string]string, len(series.Metric))
	for name, value := range series.Metric {
		labels[string(name)] = string(value)
	}
	return labels
}
