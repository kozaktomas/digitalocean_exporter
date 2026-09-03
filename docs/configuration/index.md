# Configuration

Every setting has both a command-line flag and an environment variable. **Flags win over
the environment.** `--help` prints the same list the binary actually has, which is the
authority if this page ever falls behind, and `--version` reports which build you are
holding:

```console
$ digitalocean_exporter --version
digitalocean_exporter, version 0.3.0 (commit 57f5e48, go1.26.1)
```

The version, commit and Go version are whatever your build was stamped with; the line above
is one release's, not a fixed string. Both flags print and exit 0 without needing a token.

## Credentials

```bash
digitalocean_exporter --do.token-file=/etc/digitalocean-exporter/token
```

`--do.token` and `--do.token-file` are **mutually exclusive, and exactly one must be set**.
Starting with neither, or with both, is a configuration error and the exporter exits rather
than running half-configured.

Prefer the file form anywhere real. A token on a command line is visible in `ps` to every
user on the host, and lands in shell history.

Which scopes that token needs — and how to narrow it deliberately — is
[its own page](permissions.md). The short version: `api:read` covers everything the exporter
does, and it can do nothing else.

## Turning a collector off

This trips people up once each, so it is worth stating plainly.

=== "Flags"

    ```bash
    digitalocean_exporter --no-collector.balance
    ```

    Collector switches are kingpin booleans. The negated form is the *only* way to disable
    one on the command line.

    ```bash
    digitalocean_exporter --collector.balance=false   # parse error, process exits
    ```

=== "Environment"

    ```bash
    COLLECTOR_BALANCE=false
    ```

    The environment variable does take a value, and `false` is how you disable it.

=== "Helm"

    ```yaml
    collectors:
      balance:
        enabled: false
    ```

    The chart renders the negated flag for you.

## Filtering

An account shared between teams, or one with a noisy staging region, can be narrowed to a
slice: `--filter.tag` and `--filter.region` make the resource collectors report only the
resources that pass. Both flags are repeatable, and both accept comma-separated values —
the environment form of a repeatable flag is a single string, so commas are how a list is
written there.

A resource passes when it carries **at least one** of the given tags (when any are given)
**and** lies in **one** of the given regions (when any are given). A condition that was not
configured holds for everything, so leaving both flags unset reports the whole account,
exactly as before.

=== "Flags"

    ```bash
    digitalocean_exporter --filter.tag=prod --filter.tag=web --filter.region=fra1
    ```

=== "Environment"

    ```bash
    FILTER_TAG=prod,web
    FILTER_REGION=fra1
    ```

=== "Helm"

    ```yaml
    filters:
      tags: [prod, web]
      regions: [fra1]
    ```

