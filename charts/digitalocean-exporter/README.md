# digitalocean-exporter

Prometheus exporter for DigitalOcean

![Version: 0.1.0](https://img.shields.io/badge/Version-0.1.0-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: 0.1.0](https://img.shields.io/badge/AppVersion-0.1.0-informational?style=flat-square)

Full documentation, including a guide to every collector and its cost in API requests,
lives at <https://kozaktomas.github.io/digitalocean_exporter/>.

## Install

```bash
helm repo add digitalocean-exporter https://kozaktomas.github.io/digitalocean_exporter
helm repo update
helm install digitalocean-exporter digitalocean-exporter/digitalocean-exporter \
  --set digitalocean.token=dop_v1_...
```

## Values

<!-- values-start -->
| Key | Type | Default | Description |
|-----|------|---------|-------------|
| affinity | object | `{}` | Affinity rules for the pod. |
| collectors.account.enabled | bool | `true` | Report account status and the account's resource limits. |
| collectors.account.interval | string | `"5m"` | How often the account collector refreshes. |
| collectors.balance.enabled | bool | `true` | Report account balance and month-to-date usage. Reads `/v2/customers/my/balance`, which needs a token with the billing scope; a token scoped to resources alone gets 403 there and the collector reports `collector_success 0`. Disable it for such a token. |
| collectors.balance.interval | string | `"5m"` | How often the balance collector refreshes. |
| collectors.cdn.enabled | bool | `true` | Report CDN endpoints, their cache TTL and the certificate each one serves. DigitalOcean exposes no traffic figures for CDN endpoints, so this is inventory only. |
| collectors.cdn.interval | string | `"5m"` | How often the cdn collector refreshes. |
| collectors.databases.enabled | bool | `true` | Report the state, node count and storage of every managed database cluster. This is inventory, not load: connections and queries come from a per-cluster Prometheus endpoint DigitalOcean runs with credentials of its own. |
| collectors.databases.interval | string | `"5m"` | How often the databases collector refreshes. |
| collectors.dropletmetrics.concurrency | int | `4` | How many droplets are queried at once. |
| collectors.dropletmetrics.enabled | bool | `false` | Report CPU, memory, disk and load per droplet from DigitalOcean's monitoring API. Off by default because that API answers one metric of one droplet per request: a refresh costs one droplet listing plus 10 requests per droplet, against an account limit of 5000 requests an hour. Work out `3600/interval * (1 + droplets*10)` before enabling it. |
| collectors.dropletmetrics.interval | string | `"5m"` | How often the dropletmetrics collector refreshes. The API samples every 2m, so anything shorter buys nothing. |
| collectors.dropletmetrics.timeout | string | `"2m"` | Timeout of one full dropletmetrics refresh. |
| collectors.droplets.enabled | bool | `true` | Report the state, size and price of every droplet, including the droplets that make up a managed Kubernetes cluster. |
| collectors.droplets.interval | string | `"5m"` | How often the droplets collector refreshes. |
| collectors.kubernetes.enabled | bool | `true` | Report managed Kubernetes clusters and their node pools from the outside. What runs inside a cluster is kube-state-metrics' job, not this exporter's. |
| collectors.kubernetes.interval | string | `"5m"` | How often the kubernetes collector refreshes. |
| collectors.limits.enabled | bool | `true` | Report droplets, reserved IPs and volumes in use, which pair with the account limits the account collector reports. |
| collectors.limits.interval | string | `"5m"` | How often the limits collector refreshes. |
| collectors.loadbalancermetrics.concurrency | int | `4` | How many load balancers are queried at once. |
| collectors.loadbalancermetrics.enabled | bool | `false` | Report traffic and per-backend health per load balancer, at 7 monitoring API requests per load balancer per refresh. Off by default, but an account has far fewer load balancers than droplets, so the cost is usually small — and a load balancer cannot run node_exporter, which makes this the only source for its traffic and for which backend droplet is failing its check. |
| collectors.loadbalancermetrics.interval | string | `"5m"` | How often the loadbalancermetrics collector refreshes. |
| collectors.loadbalancermetrics.timeout | string | `"2m"` | Timeout of one full loadbalancermetrics refresh. |
| collectors.loadbalancers.enabled | bool | `true` | Report the state, backend count and billed size of every load balancer. Traffic through them belongs to the loadbalancermetrics collector. |
| collectors.loadbalancers.interval | string | `"5m"` | How often the loadbalancers collector refreshes. |
| collectors.registry.enabled | bool | `true` | Report Container Registry storage, subscription tier and repositories. An account without a registry is not a failure: the collector reports no metrics and keeps `collector_success 1`, so it is safe to leave enabled everywhere. |
| collectors.registry.interval | string | `"5m"` | How often the registry collector refreshes. |
| collectors.spaces.buckets | list | `[]` | Buckets to measure, each as `name` or `name@region`. Leave empty to discover them, which needs a full-access Spaces key; with a limited key the buckets have to be named here. |
| collectors.spaces.concurrency | int | `4` | How many buckets are measured at once. |
| collectors.spaces.enabled | bool | `false` | Report the size and object count of Spaces buckets. Off by default because it needs `spaces.accessKey` and `spaces.secretKey`, a Spaces key pair, which is a separate credential from the API token. |
| collectors.spaces.interval | string | `"5m"` | How often the spaces collector refreshes. |
| collectors.spaces.region | string | `""` | Region used for bucket discovery and for buckets named without one. |
| collectors.spaces.timeout | string | `"2m"` | Timeout of one full Spaces refresh, all buckets together. Separate from the global `--do.timeout` because the collector fans out over buckets. |
| collectors.volumes.enabled | bool | `true` | Report the size of every block storage volume and how many droplets it is attached to. A volume attached to none is billed while serving nothing, which is what makes it worth an alert. |
| collectors.volumes.interval | string | `"5m"` | How often the volumes collector refreshes. |
| digitalocean.existingSecret | string | `""` | Name of a Secret you manage yourself holding the API token. Preferred over `token` in real clusters, because it keeps the token out of your Helm values. |
| digitalocean.existingSecretKey | string | `"token"` | Key inside `digitalocean.existingSecret` that holds the token. |
| digitalocean.token | string | `""` | DigitalOcean API token. The chart puts it in a Secret it owns and mounts it into the pod as a file, never as an environment variable, so it cannot leak through `kubectl describe pod`. Mutually exclusive with `existingSecret`. |
| fullnameOverride | string | `""` | Replaces the generated resource name outright. The generated name is `<release>-<chart>`, collapsed to just the release name when the release name already contains the chart name, so the common `helm install digitalocean-exporter` needs nothing here. |
| image.pullPolicy | string | `"IfNotPresent"` | Image pull policy. |
| image.repository | string | `"ghcr.io/kozaktomas/digitalocean_exporter"` | Container image repository. |
| image.tag | string | `""` | Image tag. Empty means the chart's appVersion, which is the exporter release this chart version was published with. Set it only to pin something else. |
| log.format | string | `"logfmt"` | Log format: `logfmt` or `json`. |
| log.level | string | `"info"` | Log level: `debug`, `info`, `warn` or `error`. |
| nameOverride | string | `""` | Overrides the chart name used in resource names and in the `app.kubernetes.io/name` label. |
| nodeSelector | object | `{}` | Node selector for the pod. |
| resources | object | `{"limits":{"memory":"128Mi"},"requests":{"cpu":"10m","memory":"32Mi"}}` | Container resources. The exporter holds one snapshot per collector in memory and does nothing between refreshes. |
| service.port | int | `9212` | Port the service exposes. The container always listens on 9212. |
| service.type | string | `"ClusterIP"` | Service type. |
| serviceMonitor.enabled | bool | `false` | Create a Prometheus Operator ServiceMonitor. Requires the CRD to exist in the cluster. |
| serviceMonitor.interval | string | `"60s"` | Scrape interval. Collectors refresh in the background, so this is independent of how often the DigitalOcean API is called. |
| serviceMonitor.labels | object | `{}` | Extra labels for the ServiceMonitor, for whatever your Prometheus selects on. |
| serviceMonitor.scrapeTimeout | string | `"10s"` | Scrape timeout. Serving `/metrics` only reads an in-memory snapshot, so it never waits on the API. |
| spaces.accessKey | string | `""` | Spaces access key, used only by the spaces collector. This is an S3 credential, unrelated to the API token. |
| spaces.existingSecret | string | `""` | Name of a Secret you manage yourself holding both Spaces keys. Mutually exclusive with `spaces.accessKey` and `spaces.secretKey`. |
| spaces.existingSecretAccessKeyKey | string | `"spaces-access-key"` | Key inside `spaces.existingSecret` that holds the access key. |
| spaces.existingSecretSecretKeyKey | string | `"spaces-secret-key"` | Key inside `spaces.existingSecret` that holds the secret key. |
| spaces.secretKey | string | `""` | Spaces secret key that pairs with `spaces.accessKey`. |
| tolerations | list | `[]` | Tolerations for the pod. |
<!-- values-end -->

## Maintainers

| Name | Email | Url |
| ---- | ------ | --- |
| Tomas Kozak | <kozak@talko.cz> |  |

## Source Code

* <https://github.com/kozaktomas/digitalocean_exporter>

---

The values table above is generated from the comments in `values.yaml` by
[helm-docs](https://github.com/norwoodj/helm-docs); run `make chart-docs` after editing them.
