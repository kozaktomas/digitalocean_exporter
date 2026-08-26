#!/usr/bin/env bash
# End-to-end smoke test: start the exporter against a fake DigitalOcean API,
# scrape /metrics, and assert the metrics that prove the whole chain works.
set -euo pipefail

BIN="${BIN:-./digitalocean_exporter}"
PORT="${PORT:-19212}"
API_PORT="${API_PORT:-19213}"
WORKDIR="$(mktemp -d)"
trap 'kill "${API_PID:-}" "${EXPORTER_PID:-}" 2>/dev/null || true; rm -rf "$WORKDIR"' EXIT

# A fake DigitalOcean API so the smoke test needs no real token.
cat > "$WORKDIR/api.py" <<'PY'
import http.server, json, sys

ACCOUNT = {"account": {"droplet_limit": 25, "floating_ip_limit": 3, "reserved_ip_limit": 3,
                       "volume_limit": 100, "email_verified": True, "status": "active"}}
BALANCE = {"month_to_date_balance": "23.44", "account_balance": "12.23",
           "month_to_date_usage": "11.21", "generated_at": "2026-08-24T12:00:00Z"}
REGISTRY = {"registry": {"name": "smoke", "region": "fra1", "storage_usage_bytes": 1073741824,
                         "created_at": "2026-01-01T00:00:00Z"}}
SUBSCRIPTION = {"subscription": {"tier": {"name": "Basic", "slug": "basic",
                                          "included_storage_bytes": 5368709120,
                                          "included_bandwidth_bytes": 5368709120,
                                          "monthly_price_in_cents": 500},
                                 "created_at": "2026-01-01T00:00:00Z",
                                 "updated_at": "2026-01-01T00:00:00Z"}}
DROPLETS = {"droplets": [{"id": 1, "name": "web-1", "status": "active", "vcpus": 2, "memory": 4096,
                          "disk": 80, "region": {"slug": "fra1"},
                          "size": {"slug": "s-2vcpu-4gb", "price_hourly": 0.02679, "price_monthly": 18},
                          "image": {"slug": "ubuntu-24-04"}}],
            "meta": {"total": 5}}
RESERVED_IPS = {"reserved_ips": [], "meta": {"total": 0}}
VOLUMES = {"volumes": [{"id": "vol", "name": "data", "region": {"slug": "fra1"},
                        "size_gigabytes": 100, "filesystem_type": "ext4",
                        "filesystem_label": "data", "droplet_ids": [1]}],
           "meta": {"total": 13}}
LOAD_BALANCERS = {"load_balancers": [{"id": "lb", "name": "public", "ip": "10.0.0.1",
                                      "status": "active", "size_unit": 1,
                                      "type": "REGIONAL", "algorithm": "round_robin",
                                      "size": "lb-small", "vpc_uuid": "vpc",
                                      "region": {"slug": "fra1"}, "droplet_ids": [1],
                                      "forwarding_rules": [{"entry_port": 443}]}],
                  "meta": {"total": 1}}
CDN = {"endpoints": [{"id": "cdn", "origin": "smoke.fra1.digitaloceanspaces.com",
                      "endpoint": "smoke.fra1.cdn.digitaloceanspaces.com",
                      "ttl": 3600, "certificate_id": "cert"}],
       "meta": {"total": 1}}
DOMAINS = {"domains": [{"name": "smoke.example", "ttl": 1800,
                       "zone_file": "$ORIGIN smoke.example.\n$TTL 1800\n"}],
           "meta": {"total": 1}}
FIREWALLS = {"firewalls": [{"id": "fw", "name": "web", "status": "succeeded",
                            "droplet_ids": [1], "tags": ["web"],
                            "inbound_rules": [{"protocol": "tcp", "ports": "443",
                                               "sources": {"addresses": ["0.0.0.0/0", "::/0"]}}],
                            "outbound_rules": [{"protocol": "tcp", "ports": "0",
                                                "destinations": {"addresses": ["0.0.0.0/0"]}}],
                            "pending_changes": []}],
             "meta": {"total": 1}}
CERTIFICATES = {"certificates": [{"id": "cert", "name": "cdn", "type": "lets_encrypt",
                                  "state": "verified", "not_after": "2026-11-05T07:18:56Z",
                                  "sha1_fingerprint": "a4b4e231",
                                  "dns_names": ["smoke.example"]}],
                "meta": {"total": 1}}
DATABASES = {"databases": [{"id": "1", "name": "main", "engine": "mysql", "version": "8",
                            "num_nodes": 1, "size": "db-2vcpu-4gb", "region": "fra1",
                            "status": "online", "storage_size_mib": 102400,
                            "maintenance_window": {"day": "sunday", "hour": "03:00:00",
                                                   "pending": False}}],
             "meta": {"total": 1}}
CLUSTERS = {"kubernetes_clusters": [{"id": "c1", "name": "prod", "region": "fra1",
                                     "version": "1.35.5-do.1", "status": {"state": "running"},
                                     "auto_upgrade": True, "surge_upgrade": True, "ha": False,
                                     "node_pools": [{"id": "p1", "name": "workers",
                                                     "size": "s-4vcpu-8gb", "count": 1,
                                                     "nodes": [{"id": "n1",
                                                                "status": {"state": "running"}}]}]}],
            "meta": {"total": 1}}
REPOSITORIES = {"repositories": [{"registry_name": "smoke", "name": "app", "tag_count": 3,
                                  "manifest_count": 2,
                                  "latest_manifest": {"compressed_size_bytes": 12345678,
                                                      "updated_at": "2026-08-24T12:00:00Z"}}],
                "meta": {"total": 1}}

