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
VOLUMES = {"volumes": [{"id": "vol", "name": "data"}], "meta": {"total": 13}}
DATABASES = {"databases": [{"id": "1", "name": "main", "engine": "mysql", "version": "8",
                            "num_nodes": 1, "size": "db-2vcpu-4gb", "region": "fra1",
                            "status": "online", "storage_size_mib": 102400,
                            "maintenance_window": {"day": "sunday", "hour": "03:00:00",
                                                   "pending": False}}],
             "meta": {"total": 1}}
REPOSITORIES = {"repositories": [{"registry_name": "smoke", "name": "app", "tag_count": 3,
                                  "manifest_count": 2,
                                  "latest_manifest": {"compressed_size_bytes": 12345678,
                                                      "updated_at": "2026-08-24T12:00:00Z"}}],
                "meta": {"total": 1}}

# One bucket of two objects, served S3-style so the Spaces collector has
# something to list without any credentials of its own.
LISTING = b"""<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
<Name>smoke</Name><KeyCount>2</KeyCount><MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated>
<Contents><Key>a</Key><Size>1024</Size><LastModified>2026-08-24T12:00:00.000Z</LastModified></Contents>
<Contents><Key>b</Key><Size>2048</Size><LastModified>2026-08-24T12:00:00.000Z</LastModified></Contents>
</ListBucketResult>"""

ROUTES = {"/v2/customers/my/balance": BALANCE,
          "/v2/databases": DATABASES,
          "/v2/droplets": DROPLETS,
          "/v2/reserved_ips": RESERVED_IPS,
          "/v2/volumes": VOLUMES,
          "/v2/registry": REGISTRY,
          "/v2/registry/subscription": SUBSCRIPTION,
          "/v2/registry/smoke/repositoriesV2": REPOSITORIES}

class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if "list-type=2" in self.path:
            self.respond(LISTING, "application/xml")
            return
        body = ROUTES.get(self.path.split("?")[0], ACCOUNT)
        self.respond(json.dumps(body).encode(), "application/json")

    def respond(self, payload, content_type):
        self.send_response(200)
        self.send_header("Content-Type", content_type)
        self.send_header("RateLimit-Remaining", "4999")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, *_):
        pass

http.server.HTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
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
         --collector.spaces --collector.spaces.interval=1s \
         --spaces.access-key=smoke --spaces.secret-key=smoke \
         --spaces.region=fra1 --collector.spaces.bucket=smoke &
EXPORTER_PID=$!

for _ in $(seq 1 50); do
  curl -sf "http://127.0.0.1:${PORT}/healthz" >/dev/null && break
  sleep 0.2
done

# Poll until the first collector refresh has landed.
METRICS=""
for _ in $(seq 1 50); do
  METRICS="$(curl -sf "http://127.0.0.1:${PORT}/metrics")"
  grep -q "^digitalocean_spaces_bucket_size_bytes" <<<"$METRICS" && break
  sleep 0.2
done

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
  digitalocean_database_storage_bytes
do
  if grep -q "^${metric}" <<<"$METRICS"; then
    echo "ok   ${metric}"
  else
    echo "FAIL ${metric} missing"
    fail=1
  fi
done

exit "$fail"
