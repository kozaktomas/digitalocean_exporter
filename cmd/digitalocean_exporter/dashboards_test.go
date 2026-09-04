package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/kozaktomas/digitalocean_exporter/internal/collector"
	"github.com/kozaktomas/digitalocean_exporter/internal/config"
	"github.com/kozaktomas/digitalocean_exporter/internal/doclient"
	"github.com/kozaktomas/digitalocean_exporter/internal/version"
)

// updateDashboards rewrites the committed dashboards in their normalised form
// instead of asserting that they already are. It is how a dashboard exported
// from Grafana is brought into the repository.
var updateDashboards = flag.Bool("update.dashboards", false,
	"rewrite the bundled dashboards in their normalised form")

// dashboardDir holds the bundled dashboards. They live inside the chart
// because Helm can only package files below the chart directory.
const dashboardDir = "../../charts/digitalocean-exporter/dashboards"

// descPattern pulls the metric name and its variable labels out of a
// descriptor. prometheus.Desc keeps its fields unexported and its String method
// is the only way in.
var descPattern = regexp.MustCompile(`fqName: "([^"]+)".*variableLabels: \{([^}]*)\}`)

// metricPattern matches a metric name of this exporter wherever it appears in
// a PromQL expression.
var metricPattern = regexp.MustCompile(`digitalocean_[a-z0-9_]+`)

// dashboardFiles lists the bundled dashboards by path.
func dashboardFiles(t *testing.T) []string {
	t.Helper()

	paths, err := filepath.Glob(filepath.Join(dashboardDir, "*.json"))
	if err != nil {
		t.Fatalf("glob dashboards: %v", err)
	}
	if len(paths) == 0 {
		t.Fatalf("no dashboards found in %s", dashboardDir)
	}
	sort.Strings(paths)
	return paths
}

// readDashboard parses one bundled dashboard.
func readDashboard(t *testing.T, path string) map[string]any {
	t.Helper()

	raw, err := os.ReadFile(path) //nolint:gosec // the path comes from a glob of a fixed directory.
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	dash, err := decodeDashboard(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return dash
}

// decodeDashboard parses dashboard JSON, keeping numbers exactly as written so
// that re-encoding cannot reformat them.
func decodeDashboard(raw []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()

	var dash map[string]any
	if err := decoder.Decode(&dash); err != nil {
		return nil, err
	}
	return dash, nil
}

// normalise returns the committed form of a dashboard: the instance-specific
// fields removed, the variables' remembered selections cleared, and the whole
// thing encoded with sorted keys so that a re-export diffs readably.
func normalise(raw []byte) ([]byte, error) {
	dash, err := decodeDashboard(raw)
	if err != nil {
		return nil, err
	}

	// Grafana's API wraps the dashboard in {"meta": …, "dashboard": …}; a file
	// pasted straight from it is unwrapped here rather than rejected.
	if inner, ok := dash["dashboard"].(map[string]any); ok {
		dash = inner
	}

	// id is the exporting instance's primary key and version its edit counter.
	// Neither means anything anywhere else.
	delete(dash, "id")
	delete(dash, "version")

	for _, variable := range templatingList(dash) {
		// current is whichever value the exporting instance had selected, and
		// options the values it had cached. Both describe that account.
		variable["current"] = map[string]any{}
		if _, ok := variable["options"]; ok {
			variable["options"] = []any{}
		}
	}

	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(dash); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// templatingList returns the dashboard's template variables.
func templatingList(dash map[string]any) []map[string]any {
	templating, ok := dash["templating"].(map[string]any)
	if !ok {
		return nil
	}
	list, ok := templating["list"].([]any)
	if !ok {
		return nil
	}

	variables := make([]map[string]any, 0, len(list))
	for _, entry := range list {
		if variable, ok := entry.(map[string]any); ok {
			variables = append(variables, variable)
		}
	}
	return variables
}

// Grafana does not guarantee key order, so a dashboard edited in the browser
// and exported back arrives as a diff touching most of the file, with whatever
// the exporting account had selected baked into it. Normalising on the way in
// keeps the review readable and the repository free of one account's names.
func TestDashboardsAreNormalised(t *testing.T) {
	for _, path := range dashboardFiles(t) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path) //nolint:gosec // the path comes from a glob of a fixed directory.
			if err != nil {
				t.Fatalf("read: %v", err)
			}

			want, err := normalise(raw)
			if err != nil {
				t.Fatalf("normalise: %v", err)
			}

			if *updateDashboards {
				if err := os.WriteFile(path, want, 0o644); err != nil { //nolint:gosec // dashboards are public.
					t.Fatalf("write: %v", err)
				}
				return
			}

			if !bytes.Equal(raw, want) {
				t.Errorf("%s is not in its normalised form; run "+
					"go test ./cmd/digitalocean_exporter -run TestDashboardsAreNormalised -update.dashboards",
					filepath.Base(path))
			}
		})
	}
}

