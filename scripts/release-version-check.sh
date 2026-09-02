#!/usr/bin/env bash
#
# Checks that the tree agrees with the version being released.
#
# One number drives the binary, the chart, the appVersion and the documentation
# directory. A tag that disagrees with the tree publishes a chart whose version does
# not match its own Chart.yaml, and install instructions pinning a release that does
# not exist — neither of which fails anywhere else, because both are just text.
#
# Usage: scripts/release-version-check.sh <version>   (the tag without its `v`)
set -euo pipefail

# Every path below is repository-relative, so the check reads the same tree wherever
# it is invoked from.
cd "$(git rev-parse --show-toplevel)"

if [ $# -ne 1 ]; then
	echo "usage: $0 <version>   (the tag without its leading v)" >&2
	exit 2
fi

version="$1"
chart="charts/digitalocean-exporter/Chart.yaml"
failed=0

fail() {
	echo "$1" >&2
	failed=1
}

# Chart.yaml is flat and quotes appVersion but not version, so one sed covers both
# and the check needs no YAML parser on the runner.
chart_field() {
	sed -n "s/^$1:[[:space:]]*[\"']\{0,1\}\([^\"']*\)[\"']\{0,1\}[[:space:]]*\$/\1/p" "$chart"
}

for field in version appVersion; do
	have="$(chart_field "$field")"
	if [ "$have" != "$version" ]; then
		fail "$chart: $field is '${have}', expected '${version}'"
	fi
done

# What counts as hard-coding an install version: a pinned chart version, an image
# tag, a package file name, a download variable, or the exporter's own `--version`
# output. Each pattern names the artefact it pins, so a version belonging to some
# other piece of software does not drag its page into the check.
semver='[0-9]+\.[0-9]+\.[0-9]+'
patterns=(
	"--version[= ]+${semver}"                        # helm install --version
	"version[[:space:]]*=[[:space:]]*\"${semver}\""    # terraform helm_release
	"digitalocean_exporter:${semver}"                # container image tag
	"digitalocean-exporter_${semver}_"               # deb package file name
	"VERSION=${semver}"                              # the download snippets
	"version ${semver} \(commit"                     # `digitalocean_exporter --version`
	"version=\"${semver}\""                            # digitalocean_exporter_build_info
	"\| \`${semver}\` \|"                              # the image tag table
)
pattern="$(
	IFS='|'
	echo "${patterns[*]}"
)"

# Only tracked files, so nothing untracked in the working tree can fail a release.
# `docs/design/` holds dated design records, which describe the release they were
# written for and are not rewritten afterwards.
mapfile -t pages < <(git ls-files -- 'README.md' 'docs' | grep '\.md$' | grep -v '^docs/design/')
mapfile -t files < <(grep -lE -e "$pattern" -- "${pages[@]}" | sort)

if [ "${#files[@]}" -eq 0 ]; then
	fail "no file hard-codes an install version, which means this check stopped checking anything"
fi

# The versions a page pins, rather than every number on it: a page is allowed to
# name another version in prose, and a release that is only discussed is not one a
# reader can install.
pinned_versions() {
	grep -oE -e "$pattern" "$1" | grep -oE "$semver" | sort -u
}

for file in "${files[@]}"; do
	found="$(pinned_versions "$file")"
	if ! printf '%s\n' "$found" | grep -qxF "$version"; then
		fail "${file}: pins $(printf '%s' "$found" | paste -sd ', ') and not ${version}"
	fi
done

if [ "$failed" -ne 0 ]; then
	echo "release ${version} does not match the tree; bump the chart and the documentation first" >&2
	exit 1
fi

echo "release ${version} matches ${chart} and all ${#files[@]} documentation pages that pin a version"
