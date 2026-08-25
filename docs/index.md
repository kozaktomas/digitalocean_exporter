# digitalocean_exporter

A Prometheus exporter for [DigitalOcean](https://www.digitalocean.com/), written in Go.
One binary, no state, no database. It reads the DigitalOcean API and exposes metrics on
`:9212`.

```bash
docker run --rm -p 9212:9212 \
  -e DIGITALOCEAN_TOKEN=dop_v1_... \
  ghcr.io/kozaktomas/digitalocean_exporter:latest
```

[Install](install/index.md){ .md-button .md-button--primary }
[Configuration](configuration/index.md){ .md-button }
[Metrics](metrics.md){ .md-button }

## Why another one

The existing exporters leave real gaps. The most widely used one has been unmaintained
since 2022, reports Spaces buckets only as *existing* — never their size — and does not
cover the Container Registry at all. DigitalOcean itself does not expose Spaces usage
through its monitoring API, so bucket size has to be measured over the S3-compatible
endpoint. This exporter is built around that constraint from the start.

## How it works

**Refreshing is separate from scraping.** Every collector does its I/O on its own schedule
and writes the result into an in-memory snapshot. Serving `/metrics` only ever reads that
snapshot.

```
DigitalOcean API ──(every 5m, per collector)──▶ snapshot ──(on scrape)──▶ /metrics
```

Three things follow from that, and they are worth knowing before you tune anything:

- **A slow collector cannot fail a scrape.** Sizing a large Spaces bucket takes minutes;
  a Prometheus scrape timeout is usually ten seconds. Because the two are decoupled, the
  slow measurement simply happens elsewhere.
- **Scrape interval and API cost are unrelated.** Scraping every 15 seconds does not call
  DigitalOcean any more often than scraping every 5 minutes. The API budget is set by the
  collector intervals alone, which matters against the limit of 5000 requests an hour.
- **A failed refresh keeps the previous values** and sets `collector_success` to 0. Metrics
  are never dropped on error, because a gap in a graph reads as *DigitalOcean went away*,
  which is a different incident from *the exporter cannot reach the API*. Before its first
  successful refresh a collector emits nothing at all, rather than a misleading zero.

## Collectors

| Collector | Reports | Default |
|---|---|---|
| [`account`](configuration/collectors.md#account) | Account status and resource limits | on |
| [`balance`](configuration/collectors.md#balance) | Balance and month-to-date usage | on |
| [`databases`](configuration/collectors.md#databases) | Managed database clusters, nodes, storage | on |
| [`droplets`](configuration/collectors.md#droplets) | Every droplet, its state, size and price | on |
| [`kubernetes`](configuration/collectors.md#kubernetes) | Managed clusters and their node pools | on |
| [`limits`](configuration/collectors.md#limits) | Droplets, reserved IPs and volumes in use | on |
| [`registry`](configuration/collectors.md#registry) | Container Registry storage and repositories | on |
| [`volumes`](configuration/collectors.md#volumes) | Block storage volumes and what uses them | on |
| [`loadbalancers`](configuration/collectors.md#loadbalancers) | Load balancers, backends, billed size | on |
| [`cdn`](configuration/collectors.md#cdn) | CDN endpoints and their certificates | on |
| [`spaces`](configuration/spaces.md) | Bucket size and object count | **off** |
| [`dropletmetrics`](configuration/monitoring-api.md#dropletmetrics) | CPU, memory, disk and load per droplet | **off** |
| [`loadbalancermetrics`](configuration/monitoring-api.md#loadbalancermetrics) | Traffic and backend health per load balancer | **off** |

The three that are off by default are off for a reason — each one costs far more API
requests than the others, or takes far longer. The pages linked above do the arithmetic.

Domains, firewalls and certificates are planned.

## Versioning

Releases follow [semantic versioning](https://semver.org/), and one number covers
everything: `v0.2.0` means exporter 0.2.0, chart 0.2.0, and the documentation you are
reading under `/0.2/`. Pin that one number and the documentation next to it describes
exactly the build you are running.

!!! warning "While the version is 0.x"

    A minor bump may break compatibility — metric names, flags and chart values can all
    change. Pin an exact version rather than a range until 1.0.

## Licence

[Apache License 2.0](https://github.com/kozaktomas/digitalocean_exporter/blob/main/LICENSE).
