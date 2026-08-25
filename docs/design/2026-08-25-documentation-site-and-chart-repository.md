# Documentation site and chart repository — design

Status: implemented on 2026-08-25. Date: 2026-08-25.

## The problem

The Helm chart is published only as an OCI artifact, to `ghcr.io/kozaktomas/chart`. That
works for `helm install`, but it is not what most Terraform looks like in practice:

```hcl
resource "helm_release" "loki" {
  chart      = "loki"
  repository = "https://grafana.github.io/helm-charts"
  version    = "6.55.0"
}
```

A `repository` of that shape needs a classic HTTP chart repository: a URL serving an
`index.yaml` that lists every published version and points at a `.tgz` for each. Nothing
in the project produced one.

The second half of the problem is documentation. Everything lived in `README.md`, which had
grown to a 200-line configuration table and was still missing the parts that actually cost
people time — what each collector costs in API requests, how the two secret-handling modes
differ, what to do when the balance collector returns 403.

## What the ecosystem actually does

Worth checking before inventing something, since the two obvious reference points are
public.

`grafana.github.io/helm-charts` is a **pure chart repository**. Its `gh-pages` branch holds
four entries: `README.md`, `index.yaml`, a `packages/` directory and `pubkey.gpg`. No
documentation of any kind. The chart tarballs are not even hosted there — `index.yaml`
points at GitHub Release assets:

```
- https://github.com/grafana/helm-charts/releases/download/alloy-1.12.0/alloy-1.12.0.tgz
```

`prometheus-community/helm-charts` follows the same pattern: `README.md`,
`artifacthub-repo.yml`, `changelog/`, `index.yaml`, `pubkey.gpg`.

Grafana's versioned documentation is a separate system on a separate domain — Hugo, served
from `grafana.com/docs/loki/latest/…` and `grafana.com/docs/loki/v3.5.x/…`. They can afford
two pipelines and two hosts.

This project has one GitHub Pages site, so the two concerns have to share it. They can:
Helm only ever fetches `index.yaml` from the repository root, and a documentation site's
root document is `index.html`. Different names, no collision.

One detail from Grafana's release workflow is worth recording, because it is the failure
mode of the branch-based approach: they run `chart-releaser` with `skip_upload: true` and
regenerate `index.yaml` themselves, because the action's built-in index push creates an
**unsigned** commit and their ruleset rejects it. Committed index state is a thing that goes
wrong.

## Shape

Three concerns, one owner each, meeting only at deploy time.

| Concern | Owner | Where it lives |
|---|---|---|
| Versioned documentation | `mike` | branch `docs-versions`, built HTML per version |
| Chart tarballs | `goreleaser` | GitHub Release assets |
| `index.yaml` | the Pages workflow | nowhere — regenerated on every deploy |

The resulting site:

```
https://kozaktomas.github.io/digitalocean_exporter/
├── index.html      → redirects to latest/          (mike set-default)
├── versions.json   → version selector data         (mike)
├── latest/  0.2/  0.1/  dev/                       (mike, alias_type: copy)
├── index.yaml      → Helm repository index         (generated at deploy time)
└── charts/digitalocean-exporter-0.2.0.tgz          (downloaded from releases)
```

### Why `index.yaml` is derived and never committed

The chart index is a pure function of the set of published releases. Regenerating it from
`gh release download` on every deploy means it cannot drift from what actually exists, it
repairs itself if a deploy is interrupted halfway, and the whole site can be rebuilt from
scratch with a `workflow_dispatch` run. Committing it would add a second source of truth
about which versions exist, and the two would eventually disagree.

When no releases exist yet, `helm repo index` over an empty directory produces a valid
index with no entries. `helm repo add` succeeds against it, which keeps the bootstrap
case from being a special case.

### Why documentation state is committed, and `mike` owns it

Documentation is the opposite: it is not derivable from anything cheap. Rebuilding every
historical version on every deploy would mean each old version's docs have to keep building
forever with a current MkDocs — the exact fragility `mike` exists to avoid. Its premise is
that once a version is built it is never touched again.

