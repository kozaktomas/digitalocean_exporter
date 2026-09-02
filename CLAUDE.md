# CLAUDE.md — digitalocean_exporter

Orientation for an AI session working in this repository.

## What this is

A Prometheus exporter for DigitalOcean, written in Go. One binary, no state, no database.
It reads the DigitalOcean API and exposes metrics on `:9212`.

## The API token is read-only — never mutate anything

`.env` holds a real DigitalOcean token (`DIGITALOCEAN_TOKEN`) for a live account. It is
gitignored and must stay that way; never print it, commit it, or send it anywhere except
the DigitalOcean API.

**Only ever issue read operations against the DigitalOcean API — `GET` (and `HEAD`) and
nothing else.** Never `POST`, `PUT`, `PATCH` or `DELETE`; never create, resize, power-cycle,
tag, rename or destroy a droplet, volume, snapshot, database, Kubernetes cluster, load
balancer, domain record, firewall, project, bucket or object; never upload to or delete
from Spaces. This applies to `curl`, `doctl`, the godo client and anything else. If a task
seems to need a write, stop and ask instead of trying it.

This is an exporter: it observes. There is no legitimate reason for any code path or any
ad-hoc command in this repository to change the state of the account.

## The one architectural idea

**Refreshing is separate from scraping.** Every collector implements
`internal/collector.Collector`:

- `Refresh(ctx)` does the I/O and replaces an in-memory snapshot. The scheduler calls it on
  the collector's own interval, never a scrape.
- `Collect(ch)` reads that snapshot and must never perform I/O.

This exists because the DigitalOcean API is slow and rate-limited (5000 requests an hour,
250 a minute), and because a collector that fans out over every droplet takes far past any
scrape timeout. Do not add a collector that calls the API from `Collect`.

**The burst limit is defended in two places, and a new collector inherits both.** Every
request goes through the one instrumented transport in `internal/doclient`, which paces
requests at `--do.rate-limit` across all collectors at once and retries a 429, a 5xx or a
broken connection up to three attempts; each attempt is counted separately, because each one
spends from the budget. It is counted under the collector that made it: the scheduler puts
the name on the refresh's context with `doclient.WithCollector`, and the transport reads it
back, because nothing about a request identifies its caller — `limits` and `droplets` share
a path, as do both monitoring collectors. A `Retry-After` is waited out in full, and skipped rather than
shortened when it does not fit the caller's deadline; a 429 that reports the hourly budget
spent and names no wait is not retried at all, because nothing frees it before the hour
turns. The scheduler then offsets each collector's first refresh by an
even share of one window shared by the whole set — the shortest interval among the
registered collectors, capped at three seconds — so the offsets stay distinct however the
intervals differ, the set never fires as one burst, and every later refresh keeps that
phase. Neither needs anything from a collector, and
neither belongs inside one.

A collector that measures many things at once — buckets, say — isolates them: one that
fails keeps its own previous values and reports its own `_up 0`, and logs why, since that
failure never reaches the scheduler. It must not cost the ones that succeeded.

**A failed refresh keeps the previous snapshot** and sets `collector_success` to 0. Never
drop metrics on error: a gap in a graph reads as DigitalOcean itself going away, which is a
different incident from "the exporter cannot reach the API".

Before its first successful refresh a collector emits nothing, rather than zeros. That is
what readiness is made of: `/readyz` answers 503, naming the collectors still waiting,
until every enabled one has refreshed successfully once, and 200 from then on even if one
later fails — by then the pod has values worth serving, and dropping it out of the Service
would stop the scrape that reports the failure. `/healthz` is the liveness probe and never
consults a collector.

**A panic in a `Refresh` is a failed refresh, not a dead process.** The scheduler recovers
it, logs the value and the stack, and records it exactly like any other failure — sixteen
collectors share one process, and one unexpected API response must not stop the other
fifteen, restart after restart. A refresh cut short by shutdown is the opposite case: once
the run context is cancelled the error is not recorded and not logged as one, so the last
lines before a restart do not read as an API outage.

## Adding a collector

1. New package under `internal/collector/<name>/`.
2. Implement the four methods. Keep the snapshot behind a mutex; build the whole new
   snapshot before swapping it in, so a partial failure changes nothing.
3. Add `--collector.<name>` and `--collector.<name>.interval` in `internal/config`.
4. Register it in `cmd/digitalocean_exporter/main.go`. `Register` takes a per-collector
   timeout; pass 0 unless the refresh fans out over many resources, as the monitoring-API
   collectors do.
5. Tests: drive `Refresh` against an `httptest` server, compare `Collect` against golden
   exposition text with `testutil.CollectAndCompare`. Cover the failure path.
6. Add it to the chart: `values.yaml` and the `args` block in `templates/deployment.yaml`,
   remembering the `--no-collector.<name>` branch. Every value needs a `# --` comment —
   that is what generates the values reference; a plain `#` comment is invisible to it.
   Then `make chart-docs`, and commit the regenerated `charts/*/README.md`. CI fails if it
   is stale.
7. Document it: the metrics in `docs/metrics.md`, and what it costs and when to switch it
   off in `docs/configuration/collectors.md` (or its own page, as `spaces` and the
   monitoring-API collectors have). Every page must be in the `nav` of `mkdocs.yml`, and
   `make docs` builds with `--strict`, so a broken link fails.
