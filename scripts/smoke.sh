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
import http.server, json, sys, time

ACCOUNT = {"account": {"droplet_limit": 25, "floating_ip_limit": 3, "reserved_ip_limit": 3,
                       "volume_limit": 100, "email_verified": True, "status": "active"}}
BALANCE = {"month_to_date_balance": "23.44", "account_balance": "12.23",
           "month_to_date_usage": "11.21", "generated_at": "2026-08-24T12:00:00Z"}
# Two registries, which is what a Professional subscription may hold: the
# exporter enumerates them through /v2/registries and measures each one.
REGISTRIES = {"registries": [{"name": "smoke", "region": "fra1",
                              "storage_usage_bytes": 1073741824,
                              "storage_usage_bytes_updated_at": "2026-08-24T06:00:00Z",
                              "created_at": "2026-01-01T00:00:00Z"},
                             {"name": "smoke-nyc", "region": "nyc3",
                              "storage_usage_bytes": 2147483648,
                              "storage_usage_bytes_updated_at": "2026-08-24T06:00:00Z",
                              "created_at": "2026-01-01T00:00:00Z"}],
              "total_storage_usage_bytes": 3221225472}
SUBSCRIPTION = {"subscription": {"tier": {"name": "Basic", "slug": "basic",
                                          "included_storage_bytes": 5368709120,
                                          "included_bandwidth_bytes": 5368709120,
                                          "monthly_price_in_cents": 500},
                                 "created_at": "2026-01-01T00:00:00Z",
                                 "updated_at": "2026-01-01T00:00:00Z"}}
DROPLETS = {"droplets": [{"id": 1, "name": "web-1", "status": "active", "vcpus": 2, "memory": 4096,
                          "disk": 80, "region": {"slug": "fra1"},
                          "created_at": "2026-01-01T00:00:00Z", "vpc_uuid": "vpc",
                          "features": ["backups", "monitoring"], "tags": ["web"],
                          "size": {"slug": "s-2vcpu-4gb", "price_hourly": 0.02679, "price_monthly": 18},
                          "image": {"slug": "ubuntu-24-04"}}],
            "meta": {"total": 5}}
# Two reserved IPv4 addresses, one assigned to the droplet above and one idle,
# plus a reserved IPv6. The idle one is what the reservedips collector exists
# for: DigitalOcean bills a reserved IP only while it serves nothing.
RESERVED_IPS = {"reserved_ips": [{"ip": "192.0.2.1", "region": {"slug": "fra1"},
                                  "project_id": "proj",
                                  "droplet": {"id": 1, "name": "web-1"}},
                                 {"ip": "192.0.2.2", "region": {"slug": "fra1"},
                                  "project_id": "proj", "droplet": None}],
                "meta": {"total": 2}}
RESERVED_IPV6 = {"reserved_ipv6s": [{"ip": "2604:a880::1", "region_slug": "fra1",
                                     "reserved_at": "2026-08-01T10:00:00Z"}],
                 "meta": {"total": 1}}
VOLUMES = {"volumes": [{"id": "vol", "name": "data", "region": {"slug": "fra1"},
                        "size_gigabytes": 100, "filesystem_type": "ext4",
                        "filesystem_label": "data", "droplet_ids": [1]}],
           "meta": {"total": 13}}
IMAGES = {"images": [{"id": 1, "name": "web-1-2026-08-01", "type": "snapshot",
                      "distribution": "Ubuntu", "status": "available",
                      "regions": ["fra1"], "min_disk_size": 25,
                      "size_gigabytes": 2.5, "created_at": "2026-08-01T10:00:00Z"}],
          "meta": {"total": 1}}
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
                            "users": [{"name": "doadmin"}], "db_names": ["defaultdb"],
                            "project_id": "p-1", "private_network_uuid": "vpc-1",
                            "storage_autoscale": {"enabled": True},
                            "maintenance_window": {"day": "sunday", "hour": "03:00:00",
                                                   "pending": False}}],
             "meta": {"total": 1}}
# The per-cluster detail endpoints, which --collector.databases.details pays
# two requests per cluster for: one replica and one backup.
DB_REPLICAS = {"replicas": [{"id": "r1", "name": "main-replica", "region": "fra1",
                             "status": "online"}]}
DB_BACKUPS = {"backups": [{"created_at": "2026-08-24T01:00:00Z", "size_gigabytes": 2.5}]}
# One App Platform app: a service and a static site, with a deployment that has
# gone live and another rolling out over it. The static site is the component
# App Platform runs no instances for.
APPS = {"apps": [{"id": "app-1", "tier_slug": "basic",
                  "region": {"slug": "fra1"},
                  "default_ingress": "https://smoke.ondigitalocean.app",
                  "created_at": "2026-01-01T00:00:00Z",
                  "last_deployment_active_at": "2026-08-24T09:00:00Z",
                  "active_deployment": {"id": "dep-1", "phase": "ACTIVE"},
                  "in_progress_deployment": {"id": "dep-2", "phase": "DEPLOYING"},
                  "spec": {"name": "smoke", "region": "fra",
                           "services": [{"name": "web", "instance_count": 2,
                                         "instance_size_slug": "apps-s-1vcpu-1gb"}],
                           "static_sites": [{"name": "docs"}]}}],
        "meta": {"total": 1}}
