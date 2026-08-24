# CLAUDE.md — digitalocean_exporter

Orientation for an AI session working in this repository.

## What this is

A Prometheus exporter for DigitalOcean, written in Go. One binary, no state, no database.
It reads the DigitalOcean API and exposes metrics on `:9212`.

## The one architectural idea

**Refreshing is separate from scraping.** Every collector implements
`internal/collector.Collector`:

- `Refresh(ctx)` does the I/O and replaces an in-memory snapshot. The scheduler calls it on
  the collector's own interval, never a scrape.
- `Collect(ch)` reads that snapshot and must never perform I/O.

This exists because the DigitalOcean API is slow and rate-limited (5000 requests/hour), and
because measuring a Spaces bucket means listing every object in it — minutes of work, far
past any scrape timeout. Do not add a collector that calls the API from `Collect`.

**A failed refresh keeps the previous snapshot** and sets `collector_success` to 0. Never
drop metrics on error: a gap in a graph reads as DigitalOcean itself going away, which is a
different incident from "the exporter cannot reach the API".

Before its first successful refresh a collector emits nothing, rather than zeros.

## Adding a collector

1. New package under `internal/collector/<name>/`.
2. Implement the four methods. Keep the snapshot behind a mutex; build the whole new
   snapshot before swapping it in, so a partial failure changes nothing.
3. Add `--collector.<name>` and `--collector.<name>.interval` in `internal/config`.
4. Register it in `cmd/digitalocean_exporter/main.go`.
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
- Some metric names deliberately lack an `account_` infix
  (`digitalocean_month_to_date_usage`). They match an older, widely deployed exporter so
  dashboards survive a migration. Do not "fix" them.

## Working rules

- `make check` (gofmt, go vet, golangci-lint, tests) must pass before every commit. The
  lint configuration is strict on purpose; do not soften it.
- `make smoke` runs the exporter end to end against a stub API and needs no token.
- Everything committed here is in English, and the repository must stay self-contained:
  no local filesystem paths, no references to unrelated projects or internal hosts.
