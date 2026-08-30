#!/bin/sh
set -e

# dpkg runs this as `configure <previous-version>`: the version is empty on a
# first installation and set on an upgrade. Any other action — an aborted
# upgrade or removal — needs nothing from us.
[ "$1" = "configure" ] || exit 0
previous_version="$2"

# Dedicated, unprivileged system user the service runs as.
groupadd --system digitalocean-exporter || true
useradd --system -d /nonexistent -s /usr/sbin/nologin -g digitalocean-exporter digitalocean-exporter || true

# The env file holds the API token: keep it unreadable to everyone else.
chown root:digitalocean-exporter /etc/digitalocean-exporter/digitalocean-exporter.env
chmod 640 /etc/digitalocean-exporter/digitalocean-exporter.env

systemctl daemon-reload
systemctl enable digitalocean-exporter || true

if [ -n "$previous_version" ]; then
    # An upgrade. The pre-removal script leaves the service running, so the old
    # binary is still the one in memory: restart it to pick up the new one.
    # try-restart touches only a unit that is actually running, which leaves a
    # deliberately stopped exporter — one that has never been configured, say —
    # exactly as it was.
    systemctl try-restart digitalocean-exporter || true
    exit 0
fi

cat <<'EOF_BANNER'

─────────────────────────────────────────────────────────────
 digitalocean-exporter installed.

 Next steps:
   1. Put a read-only API token into
        /etc/digitalocean-exporter/digitalocean-exporter.env
   2. sudo systemctl start digitalocean-exporter
   3. curl http://localhost:9212/metrics
─────────────────────────────────────────────────────────────

EOF_BANNER
