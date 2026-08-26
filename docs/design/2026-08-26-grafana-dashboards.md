# Grafana dashboards — design

Status: proposed on 2026-08-26. Date: 2026-08-26.

## Why the exporter ships dashboards

An exporter's real contract is its metric names, and the fastest way to find out whether
that contract holds is to point a dashboard at it. Seven dashboards already exist and run
against a live account; without shipping them, every operator who installs this exporter
rebuilds the same panels from the table in [Metrics](../metrics.md).

Shipping them also closes a loop the repository does not currently close. A collector can be
renamed, a metric can lose a label, and nothing fails — the metric is simply absent from a
graph nobody has built yet. Once the dashboards live here, a test can hold them against the
descriptors the exporter actually registers, and a rename becomes a failed build rather than
an empty panel discovered months later.

Two more are added here, for `balance`, `certificates` and `firewalls`, which no existing
dashboard touches. That brings the set to nine, one covering every collector.

## Where the files live

The dashboards are committed to `charts/digitalocean-exporter/dashboards/`, inside the chart
rather than at the repository root.

Helm can only package files that sit inside the chart directory: `.Files.Glob` does not read
above it, and `helm package` does not reliably follow symlinks out of it. A canonical copy
inside the chart is therefore the only arrangement with exactly one copy of each file.

Two alternatives were rejected. Keeping the canonical files at the repository root and
copying them into the chart at package time means `helm lint` and `helm template` render
nothing locally until someone remembers to run the copy, and the alternative — committing
both copies and adding a CI step that compares them — is machinery bought purely to make a
path look nicer. Symlinking from the chart into a root directory is worse: packaging through
symlinks is unreliable, and this repository already carries one symlink trap, since GitHub
Pages does not follow them either.

The documentation links directly at the chart directory, so an operator who does not use
Helm still has one place to fetch the JSON from.

## The dashboards

| File | UID | Title | Collectors it needs |
|---|---|---|---|
| `overview.json` | `digitalocean-overview` | DigitalOcean / Overview | `account`, `droplets`, `kubernetes`, `loadbalancers`, `databases`, `cdn`, `domains`, `registry`, `spaces` |
| `droplets.json` | `digitalocean-droplets` | DigitalOcean / Droplets | `droplets`, `dropletmetrics` |
| `kubernetes.json` | `digitalocean-kubernetes` | DigitalOcean / Kubernetes | `kubernetes` |
| `loadbalancers.json` | `digitalocean-loadbalancers` | DigitalOcean / Load Balancers | `loadbalancers`, `loadbalancermetrics` |
| `storage.json` | `digitalocean-storage` | DigitalOcean / Storage | `volumes`, `registry` |
| `spaces.json` | `digitalocean-spaces` | DigitalOcean / Spaces | `spaces` |
| `exporter.json` | `digitalocean-exporter` | DigitalOcean / Exporter | none; self-metrics only |
| `billing.json` | `digitalocean-billing` | DigitalOcean / Billing | `balance`, `droplets`, `loadbalancers`, `volumes`, `registry` |
| `security.json` | `digitalocean-security` | DigitalOcean / Security | `certificates`, `firewalls` |

A dashboard whose collector is off shows empty panels rather than breaking, which is why all
nine ship together and the chart has one switch rather than nine.

## The committed form

Grafana's API returns `{"meta": …, "dashboard": …}`. Only `.dashboard` is committed, and two
of its fields are dropped:

- `id` — the numeric primary key of the instance the dashboard was exported from. It means
  nothing anywhere else and collides on import.
- `version` — the instance's edit counter. Keeping it produces a one-line diff on every
  re-export that carries no information.

Each templating variable's `current` is reset, so that whatever the exporting instance
happened to have selected — a bucket name, a droplet name — cannot reach the repository.

The file is then written with sorted keys, two-space indentation and a trailing newline. The
sorting is what makes a re-export reviewable: Grafana does not guarantee key order, so
without it a one-panel edit arrives as a diff touching most of the file.

A test asserts that each committed file equals its own normalised re-encoding. Somebody
will eventually edit a dashboard in Grafana, export it and paste it in; that gets a red
build with a clear reason instead of an unreadable diff.

