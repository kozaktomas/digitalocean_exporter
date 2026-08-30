# Fix the Pages guard so a release actually publishes

The `guard` job in `.github/workflows/pages.yml` cancels the publish for the release run it was meant to protect.

## Background

The guard exists so that a push to `main` whose commit already carries a `vX.Y.Z` tag stands down and leaves the Pages deployment to the release. It decides by checking that the event is a `push` and that a semver tag points at HEAD.

A reusable workflow invoked through `workflow_call` inherits the *caller's* `github` context. `release.yml` is triggered by a tag push, so inside `pages.yml` `github.event_name` is `push` — not `workflow_call` — and the tag does point at HEAD. The guard therefore sets `publish=false` for the release itself. The `docs` job is skipped, `deploy` is skipped with it, and the workflow reports success while publishing neither the new documentation version nor the regenerated chart `index.yaml`.

This has never been observed on a real release: the guard was added after v0.3.0, whose Pages deployment was triggered by hand.

## Requirements

- A run started by the release workflow through `workflow_call` must publish, regardless of the tag on HEAD.
- A push to a *branch* whose commit already carries a `vX.Y.Z` tag must still stand down, which is the behaviour the guard was written for.
- A push to a branch with no such tag must publish, as now.
- A `workflow_dispatch` run must publish.
- The decision must not rest on `github.event_name` alone, since that value is the caller's event. `github.ref_type` is inherited the same way and does separate a tag push from a branch push.
- The comment above the job must explain that the context is inherited, so the next reader does not reintroduce the same assumption.

## Acceptance

- Reading the workflow, each of the four cases above resolves to the intended `publish` value.
- The next release publishes a new documentation version and a chart package that appears in `index.yaml`.

## Constraints

- Never commit the task spec, notes, plans or any other scheduler scratch file into the repository — nothing about the task-tracking system belongs in git.
- `make check` must pass before committing.
- Commit straight to `main` — no branches, no pull requests.
- Everything committed is in English, with no local paths or references to other projects.