# One bucket of two objects. Spaces reports a bucket's usage in the Ceph
# gateway's own headers on a HEAD, which is all the Spaces collector asks for.
BUCKET_USAGE = {"x-rgw-object-count": "2", "x-rgw-bytes-used": "3072"}

ROUTES = {"/v2/customers/my/balance": BALANCE,
          "/v2/databases": DATABASES,
          "/v2/droplets": DROPLETS,
          "/v2/kubernetes/clusters": CLUSTERS,
          "/v2/reserved_ips": RESERVED_IPS,
          "/v2/volumes": VOLUMES,
          "/v2/load_balancers": LOAD_BALANCERS,
          "/v2/cdn/endpoints": CDN,
          "/v2/domains": DOMAINS,
          "/v2/firewalls": FIREWALLS,
          "/v2/certificates": CERTIFICATES,
          "/v2/registry": REGISTRY,
          "/v2/registry/subscription": SUBSCRIPTION,
          "/v2/registry/smoke/repositoriesV2": REPOSITORIES}

class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        body = ROUTES.get(self.path.split("?")[0], ACCOUNT)
        self.respond(json.dumps(body).encode(), "application/json")

    def do_HEAD(self):
        self.send_response(200)
        for name, value in BUCKET_USAGE.items():
            self.send_header(name, value)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def respond(self, payload, content_type):
        self.send_response(200)
        self.send_header("Content-Type", content_type)
        self.send_header("RateLimit-Remaining", "4999")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, *_):
        pass

# Threading: the exporter refreshes every collector concurrently at startup,
# and a serialising server would make that first round take longer than the
# scrape below waits for.
http.server.ThreadingHTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
PY

python3 "$WORKDIR/api.py" "$API_PORT" &
API_PID=$!

# Wait for the stub API before starting the exporter: the first refresh happens
# immediately at startup and would otherwise race the stub and fail.
for _ in $(seq 1 50); do
  curl -sf "http://127.0.0.1:${API_PORT}/v2/account" >/dev/null && break
  sleep 0.2
done

DO_API_BASE_URL="http://127.0.0.1:${API_PORT}/" \
DO_SPACES_ENDPOINT="http://127.0.0.1:${API_PORT}" \
  "$BIN" --do.token=smoke-token --web.listen-address="127.0.0.1:${PORT}" \
         --collector.account.interval=1s --collector.balance.interval=1s \
         --collector.registry.interval=1s --collector.limits.interval=1s \
         --collector.droplets.interval=1s --collector.databases.interval=1s \
         --collector.kubernetes.interval=1s \
         --collector.domains.interval=1s \
         --collector.firewalls --collector.firewalls.interval=1s \
         --collector.certificates --collector.certificates.interval=1s \
         --collector.spaces --collector.spaces.interval=1s \
         --spaces.access-key=smoke --spaces.secret-key=smoke \
         --spaces.region=fra1 --collector.spaces.bucket=smoke &
EXPORTER_PID=$!

for _ in $(seq 1 50); do
  curl -sf "http://127.0.0.1:${PORT}/healthz" >/dev/null && break
  sleep 0.2
done

# Every collector this run enables: the defaults plus spaces, firewalls and
# certificates. The count is
# spelled out because collector_success is a GaugeVec whose per-collector
# sample only appears once that collector's first refresh has finished. Waiting
# for "no sample equals 0" would therefore pass while a collector had not
# started yet, and the assertions below would race it — a flake that looks
# exactly like a broken collector. Bump this when adding a collector.
EXPECTED_COLLECTORS=14

# Poll until all of them have reported a successful refresh.
METRICS=""
for _ in $(seq 1 100); do
  METRICS="$(curl -sf "http://127.0.0.1:${PORT}/metrics")"
  reported="$(grep -c "^digitalocean_exporter_collector_success" <<<"$METRICS" || true)"
  succeeded="$(grep -c "^digitalocean_exporter_collector_success.* 1$" <<<"$METRICS" || true)"
  if [ "$reported" -eq "$EXPECTED_COLLECTORS" ] && [ "$succeeded" -eq "$EXPECTED_COLLECTORS" ]; then
    break
  fi
  sleep 0.2
done

if [ "$succeeded" -ne "$EXPECTED_COLLECTORS" ]; then
  echo "FAIL only ${succeeded}/${EXPECTED_COLLECTORS} collectors refreshed successfully"
  grep "^digitalocean_exporter_collector_success" <<<"$METRICS"
  exit 1
fi

fail=0
for metric in \
  digitalocean_exporter_build_info \
  digitalocean_exporter_collector_success \
  digitalocean_account_active \
  digitalocean_month_to_date_usage \
  digitalocean_spaces_bucket_size_bytes \
  digitalocean_spaces_bucket_objects \
  digitalocean_registry_storage_usage_bytes \
  digitalocean_registry_repository_tags \
  digitalocean_account_droplets \
  digitalocean_account_volumes \
  digitalocean_droplet_up \
  digitalocean_droplet_price_monthly \
  digitalocean_database_status \
  digitalocean_database_storage_bytes \
  digitalocean_kubernetes_cluster_up \
  digitalocean_kubernetes_node_pool_nodes_running \
  digitalocean_volume_size_bytes \
  digitalocean_volume_droplets \
  digitalocean_loadbalancer_status \
  digitalocean_loadbalancer_droplets \
  digitalocean_cdn_endpoint_ttl_seconds \
  digitalocean_domain_ttl_seconds \
  digitalocean_firewall_inbound_rules_open \
  digitalocean_firewall_pending_changes \
  digitalocean_certificate_expiry_timestamp_seconds \
  digitalocean_certificate_dns_names
do
  if grep -q "^${metric}" <<<"$METRICS"; then
    echo "ok   ${metric}"
  else
    echo "FAIL ${metric} missing"
    fail=1
  fi
done

exit "$fail"
