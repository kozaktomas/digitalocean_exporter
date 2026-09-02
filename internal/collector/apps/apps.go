// Package apps collects the App Platform applications of the account: what
// tier each one runs on, where the deployment it is serving got to, and how
// many instances of each component its spec asks for.
//
// One list request answers all of it. An app carries its tier, its region, its
// active and in-progress deployments and the whole spec in the same response,
// so nothing here costs a request per app.
//
// What is not here is the load: CPU, memory and restarts per component live
// behind DigitalOcean's monitoring API, which godo has no methods for. The
// docs say so rather than leaving it looking like an oversight.
package apps

import (
	"context"
	"log/slog"
	"slices"
	"sync"

	"github.com/digitalocean/godo"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/kozaktomas/digitalocean_exporter/internal/paging"
)

// knownPhases are the deployment phases DigitalOcean documents, spelled the way
// the API spells them. Every one of them is reported for every app on every
// scrape, so an alert or a panel has a series for the phase it looks for before
// a deployment ever enters it: a phase that only appears once something is
// wrong is a query returning no data exactly when it matters.
//
// UNKNOWN is not among them. It is what the API returns for a deployment whose
// phase it cannot name, which is not a state to graph; an app reporting it is
// carried in the same way as a phase invented after this was written.
var knownPhases = []string{
	string(godo.DeploymentPhase_PendingBuild),
	string(godo.DeploymentPhase_Building),
	string(godo.DeploymentPhase_PendingDeploy),
	string(godo.DeploymentPhase_Deploying),
	string(godo.DeploymentPhase_Active),
	string(godo.DeploymentPhase_Superseded),
	string(godo.DeploymentPhase_Error),
	string(godo.DeploymentPhase_Canceled),
}

// Component kinds, as the spec's own field names have them. A static site is
// listed beside the three that run instances even though it runs none: leaving
// it out would make a table of an app's components silently miss half of a
// site-plus-API app.
const (
	kindService    = "service"
	kindWorker     = "worker"
	kindJob        = "job"
	kindStaticSite = "static_site"
)

// Metric descriptors. Every metric carries the app's id and its name: the name
// is what a dashboard variable and a summary line read, the id is what joins
// the series together and the half that survives a rename.
var (
	infoDesc = prometheus.NewDesc("digitalocean_app_info",
		"Always 1. Its labels describe the app's tier, region and default ingress.",
		[]string{"id", "name", "tier", "region", "default_ingress"}, nil)
	phaseDesc = prometheus.NewDesc("digitalocean_app_deployment_phase",
		"Always 1 for the phase the app's active deployment is in and 0 for every other known one.",
		[]string{"id", "name", "phase"}, nil)
	inProgressDesc = prometheus.NewDesc("digitalocean_app_deployment_in_progress",
		"Whether a deployment of the app is currently in progress.",
		[]string{"id", "name"}, nil)
	lastActiveDesc = prometheus.NewDesc("digitalocean_app_last_deployment_active_timestamp_seconds",
		"When the app's most recent deployment went active, as a Unix timestamp.",
		[]string{"id", "name"}, nil)
	createdDesc = prometheus.NewDesc("digitalocean_app_created_timestamp_seconds",
		"Creation time of the app as a Unix timestamp.",
		[]string{"id", "name"}, nil)
	componentInstancesDesc = prometheus.NewDesc("digitalocean_app_component_instances",
		"Number of instances the app's spec asks for of this component.",
		[]string{"id", "name", "component", "kind", "instance_size"}, nil)
)

// descriptors lists every metric the collector can emit.
var descriptors = []*prometheus.Desc{
	infoDesc, phaseDesc, inProgressDesc, lastActiveDesc, createdDesc, componentInstancesDesc,
}

// component is one workload of an app's spec. A static site has neither an
// instance count nor an instance size — App Platform serves it from its CDN —
// so it reports zero instances and an empty size rather than being left out.
type component struct {
	name      string
	kind      string
	size      string
	instances float64
}

// app is what one refresh learned about a single App Platform application.
//
// phase is empty when the app has no active deployment, which is the state an
// app has between being created and its first successful build; the phase
// metric is then not reported at all, because a zero for every phase reads as a
// deployment that is in none of them.
type app struct {
	id             string
	name           string
	tier           string
	region         string
	defaultIngress string

	phase      string
	hasActive  bool
	inProgress float64

	lastDeploymentActive int64
	hasLastDeployment    bool
	created              int64
	hasCreated           bool

	components []component
}

// Collector reports the App Platform applications of the account.
type Collector struct {
	client *godo.Client
	logger *slog.Logger

	mu   sync.RWMutex
	snap []app
}

// New returns an apps collector backed by client. The logger records what the
// scheduler never sees: a duplicate app dropped from a list that shifted
// between two page requests. A nil logger discards it.
func New(client *godo.Client, logger *slog.Logger) *Collector {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Collector{client: client, logger: logger}
}

// Name implements collector.Collector.
func (c *Collector) Name() string { return "apps" }

// Describe implements collector.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range descriptors {
		ch <- d
	}
}