## Portability rules

These already hold for the seven existing dashboards. The test enforces them for all nine,
so a later addition cannot quietly hard-code an environment.

- Every `datasource` reference is `${datasource}`, resolved by a `datasource` variable of
  type `prometheus`. No dashboard names a datasource UID.
- A `job` variable is populated from `label_values(digitalocean_exporter_build_info, job)`,
  multi-select with an `All` option, and every query filters on `job=~"$job"`. Two exporters
  scraped by one Prometheus stay separable.
- `uid` is `digitalocean-<file name>`, so links and bookmarks survive a re-import.
- `tags` contains `digitalocean`, which is what the cross-dashboard dropdown selects on: each
  dashboard carries a `dashboards`-type link filtered to that tag, so the nine navigate
  between each other without hard-coded URLs.

## Helm delivery

A new template, `templates/dashboards.yaml`, ranges over `.Files.Glob "dashboards/*.json"`
and renders one ConfigMap per dashboard, named
`<fullname>-dashboard-<name>`, each labelled for the Grafana sidecar to pick up. One
ConfigMap per dashboard rather than one for all nine: it keeps each one under the 1 MiB
object limit with room to spare, and lets the folder annotation differ per dashboard later
if that is ever wanted.

| Value | Default | Meaning |
|---|---|---|
| `grafana.dashboards.enabled` | `false` | Render the ConfigMaps at all |
| `grafana.dashboards.label` | `grafana_dashboard` | Label key the Grafana sidecar watches |
| `grafana.dashboards.labelValue` | `"1"` | Value of that label |
| `grafana.dashboards.folder` | `""` | When set, adds a `grafana_folder` annotation |

Disabled by default, for the same reason `serviceMonitor.enabled` is: a cluster without the
Grafana sidecar would get nine ConfigMaps that nothing ever reads.

`grafana.dashboards.folder` **only takes effect if the Grafana side is configured for it** —
the sidecar has to run with `folderAnnotation: grafana_folder` and
`provider.foldersFromFilesStructure: true`. The chart cannot check that, so the value's
documentation has to say so plainly; set without the matching sidecar configuration, the
annotation is inert and the dashboards land in the sidecar's default folder.

`make chart-lint` gains a second `helm template` run with `--set
grafana.dashboards.enabled=true`. Without it CI would never render the branch that matters,
since the default leaves it switched off.

## The guard against drift

`cmd/digitalocean_exporter/dashboards_test.go`, in `package main` so that it can call
`registerCollectors` directly. That is the whole point: the list of metrics the test checks
against is built by the same function `run` uses, so it cannot fall behind when a collector
is added.

The inventory is assembled by:

1. Building a config with every collector enabled and passing it to `registerCollectors`
   along with a client built on a dummy token. No I/O happens — `Describe` never touches the
   client.
2. Draining `Scheduler.Describe`, which fans out to every registered collector.
3. Capturing the exporter's own metrics through a `prometheus.Registerer` that records each
   collector handed to it, and describing those too. This covers `build_info` from
   `internal/version`, the `collector_*` gauges the scheduler registers, and the `api_*`
   metrics from `internal/doclient` — without any change to production code.

From each dashboard the test extracts PromQL from `panels[].targets[].expr`, recursing into
the `panels` of a row, and from `templating.list[].query`, which Grafana writes either as a
string or as an object with a `query` field. Every `digitalocean_[a-z0-9_]+` identifier in
those strings must be in the inventory. A name that is not found directly is retried with a
`_bucket`, `_sum` or `_count` suffix stripped, so a histogram would not need a special case.

The same test asserts the portability rules and the normalised form. It runs as part of
`make check`; no new CI job is needed.

What this does not catch is a query that references live metrics and still asks the wrong
question. Nothing automated will; that is what review and the live check below are for.

## The Billing dashboard

Everything the exporter knows about what the account costs, which is currently spread across
`balance`, `droplets`, `loadbalancers`, `volumes` and `registry`.

- Current balance, month-to-date balance and month-to-date usage as stats, with month-to-date
  usage also plotted over the range so that a jump has a visible date.
- `sum(digitalocean_droplet_price_monthly)` as the account's droplet run-rate, and a table
  per droplet ordered by price — the answer to "what is the expensive one".
