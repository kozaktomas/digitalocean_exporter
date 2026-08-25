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
because measuring a Spaces bucket means listing every object in it — minutes of work, far
past any scrape timeout. Do not add a collector that calls the API from `Collect`.

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
   timeout; pass 0 unless the refresh is far slower than an API call, as listing a Spaces
   bucket is.
5. Tests: drive `Refresh` against an `httptest` server, compare `Collect` against golden
   exposition text with `testutil.CollectAndCompare`. Cover the failure path.
6. Document the metrics in `docs/metrics.md`.

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
  commit. The lint configuration is strict on purpose; do not soften it. The race detector
  is part of the gate because CI runs it and a collector that fans out over buckets can
  race in its own test stub, which the plain test run will not notice.
- `make smoke` runs the exporter end to end against a stub API and needs no token.
- **Commit and push straight to `main`.** The project is in its development phase: no feature
  branches, no pull requests. `make check` still has to pass first.
- Everything committed here is in English, and the repository must stay self-contained:
  no local filesystem paths, no references to unrelated projects or internal hosts.
