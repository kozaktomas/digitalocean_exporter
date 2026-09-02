# Development

```bash
git clone https://github.com/kozaktomas/digitalocean_exporter.git
cd digitalocean_exporter
make check
```

Go 1.26 or newer. Nothing else is required to build and test — `make smoke` runs the whole
exporter against a stub API, so you do not need a DigitalOcean token to work on it.

## Make targets

| Target | What it does |
|---|---|
| `make build` | Compile the binary, stamping version and commit |
| `make check` | The quality gate: `fmt-check`, go vet, golangci-lint, tests, race detector |
| `make fmt` | Format the Go sources in place |
| `make fmt-check` | List the unformatted files and fail, changing nothing |
| `make test` | Tests with coverage |
| `make test-race` | Tests under the race detector |
| `make smoke` | End-to-end run against a stub API, no token needed |
| `make alerts-lint` | Check the bundled alerting rules with promtool |
| `make chart-lint` | `helm lint` and `helm template` the chart |
| `make chart-docs` | Regenerate the chart README from `values.yaml` |
| `make docs` | Build the documentation site into `site/` |
| `make docs-serve` | Serve the documentation at <http://localhost:8000> with live reload |
| `make snapshot` | Dry-run the release: binaries, deb, tarballs |
| `make docker` | Build the multi-arch image |

**`make check` must pass before every commit.** The lint configuration is strict on
purpose. The race detector is part of the gate because CI runs it, and a collector that fans
out over buckets or droplets can race in its own test stub in a way the plain test run does
not notice. CI runs it exactly once: `make check` ends with it, so there is no separate step.

The gate checks the formatting with `gofmt -l` and refuses to touch the tree — `gofmt -w`
would quietly reformat the workspace in CI, leaving every later step to see clean files and
the job to pass over code that was never formatted in the repository. `make fmt` is the one
that rewrites, and it is for local work.

## The one architectural rule

**Refreshing is separate from scraping.** Every collector implements
`internal/collector.Collector`:

- `Refresh(ctx)` does the I/O and replaces an in-memory snapshot. The scheduler calls it on
  the collector's own interval — never a scrape.
- `Collect(ch)` reads that snapshot and **must never perform I/O**.

This exists because the DigitalOcean API is slow and rate-limited, and because a collector
that fans out over every droplet takes far past any scrape timeout. Do not add a collector
that calls the API from `Collect`.

Two rules follow:

- **A failed refresh keeps the previous snapshot** and sets `collector_success` to 0.
  Never drop metrics on error.
- **Build the whole new snapshot before swapping it in**, behind a mutex, so a partial
  failure changes nothing.
- **A panic is just a failed refresh.** The scheduler recovers panics raised by `Refresh`,
  logs the value and the stack, and records `collector_success 0`; the collector keeps its
  previous snapshot and is refreshed again on schedule. One unexpected API response must
  not take the other collectors down with it.

A collector that measures many things at once — buckets, droplets — isolates them: one that
fails keeps its own previous values, reports its own `_up 0`, and logs why, since that
failure never reaches the scheduler. It must not cost the ones that succeeded.

## Adding a collector

1. New package under `internal/collector/<name>/`.
2. Implement the four methods, following the rules above.
3. Add `--collector.<name>` and `--collector.<name>.interval` in `internal/config`.
4. Register it in `cmd/digitalocean_exporter/main.go`. `Register` takes a per-collector
   timeout; pass 0 — meaning `--do.timeout` — unless the refresh fans out over many
   resources, as `spaces` and the two monitoring-API collectors do.
5. Tests: drive `Refresh` against an `httptest` server and compare `Collect` against golden
   exposition text with `testutil.CollectAndCompare`. Cover the failure path.
6. Add it to the chart: `values.yaml` — with a `# --` comment, which is what generates the
   [values reference](helm/values.md) — and the `args` block in `templates/deployment.yaml`,
   remembering the `--no-collector.<name>` branch.