- `sum(digitalocean_loadbalancer_size_units)`, the units load balancers are billed for.
- `sum(digitalocean_volume_size_bytes)`, block storage billed by allocated size.
- Registry: `digitalocean_registry_subscription_monthly_price_usd`, and
  `digitalocean_registry_storage_usage_bytes` against
  `digitalocean_registry_storage_included_bytes`, which is where an overage starts.
- `digitalocean_balance_generated_at` rendered as age, because DigitalOcean generates these
  figures periodically rather than on request, and a stale number read as current is a
  wrong answer rather than a missing one.

The balance panels carry a description saying that this collector needs a token with the
billing scope, and that with a resource-scoped token they are empty while
`digitalocean_exporter_collector_success{collector="balance"}` reads 0. That distinction is
already documented in [Metrics](../metrics.md); a panel that looks broken should say which
of the two it is.

## The Security dashboard

The `certificates` and `firewalls` collectors have no dashboard at all today, and both are
off by default, so nothing currently makes the case for switching them on.

- Certificates as a table with days remaining,
  `(digitalocean_certificate_expiry_timestamp_seconds - time()) / 86400`, thresholded by
  colour, plus a stat counting those under 30 days. This is the panel that catches a
  `lets_encrypt` renewal that failed quietly, which is the reason the collector exists.
- `digitalocean_certificate_info` for `type` and `state`, and
  `digitalocean_certificate_dns_names` for coverage.
- Firewalls as a table: `digitalocean_firewall_inbound_rules` and `_outbound_rules`,
  `digitalocean_firewall_droplets` and `digitalocean_firewall_tags` for what each covers.
- `digitalocean_firewall_inbound_rules_open` as a stat with the change over a day beside it.
  The panel description has to make the point [Metrics](../metrics.md) already makes: the
  absolute number is not an alert, a public web server needs one, but an unexplained change
  in it is worth knowing about.
- `digitalocean_firewall_pending_changes` as a stat. Non-zero for more than seconds means
  the firewall is not protecting what its ruleset claims.

Both collectors' panels say in their descriptions that the collector is off by default.

## Verification

Beyond the test, the PromQL of every new panel is run against a live Prometheus holding this
exporter's data, as read-only instant queries, before the dashboard is committed. Where the
collector is enabled that shows real values; where it is not — `certificates` and
`firewalls` typically — it still proves the query parses and returns an empty result rather
than an error.

The seven ported dashboards need no such check: they are already running against live data,
which is where they come from.

## Documentation

A new page, `docs/dashboards.md`, added to the `nav` in `mkdocs.yml` after Metrics. It
covers what each of the nine shows, which collectors each needs, both ways to install them —
importing the JSON by hand, and the chart's ConfigMaps — and what the `datasource` and `job`
variables are for.

[Kubernetes and Helm](../install/kubernetes.md) and the [chart page](../helm/index.md) get a
pointer to it. The chart's own values reference is generated, so the `# --` comments on the
four new values are the documentation there, and `make chart-docs` has to be run before
committing or CI fails on a stale README.

`CLAUDE.md` gains a step in "Adding a collector": a metric worth watching belongs on a
dashboard too, and the guard exists and will fail the build if a dashboard names a metric
that no longer exists.

## Out of scope

- **Alert rules.** A `PrometheusRule` template shipping the alerts that
  [Metrics](../metrics.md) already writes out in PromQL is an obvious companion to this, and
  a separate piece of work: alerts need routing, severities and a story about what an
  operator is expected to do, none of which a dashboard needs.
- **Publishing to grafana.com.** Import-by-ID is convenient, but every change would need a
  manual upload step that no CI job can perform, and the catalogue entry would drift from the
  repository the first time somebody forgot.
- **A release asset.** The chart already carries the dashboards, and the repository serves
  them; a third copy tied to a tag is one more thing to keep in step.
- **Per-dashboard switches in the chart.** Nine more values to document so that an operator
  can avoid ConfigMaps that cost nothing when unused.
- **Screenshots in the documentation.** They go stale silently, and the metric tables plus
  the dashboards themselves already say what is on them.
