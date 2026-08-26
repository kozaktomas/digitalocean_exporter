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

This exists because the DigitalOcean API is slow and rate-limited (5000 requests/hour), and
because a collector that fans out over every droplet takes far past any scrape timeout. Do
not add a collector that calls the API from `Collect`.

A collector that measures many things at once — buckets, say — isolates them: one that
fails keeps its own previous values and reports its own `_up 0`, and logs why, since that
failure never reaches the scheduler. It must not cost the ones that succeeded.

**A failed refresh keeps the previous snapshot** and sets `collector_success` to 0. Never
drop metrics on error: a gap in a graph reads as DigitalOcean itself going away, which is a
different incident from "the exporter cannot reach the API".

Before its first successful refresh a collector emits nothing, rather than zeros.

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

## The dashboards are held against the collectors

`cmd/digitalocean_exporter/dashboards_test.go` extracts every PromQL expression from the
bundled dashboards and checks each `digitalocean_` metric against the descriptors the
collectors register — through `registerCollectors`, so the list cannot fall behind. Renaming
or dropping a metric that a dashboard uses fails `make check` rather than quietly emptying a
panel.

The files are committed in a normalised form: no `id` or `version`, no remembered variable
selections, sorted keys. After replacing one with an export from Grafana, run
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

- `make check` (gofmt, go vet, golangci-lint, tests, race detector) must pass before every
  commit. Touching the chart also means `make chart-docs`; touching `docs/` means
  `make docs`. The lint configuration is strict on purpose; do not soften it. The race detector
  is part of the gate because CI runs it and a collector that fans out over buckets can
  race in its own test stub, which the plain test run will not notice.
- `make smoke` runs the exporter end to end against a stub API and needs no token.
- **Commit and push straight to `main`.** The project is in its development phase: no feature
  branches, no pull requests. `make check` still has to pass first.
- Everything committed here is in English, and the repository must stay self-contained:
  no local filesystem paths, no references to unrelated projects or internal hosts.
