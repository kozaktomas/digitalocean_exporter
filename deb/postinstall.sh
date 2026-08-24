#!/bin/sh
set -e

# Dedicated, unprivileged system user the service runs as.
groupadd --system digitalocean-exporter || true
useradd --system -d /nonexistent -s /usr/sbin/nologin -g digitalocean-exporter digitalocean-exporter || true

# The env file holds the API token: keep it unreadable to everyone else.
chown root:digitalocean-exporter /etc/digitalocean-exporter/digitalocean-exporter.env
chmod 640 /etc/digitalocean-exporter/digitalocean-exporter.env

systemctl daemon-reload
systemctl enable digitalocean-exporter || true

cat <<'EOF'

─────────────────────────────────────────────────────────────
 digitalocean-exporter installed.

 Next steps:
   1. Put a read-only API token into
        /etc/digitalocean-exporter/digitalocean-exporter.env
   2. sudo systemctl start digitalocean-exporter
   3. curl http://localhost:9212/metrics
─────────────────────────────────────────────────────────────

EOF