// capturingRegisterer records every collector registered with it, so that the
// exporter's own metrics can be described without production code having to
// hand them out.
type capturingRegisterer struct {
	prometheus.Registerer
	collectors []prometheus.Collector
}

// Register records the collector and registers it as usual.
func (r *capturingRegisterer) Register(c prometheus.Collector) error {
	r.collectors = append(r.collectors, c)
	return r.Registerer.Register(c)
}

// MustRegister records the collectors and registers them as usual.
func (r *capturingRegisterer) MustRegister(cs ...prometheus.Collector) {
	r.collectors = append(r.collectors, cs...)
	r.Registerer.MustRegister(cs...)
}

// exportedMetrics maps every metric the exporter can emit to its labels, from
// the same wiring run uses. Every collector is enabled, so a metric only
// reachable through a collector that is off by default still counts.
func exportedMetrics(t *testing.T) map[string]map[string]bool {
	t.Helper()

	cfg, err := config.Parse([]string{
		"--do.token", "secret",
		"--collector.dropletmetrics",
		"--collector.loadbalancermetrics",
		"--collector.firewalls",
		"--collector.certificates",
		"--collector.spaces",
		"--collector.uptime",
		"--spaces.access-key", "key",
		"--spaces.secret-key", "secret",
		"--spaces.region", "fra1",
	})
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}

	capturing := &capturingRegisterer{Registerer: prometheus.NewRegistry()}
	client, err := doclient.New(doclient.Config{
		Token: "secret", UserAgent: "test", Timeout: time.Second,
		Metrics: doclient.NewMetrics(capturing),
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	capturing.MustRegister(version.NewCollector())

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	scheduler := collector.NewScheduler(time.Second, logger, capturing)
	registerCollectors(scheduler, cfg, client, logger)

	// Describe performs no I/O, so the nil-bound client is never called.
	metrics := make(map[string]map[string]bool)
	for _, c := range append(capturing.collectors, scheduler) {
		for name, labels := range describeMetrics(c) {
			metrics[name] = labels
		}
	}
	return metrics
}

// describeMetrics returns the metrics a collector describes, each with its
// variable labels.
func describeMetrics(c prometheus.Collector) map[string]map[string]bool {
	descs := make(chan *prometheus.Desc)
	go func() {
		c.Describe(descs)
		close(descs)
	}()

	metrics := make(map[string]map[string]bool)
	for desc := range descs {
		match := descPattern.FindStringSubmatch(desc.String())
		if match == nil {
			continue
		}
		labels := make(map[string]bool)
		for _, label := range strings.Split(match[2], ",") {
			if label = strings.TrimSpace(label); label != "" {
				labels[label] = true
			}
		}
		metrics[match[1]] = labels
	}
	return metrics
}

// A metric can be renamed or dropped without anything failing: the graph that
// used it simply goes empty, months before anyone looks. This holds every
// bundled dashboard against the descriptors the exporter actually registers.
func TestDashboardsOnlyUseExportedMetrics(t *testing.T) {
	exported := exportedMetrics(t)
	if len(exported) == 0 {
		t.Fatal("no exported metric names found")
	}

	for _, path := range dashboardFiles(t) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			for _, query := range dashboardQueries(readDashboard(t, path)) {
				for _, name := range metricPattern.FindAllString(query, -1) {
					if !isExported(name, exported) {
						t.Errorf("query %q uses %q, which the exporter does not export", query, name)
					}
				}
			}
		})
	}
}

// isExported reports whether a name from a query is one the exporter emits. A
// name is retried without a histogram or summary suffix, which a query adds and
// a descriptor never carries.
func isExported(name string, exported map[string]map[string]bool) bool {
	if _, ok := exported[name]; ok {
		return true
	}
	for _, suffix := range []string{"_bucket", "_sum", "_count"} {
		base, found := strings.CutSuffix(name, suffix)
		if !found {
			continue
		}
		if _, ok := exported[base]; ok {
			return true
		}
	}
	return false
}