CLUSTERS = {"kubernetes_clusters": [{"id": "c1", "name": "prod", "region": "fra1",
                                     "version": "1.35.5-do.1", "status": {"state": "running"},
                                     "auto_upgrade": True, "surge_upgrade": True, "ha": False,
                                     "node_pools": [{"id": "p1", "name": "workers",
                                                     "size": "s-4vcpu-8gb", "count": 1,
                                                     "nodes": [{"id": "n1",
                                                                "name": "workers-3jkl",
                                                                "droplet_id": "1",
                                                                "status": {"state": "running"}}]}]}],
            "meta": {"total": 1}}
# The versions a cluster can move to come from an endpoint of their own, one
# request per cluster, which is what --collector.kubernetes.upgrades pays for.
UPGRADES = {"available_upgrade_versions": [{"slug": "1.36.1-do.0",
                                            "kubernetes_version": "1.36.1"}]}


def repositories(registry):
    return {"repositories": [{"registry_name": registry, "name": "app", "tag_count": 3,
                              "manifest_count": 2,
                              "latest_manifest": {"compressed_size_bytes": 12345678,
                                                  "updated_at": "2026-08-24T12:00:00Z"}}],
            "meta": {"total": 1}}

# The monitoring API is a Prometheus range-query API: one matrix of series per
# request, one request per metric per resource. The same body answers every one
# of those routes, which is enough to prove the two monitoring collectors read
# it, merge it and report a resource as up. The sample is stamped now, since
# both collectors ask for a window ending at the current time.
def monitoring():
    return {"status": "success",
            "data": {"resultType": "matrix",
                     "result": [{"metric": {}, "values": [[int(time.time()), "1"]]}]}}

# One bucket of two objects. Spaces reports a bucket's usage in the Ceph
# gateway's own headers on a HEAD, which is all the Spaces collector asks for.
BUCKET_USAGE = {"x-rgw-object-count": "2", "x-rgw-bytes-used": "3072"}

ROUTES = {"/v2/customers/my/balance": BALANCE,
          "/v2/apps": APPS,
          "/v2/databases": DATABASES,
          "/v2/databases/1/replicas": DB_REPLICAS,
          "/v2/databases/1/backups": DB_BACKUPS,
          "/v2/droplets": DROPLETS,
          "/v2/kubernetes/clusters": CLUSTERS,
          "/v2/kubernetes/clusters/c1/upgrades": UPGRADES,
          "/v2/reserved_ips": RESERVED_IPS,
          "/v2/reserved_ipv6": RESERVED_IPV6,
          "/v2/volumes": VOLUMES,
          "/v2/images": IMAGES,
          "/v2/load_balancers": LOAD_BALANCERS,
          "/v2/cdn/endpoints": CDN,
          "/v2/domains": DOMAINS,
          "/v2/firewalls": FIREWALLS,
          "/v2/certificates": CERTIFICATES,
          "/v2/registries": REGISTRIES,
          "/v2/registries/subscription": SUBSCRIPTION,
          "/v2/registries/smoke/repositoriesV2": repositories("smoke"),
          "/v2/registries/smoke-nyc/repositoriesV2": repositories("smoke-nyc")}

class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        path = self.path.split("?")[0]
        body = monitoring() if path.startswith("/v2/monitoring/metrics/") \
            else ROUTES.get(path, ACCOUNT)
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

# The client-side rate limit defends DigitalOcean's 250-requests-a-minute burst
# limit, which the stub does not have. At the default of 4 a second the 1s
# intervals below would queue up behind the limiter and refreshes would start
# timing out, so this run raises it well past what it needs.
DO_API_BASE_URL="http://127.0.0.1:${API_PORT}/" \
DO_SPACES_ENDPOINT="http://127.0.0.1:${API_PORT}" \
  "$BIN" --do.token=smoke-token --web.listen-address="127.0.0.1:${PORT}" \
         --do.rate-limit=100 \
         --collector.account.interval=1s --collector.balance.interval=1s \
         --collector.registry.interval=1s --collector.limits.interval=1s \
         --collector.reservedips.interval=1s \
         --collector.droplets.interval=1s --collector.databases.interval=1s \
         --collector.kubernetes.interval=1s \
         --collector.domains.interval=1s \
         --collector.apps.interval=1s \
         --collector.images.interval=1s \
         --collector.firewalls --collector.firewalls.interval=1s \
         --collector.certificates --collector.certificates.interval=1s \
         --collector.spaces --collector.spaces.interval=1s \
         --spaces.access-key=smoke --spaces.secret-key=smoke \
         --spaces.region=fra1 --collector.spaces.bucket=smoke \
         --collector.dropletmetrics --collector.dropletmetrics.interval=1s \
         --collector.loadbalancermetrics --collector.loadbalancermetrics.interval=1s &
EXPORTER_PID=$!