7. Document it: `docs/metrics.md` for the metrics, and
   [`docs/configuration/collectors.md`](configuration/collectors.md) for what it costs and
   when to turn it off. Every page has to be in the `nav` of `mkdocs.yml`.
8. If a metric is worth watching, put it on a dashboard in
   `charts/digitalocean-exporter/dashboards/` and add a row to
   [`docs/dashboards.md`](dashboards.md) — a dashboard no page mentions ships invisibly, and
   the test says so.
9. If it is worth waking somebody, add a rule to
   `charts/digitalocean-exporter/alerts/digitalocean.rules.yaml` and a row to
   [`docs/alerting.md`](alerting.md). `make alerts-lint` runs promtool over the file.

## Naming traps

- The binary and repository use an **underscore** (`digitalocean_exporter`); the deb package
  and the Helm chart use a **hyphen** (`digitalocean-exporter`), because Debian rejects
  underscores in package names.
- The `.gitignore` entry for the built binary must stay **anchored** (`/digitalocean_exporter`).
  Unanchored it also matches the `cmd/digitalocean_exporter` directory and silently excludes
  `main.go` from the repository.
- Collector switches are kingpin booleans: `--no-collector.<name>` disables one, and
  `--collector.<name>=false` is a parse error that crashes the process at startup. The
  environment variable does take a value.
- Some metric names deliberately lack an `account_` infix
  (`digitalocean_month_to_date_usage`). They match an older, widely deployed exporter so
  dashboards survive a migration. Do not "fix" them.

## Working on the documentation

```bash
python3 -m venv .venv
.venv/bin/pip install -r requirements-docs.txt
make docs-serve
```

Sources are markdown under `docs/`, built with
[MkDocs Material](https://squidfunk.github.io/mkdocs-material/). `make docs` builds with
`--strict`, so a broken link or a page missing from the nav fails the build — CI runs the
same thing on every pull request.

The [values reference](helm/values.md) is **generated**. Edit the `# --` comments in
`charts/digitalocean-exporter/values.yaml`, run `make chart-docs`, and commit the result;
CI fails if the generated file is out of date.

## Releasing

Tag it. Everything else follows.

```bash
git tag v0.3.0
git push origin v0.3.0     # the tag first
git push origin main       # then the branch
```

That runs goreleaser (binaries, tarballs, deb packages, GitHub Release), packages the chart
at the same version and attaches it to the release, then publishes the documentation for
`0.3` and repoints `latest` at it and regenerates the chart repository index.

**Push the tag before the branch.** Pages identifies a deployment by commit SHA and keeps the
first one it saw, and a release tags the commit that bumps the chart version — so a branch
push that goes first would publish `dev` from that SHA and leave the release's own deployment
reporting success over an unchanged site. The workflow enforces the order rather than
trusting it: a push to a branch whose commit already carries a `vX.Y.Z` tag stands down and
leaves the publishing to the release. The tag must be plain `vMAJOR.MINOR.PATCH`; anything
else is rejected.

Semantic versioning, with one number for everything: exporter, chart, `appVersion` and the
documentation directory. While the project is `0.x`, a minor bump is allowed to break
compatibility.

!!! warning "One repository setting the release depends on"

    Publishing Pages from a release means deploying from a **tag**, and the `github-pages`
    environment only accepts the refs it is told to. Under *Settings → Environments →
    github-pages → Deployment branches and tags* there has to be a rule of type **Tag**
    matching `v*.*.*`, alongside the `main` branch rule.

    Without it the release's deploy job fails before its first step, with no log to explain
    why — the documentation and the chart asset are published, but the site is never
    refreshed. Adding the rule as a *branch* pattern looks identical in the list and does
    not work; the ref type has to be Tag.

    The recovery, if a release ever lands without it: run the Pages workflow by hand from
    `main` with `ref` set to the tag, `version` to the minor and `alias` to `latest`.

How that pipeline is put together, and why, is written up in
[the design note](design/2026-08-25-documentation-site-and-chart-repository.md).