// Refresh implements collector.Collector. Every page is read before the
// snapshot is replaced, so a failure halfway through the list leaves the
// previous apps in place rather than reporting half an account.
func (c *Collector) Refresh(ctx context.Context) error {
	listed, err := paging.All(ctx, c.logger, "apps",
		func(a *godo.App) string { return a.ID }, c.client.Apps.List)
	if err != nil {
		return err
	}

	next := make([]app, 0, len(listed))
	for _, a := range listed {
		if a == nil {
			continue
		}
		next = append(next, newApp(a))
	}

	c.mu.Lock()
	c.snap = next
	c.mu.Unlock()
	return nil
}

// newApp converts one API app into its snapshot form.
func newApp(a *godo.App) app {
	out := app{
		id:             a.ID,
		tier:           a.TierSlug,
		defaultIngress: a.DefaultIngress,
		inProgress:     boolToFloat(a.InProgressDeployment != nil),
	}
	if a.Region != nil {
		out.region = a.Region.Slug
	}
	if a.ActiveDeployment != nil {
		out.hasActive = true
		out.phase = string(a.ActiveDeployment.Phase)
	}
	// A timestamp the API left out is not reported at all. Zero would be the
	// epoch, and every "how long since it deployed" query would read it as
	// fifty-six years, which is worse than a missing series.
	if !a.LastDeploymentActiveAt.IsZero() {
		out.hasLastDeployment = true
		out.lastDeploymentActive = a.LastDeploymentActiveAt.Unix()
	}
	if !a.CreatedAt.IsZero() {
		out.hasCreated = true
		out.created = a.CreatedAt.Unix()
	}
	if a.Spec != nil {
		// The app's name lives on the spec rather than on the app: the API
		// treats it as part of what was declared, not as metadata.
		out.name = a.Spec.Name
		// The region falls back to the spec's own, which is what an app
		// declares before DigitalOcean has resolved it to a data centre.
		if out.region == "" {
			out.region = a.Spec.Region
		}
		out.components = newComponents(a.Spec)
	}
	return out
}

// newComponents converts an app spec's workloads into their snapshot form, in
// the order the spec lists them by kind.
func newComponents(spec *godo.AppSpec) []component {
	components := make([]component, 0,
		len(spec.Services)+len(spec.Workers)+len(spec.Jobs)+len(spec.StaticSites))

	for _, s := range spec.Services {
		if s != nil {
			components = append(components, component{
				name: s.Name, kind: kindService,
				size: s.InstanceSizeSlug, instances: float64(s.InstanceCount),
			})
		}
	}
	for _, w := range spec.Workers {
		if w != nil {
			components = append(components, component{
				name: w.Name, kind: kindWorker,
				size: w.InstanceSizeSlug, instances: float64(w.InstanceCount),
			})
		}
	}
	for _, j := range spec.Jobs {
		if j != nil {
			components = append(components, component{
				name: j.Name, kind: kindJob,
				size: j.InstanceSizeSlug, instances: float64(j.InstanceCount),
			})
		}
	}
	for _, s := range spec.StaticSites {
		if s != nil {
			components = append(components, component{name: s.Name, kind: kindStaticSite})
		}
	}
	return components
}

// boolToFloat maps a boolean to the 1/0 convention Prometheus expects.
func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// Collect implements collector.Collector. Before the first successful refresh,
// and on an account with no apps, it emits nothing.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	snap := c.snap
	c.mu.RUnlock()

	for _, a := range snap {
		gauge(ch, infoDesc, 1, a.id, a.name, a.tier, a.region, a.defaultIngress)
		gauge(ch, inProgressDesc, a.inProgress, a.id, a.name)
		if a.hasLastDeployment {
			gauge(ch, lastActiveDesc, float64(a.lastDeploymentActive), a.id, a.name)
		}
		if a.hasCreated {
			gauge(ch, createdDesc, float64(a.created), a.id, a.name)
		}
		collectPhases(ch, a)
		for _, comp := range a.components {
			gauge(ch, componentInstancesDesc, comp.instances,
				a.id, a.name, comp.name, comp.kind, comp.size)
		}
	}
}

// collectPhases emits one series per known phase for the app's active
// deployment. An app without one emits none: an app whose first build has never
// finished is not a deployment sitting in every phase at zero.
//
// A phase DigitalOcean has invented since this was written is reported beside
// the documented ones. Left out, it would make every series of that app read 0,
// which is indistinguishable from the app having no deployment at all.
func collectPhases(ch chan<- prometheus.Metric, a app) {
	if !a.hasActive {
		return
	}

	phases := knownPhases
	if a.phase != "" && !slices.Contains(phases, a.phase) {
		phases = append(slices.Clone(phases), a.phase)
	}
	for _, phase := range phases {
		gauge(ch, phaseDesc, boolToFloat(phase == a.phase), a.id, a.name, phase)
	}
}

// gauge sends one gauge sample of desc with the given label values.
func gauge(ch chan<- prometheus.Metric, desc *prometheus.Desc, value float64, labels ...string) {
	ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, value, labels...)
}
