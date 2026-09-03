// Package uptime collects the account's Uptime checks: what each check
// probes, whether each probing region currently sees the target up, the
// thirty-day uptime each region has measured, and the previous outage.
//
// The listing answers only the configuration; everything a region has
// observed lives behind a per-check state endpoint, so the refresh fans out
// with one request per check on top of the listing. That is why the collector
// is off by default and carries a timeout of its own.
//
// The state endpoint reports no latency measurement — DigitalOcean's latency
// alerts are configured against a threshold, but no reading is exposed through
// the API — so there is no latency metric here to go with the status.
package uptime

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/digitalocean/godo"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/kozaktomas/digitalocean_exporter/internal/paging"
)

// knownRegionStatuses are the region statuses DigitalOcean documents, spelled
// the way the API spells them. Every one of them is reported for every region
// on every scrape, so an alert has a series for the status it looks for before
// a region ever enters it: a status that only appears once something is wrong
// is a query returning no data exactly when it matters.
var knownRegionStatuses = []string{"UP", "DOWN", "CHECKING"}

// Metric descriptors.
var (
	infoDesc = prometheus.NewDesc("digitalocean_uptime_check_info",
		"Always 1. Its labels describe the check: what it probes and whether it is enabled.",
		[]string{"id", "name", "type", "target", "enabled"}, nil)
	regionStatusDesc = prometheus.NewDesc("digitalocean_uptime_check_region_status",
		"Always 1 for the region's current status and 0 for every other known one.",
		[]string{"id", "name", "region", "status"}, nil)
	ratioDesc = prometheus.NewDesc("digitalocean_uptime_check_uptime_ratio",
		"Thirty-day uptime of the check as measured from the region, as a ratio between 0 and 1.",
		[]string{"id", "name", "region"}, nil)
	outageStartDesc = prometheus.NewDesc(
		"digitalocean_uptime_check_previous_outage_start_timestamp_seconds",
		"Unix time the check's previous outage began, as seen from the region that reported it.",
		[]string{"id", "name", "region"}, nil)
	outageDurationDesc = prometheus.NewDesc(
		"digitalocean_uptime_check_previous_outage_duration_seconds",
		"How long the check's previous outage lasted.",
		[]string{"id", "name", "region"}, nil)
	upDesc = prometheus.NewDesc("digitalocean_uptime_check_up",
		"Whether the check's last state lookup succeeded.", []string{"id", "name"}, nil)
)

// descriptors lists every metric the collector can emit.
var descriptors = []*prometheus.Desc{
	infoDesc, regionStatusDesc, ratioDesc, outageStartDesc, outageDurationDesc, upDesc,
}

// regionState is what one region has observed about a check, in snapshot form.
type regionState struct {
	region string
	status string
	ratio  float64
}

// outage is the check's previous outage. It is a pointer on the state because
// a check that has never gone down reports none, and an absent outage is no
// series rather than zeros.
type outage struct {
	region   string
	start    float64
	hasStart bool
	duration float64
}

// state is what the per-check state lookup answered. known separates "never
// looked up successfully" from "looked up, nothing observed yet": the first
// emits no state series at all.
type state struct {
	known   bool
	regions []regionState
	outage  *outage
}

// check is what one refresh learned about a single Uptime check.
type check struct {
	id      string
	name    string
	ctype   string
	target  string
	enabled string

	// up reports whether this refresh's state lookup succeeded; on failure the
	// check keeps the state its last successful lookup found.
	up    bool
	state state
}

// Collector reports the Uptime checks of the account.
type Collector struct {
	client *godo.Client
	logger *slog.Logger

	mu   sync.RWMutex
	snap []check
}

// New returns an Uptime collector backed by client. The logger records what
// the scheduler never sees: a duplicate check dropped from a list that shifted
// between two page requests, and a state lookup that failed for one check. A
// nil logger discards it.
func New(client *godo.Client, logger *slog.Logger) *Collector {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Collector{client: client, logger: logger}
}

// Name implements collector.Collector.
func (c *Collector) Name() string { return "uptime" }

// Describe implements collector.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range descriptors {
		ch <- d
	}
}

// Refresh implements collector.Collector. Every page and every state lookup is
// read before the snapshot is replaced, so a failure halfway through leaves
// the previous checks in place.
func (c *Collector) Refresh(ctx context.Context) error {
	listed, err := paging.All(ctx, c.logger, "uptime checks",
		func(uc godo.UptimeCheck) string { return uc.ID }, c.client.UptimeChecks.List)
	if err != nil {
		return err
	}

	next := make([]check, 0, len(listed))
	for i := range listed {
		next = append(next, newCheck(&listed[i]))
	}

	previous := c.previousStates()
	for i := range next {
		if err := c.refreshState(ctx, &next[i], previous); err != nil {
			return err
		}
	}

	c.mu.Lock()
	c.snap = next
	c.mu.Unlock()
	return nil
}

