# digitalocean_exporter

A Prometheus exporter for [DigitalOcean](https://www.digitalocean.com/), written in Go.

## Why another one

The existing exporters leave real gaps. The most widely used one has been unmaintained
since 2022, reports Spaces buckets only as *existing* — never their size — and does not
cover the Container Registry at all. DigitalOcean itself does not expose Spaces usage
through its monitoring API, so bucket size has to be measured over the S3-compatible
endpoint. This exporter is built around that constraint from the start.

Collectors refresh in the background on their own intervals and write into an in-memory
snapshot; `/metrics` only ever reads that snapshot. A slow collector can therefore never
block or fail a Prometheus scrape, and each collector can run at whatever cadence its data
actually deserves.

## Status

Early. This release ships the exporter skeleton, the full build and release pipeline, and
thirteen collectors:

| Collector | State |
|---|---|
| `account` — status and resource limits | available |
| `balance` — balance and month-to-date usage (needs a billing-scoped token) | available |
| `databases` — state, nodes and storage of every managed database cluster | available |
| `droplets` — state, size and price of every droplet | available |
| `kubernetes` — cluster state and node pools of every managed cluster | available |
| `limits` — droplets, reserved IPs and volumes in use against the account limits | available |
| `registry` — Container Registry storage, subscription and repositories | available |
| `spaces` — bucket size and object count (needs a Spaces access key) | available |
| `volumes` — size of every block storage volume and what it is attached to | available |
| `loadbalancers` — state, backends and billed size of every load balancer | available |
| `cdn` — CDN endpoints, their cache TTL and certificate | available |
| `dropletmetrics` — CPU, memory, disk and load per droplet (off by default) | available |
| `loadbalancermetrics` — traffic and backend health per load balancer (off by default) | available |
| domains, firewalls, certificates | planned |

## Install

> **No tagged release yet.** Only the `latest` container image is published today, built
> from `main`. The Debian packages and the Helm chart are produced by the release pipeline
> and become available with the first tag.

### Docker

```bash
docker run --rm -p 9212:9212 \
  -e DIGITALOCEAN_TOKEN=dop_v1_... \
  ghcr.io/kozaktomas/digitalocean_exporter:latest
```

The image is published for `linux/amd64` and `linux/arm64`.

### Debian package

Once a release is tagged, download the `.deb` for your architecture from the
[releases page](https://github.com/kozaktomas/digitalocean_exporter/releases), then:

```bash
sudo dpkg -i digitalocean-exporter_*_linux_arm64.deb
sudoedit /etc/digitalocean-exporter/digitalocean-exporter.env   # set DIGITALOCEAN_TOKEN
sudo systemctl start digitalocean-exporter
```

### Helm

From a checkout today, or from the OCI registry once a release is tagged:

```bash
# today
helm install digitalocean-exporter ./charts/digitalocean-exporter \
  --set digitalocean.token=dop_v1_...

# after the first release
helm install digitalocean-exporter \
  oci://ghcr.io/kozaktomas/chart/digitalocean-exporter \
  --set digitalocean.token=dop_v1_...
```

Prefer `--set digitalocean.existingSecret=<name>` in real clusters. The token reaches the
pod as a mounted file rather than an environment variable, so it does not show up in
`kubectl describe pod`.

## Configuration

Every flag has an environment-variable equivalent. Flags win over the environment.

| Flag | Environment variable | Default | Description |
|---|---|---|---|
| `--web.listen-address` | `WEB_LISTEN_ADDRESS` | `:9212` | Address to expose metrics on |
| `--web.config.file` | `WEB_CONFIG_FILE` | — | [exporter-toolkit](https://github.com/prometheus/exporter-toolkit) web config (TLS, basic auth) |
| `--do.token` | `DIGITALOCEAN_TOKEN` | — | API token; read-only is enough, plus the billing scope for `balance` |
| `--do.token-file` | `DIGITALOCEAN_TOKEN_FILE` | — | File holding the API token |
| `--do.timeout` | `DO_TIMEOUT` | `30s` | Timeout of a single collector refresh |
| `--collector.account` | `COLLECTOR_ACCOUNT` | `true` | Enable the account collector |
| `--collector.account.interval` | `COLLECTOR_ACCOUNT_INTERVAL` | `5m` | Its refresh interval |
| `--collector.balance` | `COLLECTOR_BALANCE` | `true` | Enable the balance collector |
| `--collector.balance.interval` | `COLLECTOR_BALANCE_INTERVAL` | `5m` | Its refresh interval |
| `--collector.databases` | `COLLECTOR_DATABASES` | `true` | Enable the managed databases collector |
| `--collector.databases.interval` | `COLLECTOR_DATABASES_INTERVAL` | `5m` | Its refresh interval |
| `--collector.droplets` | `COLLECTOR_DROPLETS` | `true` | Enable the droplets collector |
| `--collector.droplets.interval` | `COLLECTOR_DROPLETS_INTERVAL` | `5m` | Its refresh interval |
| `--collector.kubernetes` | `COLLECTOR_KUBERNETES` | `true` | Enable the managed Kubernetes collector |
| `--collector.kubernetes.interval` | `COLLECTOR_KUBERNETES_INTERVAL` | `5m` | Its refresh interval |
| `--collector.limits` | `COLLECTOR_LIMITS` | `true` | Enable the limits collector |
| `--collector.limits.interval` | `COLLECTOR_LIMITS_INTERVAL` | `5m` | Its refresh interval |
| `--collector.registry` | `COLLECTOR_REGISTRY` | `true` | Enable the container registry collector |
| `--collector.registry.interval` | `COLLECTOR_REGISTRY_INTERVAL` | `5m` | Its refresh interval |
| `--collector.spaces` | `COLLECTOR_SPACES` | `false` | Enable the Spaces collector |
| `--collector.spaces.interval` | `COLLECTOR_SPACES_INTERVAL` | `6h` | Its refresh interval |
| `--collector.spaces.timeout` | `COLLECTOR_SPACES_TIMEOUT` | `15m` | Timeout of one full Spaces refresh |
| `--collector.spaces.bucket` | `COLLECTOR_SPACES_BUCKET` | — | Bucket as `name` or `name@region`, repeatable |
| `--collector.spaces.concurrency` | `COLLECTOR_SPACES_CONCURRENCY` | `4` | Buckets listed at once |
| `--spaces.access-key` | `DIGITALOCEAN_SPACES_KEY` | — | Spaces access key |
| `--spaces.access-key-file` | `DIGITALOCEAN_SPACES_KEY_FILE` | — | File holding the access key |
| `--spaces.secret-key` | `DIGITALOCEAN_SPACES_SECRET` | — | Spaces secret key |
| `--spaces.secret-key-file` | `DIGITALOCEAN_SPACES_SECRET_FILE` | — | File holding the secret key |
| `--spaces.region` | `SPACES_REGION` | — | Region for discovery and for buckets without one |
| `--log.level` | `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `--log.format` | `LOG_FORMAT` | `logfmt` | `logfmt` or `json` |

`--do.token` and `--do.token-file` are mutually exclusive; exactly one must be set.

A collector is switched off with the negated flag — `--no-collector.balance`, not
`--collector.balance=false`, which the flag parser rejects. The environment variable does
take a value: `COLLECTOR_BALANCE=false`. Reach for this when the token cannot read billing:
the balance endpoint answers `403 Forbidden` to a token without the billing scope.

## Spaces

The Spaces collector authenticates with a Spaces access key pair, which is unrelated to the
API token, and is off by default. Sizing a bucket means listing every object in it, so it
refreshes every 6h with a 15m timeout of its own rather than the shared one.

It only ever lists, never downloads, so a **Limited access** key with **Read** on the
observed buckets is enough:

```
digitalocean_exporter --collector.spaces --spaces.region=fra1 \
  --collector.spaces.bucket=assets --collector.spaces.bucket=backups@ams3
```

Given no bucket the collector discovers them itself, which needs a full-access key —
listing all buckets is a full-access capability, and a limited key is told to name its
buckets instead.

## Scraping

```yaml
scrape_configs:
  - job_name: digitalocean
    static_configs:
      - targets: ["localhost:9212"]
```

Because collectors refresh in the background, the scrape interval is independent of how
often the DigitalOcean API is called — scrape as often as you like.

## Metrics

See [docs/metrics.md](docs/metrics.md) for the full list, including the exporter's own
health metrics and suggested alerting rules.

## Development

```bash
make check        # gofmt, go vet, golangci-lint, tests
make test-race    # tests under the race detector
make smoke        # end-to-end run against a stub API, no token needed
make snapshot     # dry-run the release: binaries, deb, tarballs
make docker       # build the multi-arch image
```

`make smoke` starts the exporter against a local stub of the DigitalOcean API, so the whole
chain can be exercised offline.

## Licence

[Apache License 2.0](LICENSE).
