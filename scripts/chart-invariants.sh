#!/usr/bin/env bash
#
# Chart invariants that `helm lint` and a plain `helm template` cannot see:
# duplicate object names render happily, a checksum annotation that is not stable
# rolls the pod on every upgrade, and a collector whose disabled branch was never
# written renders a Deployment that simply keeps collecting.
set -euo pipefail

CHART="charts/digitalocean-exporter"
COMMON=(--set digitalocean.token=dummy)

# The longest release name the chart's own name helper accepts. It used to leave
# "security", "spaces" and "storage" sharing a single dashboard ConfigMap name,
# because the assembled name was truncated to 63 characters from the right.
LONG_RELEASE="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" # 51 characters

dashboard_names() {
	helm template "$LONG_RELEASE" "$CHART" "${COMMON[@]}" \
		--set grafana.dashboards.enabled=true \
		| awk '/^kind: ConfigMap$/ { want = 1; next } want && /^  name: / { print $2; want = 0 }'
}

names="$(dashboard_names)"
total="$(printf '%s\n' "$names" | wc -l)"
unique="$(printf '%s\n' "$names" | sort -u | wc -l)"
expected="$(ls "$CHART"/dashboards/*.json | wc -l)"

if [ "$total" -ne "$expected" ] || [ "$unique" -ne "$expected" ]; then
	echo "dashboard ConfigMap names collide under a ${#LONG_RELEASE}-character release name:" >&2
	printf '%s\n' "$names" | sort | uniq -c >&2
	exit 1
fi

# Kubernetes object names are limited to 63 characters.
while read -r name; do
	if [ "${#name}" -gt 63 ]; then
		echo "dashboard ConfigMap name is ${#name} characters, over the 63 allowed: $name" >&2
		exit 1
	fi
done <<<"$names"

secret_checksum() {
	helm template "$CHART" "${COMMON[@]}" --set digitalocean.token="$1" \
		| awk '/^        checksum\/secret: / { print $2 }'
}

first="$(secret_checksum dummy)"
second="$(secret_checksum dummy)"
changed="$(secret_checksum rotated)"

if [ -z "$first" ]; then
	echo "the pod template carries no checksum/secret annotation" >&2
	exit 1
fi
if [ "$first" != "$second" ]; then
	echo "checksum/secret differs between two renders of unchanged values: $first vs $second" >&2
	exit 1
fi
if [ "$first" = "$changed" ]; then
	echo "checksum/secret did not change when the token changed: $first" >&2
	exit 1
fi

echo "chart invariants ok: $unique distinct dashboard ConfigMaps, checksum/secret stable and token-sensitive"

# Every collector switched off at once. A collector is disabled with
# `--no-collector.<name>`, never `--collector.<name>=false`, which is a parse error
# that crash-loops the pod — so the disabled branch of each one has to be rendered
# somewhere, and only a render with the whole set off reaches all of them. The names
# come from values.yaml rather than a list here, so a collector added later is
# covered without anyone remembering this file.
collector_names() {
	awk '
		/^collectors:/ { inside = 1; next }
		inside && /^[^[:space:]]/ { inside = 0 }
		inside && /^  [a-z][a-z0-9]*:[[:space:]]*$/ { sub(/:.*/, "", $1); print $1 }
	' "$CHART/values.yaml"
}

mapfile -t collectors < <(collector_names)
if [ "${#collectors[@]}" -eq 0 ]; then
	echo "found no collectors in $CHART/values.yaml" >&2
	exit 1
fi

disabled=()
for name in "${collectors[@]}"; do
	disabled+=(--set "collectors.$name.enabled=false")
done

all_off="$(helm template "$CHART" "${COMMON[@]}" "${disabled[@]}")"

for name in "${collectors[@]}"; do
	if ! printf '%s\n' "$all_off" | grep -qx -- "            - --no-collector.$name"; then
		echo "collectors.$name is in values.yaml but disabling it renders no --no-collector.$name flag" >&2
		exit 1
	fi
done

# The other direction: a flag whose value has been dropped from values.yaml would go
# on rendering with whatever `.Values` gives a missing key.
rendered_off="$(printf '%s\n' "$all_off" | grep -oE -- '--no-collector\.[a-z]+' | sort -u | wc -l)"
if [ "$rendered_off" -ne "${#collectors[@]}" ]; then
	echo "with every collector disabled the Deployment renders $rendered_off --no-collector flags for ${#collectors[@]} collectors" >&2
	exit 1
fi

# Nothing may still be switched on, or the render never reached the branch it claims to.
if printf '%s\n' "$all_off" | grep -qE -- '^ +- --collector\.'; then
	echo "with every collector disabled the Deployment still renders --collector flags:" >&2
	printf '%s\n' "$all_off" | grep -E -- '^ +- --collector\.' >&2
	exit 1
fi

echo "chart invariants ok: ${#collectors[@]} collectors, each with a rendered --no-collector branch"