So `mike` gets a branch of its own and owns it exclusively. Nothing else is committed there,
which removes the interference that the branch-based approach usually suffers from.

### Why the branch is not called `gh-pages`

GitHub Pages stays on `build_type: workflow`. The branch is `mike`'s storage, not the thing
being served — a deploy job assembles the site from it. Calling it `gh-pages` would suggest
the opposite. `docs-versions` says what it is.

### Why `alias_type: copy`

`mike` defaults to `alias_type: symlink`, which does not survive GitHub Pages: the static
server does not follow symlinks, so `latest/` would 404. `copy` makes the alias a real
directory of pages. `canonical_version: latest` then tells search engines which of the
identical copies is the one to index.

## Versioning

Semver, with chart version, `appVersion` and the documentation directory all locked to the
git tag. `v0.2.0` means binary 0.2.0, chart 0.2.0 and docs under `/0.2/`. One number to pin
in `helm_release`, and the documentation read next to it is guaranteed to describe that
build.

The cost is that a chart-template-only fix requires a patch release of the whole project.
That is cheap — goreleaser rebuilds the same source — and it beats two version streams that
drift apart.

Documentation is versioned per **minor**, following `mike`'s own recommendation, with the
content coming from the highest patch of that minor. Patch releases do not multiply
documentation versions.

`latest` tracks the highest tag **by semver, not by date**, so a patch published against an
older minor after a newer minor is out does not hijack the alias. `dev` tracks `main`, so a
documentation fix is visible without waiting for a release. Retention is the last five
minors; `mike list --json` drives the prune.

While the version is `0.x`, a minor bump is allowed to break compatibility. The
documentation says so explicitly, because otherwise people pin `~> 0.1` and are surprised.

## Workflows

`pages.yml`, triggered by a push to `main`, by `workflow_call`, or manually:

1. **docs** (`contents: write`) — `mike deploy --push --branch docs-versions`. From `main`
   that deploys `dev`; called from a release it deploys `<major>.<minor>` with the `latest`
   alias (`--update-aliases`) and runs `mike set-default latest`. Then prunes minors beyond
   the newest five.
2. **deploy** (`pages: write`, `id-token: write`) — checks out `docs-versions` into `site/`,
   downloads every release's `*.tgz` into `site/charts/`, runs `helm repo index`, moves the
   result to the site root, and publishes with `upload-pages-artifact` + `deploy-pages`.

`workflow_dispatch` takes optional `ref` and `version` inputs, which is how an old version's
documentation gets rebuilt after a fix.

`release.yml` packages the chart with the tag's version, hands the `.tgz` to goreleaser
through `release.extra_files` so it becomes a release asset, and then calls `pages.yml`. The
OCI push to GHCR is removed, and with it the `packages: write` permission.

A release cannot simply trigger `pages.yml` through `on: release`. GitHub does not fire
workflow events for actions taken with `GITHUB_TOKEN`, and goreleaser creates the release
with exactly that token. `workflow_call` is the direct route; `workflow_run` would be the
alternative.

`ci.yml` gains a `docs` job: `mkdocs build --strict` catches broken links and missing nav
entries on a pull request, and `helm-docs` followed by `git diff --exit-code` fails if the
generated chart README is out of date.

## Documentation source

MkDocs Material, sources staying as markdown under `docs/`.

The Helm values reference is generated by `helm-docs` from the comments in `values.yaml`
into the chart's `README.md`, which is also what ArtifactHub and the GitHub chart directory
show. `docs/helm/values.md` pulls that file in through
`mkdocs-include-markdown-plugin`, so the table exists once. Prose that explains rather than
enumerates — what a collector costs in API requests, which secret mode to choose — stays
hand-written.

`README.md` shrinks to an introduction, the status table, a Docker quick start and a link to
the site. The configuration table moves into the documentation rather than being duplicated.

## Out of scope

- Chart signing (`helm package --sign`) and the `pubkey.gpg` both reference repositories
  publish. Worth doing, needs a key and a decision about where it lives.
- An `artifacthub-repo.yml` to list the chart on ArtifactHub.
- Any documentation host other than GitHub Pages.
