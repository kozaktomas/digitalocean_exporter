package timeseries_test

import (
	"encoding/json"
	"errors"
	"math"
	"testing"

	"github.com/digitalocean/godo"

	"github.com/kozaktomas/digitalocean_exporter/internal/timeseries"
)

// decode builds a response the way the API client does, from the JSON the
// monitoring API actually returns, so the fixtures below are the real wire
// format rather than a hand-built struct.
func decode(t *testing.T, body string) *godo.MetricsResponse {
	t.Helper()
	var resp godo.MetricsResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return &resp
}

// A droplet CPU response: one series per mode, values ascending in time.
const cpuBody = `{"status":"success","data":{"resultType":"matrix","result":[
  {"metric":{"host_id":"1","mode":"idle"},
   "values":[[1787676840,"100.5"],[1787676960,"200.5"]]},
  {"metric":{"host_id":"1","mode":"user"},
   "values":[[1787676840,"10"],[1787676960,"20"]]}]}}`

func TestLatestTakesTheNewestPointOfEverySeries(t *testing.T) {
	t.Parallel()

	got, err := timeseries.Latest(decode(t, cpuBody))
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Latest returned %d samples, want 2", len(got))
	}

	byMode := map[string]float64{}
	for _, s := range got {
		byMode[s.Label("mode")] = s.Value
	}
	if byMode["idle"] != 200.5 || byMode["user"] != 20 {
		t.Errorf("values by mode = %v, want idle 200.5 and user 20", byMode)
	}
	if got[0].Label("host_id") != "1" {
		t.Errorf("host_id = %q, want %q", got[0].Label("host_id"), "1")
	}
	if got[0].Time.Unix() != 1787676960 {
		t.Errorf("time = %d, want %d", got[0].Time.Unix(), 1787676960)
	}
}

// The API returns points in ascending order, but the newest one is found by
// timestamp rather than by position.
func TestLatestIgnoresPointOrder(t *testing.T) {
	t.Parallel()

	const body = `{"status":"success","data":{"resultType":"matrix","result":[
	  {"metric":{},"values":[[1787676960,"7"],[1787676840,"3"]]}]}}`

	got, err := timeseries.Latest(decode(t, body))
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if len(got) != 1 || got[0].Value != 7 {
		t.Errorf("Latest = %v, want a single sample of 7", got)
	}
}

// A load balancer with no traffic really has no series. That is data, not a
// failure, and the caller has to be able to tell the two apart.
func TestLatestOnEmptyResultReturnsNoSamplesAndNoError(t *testing.T) {
	t.Parallel()

	const body = `{"status":"success","data":{"resultType":"matrix","result":[]}}`

	got, err := timeseries.Latest(decode(t, body))
	if err != nil {
		t.Fatalf("Latest on an empty result: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Latest returned %d samples, want 0", len(got))
	}
}

// A series present but carrying no points has nothing to report either.
func TestLatestSkipsSeriesWithoutPoints(t *testing.T) {
	t.Parallel()

	const body = `{"status":"success","data":{"resultType":"matrix","result":[
	  {"metric":{"mode":"idle"},"values":[]},
	  {"metric":{"mode":"user"},"values":[[1787676840,"5"]]}]}}`

	got, err := timeseries.Latest(decode(t, body))
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if len(got) != 1 || got[0].Label("mode") != "user" {
		t.Errorf("Latest = %v, want only the user series", got)
	}
}

// NaN is how the API says it has no reading, so it must not be published as
// one. A series whose only point is NaN drops out entirely.
func TestLatestSkipsNaNPoints(t *testing.T) {
	t.Parallel()

	const body = `{"status":"success","data":{"resultType":"matrix","result":[
	  {"metric":{"mode":"idle"},"values":[[1787676840,"5"],[1787676960,"NaN"]]},
	  {"metric":{"mode":"user"},"values":[[1787676960,"NaN"]]}]}}`

	got, err := timeseries.Latest(decode(t, body))
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Latest returned %d samples, want 1", len(got))
	}
	if got[0].Label("mode") != "idle" || got[0].Value != 5 {
		t.Errorf("Latest = %v, want the idle series at its last real value 5", got[0])
	}
	if math.IsNaN(got[0].Value) {
		t.Error("Latest published a NaN value")
	}
}

func TestLatestRejectsAnUnsuccessfulResponse(t *testing.T) {
	t.Parallel()

	const body = `{"status":"error","data":{"resultType":"matrix","result":[]}}`

	_, err := timeseries.Latest(decode(t, body))
	if !errors.Is(err, timeseries.ErrNotSuccess) {
		t.Errorf("Latest error = %v, want ErrNotSuccess", err)
	}
}

func TestLatestRejectsAMissingResponse(t *testing.T) {
	t.Parallel()

	_, err := timeseries.Latest(nil)
	if !errors.Is(err, timeseries.ErrNoResponse) {
		t.Errorf("Latest error = %v, want ErrNoResponse", err)
	}
}

// A metric that carries no labels at all, such as a load balancer's current
// connection count, must still yield a sample.
func TestLatestHandlesSeriesWithoutLabels(t *testing.T) {
	t.Parallel()

	const body = `{"status":"success","data":{"resultType":"matrix","result":[
	  {"metric":{},"values":[[1787676960,"132"]]}]}}`

	got, err := timeseries.Latest(decode(t, body))
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if len(got) != 1 || got[0].Value != 132 {
		t.Fatalf("Latest = %v, want a single sample of 132", got)
	}
	if got[0].Label("anything") != "" {
		t.Errorf("Label on an absent name = %q, want empty", got[0].Label("anything"))
	}
}