for _ in $(seq 1 50); do
  curl -sf "http://127.0.0.1:${PORT}/healthz" >/dev/null && break
  sleep 0.2
done

# Every collector this run enables: the defaults plus spaces, firewalls,
# certificates and the two monitoring-API ones — every collector there is, so
# nothing ships untested end to end. The count is
# spelled out because collector_success is a GaugeVec whose per-collector
# sample only appears once that collector's first refresh has finished. Waiting
# for "no sample equals 0" would therefore pass while a collector had not
# started yet, and the assertions below would race it — a flake that looks
# exactly like a broken collector. Bump this when adding a collector.
EXPECTED_COLLECTORS=19

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

# Readiness is a claim about exactly that state, so assert it against it: with
# every collector refreshed, /readyz must be 200. It answers 503 until then,
# which is why the startup wait above uses /healthz instead.
ready_status="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${PORT}/readyz")"
if [ "$ready_status" = "200" ]; then
  echo "ok   /readyz is 200 once every collector has refreshed"
else
  echo "FAIL /readyz returned ${ready_status} with every collector refreshed"
  curl -s "http://127.0.0.1:${PORT}/readyz"
  exit 1
fi

# Every API request is attributed to the collector whose refresh made it, which
# only works if the scheduler's context reaches the transport. A counter with
# nothing but collector="none" would still look fine metric by metric.
if grep -q '^digitalocean_exporter_api_requests_total{collector="account"' <<<"$METRICS"; then
  echo "ok   digitalocean_exporter_api_requests_total is attributed per collector"
else
  echo "FAIL digitalocean_exporter_api_requests_total carries no collector attribution"
  grep "^digitalocean_exporter_api_requests_total" <<<"$METRICS"
  exit 1
fi

fail=0
for metric in \
  digitalocean_exporter_build_info \
  digitalocean_exporter_collector_success \
  digitalocean_exporter_api_request_duration_seconds \
  digitalocean_exporter_api_rate_limit \
  digitalocean_exporter_api_rate_limit_reset_timestamp_seconds \
  digitalocean_account_active \
  digitalocean_account_status \
  digitalocean_month_to_date_usage \
  digitalocean_spaces_bucket_size_bytes \
  digitalocean_spaces_bucket_objects \
  digitalocean_registry_storage_usage_bytes \
  digitalocean_registry_storage_usage_updated_timestamp_seconds \
  digitalocean_registry_repository_tags \
  digitalocean_account_droplets \
  digitalocean_reserved_ip_assigned \
  digitalocean_reserved_ips \
  digitalocean_account_volumes \
  digitalocean_droplet_up \
  digitalocean_droplet_price_monthly \
  digitalocean_droplet_backups_enabled \
  digitalocean_droplet_monitoring_agent \
  digitalocean_droplet_created_timestamp_seconds \
  digitalocean_database_status \
  digitalocean_database_storage_bytes \
  digitalocean_database_users \
  digitalocean_database_storage_autoscale_enabled \
  digitalocean_database_cluster_info \
  digitalocean_database_replicas \
  digitalocean_database_replica_status \
  digitalocean_database_last_backup_timestamp_seconds \
  digitalocean_kubernetes_cluster_up \
  digitalocean_kubernetes_node_pool_nodes_running \
  digitalocean_kubernetes_node_state \
  digitalocean_kubernetes_node_info \
  digitalocean_kubernetes_cluster_upgrade_available \
  digitalocean_kubernetes_cluster_available_version_info \
  digitalocean_volume_size_bytes \
  digitalocean_volume_droplets \
  digitalocean_image_size_bytes \
  digitalocean_images \
  digitalocean_loadbalancer_status \
  digitalocean_loadbalancer_droplets \
  digitalocean_cdn_endpoint_ttl_seconds \
  digitalocean_app_info \
  digitalocean_app_deployment_phase \
  digitalocean_app_deployment_in_progress \
  digitalocean_app_component_instances \
  digitalocean_domain_ttl_seconds \
  digitalocean_firewall_inbound_rules_open \
  digitalocean_firewall_pending_changes \
  digitalocean_certificate_expiry_timestamp_seconds \
  digitalocean_certificate_dns_names \
  digitalocean_droplet_metrics_up \
  digitalocean_loadbalancer_metrics_up
do
  if grep -q "^${metric}" <<<"$METRICS"; then
    echo "ok   ${metric}"
  else
    echo "FAIL ${metric} missing"
    fail=1
  fi
done

# The exporter is stopped the way a deployment stops it, and its exit status is
# part of what this test asserts: a non-zero one is a crash on the way out — a
# panic in a shutdown path, a server that never returned — which nothing else
# here would notice, since the process is on its way down anyway.
kill -TERM "$EXPORTER_PID"
shutdown_status=0
wait "$EXPORTER_PID" || shutdown_status=$?
EXPORTER_PID=""
if [ "$shutdown_status" -eq 0 ]; then
  echo "ok   the exporter exited cleanly on SIGTERM"
else
  echo "FAIL the exporter exited with status ${shutdown_status} on SIGTERM"
  fail=1
fi

exit "$fail"