// dashboardQueries returns every PromQL expression a dashboard runs, from its
// panels and from the queries that populate its variables.
func dashboardQueries(dash map[string]any) []string {
	var queries []string

	forEachPanel(dash, func(panel map[string]any) {
		targets, ok := panel["targets"].([]any)
		if !ok {
			return
		}
		for _, entry := range targets {
			target, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			if expr, ok := target["expr"].(string); ok {
				queries = append(queries, expr)
			}
		}
	})

	for _, variable := range templatingList(dash) {
		queries = append(queries, variableQueries(variable)...)
	}
	return queries
}

// variableQueries returns the PromQL a template variable runs. Grafana writes
// the query either as a plain string or as an object, and repeats it in
// definition, which is what the variable editor shows.
func variableQueries(variable map[string]any) []string {
	var queries []string

	switch query := variable["query"].(type) {
	case string:
		queries = append(queries, query)
	case map[string]any:
		if inner, ok := query["query"].(string); ok {
			queries = append(queries, inner)
		}
	}

	if definition, ok := variable["definition"].(string); ok {
		queries = append(queries, definition)
	}
	return queries
}

// forEachPanel calls fn for every panel in the dashboard, including the panels
// nested inside a collapsed row.
func forEachPanel(dash map[string]any, fn func(map[string]any)) {
	var walk func(entries []any)
	walk = func(entries []any) {
		for _, entry := range entries {
			panel, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			fn(panel)
			if nested, ok := panel["panels"].([]any); ok {
				walk(nested)
			}
		}
	}

	if panels, ok := dash["panels"].([]any); ok {
		walk(panels)
	}
}

// A dashboard that names a datasource UID, or drops the job filter, works
// perfectly on the machine it was exported from and nowhere else. These are the
// rules that make the bundled set portable.
func TestDashboardsArePortable(t *testing.T) {
	for _, path := range dashboardFiles(t) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			dash := readDashboard(t, path)

			name := strings.TrimSuffix(filepath.Base(path), ".json")
			if want := "digitalocean-" + name; dash["uid"] != want {
				t.Errorf("uid = %v, want %q", dash["uid"], want)
			}
			if title, _ := dash["title"].(string); !strings.HasPrefix(title, "DigitalOcean / ") {
				t.Errorf("title = %q, want a DigitalOcean / prefix", title)
			}
			if !hasTag(dash, "digitalocean") {
				t.Error("tags do not include digitalocean, so the dashboard is missing from the links dropdown")
			}

			assertVariable(t, dash, "datasource")
			assertVariable(t, dash, "job")
			assertDatasourcesAreVariable(t, dash)
		})
	}
}

// hasTag reports whether the dashboard carries the given tag.
func hasTag(dash map[string]any, want string) bool {
	tags, ok := dash["tags"].([]any)
	if !ok {
		return false
	}
	for _, tag := range tags {
		if tag == want {
			return true
		}
	}
	return false
}

// assertVariable fails unless the dashboard declares the named variable.
func assertVariable(t *testing.T, dash map[string]any, want string) {
	t.Helper()

	for _, variable := range templatingList(dash) {
		if variable["name"] == want {
			return
		}
	}
	t.Errorf("no %q template variable", want)
}

// assertDatasourcesAreVariable fails unless every datasource reference in the
// dashboard resolves through the datasource variable.
func assertDatasourcesAreVariable(t *testing.T, dash map[string]any) {
	t.Helper()

	var walk func(node any)
	walk = func(node any) {
		switch value := node.(type) {
		case map[string]any:
			if datasource, ok := value["datasource"].(map[string]any); ok {
				if uid, ok := datasource["uid"].(string); ok && uid != "${datasource}" {
					t.Errorf("datasource uid = %q, want ${datasource}", uid)
				}
			}
			for _, nested := range value {
				walk(nested)
			}
		case []any:
			for _, nested := range value {
				walk(nested)
			}
		}
	}
	walk(dash)
}

// The chart renders one ConfigMap per file in the dashboard directory, and the
// documentation names them one by one. A file added without a line in the
// documentation ships invisibly.
func TestDashboardsAreDocumented(t *testing.T) {
	page, err := os.ReadFile("../../docs/dashboards.md")
	if err != nil {
		t.Fatalf("read the dashboards page: %v", err)
	}

	for _, path := range dashboardFiles(t) {
		name := filepath.Base(path)
		if !bytes.Contains(page, []byte(name)) {
			t.Errorf("%s is not mentioned in docs/dashboards.md", name)
		}
	}
}
