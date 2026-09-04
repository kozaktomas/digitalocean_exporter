# digitalocean_exporter

A Prometheus exporter for [DigitalOcean](https://www.digitalocean.com/), written in Go.
One binary, no state, no database. It reads the DigitalOcean API and exposes metrics on
`:9212`.

**📖 [Documentation](https://kozaktomas.github.io/digitalocean_exporter/)** — installation,
every configuration option, every metric, and what each collector costs in API requests.

```bash
docker run --rm -p 9212:9212 \
  -e DIGITALOCEAN_TOKEN=dop_v1_... \
  ghcr.io/kozaktomas/digitalocean_exporter:latest
```

## Why another one

The existing exporters leave real gaps. The most widely used one has been unmaintained
since 2022, reports Spaces buckets only as *existing* — never their size — and does not
cover the Container Registry at all. DigitalOcean itself does not expose Spaces usage
through its API at all, so bucket size has to come from the S3-compatible endpoint. This
exporter is built around that constraint from the start.

Collectors refresh in the background on their own intervals and write into an in-memory
snapshot; `/metrics` only ever reads that snapshot. A slow collector can therefore never
block or fail a Prometheus scrape, and each collector can run at whatever cadence its data
actually deserves.

## Status

Early. This release ships the exporter, the full build and release pipeline, and twenty-three
collectors:

| Collector | State |
|---|---|
| `account` — status and resource limits | available |
| `balance` — balance and month-to-date usage (needs a billing-scoped token) | available |
| `databases` — state, nodes and storage of every managed database cluster | available |
| `droplets` — state, size and price of every droplet | available |
| `dropletautoscale` — size, targets and utilisation of every droplet autoscale pool | available |
| `kubernetes` — cluster state and node pools of every managed cluster | available |
| `limits` — droplets, reserved IPs and volumes in use against the account limits | available |
| `registry` — Container Registry storage, subscription and repositories | available |
| `reservedips` — every reserved IP and the droplet it is assigned to, if any | available |
| `spaces` — bucket size and object count (needs a Spaces access key) | available |
| `volumes` — size of every block storage volume and what it is attached to | available |
| `images` — size and age of every snapshot, droplet backup and custom image | available |
| `loadbalancers` — state, backends and billed size of every load balancer | available |
| `cdn` — CDN endpoints, their cache TTL and the certificate each one serves | available |
| `apps` — App Platform tier, deployment phase and component instances of every app | available |
| `domains` — DNS zones the account hosts and their default TTL | available |
| `tags` — resources of each type carrying every tag | available |
| `projects` — every project and the resources of each type it owns | available |
| `firewalls` — rules, attachments and pending changes of every cloud firewall (off by default) | available |
| `certificates` — TLS certificates and when each one expires (off by default) | available |
| `dropletmetrics` — CPU, memory, disk and load per droplet (off by default) | available |
| `loadbalancermetrics` — traffic and backend health per load balancer (off by default) | available |
| `uptime` — Uptime checks, their per-region status and the last outage (off by default) | available |

Eleven Grafana dashboards ship with it, covering every collector. Import the JSON from
`charts/digitalocean-exporter/dashboards/`, or let the chart render them as ConfigMaps for
the Grafana sidecar — see the
[dashboards page](https://kozaktomas.github.io/digitalocean_exporter/latest/dashboards/).

A set of alerting rules ships alongside them, as a plain Prometheus rule file the chart can
install as a `PrometheusRule` — see the
[alerting page](https://kozaktomas.github.io/digitalocean_exporter/latest/alerting/).

## Install

Full instructions for each method are in the
[documentation](https://kozaktomas.github.io/digitalocean_exporter/latest/install/).

### Helm

```bash
helm repo add digitalocean-exporter https://kozaktomas.github.io/digitalocean_exporter
helm repo update
helm install digitalocean-exporter digitalocean-exporter/digitalocean-exporter \
  --version 0.3.0 --set digitalocean.existingSecret=digitalocean-token
```

### Terraform

```hcl
resource "helm_release" "digitalocean_exporter" {
  name       = "digitalocean-exporter"
  namespace  = "monitoring"
  repository = "https://kozaktomas.github.io/digitalocean_exporter"
  chart      = "digitalocean-exporter"
  version    = "0.3.0"
}
```

### Debian package

Download the `.deb` for your architecture from the
[releases page](https://github.com/kozaktomas/digitalocean_exporter/releases), then:

```bash
sudo dpkg -i digitalocean-exporter_*_linux_arm64.deb
sudoedit /etc/digitalocean-exporter/digitalocean-exporter.env   # set DIGITALOCEAN_TOKEN
sudo systemctl start digitalocean-exporter
```

## Configuration

Every flag has an environment-variable equivalent, and flags win over the environment. The
[full reference](https://kozaktomas.github.io/digitalocean_exporter/latest/configuration/) lists
all of them; `--help` prints the same list from the binary itself.

### Token

The exporter only ever reads, so a token with the single scope **`api:read`** — every read
permission, no write permission — runs every collector. You can scope it more tightly and
switch off the collectors you scoped it out of; see
[token permissions](https://kozaktomas.github.io/digitalocean_exporter/latest/configuration/permissions/).

The `spaces` collector is the exception: it uses a Spaces access key, which is an S3
credential unrelated to the API token, and a Read-only limited key is enough.

### Two things that catch everyone once

- A collector is disabled with the **negated flag** — `--no-collector.balance`. Writing
  `--collector.balance=false` is a parse error that stops the process at startup. The
  environment variable does take a value: `COLLECTOR_BALANCE=false`.
- The `balance` collector needs `billing:read`, which is not grantable on every team role.
  A token without it gets `403 Forbidden` from the balance endpoint; turn that one
  collector off.

## Scraping

```yaml
scrape_configs:
  - job_name: digitalocean
    static_configs:
      - targets: ["localhost:9212"]
```

Because collectors refresh in the background, the scrape interval is independent of how
often the DigitalOcean API is called — scrape as often as you like.

## Versioning

Semantic versioning, with one number for everything: `v0.2.0` means exporter 0.2.0, chart
0.2.0, and the documentation at `/0.2/`. While the project is `0.x`, a minor bump may break
compatibility, so pin an exact version.

## Development

```bash
make check        # gofmt -l, go vet, golangci-lint, tests, race detector
make smoke        # end-to-end run against a stub API, no token needed
make docs-serve   # the documentation site, locally
```

See the [development guide](https://kozaktomas.github.io/digitalocean_exporter/latest/development/)
for the architecture, how to add a collector, and how a release is cut.

## Licence

[Apache License 2.0](LICENSE).