The filter is honoured by exactly these collectors: [`droplets`](collectors.md#droplets),
[`volumes`](collectors.md#volumes), [`loadbalancers`](collectors.md#loadbalancers),
[`databases`](collectors.md#databases), [`kubernetes`](collectors.md#kubernetes),
[`firewalls`](collectors.md#firewalls) — by tag alone, since a cloud firewall has no
region — and both [monitoring-API collectors](monitoring-api.md), which measure only the
droplets and load balancers that pass. Every other collector is unaffected — the
account-wide ones (`account`, `balance`, `limits`, `domains`, `registry`, `spaces`, `cdn`,
`certificates`) describe the account rather than a filterable resource, and the remaining
inventory collectors (`images`, `reservedips`, `apps`, `tags`, `projects`,
`dropletautoscale`) report everything they list.

Filtering happens in the exporter, after listing, so it changes what is reported without
changing what a refresh costs — with two exceptions in the cheaper direction. A filter of
exactly one tag and no region is the one shape the API applies server-side, so the droplet
collectors then use the tag-scoped droplet listing and the pages arrive pre-narrowed. And
the monitoring-API collectors skip the per-resource metric requests of everything the
filter rejects, which on a large account is most of their cost.

## Intervals, timeouts and the API budget

**A scrape costs nothing.** Serving `/metrics` reads an in-memory snapshot and issues no API
requests at all — measured: twenty consecutive scrapes moved
`digitalocean_exporter_api_requests_total` by zero. The API budget is spent by collector
refreshes alone, on their own intervals — and by which collector, since every request is
counted under the name of the refresh that made it:

```promql
sum by (collector) (rate(digitalocean_exporter_api_requests_total[5m])) * 3600
```

That is the figures below, measured rather than assumed. A request made outside a refresh
carries `collector="none"`.

DigitalOcean applies [two limits at once](https://docs.digitalocean.com/reference/api/reference/public-apis/):

| Limit | Value |
|---|---|
| Hourly | **5,000 requests per hour** |
| Burst | **250 requests per minute** |

The burst limit can be tripped while the hourly budget is nearly untouched, which is the
trap worth knowing about — see [the monitoring API](monitoring-api.md#the-budget). The
exporter defends it by [rate-limiting itself](#staying-under-the-burst-limit), which is on
by default.

The seventeen collectors that are on by default cost one to three requests each per refresh.
On a real account with droplets, volumes, a load balancer, a Kubernetes cluster, a database,
a registry, a DNS zone, a snapshot, a reserved IP, an App Platform app, a few tags and one
project, one full refresh comes to **25 requests**:

```
account 1 · apps 1 · balance 1 · cdn 1 · databases 1 · domains 1 · droplet_autoscale 1
droplets 2 · images 1 · kubernetes 2 · load_balancers 1 · projects 2 · registry 3
reserved_ips 2 · reserved_ipv6 1 · tags 1 · volumes 2
```

At the default `5m` that is 12 refreshes an hour — **300 requests, 6% of the hourly
budget**, and 25 in a burst, 10% of the per-minute one. Slightly less in practice, since
`images`, `tags` and `projects` default to a `10m` interval and spend half that often;
slightly more from [`dropletautoscale`](collectors.md#dropletautoscale), which defaults to
`2m` and spends its one request thirty times an hour instead of twelve.
Comfortable, and it scales with how much you own rather than with how often Prometheus
scrapes.

`kubernetes` is the one that counts twice for a single cluster: the list, and then the
versions that cluster could be upgraded to, which is an endpoint of its own and costs a
request per cluster. `--no-collector.kubernetes.upgrades` takes that half away and leaves the
rest.

`reserved_ips` is asked for twice because two collectors read it: [`limits`](collectors.md#limits)
for the account-wide count and [`reservedips`](collectors.md#reservedips) for the addresses
themselves.

Enabling [`firewalls`](collectors.md#firewalls) and
[`certificates`](collectors.md#certificates) adds one request each, so the same account costs
27. The other three are off for reasons of their own: [`spaces`](spaces.md) needs a second
credential, and the two [monitoring API](monitoring-api.md) collectors cost a multiple of
everything above.

!!! note "One endpoint has a stricter limit of its own"

    CDN endpoints allow only **5 requests per 10 seconds**, independently of the two limits
    above. The `cdn` collector makes one request per refresh, so the default `5m` is nowhere
    near it — but an interval under 2 seconds would be.

`--do.timeout` bounds a single collector refresh, and defaults to `30s`. Collectors whose
work is genuinely slower carry a timeout of their own instead — `spaces`,
`dropletmetrics` and `loadbalancermetrics`. Raising the global timeout to accommodate a
slow collector is the wrong lever.

**Every interval and every timeout must be greater than zero**, and the exporter refuses to
start otherwise, naming the flag and the value on stderr and exiting 1. Neither is
survivable at runtime: a zero interval would panic the scheduler once the metrics port was
already bound, and a zero timeout would fail every refresh with a deadline exceeded,
forever. Both are reachable from a chart value, so they are caught at startup instead.

## Staying under the burst limit

Three things keep the exporter inside the 250-requests-a-minute allowance, and none of them
needs tuning on an ordinary account.

**A client-side rate limit.** `--do.rate-limit` (`DO_RATE_LIMIT`, default `4`) caps how many
API requests per second the exporter makes, across every collector at once. Four a second is
240 a minute, just inside the limit, and requests are paced evenly rather than let out in a
clump — an even trickle is what the burst limit is asking for. It applies to the DigitalOcean
API only; the `spaces` collector talks to the S3-compatible endpoint, which has limits of its
own. Set it to `0` to turn it off, which is what a stub API deserves and a real one does not.

Lowering it below the default is the lever to reach for when
[`dropletmetrics`](monitoring-api.md) makes a large account uncomfortable; raising it above
`4` moves the exporter towards being rejected rather than throttled.

**Retries.** A request rejected with `429`, or failed with a `5xx`, is tried again — three
attempts in total. What the exporter waits between them is whatever the response asks for:

- **A `Retry-After` is honoured in full**, however long it is. DigitalOcean sends one when
  the burst limit is hit, and it names the moment the window reopens; coming back sooner is
  an attempt certain to be rejected, and a rejected attempt spends from the hourly budget
  like any other. If that wait does not fit in the time the caller has left — a collector's
  timeout, or the shutdown of the process — the rejection is returned straight away instead,
  so the refresh fails on the API's own answer rather than on a deadline, and the attempts
  it would have spent stay in the budget.
- **A response that names no wait** — a `5xx`, or a `429` carrying neither signal — is
  retried after one second, then two, capped at ten.
- **A connection that fails outright** — a reset, an unexpected EOF, a name that does not
  resolve — is retried on the same budget and the same fallback wait. Every request the
  exporter makes is a bodiless `GET`, so replaying one is safe; a request with a body would
  never be retried.

Two rejections are not retried at all. A `429` that carries no `Retry-After` and reports
`RateLimit-Remaining: 0` is the **hourly** limit rather than the burst one, and nothing
frees that up before the hour turns. And a `4xx` other than `429` is the exporter's own
fault: a `401` is a bad token and a `403` a missing scope, neither of which improves on a
second attempt.

So a rejected burst costs a slower refresh rather than a failed one. Every attempt is
counted by `digitalocean_exporter_api_requests_total`, because every attempt spends from
the budget:

```promql
sum by (status) (rate(digitalocean_exporter_api_requests_total[5m]))
```

A rising `429` rate means the rate limit is set too high for what the collectors are asking
of it; grouping by `collector` instead names the refresh that is asking.

**What is left of the budget.** Three gauges come straight from DigitalOcean's own response
headers — `digitalocean_exporter_api_rate_limit_remaining`,
`digitalocean_exporter_api_rate_limit` and
`digitalocean_exporter_api_rate_limit_reset_timestamp_seconds`. Watch the first against the
second, because the hourly ceiling varies by account and a threshold in requests would not
travel between them:

```promql
digitalocean_exporter_api_rate_limit_remaining / digitalocean_exporter_api_rate_limit
```

The third says when the window refills, which is how long a spent budget keeps the metrics
stale. A response without the headers leaves all three as they were rather than zeroing
them. The bundled [`DigitalOceanExporterRateLimitLow`](../alerting.md) fires below ten
percent.

**Staggered refreshes.** Collectors default to a `5m` interval — `images` to `10m` — and all
start together, so without help they would fire as one burst. Each one's first refresh is
therefore held back by an even share of a window — the **shortest interval** any enabled
collector is configured with, or **three seconds**, whichever is smaller — and every later
refresh keeps that phase. One window for the whole set is what makes the offsets distinct
even when the intervals differ; the ceiling is what keeps `/metrics` worth scraping moments
after startup; and the order is the order the collectors are registered in, so it is the
same on every run.

## Serving metrics

`--web.listen-address` defaults to `:9212`, the port
[allocated to this exporter](https://github.com/prometheus/prometheus/wiki/Default-port-allocations).

`--web.config.file` takes an [exporter-toolkit](https://github.com/prometheus/exporter-toolkit/blob/master/docs/web-configuration.md)
web configuration, which is how you get TLS and basic auth:

```yaml
# web-config.yml
tls_server_config:
  cert_file: /etc/digitalocean-exporter/tls.crt
  key_file: /etc/digitalocean-exporter/tls.key

basic_auth_users:
  prometheus: $2y$10$...   # bcrypt, e.g. from `htpasswd -nBC 10 "" | tr -d ':\n'`
```

## Full reference

### Server and credentials

| Flag | Environment variable | Default | Description |
|---|---|---|---|
| `--web.listen-address` | `WEB_LISTEN_ADDRESS` | `:9212` | Address to expose metrics on |
| `--web.config.file` | `WEB_CONFIG_FILE` | — | exporter-toolkit web config (TLS, basic auth) |
| `--do.token` | `DIGITALOCEAN_TOKEN` | — | API token; read-only is enough |
| `--do.token-file` | `DIGITALOCEAN_TOKEN_FILE` | — | File holding the API token |
| `--do.timeout` | `DO_TIMEOUT` | `30s` | Timeout of a single collector refresh |
| `--do.rate-limit` | `DO_RATE_LIMIT` | `4` | API requests per second, over all collectors; `0` disables it |
| `--log.level` | `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `--log.format` | `LOG_FORMAT` | `logfmt` | `logfmt` or `json` |
| `--filter.tag` | `FILTER_TAG` | — | [Report only resources carrying one of these tags](#filtering); repeatable, or comma-separated |
| `--filter.region` | `FILTER_REGION` | — | [Report only resources in these regions](#filtering); repeatable, or comma-separated |

### Collectors on by default

| Flag | Environment variable | Default | Description |
|---|---|---|---|
| `--collector.account` | `COLLECTOR_ACCOUNT` | `true` | [Account status and limits](collectors.md#account) |
| `--collector.account.interval` | `COLLECTOR_ACCOUNT_INTERVAL` | `5m` | Its refresh interval |
| `--collector.balance` | `COLLECTOR_BALANCE` | `true` | [Balance and month-to-date usage](collectors.md#balance) |
| `--collector.balance.interval` | `COLLECTOR_BALANCE_INTERVAL` | `5m` | Its refresh interval |
| `--collector.databases` | `COLLECTOR_DATABASES` | `true` | [Managed database clusters](collectors.md#databases) |
| `--collector.databases.interval` | `COLLECTOR_DATABASES_INTERVAL` | `5m` | Its refresh interval |
| `--collector.droplets` | `COLLECTOR_DROPLETS` | `true` | [Droplets](collectors.md#droplets) |
| `--collector.droplets.interval` | `COLLECTOR_DROPLETS_INTERVAL` | `5m` | Its refresh interval |
| `--collector.kubernetes` | `COLLECTOR_KUBERNETES` | `true` | [Managed Kubernetes clusters](collectors.md#kubernetes) |
| `--collector.kubernetes.interval` | `COLLECTOR_KUBERNETES_INTERVAL` | `5m` | Its refresh interval |
| `--collector.kubernetes.upgrades` | `COLLECTOR_KUBERNETES_UPGRADES` | `true` | [Ask what each cluster can be upgraded to](collectors.md#kubernetes), at one request per cluster |
| `--collector.limits` | `COLLECTOR_LIMITS` | `true` | [Resources in use against limits](collectors.md#limits) |
| `--collector.limits.interval` | `COLLECTOR_LIMITS_INTERVAL` | `5m` | Its refresh interval |
| `--collector.registry` | `COLLECTOR_REGISTRY` | `true` | [Container Registry](collectors.md#registry) |
| `--collector.registry.interval` | `COLLECTOR_REGISTRY_INTERVAL` | `5m` | Its refresh interval |
| `--collector.reservedips` | `COLLECTOR_RESERVEDIPS` | `true` | [Reserved IPs and what they are assigned to](collectors.md#reservedips) |
| `--collector.reservedips.interval` | `COLLECTOR_RESERVEDIPS_INTERVAL` | `5m` | Its refresh interval |
| `--collector.volumes` | `COLLECTOR_VOLUMES` | `true` | [Block storage volumes](collectors.md#volumes) |
| `--collector.volumes.interval` | `COLLECTOR_VOLUMES_INTERVAL` | `5m` | Its refresh interval |
| `--collector.images` | `COLLECTOR_IMAGES` | `true` | [Snapshots, backups and custom images](collectors.md#images) |
| `--collector.images.interval` | `COLLECTOR_IMAGES_INTERVAL` | `10m` | Its refresh interval |
| `--collector.loadbalancers` | `COLLECTOR_LOADBALANCERS` | `true` | [Load balancers](collectors.md#loadbalancers) |
| `--collector.loadbalancers.interval` | `COLLECTOR_LOADBALANCERS_INTERVAL` | `5m` | Its refresh interval |
| `--collector.cdn` | `COLLECTOR_CDN` | `true` | [CDN endpoints](collectors.md#cdn) |
| `--collector.cdn.interval` | `COLLECTOR_CDN_INTERVAL` | `5m` | Its refresh interval |
| `--collector.domains` | `COLLECTOR_DOMAINS` | `true` | [DNS zones](collectors.md#domains) |
| `--collector.domains.interval` | `COLLECTOR_DOMAINS_INTERVAL` | `5m` | Its refresh interval |

### Firewalls and certificates

Off by default, but not because of what they cost — one request each per refresh, the same as
the collectors above. They are off because a firewall ruleset changes when somebody deploys
and a certificate when it is renewed, so most accounts have no reason to scrape them. Enable
them to alert on [pending firewall changes](collectors.md#firewalls) or
[certificate expiry](collectors.md#certificates).

| Flag | Environment variable | Default | Description |
|---|---|---|---|
| `--collector.firewalls` | `COLLECTOR_FIREWALLS` | `false` | [Cloud firewalls](collectors.md#firewalls) |
| `--collector.firewalls.interval` | `COLLECTOR_FIREWALLS_INTERVAL` | `5m` | Its refresh interval |
| `--collector.certificates` | `COLLECTOR_CERTIFICATES` | `false` | [TLS certificates](collectors.md#certificates) |
| `--collector.certificates.interval` | `COLLECTOR_CERTIFICATES_INTERVAL` | `5m` | Its refresh interval |

### Spaces

Off by default, because it takes a Spaces key pair rather than the API token. See
[Spaces](spaces.md) for where the size comes from and which kind of key you need.

| Flag | Environment variable | Default | Description |
|---|---|---|---|
| `--collector.spaces` | `COLLECTOR_SPACES` | `false` | Enable the Spaces collector |
| `--collector.spaces.interval` | `COLLECTOR_SPACES_INTERVAL` | `5m` | Its refresh interval |
| `--collector.spaces.timeout` | `COLLECTOR_SPACES_TIMEOUT` | `2m` | Timeout of one full Spaces refresh |
| `--collector.spaces.bucket` | `COLLECTOR_SPACES_BUCKET` | — | Bucket as `name` or `name@region`; repeatable, or comma-separated |
| `--collector.spaces.concurrency` | `COLLECTOR_SPACES_CONCURRENCY` | `4` | Buckets measured at once |
| `--spaces.access-key` | `DIGITALOCEAN_SPACES_KEY` | — | Spaces access key |
| `--spaces.access-key-file` | `DIGITALOCEAN_SPACES_KEY_FILE` | — | File holding the access key |
| `--spaces.secret-key` | `DIGITALOCEAN_SPACES_SECRET` | — | Spaces secret key |
| `--spaces.secret-key-file` | `DIGITALOCEAN_SPACES_SECRET_FILE` | — | File holding the secret key |
| `--spaces.region` | `SPACES_REGION` | — | Region for discovery and for buckets without one |

### Monitoring API collectors

Off by default because of what they cost. Do the arithmetic on
[the monitoring API page](monitoring-api.md) before enabling either.

| Flag | Environment variable | Default | Description |
|---|---|---|---|
| `--collector.dropletmetrics` | `COLLECTOR_DROPLETMETRICS` | `false` | [CPU, memory, disk and load per droplet](monitoring-api.md#dropletmetrics) |
| `--collector.dropletmetrics.interval` | `COLLECTOR_DROPLETMETRICS_INTERVAL` | `5m` | Its refresh interval |
| `--collector.dropletmetrics.timeout` | `COLLECTOR_DROPLETMETRICS_TIMEOUT` | `2m` | Timeout of one full refresh |
| `--collector.dropletmetrics.concurrency` | `COLLECTOR_DROPLETMETRICS_CONCURRENCY` | `4` | Droplets queried at once |
| `--collector.dropletmetrics.agent-only` | `COLLECTOR_DROPLETMETRICS_AGENT_ONLY` | `false` | [Skip droplets whose listing reports no monitoring agent](monitoring-api.md#dropletmetrics) |
| `--collector.loadbalancermetrics` | `COLLECTOR_LOADBALANCERMETRICS` | `false` | [Traffic and backend health](monitoring-api.md#loadbalancermetrics) |
| `--collector.loadbalancermetrics.interval` | `COLLECTOR_LOADBALANCERMETRICS_INTERVAL` | `5m` | Its refresh interval |
| `--collector.loadbalancermetrics.timeout` | `COLLECTOR_LOADBALANCERMETRICS_TIMEOUT` | `2m` | Timeout of one full refresh |
| `--collector.loadbalancermetrics.concurrency` | `COLLECTOR_LOADBALANCERMETRICS_CONCURRENCY` | `4` | Load balancers queried at once |