// newCheck converts one API check into its snapshot form.
func newCheck(uc *godo.UptimeCheck) check {
	return check{
		id:      uc.ID,
		name:    uc.Name,
		ctype:   uc.Type,
		target:  uc.Target,
		enabled: strconv.FormatBool(uc.Enabled),
	}
}

// previousStates returns what the last refresh knew about each check's state,
// keyed by check id, so a lookup that fails this time can keep reporting it.
func (c *Collector) previousStates() map[string]state {
	c.mu.RLock()
	defer c.mu.RUnlock()

	previous := make(map[string]state, len(c.snap))
	for _, ck := range c.snap {
		previous[ck.id] = ck.state
	}
	return previous
}

// refreshState fills in what the regions have observed about ck.
//
// One check's lookup failing is not the refresh failing: the check keeps the
// state its last successful lookup found, is marked down, and the failure is
// logged, because nothing else reports it. Running out of time is the other
// case — the deadline belongs to the whole refresh, so there is no point
// asking about the checks that are left, and the error is returned as any
// other.
func (c *Collector) refreshState(ctx context.Context, ck *check, previous map[string]state) error {
	st, _, err := c.client.UptimeChecks.GetState(ctx, ck.id)
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		ck.state = previous[ck.id]
		c.logger.Warn("uptime check state lookup failed",
			"check", ck.name, "check_id", ck.id, "err", err)
		return nil
	}
	ck.up = true
	ck.state = newState(st)
	return nil
}

// newState converts one API state into its snapshot form. The regions arrive
// as a map, so they are sorted to keep two refreshes of the same account
// identical.
func newState(st *godo.UptimeCheckState) state {
	out := state{known: true, regions: make([]regionState, 0, len(st.Regions))}
	for _, region := range slices.Sorted(maps.Keys(st.Regions)) {
		r := st.Regions[region]
		out.regions = append(out.regions, regionState{
			region: region,
			status: r.Status,
			ratio:  float64(r.ThirtyDayUptimePercentage) / 100,
		})
	}
	out.outage = newOutage(&st.PreviousOutage)
	return out
}

// newOutage converts the API's previous outage into its snapshot form. The
// API reports it as an object that is simply empty for a check that has never
// gone down, and an empty region is how that reads here.
func newOutage(po *godo.UptimePreviousOutage) *outage {
	if po.Region == "" {
		return nil
	}
	out := &outage{region: po.Region, duration: float64(po.DurationSeconds)}
	if started, err := time.Parse(time.RFC3339, po.StartedAt); err == nil {
		out.start = float64(started.Unix())
		out.hasStart = true
	}
	return out
}

// Collect implements collector.Collector. Before the first successful refresh,
// and on an account with no Uptime checks, it emits nothing.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	snap := c.snap
	c.mu.RUnlock()

	for i := range snap {
		ck := &snap[i]
		gauge(ch, infoDesc, 1, ck.id, ck.name, ck.ctype, ck.target, ck.enabled)
		gauge(ch, upDesc, boolToFloat(ck.up), ck.id, ck.name)
		collectState(ch, ck)
	}
}

// collectState emits the state series of one check, or nothing when its
// lookup has never succeeded — zeros there would read as "down from every
// region" while nothing had been observed at all.
func collectState(ch chan<- prometheus.Metric, ck *check) {
	if !ck.state.known {
		return
	}
	for _, r := range ck.state.regions {
		collectRegionStatus(ch, ck, r)
		gauge(ch, ratioDesc, r.ratio, ck.id, ck.name, r.region)
	}
	if o := ck.state.outage; o != nil {
		if o.hasStart {
			gauge(ch, outageStartDesc, o.start, ck.id, ck.name, o.region)
		}
		gauge(ch, outageDurationDesc, o.duration, ck.id, ck.name, o.region)
	}
}

// collectRegionStatus emits the status of a single region.
//
// A status DigitalOcean has invented since this was written is reported beside
// the documented ones: left out, it would make every status series of that
// region read 0, which is indistinguishable from the region having gone away.
func collectRegionStatus(ch chan<- prometheus.Metric, ck *check, r regionState) {
	statuses := knownRegionStatuses
	if r.status != "" && !slices.Contains(statuses, r.status) {
		statuses = append(slices.Clone(statuses), r.status)
	}
	for _, status := range statuses {
		gauge(ch, regionStatusDesc, boolToFloat(status == r.status),
			ck.id, ck.name, r.region, status)
	}
}

// boolToFloat maps a boolean to the 1/0 convention Prometheus expects.
func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// gauge sends one gauge sample of desc with the given label values.
func gauge(ch chan<- prometheus.Metric, desc *prometheus.Desc, value float64, labels ...string) {
	ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, value, labels...)
}