8. If a metric is worth watching, put it on a dashboard in
   `charts/digitalocean-exporter/dashboards/` and add a row to `docs/dashboards.md`. A
   dashboard that no page mentions ships invisibly, and the test says so.
9. If it is worth waking somebody, add a rule to
   `charts/digitalocean-exporter/alerts/digitalocean.rules.yaml` and a row to
   `docs/alerting.md`. Every alert needs a `severity` of `critical`, `warning` or `info`, a
   `summary` and a `description`; `make alerts-lint` runs promtool over the file.

## The dashboards are held against the collectors

`cmd/digitalocean_exporter/dashboards_test.go` extracts every PromQL expression from the
bundled dashboards and checks each `digitalocean_` metric against the descriptors the
collectors register — through `registerCollectors`, so the list cannot fall behind. Renaming
or dropping a metric that a dashboard uses fails `make check` rather than quietly emptying a
panel.

The alerting rules get the same treatment, plus a check that every `{{ $labels.x }}` in an
annotation is a label the alert's own metrics carry — a mistyped one renders as a blank in
Alertmanager and is invisible until it pages somebody.

The dashboard files are committed in a normalised form: no `id` or `version`, no remembered
variable selections, sorted keys. After replacing one with an export from Grafana, run
`go test ./cmd/digitalocean_exporter -run TestDashboardsAreNormalised -update.dashboards`
and commit what it writes. Never hand-edit a datasource UID into a dashboard; every one of
them resolves through the `${datasource}` variable, and a test enforces that.

## Publishing

One GitHub Pages site carries two things, and they have separate owners:

- **Versioned documentation**, owned by `mike`, which stores every built version on the
  `docs-versions` branch. Pages is *not* served from that branch — it stays on
  `build_type: workflow`, and the deploy job assembles the site from it.
- **The Helm chart repository** (`index.yaml` + `charts/*.tgz`), which is regenerated from
  the GitHub Release assets on every deploy and never committed. Do not add a step that
  commits `index.yaml`; the whole point is that it cannot drift from the releases.

A release is cut by pushing a tag. `release.yml` packages the chart at the tag's version,
hands it to goreleaser as a release asset, then calls `pages.yml`. Nothing else is manual.
The tag must be plain `vMAJOR.MINOR.PATCH` — the workflow rejects anything else, because
one number drives the binary, the chart, the `appVersion` and the documentation directory.

**Push the tag before pushing the branch.** GitHub Pages identifies a deployment by commit
SHA and keeps the first one it saw for that SHA; a later deployment from the same commit
reports success and changes nothing. A release tags the commit that bumps the chart version,
and that commit also touches `charts/` and `docs/`, so pushing the branch first lets the
push-triggered Pages run publish `dev` from that SHA — after which the release's own run
cannot publish anything. Pushing the tag first makes the release's deployment the one Pages
keeps, and the branch push that follows becomes the harmless no-op instead.

    git push origin v0.3.0
    git push origin main

This is enforced rather than merely written down: a push to a *branch* whose commit already
carries a `vX.Y.Z` tag makes `pages.yml` stand down, leaving the publish to the release —
the release's own run of `pages.yml` arrives on the tag and publishes. Push the tag first
and the guard does the rest. `pages.yml` then verifies after deploying that the site
really serves the version it just published, so getting the order wrong fails the workflow
instead of going green over an unchanged site.

`docs/design/2026-08-25-documentation-site-and-chart-repository.md` explains why it is
arranged this way.

## Naming traps

- Binary and repository use an underscore (`digitalocean_exporter`); the **deb package and
  the Helm chart use a hyphen** (`digitalocean-exporter`). Debian rejects underscores in
  package names.
- The `.gitignore` entry for the built binary must stay anchored (`/digitalocean_exporter`).
  Unanchored it also matches the `cmd/digitalocean_exporter` directory and silently
  excludes `main.go` from the repository.
- Collector switches are kingpin booleans: a collector is disabled with
  `--no-collector.<name>`, never `--collector.<name>=false`, which is a parse error and
  crashes the process at startup. The environment variable does take a value
  (`COLLECTOR_BALANCE=false`), and the Helm chart must render the negated flag.
- Some metric names deliberately lack an `account_` infix
  (`digitalocean_month_to_date_usage`). They match an older, widely deployed exporter so
  dashboards survive a migration. Do not "fix" them.

## Working rules

- `make check` (`gofmt -l`, go vet, golangci-lint, tests, race detector) must pass before every
  commit. Touching the chart also means `make chart-docs`; touching `docs/` means
  `make docs`. The lint configuration is strict on purpose; do not soften it. The race detector
  is part of the gate because CI runs it and a collector that fans out over buckets can
  race in its own test stub, which the plain test run will not notice. The gate only reports
  formatting (`make fmt-check`) and never rewrites; `make fmt` is the one that formats in
  place, for local work.
- `make smoke` runs the exporter end to end against a stub API and needs no token.
- **Commit and push straight to `main`.** The project is in its development phase: no feature
  branches, no pull requests. `make check` still has to pass first.
- Everything committed here is in English, and the repository must stay self-contained:
  no local filesystem paths, no references to unrelated projects or internal hosts.
