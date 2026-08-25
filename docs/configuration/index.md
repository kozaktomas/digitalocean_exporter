# Configuration

Every setting has both a command-line flag and an environment variable. **Flags win over
the environment.** `--help` prints the same list the binary actually has, which is the
authority if this page ever falls behind.

## Credentials

```bash
digitalocean_exporter --do.token-file=/etc/digitalocean-exporter/token
```

`--do.token` and `--do.token-file` are **mutually exclusive, and exactly one must be set**.
Starting with neither, or with both, is a configuration error and the exporter exits rather
than running half-configured.

Prefer the file form anywhere real. A token on a command line is visible in `ps` to every
user on the host, and lands in shell history.

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

## Intervals, timeouts and the API budget

Each collector refreshes on its own interval, independent of scraping. The DigitalOcean API
allows **5000 requests an hour**, which is the budget every interval spends from.

Most collectors cost one or two requests per refresh, so at the default `5m` the ten
enabled-by-default collectors use a couple of hundred requests an hour — a few per cent of
the budget. The three that are off by default are off because they are not like that; see
[Spaces](spaces.md) and [the monitoring API](monitoring-api.md).

`--do.timeout` bounds a single collector refresh, and defaults to `30s`. Collectors whose
work is genuinely slower carry a timeout of their own instead — `spaces`,
`dropletmetrics` and `loadbalancermetrics`. Raising the global timeout to accommodate a
slow collector is the wrong lever.

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
| `--log.level` | `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `--log.format` | `LOG_FORMAT` | `logfmt` | `logfmt` or `json` |

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
| `--collector.limits` | `COLLECTOR_LIMITS` | `true` | [Resources in use against limits](collectors.md#limits) |
| `--collector.limits.interval` | `COLLECTOR_LIMITS_INTERVAL` | `5m` | Its refresh interval |
| `--collector.registry` | `COLLECTOR_REGISTRY` | `true` | [Container Registry](collectors.md#registry) |
| `--collector.registry.interval` | `COLLECTOR_REGISTRY_INTERVAL` | `5m` | Its refresh interval |
| `--collector.volumes` | `COLLECTOR_VOLUMES` | `true` | [Block storage volumes](collectors.md#volumes) |
| `--collector.volumes.interval` | `COLLECTOR_VOLUMES_INTERVAL` | `5m` | Its refresh interval |
| `--collector.loadbalancers` | `COLLECTOR_LOADBALANCERS` | `true` | [Load balancers](collectors.md#loadbalancers) |
| `--collector.loadbalancers.interval` | `COLLECTOR_LOADBALANCERS_INTERVAL` | `5m` | Its refresh interval |
| `--collector.cdn` | `COLLECTOR_CDN` | `true` | [CDN endpoints](collectors.md#cdn) |
| `--collector.cdn.interval` | `COLLECTOR_CDN_INTERVAL` | `5m` | Its refresh interval |

### Spaces

Off by default. See [Spaces](spaces.md) for why, and for which kind of key you need.

| Flag | Environment variable | Default | Description |
|---|---|---|---|
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

### Monitoring API collectors

Off by default because of what they cost. Do the arithmetic on
[the monitoring API page](monitoring-api.md) before enabling either.

| Flag | Environment variable | Default | Description |
|---|---|---|---|
| `--collector.dropletmetrics` | `COLLECTOR_DROPLETMETRICS` | `false` | [CPU, memory, disk and load per droplet](monitoring-api.md#dropletmetrics) |
| `--collector.dropletmetrics.interval` | `COLLECTOR_DROPLETMETRICS_INTERVAL` | `5m` | Its refresh interval |
| `--collector.dropletmetrics.timeout` | `COLLECTOR_DROPLETMETRICS_TIMEOUT` | `2m` | Timeout of one full refresh |
| `--collector.dropletmetrics.concurrency` | `COLLECTOR_DROPLETMETRICS_CONCURRENCY` | `4` | Droplets queried at once |
| `--collector.loadbalancermetrics` | `COLLECTOR_LOADBALANCERMETRICS` | `false` | [Traffic and backend health](monitoring-api.md#loadbalancermetrics) |
| `--collector.loadbalancermetrics.interval` | `COLLECTOR_LOADBALANCERMETRICS_INTERVAL` | `5m` | Its refresh interval |
| `--collector.loadbalancermetrics.timeout` | `COLLECTOR_LOADBALANCERMETRICS_TIMEOUT` | `2m` | Timeout of one full refresh |
| `--collector.loadbalancermetrics.concurrency` | `COLLECTOR_LOADBALANCERMETRICS_CONCURRENCY` | `4` | Load balancers queried at once |
