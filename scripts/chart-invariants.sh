#!/usr/bin/env bash
#
# Chart invariants that `helm lint` and a plain `helm template` cannot see:
# duplicate object names render happily, and a checksum annotation that is not
# stable rolls the pod on every upgrade.
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
