package main

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// alertRulesFile holds the bundled alerting rules. It is a plain Prometheus
// rule file, which the chart wraps in a PrometheusRule.
const alertRulesFile = "../../charts/digitalocean-exporter/alerts/digitalocean.rules.yaml"

// labelRefPattern matches a label reference in an alert's annotations.
var labelRefPattern = regexp.MustCompile(`\$labels\.([a-zA-Z_][a-zA-Z0-9_]*)`)

// ruleFile mirrors the parts of the Prometheus rule format the tests read.
type ruleFile struct {
	Groups []struct {
		Name  string `yaml:"name"`
		Rules []struct {
			Alert       string            `yaml:"alert"`
			Expr        string            `yaml:"expr"`
			For         string            `yaml:"for"`
			Labels      map[string]string `yaml:"labels"`
			Annotations map[string]string `yaml:"annotations"`
		} `yaml:"rules"`
	} `yaml:"groups"`
}

// readAlertRules parses the bundled rule file.
func readAlertRules(t *testing.T) ruleFile {
	t.Helper()

	raw, err := os.ReadFile(alertRulesFile)
	if err != nil {
		t.Fatalf("read the alert rules: %v", err)
	}

	var rules ruleFile
	if err := yaml.Unmarshal(raw, &rules); err != nil {
		t.Fatalf("parse the alert rules: %v", err)
	}
	if len(rules.Groups) == 0 {
		t.Fatal("no rule groups found")
	}
	return rules
}

// An alert on a metric that no longer exists never fires, which reads exactly
// like the thing being fine. This holds the rules against the descriptors the
// collectors register, the same way the dashboards are held.
func TestAlertRulesOnlyUseExportedMetrics(t *testing.T) {
	exported := exportedMetrics(t)

	for _, group := range readAlertRules(t).Groups {
		for _, rule := range group.Rules {
			for _, name := range metricPattern.FindAllString(rule.Expr, -1) {
				if !isExported(name, exported) {
					t.Errorf("alert %s uses %q, which the exporter does not export", rule.Alert, name)
				}
			}
		}
	}
}

// A summary saying "Pool  is short of nodes" is what a label that does not
// exist looks like once Alertmanager has rendered it. Every $labels reference
// has to be a label of a metric the alert's own expression reads.
func TestAlertAnnotationsReferenceRealLabels(t *testing.T) {
	exported := exportedMetrics(t)

	for _, group := range readAlertRules(t).Groups {
		for _, rule := range group.Rules {
			available := labelsAvailableTo(rule.Expr, exported)
			for _, annotation := range rule.Annotations {
				for _, match := range labelRefPattern.FindAllStringSubmatch(annotation, -1) {
					if !available[match[1]] {
						t.Errorf("alert %s renders $labels.%s, which none of its metrics carry",
							rule.Alert, match[1])
					}
				}
			}
		}
	}
}

// labelsAvailableTo returns the labels an alert can render: those of every
// metric its expression reads, plus the ones Prometheus attaches on scrape.
func labelsAvailableTo(expr string, exported map[string]map[string]bool) map[string]bool {
	available := map[string]bool{"job": true, "instance": true}
	for _, name := range metricPattern.FindAllString(expr, -1) {
		for label := range exported[name] {
			available[label] = true
		}
	}
	return available
}

// An alert with no severity cannot be routed and one with no description
// arrives as a name and a graph. Both are the difference between an alert
// somebody acts on and one somebody silences.
func TestAlertRulesAreComplete(t *testing.T) {
	seen := make(map[string]bool)

	for _, group := range readAlertRules(t).Groups {
		if !strings.HasPrefix(group.Name, "digitalocean") {
			t.Errorf("group %q does not start with digitalocean, so it may collide with another exporter's",
				group.Name)
		}

		for _, rule := range group.Rules {
			if rule.Alert == "" {
				t.Errorf("group %s has a rule with no alert name", group.Name)
				continue
			}
			if seen[rule.Alert] {
				t.Errorf("alert %s is defined twice", rule.Alert)
			}
			seen[rule.Alert] = true

			if !strings.HasPrefix(rule.Alert, "DigitalOcean") {
				t.Errorf("alert %s does not start with DigitalOcean", rule.Alert)
			}
			switch rule.Labels["severity"] {
			case "critical", "warning", "info":
			default:
				t.Errorf("alert %s has severity %q, want critical, warning or info",
					rule.Alert, rule.Labels["severity"])
			}
			for _, annotation := range []string{"summary", "description"} {
				if strings.TrimSpace(rule.Annotations[annotation]) == "" {
					t.Errorf("alert %s has no %s annotation", rule.Alert, annotation)
				}
			}
		}
	}
}

// alertingPagePrefix is where every runbook_url must point: the published
// alerting page, at the anchor of the alert's own section.
const alertingPagePrefix = "https://kozaktomas.github.io/digitalocean_exporter/latest/alerting/#"

// A runbook_url whose anchor no heading provides lands the reader on the top
// of the page instead of the alert they were paged for, which is invisible in
// a Markdown file: the site's anchors only exist once mkdocs renders it. This
// re-derives them from the headings the same way mkdocs does.
func TestAlertRunbookURLsPointAtRealSections(t *testing.T) {
	anchors := alertingPageAnchors(t)

	for _, group := range readAlertRules(t).Groups {
		for _, rule := range group.Rules {
			url := rule.Annotations["runbook_url"]
			if url == "" {
				t.Errorf("alert %s has no runbook_url annotation", rule.Alert)
				continue
			}
			anchor, ok := strings.CutPrefix(url, alertingPagePrefix)
			if !ok {
				t.Errorf("alert %s has runbook_url %q, which is not an anchor of the published alerting page",
					rule.Alert, url)
				continue
			}
			if !anchors[anchor] {
				t.Errorf("alert %s has runbook_url anchor %q, which no heading in docs/alerting.md provides",
					rule.Alert, anchor)
			}
		}
	}
}

// alertingPageAnchors returns the anchor of every heading in docs/alerting.md.
func alertingPageAnchors(t *testing.T) map[string]bool {
	t.Helper()

	page, err := os.ReadFile("../../docs/alerting.md")
	if err != nil {
		t.Fatalf("read the alerting page: %v", err)
	}

	anchors := make(map[string]bool)
	for _, line := range strings.Split(string(page), "\n") {
		if !strings.HasPrefix(line, "#") {
			continue
		}
		anchors[slugify(strings.TrimLeft(line, "# "))] = true
	}
	if len(anchors) == 0 {
		t.Fatal("no headings found in docs/alerting.md")
	}
	return anchors
}

// slugify turns a heading into the anchor mkdocs derives from it: lowercased,
// spaces to hyphens, everything else that is not a word character dropped.
func slugify(heading string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(heading) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('-')
		}
	}
	return b.String()
}

// An alert nobody can look up is one nobody trusts, so every one of them is
// named on the alerting page.
func TestAlertsAreDocumented(t *testing.T) {
	page, err := os.ReadFile("../../docs/alerting.md")
	if err != nil {
		t.Fatalf("read the alerting page: %v", err)
	}

	for _, group := range readAlertRules(t).Groups {
		for _, rule := range group.Rules {
			if !strings.Contains(string(page), rule.Alert) {
				t.Errorf("alert %s is not mentioned in docs/alerting.md", rule.Alert)
			}
		}
	}
}
