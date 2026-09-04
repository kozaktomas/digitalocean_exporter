# digitalocean-exporter

Prometheus exporter for DigitalOcean

![Version: 0.4.0](https://img.shields.io/badge/Version-0.4.0-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: 0.4.0](https://img.shields.io/badge/AppVersion-0.4.0-informational?style=flat-square)

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
| collectors.apps.enabled | bool | `true` | Report App Platform apps: tier, region, the phase of the active deployment, whether one is in progress, and the instances each component of the spec asks for. One list request covers the whole account. The runtime metrics of an app — CPU, memory, restarts — are not here: they live behind DigitalOcean's monitoring API, which the API client has no methods for. |
| collectors.apps.interval | string | `"5m"` | How often the apps collector refreshes. |
| collectors.balance.enabled | bool | `true` | Report account balance and month-to-date usage. Reads `/v2/customers/my/balance`, which needs a token with the billing scope; a token scoped to resources alone gets 403 there and the collector reports `collector_success 0`. Disable it for such a token. |
| collectors.balance.interval | string | `"5m"` | How often the balance collector refreshes. |
| collectors.cdn.enabled | bool | `true` | Report CDN endpoints, their cache TTL and the certificate each one serves. DigitalOcean exposes no traffic figures for CDN endpoints, so this is inventory only. |
| collectors.cdn.interval | string | `"5m"` | How often the cdn collector refreshes. |
| collectors.certificates.enabled | bool | `false` | Report the TLS certificates the account holds, and when each one expires. Off by default because a certificate changes when it is renewed and not otherwise; enable it to alert on `digitalocean_certificate_expiry_timestamp_seconds`, which catches a `lets_encrypt` renewal that failed quietly. |
| collectors.certificates.interval | string | `"5m"` | How often the certificates collector refreshes. |
| collectors.databases.details | bool | `true` | Ask each cluster for its read-only replicas and the age of its newest backup. The only part of this collector that costs requests per cluster — two per refresh — rather than one per refresh, and the only source of the replica and backup metrics an alert can watch. Turn it off on an account with many clusters where those requests are worth saving. |
| collectors.databases.enabled | bool | `true` | Report the state, node count and storage of every managed database cluster. This is inventory, not load: connections and queries come from a per-cluster Prometheus endpoint DigitalOcean runs with credentials of its own. |
| collectors.databases.interval | string | `"5m"` | How often the databases collector refreshes. |
| collectors.databases.timeout | string | `"2m"` | Timeout of one full databases refresh, including the per-cluster detail lookups. |
| collectors.domains.enabled | bool | `true` | Report the DNS zones the account hosts and their default TTL. One list request covers the whole account. The records inside a zone are not reported: counting them would cost a request per zone. |
| collectors.domains.interval | string | `"5m"` | How often the domains collector refreshes. |
| collectors.dropletautoscale.enabled | bool | `true` | Report every droplet autoscale pool: how many droplets it runs against its minimum, maximum or fixed target, and the current CPU and memory utilisation against the targets it scales on. One paged list request covers the whole account. |
| collectors.dropletautoscale.interval | string | `"2m"` | How often the dropletautoscale collector refreshes. Two minutes rather than five because the utilisation the pool scales on moves on that cadence, and a scaling event is exactly what this collector is watching for. |
| collectors.dropletmetrics.agentOnly | bool | `false` | Measure only droplets whose listing reports DigitalOcean's monitoring agent, saving the 10 requests a droplet without it costs for no readings. Off by default: the feature is set on droplets created with the agent, so one installed later — or a managed Kubernetes node, which reports readings without it — would go unmeasured and disappear from the metrics. |
| collectors.dropletmetrics.concurrency | int | `4` | How many droplets are queried at once. |
| collectors.dropletmetrics.enabled | bool | `false` | Report CPU, memory, disk and load per droplet from DigitalOcean's monitoring API. Off by default because that API answers one metric of one droplet per request: a refresh costs one droplet listing plus 10 requests per droplet, against an account limit of 5000 requests an hour. Work out `3600/interval * (1 + droplets*10)` before enabling it. |
| collectors.dropletmetrics.interval | string | `"5m"` | How often the dropletmetrics collector refreshes. The API samples every 2m, so anything shorter buys nothing. |
| collectors.dropletmetrics.timeout | string | `"2m"` | Timeout of one full dropletmetrics refresh. |
| collectors.droplets.enabled | bool | `true` | Report the state, size and price of every droplet, including the droplets that make up a managed Kubernetes cluster. |
| collectors.droplets.interval | string | `"5m"` | How often the droplets collector refreshes. |
| collectors.firewalls.enabled | bool | `false` | Report cloud firewalls: what each is attached to, how many rules it carries, how many of those are open to the whole internet, and how many droplets a change has not reached yet. Off by default because a ruleset changes on deploys rather than continuously. |
| collectors.firewalls.interval | string | `"5m"` | How often the firewalls collector refreshes. |
| collectors.images.enabled | bool | `true` | Report the private images the account stores: droplet and volume snapshots, automatic droplet backups and uploaded custom images. DigitalOcean bills every one of them by size for as long as it exists, and nothing in the control panel nags about a snapshot nobody needs any more. |
| collectors.images.interval | string | `"10m"` | How often the images collector refreshes. Ten minutes rather than five: an image is created by a snapshot or a nightly backup, hours apart, so reading the list more often only spends requests. |
| collectors.kubernetes.enabled | bool | `true` | Report managed Kubernetes clusters and their node pools from the outside. What runs inside a cluster is kube-state-metrics' job, not this exporter's. |
| collectors.kubernetes.interval | string | `"5m"` | How often the kubernetes collector refreshes. |
| collectors.kubernetes.upgrades | bool | `true` | Ask what each cluster can be upgraded to. It is the only part of this collector that costs a request per cluster rather than one per refresh, and the only source of the upgrade metrics an alert can watch. Turn it off on an account with many clusters where that per-cluster request is worth saving. |
| collectors.limits.enabled | bool | `true` | Report droplets, reserved IPs and volumes in use, which pair with the account limits the account collector reports. |
| collectors.limits.interval | string | `"5m"` | How often the limits collector refreshes. |
| collectors.loadbalancermetrics.concurrency | int | `4` | How many load balancers are queried at once. |
| collectors.loadbalancermetrics.enabled | bool | `false` | Report traffic and per-backend health per load balancer, at 7 monitoring API requests per load balancer per refresh. Off by default, but an account has far fewer load balancers than droplets, so the cost is usually small — and a load balancer cannot run node_exporter, which makes this the only source for its traffic and for which backend droplet is failing its check. |
| collectors.loadbalancermetrics.extended | bool | `false` | Also read the extended metric set per load balancer: TLS connections, request queue size, latency percentiles (p50/p95/p99 and averages), network throughput and firewall drops. Off by default because it raises the cost from 7 to 27 monitoring API requests per load balancer per refresh. |
| collectors.loadbalancermetrics.interval | string | `"5m"` | How often the loadbalancermetrics collector refreshes. |
| collectors.loadbalancermetrics.timeout | string | `"2m"` | Timeout of one full loadbalancermetrics refresh. |
| collectors.loadbalancers.enabled | bool | `true` | Report the state, backend count and billed size of every load balancer. Traffic through them belongs to the loadbalancermetrics collector. |
| collectors.loadbalancers.interval | string | `"5m"` | How often the loadbalancers collector refreshes. |
| collectors.projects.enabled | bool | `true` | Report every project and how many resources of each type it owns, counted by URN type from the project's resources list. Costs one API request per project per refresh on top of the project list. |
| collectors.projects.interval | string | `"10m"` | How often the projects collector refreshes. Resources move between projects when somebody reassigns them, so ten minutes is plenty. |
| collectors.projects.timeout | string | `"2m"` | Timeout of one full projects refresh, including the per-project resources lookups. Separate from the global `--do.timeout` because the collector fans out over projects. |
| collectors.registry.enabled | bool | `true` | Report Container Registry storage, subscription tier and repositories. An account without a registry is not a failure: the collector reports no metrics and keeps `collector_success 1`, so it is safe to leave enabled everywhere. |
| collectors.registry.interval | string | `"5m"` | How often the registry collector refreshes. |
| collectors.reservedips.enabled | bool | `true` | Report every reserved IP address and whether it is assigned to a droplet. DigitalOcean bills a reserved IP that is assigned to nothing, the same way it bills an unattached volume, and both IPv4 and IPv6 addresses are read. |
| collectors.reservedips.interval | string | `"5m"` | How often the reservedips collector refreshes. |
| collectors.spaces.buckets | list | `[]` | Buckets to measure, each as `name` or `name@region`. An entry may also name several buckets separated by commas. Leave empty to discover them, which needs a full-access Spaces key; with a limited key the buckets have to be named here. |
| collectors.spaces.concurrency | int | `4` | How many buckets are measured at once. |
| collectors.spaces.enabled | bool | `false` | Report the size and object count of Spaces buckets. Off by default because it needs `spaces.accessKey` and `spaces.secretKey`, a Spaces key pair, which is a separate credential from the API token. |
| collectors.spaces.interval | string | `"5m"` | How often the spaces collector refreshes. |
| collectors.spaces.region | string | `""` | Region used for bucket discovery and for buckets named without one. |
| collectors.spaces.timeout | string | `"2m"` | Timeout of one full Spaces refresh, all buckets together. Separate from the global `--do.timeout` because the collector fans out over buckets. |
| collectors.tags.enabled | bool | `true` | Report how many resources of each type carry every tag, straight from the tag list: one paged request per refresh however many resources the tags are spread across. |
| collectors.tags.interval | string | `"10m"` | How often the tags collector refreshes. A tag set changes on deploys rather than continuously, so ten minutes is plenty. |
| collectors.uptime.enabled | bool | `false` | Report DigitalOcean Uptime checks: what each one probes, the status and thirty-day uptime each probing region measured, and the previous outage. Off by default because the check list answers only the configuration — everything a region observed costs one further request per check per refresh — and because Uptime is a paid feature an account may not have, where every refresh would spend a request being told so. |
| collectors.uptime.interval | string | `"2m"` | How often the uptime collector refreshes. DigitalOcean probes on the order of a minute, so two minutes keeps a region's status roughly one probe old without doubling the request cost. |
| collectors.uptime.timeout | string | `"1m"` | Timeout of one full uptime refresh, every check together. Separate from the global `--do.timeout` because the collector fans out over checks. |
| collectors.volumes.enabled | bool | `true` | Report the size of every block storage volume and how many droplets it is attached to. A volume attached to none is billed while serving nothing, which is what makes it worth an alert. |
| collectors.volumes.interval | string | `"5m"` | How often the volumes collector refreshes. |
| digitalocean.existingSecret | string | `""` | Name of a Secret you manage yourself holding the API token. Preferred over `token` in real clusters, because it keeps the token out of your Helm values. |
| digitalocean.existingSecretKey | string | `"token"` | Key inside `digitalocean.existingSecret` that holds the token. |
| digitalocean.rateLimit | int | `4` | Client-side limit on API requests per second, shared by every collector. DigitalOcean allows 250 requests a minute, and the collectors would otherwise spend their refreshes in bursts; 4 a second is 240 a minute, just inside it. Set it to `0` to turn the limiter off; a negative value is rejected at startup. |
| digitalocean.token | string | `""` | DigitalOcean API token. The chart puts it in a Secret it owns and mounts it into the pod as a file, never as an environment variable, so it cannot leak through `kubectl describe pod`. Mutually exclusive with `existingSecret`. |
| extraArgs | list | `[]` | Extra command-line flags for the exporter, appended after the ones the chart renders. This is for a flag that has no value of its own here yet, such as `--do.timeout=20s`. It cannot override one the chart already renders: the flag parser rejects a repeated flag and the container then crash-loops. |
| extraEnv | list | `[]` | Extra environment entries for the container, in Kubernetes `env` form, so `valueFrom` works as well as plain `value`. This is for what the cluster imposes — a proxy, a resource attribute for a tracing sidecar — not for credentials: the token stays in the mounted Secret, where it cannot leak through `kubectl describe pod`. |
| extraVolumeMounts | list | `[]` | Extra volume mounts for the container. The container's root filesystem is read-only, so any file the exporter must read — a web configuration, a private CA bundle — has to arrive through one of these. |
| extraVolumes | list | `[]` | Extra volumes for the pod, in Kubernetes `volumes` form — e.g. a Secret holding an exporter-toolkit web configuration for TLS or basic auth. Pair each entry with one in `extraVolumeMounts`. |
| filters.regions | list | `[]` | Report only resources in these regions, by slug (for example `fra1`). Empty means every region. When both lists are set a resource must satisfy both. Cloud firewalls have no region and are matched by tag alone. |
| filters.tags | list | `[]` | Report only resources carrying at least one of these tags. Empty means every resource. The filter is applied by the resource collectors — droplets, volumes, load balancers, databases, Kubernetes, firewalls and both monitoring-API collectors; the account-wide collectors ignore it. |
| fullnameOverride | string | `""` | Replaces the generated resource name outright. The generated name is `<release>-<chart>`, collapsed to just the release name when the release name already contains the chart name, so the common `helm install digitalocean-exporter` needs nothing here. |
| grafana.dashboards.enabled | bool | `false` | Render the bundled Grafana dashboards as ConfigMaps, one per dashboard, labelled for the Grafana sidecar to load. Off by default: without the sidecar running, these are ConfigMaps nothing ever reads. |
| grafana.dashboards.folder | string | `""` | Grafana folder to file the dashboards into, through a `grafana_folder` annotation. It only takes effect if the Grafana sidecar itself runs with `folderAnnotation: grafana_folder` and `provider.foldersFromFilesStructure: true`; without that the annotation is inert and the dashboards land in the sidecar's default folder. |
| grafana.dashboards.label | string | `"grafana_dashboard"` | Label key the Grafana sidecar watches for. The default matches the sidecar shipped with kube-prometheus-stack and the Grafana chart. |
| grafana.dashboards.labelValue | string | `"1"` | Value of that label. |
| image.pullPolicy | string | `"IfNotPresent"` | Image pull policy. |
| image.repository | string | `"ghcr.io/kozaktomas/digitalocean_exporter"` | Container image repository. |
| image.tag | string | `""` | Image tag. Empty means the chart's appVersion, which is the exporter release this chart version was published with. Set it only to pin something else. |
| imagePullSecrets | list | `[]` | Secrets granting the node access to the image registry, as `[{name: my-pull-secret}]`. Only needed when `image.repository` has been repointed at a private mirror; the public GHCR image needs none. |
| log.format | string | `"logfmt"` | Log format: `logfmt` or `json`. |
| log.level | string | `"info"` | Log level: `debug`, `info`, `warn` or `error`. |
| nameOverride | string | `""` | Overrides the chart name used in resource names and in the `app.kubernetes.io/name` label. |
| networkPolicy.enabled | bool | `false` | Create a NetworkPolicy for the pod. Off by default because it only does anything on a cluster whose CNI enforces NetworkPolicy — on one that does not, it renders and is silently ignored. When on, egress is limited to DNS and to TCP 443, which is all the exporter needs: the DigitalOcean API and Spaces both live behind HTTPS. Ingress is limited to the metrics port, from whatever the two selectors below allow. |
| networkPolicy.ingress.namespaceSelector | object | `{}` | Namespaces allowed to scrape the metrics port, as a label selector. The empty selector allows every namespace; narrow it to where your Prometheus runs, e.g. `{matchLabels: {kubernetes.io/metadata.name: monitoring}}`. |
| networkPolicy.ingress.podSelector | object | `{}` | Pods within those namespaces allowed to scrape, as a label selector. The empty selector allows every pod. |
| nodeSelector | object | `{}` | Node selector for the pod. |
| podAnnotations | object | `{}` | Extra annotations for the pod. The chart already sets a checksum of the Secret here, and merges these on top of it. |
| podLabels | object | `{}` | Extra labels for the pod, merged over the chart's own — for whatever your cluster tooling selects pods on, such as a cost allocation or network policy label. Do not repeat a label the chart already sets. |
| priorityClassName | string | `""` | PriorityClass for the pod. An exporter is worth less than what it watches, so a cluster with priority classes usually wants a low one here rather than the default. |
| probes.scheme | string | `"HTTP"` | Scheme the liveness and readiness probes use, `HTTP` or `HTTPS`. Switch it to `HTTPS` whenever a web configuration passed through `extraArgs` turns TLS on: the toolkit serves every path over TLS, probes included, and a probe left on plain HTTP fails against the TLS listener until the pod crash-loops. The kubelet skips certificate verification on HTTPS probes, so a self-signed pair works. |
| prometheusRule.enabled | bool | `false` | Create a Prometheus Operator PrometheusRule holding the bundled alerting rules. Requires the CRD to exist in the cluster. Off by default, like the ServiceMonitor: a chart that installs alerts nobody asked for is a chart that pages somebody at 3am. |
| prometheusRule.labels | object | `{}` | Extra labels for the PrometheusRule, for whatever your Prometheus selects rules on. For kube-prometheus-stack that is usually `release: <release name>`. |
| prometheusRule.namespace | string | `""` | Namespace to create the PrometheusRule in. Empty means the release namespace. Some Prometheus installations only pick up rules from their own namespace (`ruleNamespaceSelector` unset with Prometheus running elsewhere), which is when this is worth setting. |
| resources | object | `{"limits":{"memory":"128Mi"},"requests":{"cpu":"10m","memory":"32Mi"}}` | Container resources. The exporter holds one snapshot per collector in memory and does nothing between refreshes. |
| service.port | int | `9212` | Port the service exposes, and the port the container listens on: the chart renders this value into `--web.listen-address` as well as into `containerPort`. |
| service.type | string | `"ClusterIP"` | Service type. |
| serviceMonitor.enabled | bool | `false` | Create a Prometheus Operator ServiceMonitor. Requires the CRD to exist in the cluster. |
| serviceMonitor.honorLabels | bool | `false` | Keep the label values the exporter serves when they collide with the target labels Prometheus attaches. The exporter's own metrics collide with nothing, so the default off is the safe one; turn it on only if a relabeling here creates a collision on purpose. |
| serviceMonitor.interval | string | `"60s"` | Scrape interval. Collectors refresh in the background, so this is independent of how often the DigitalOcean API is called. |
| serviceMonitor.jobLabel | string | `""` | Label on the Service whose value becomes the `job` label of every scraped series. Empty keeps the operator's default, which is the Service name. |
| serviceMonitor.labels | object | `{}` | Extra labels for the ServiceMonitor, for whatever your Prometheus selects on. |
| serviceMonitor.metricRelabelings | list | `[]` | Relabelings applied to the scraped samples before ingestion — the place to drop a metric you do not want to store, such as the Go runtime series. |
| serviceMonitor.namespaceSelector | object | `{}` | Namespaces the ServiceMonitor discovers the Service in, e.g. `{matchNames: [monitoring]}`. Empty means the ServiceMonitor's own namespace, which is right whenever it is installed alongside the chart; set it only when the ServiceMonitor lives somewhere else, such as a central monitoring namespace. |
| serviceMonitor.relabelings | list | `[]` | Relabelings applied to the discovered targets before scraping, in Prometheus Operator `RelabelConfig` form. |
| serviceMonitor.scrapeTimeout | string | `"10s"` | Scrape timeout. Serving `/metrics` only reads an in-memory snapshot, so it never waits on the API. |
| spaces.accessKey | string | `""` | Spaces access key, used only by the spaces collector. This is an S3 credential, unrelated to the API token. |
| spaces.existingSecret | string | `""` | Name of a Secret you manage yourself holding both Spaces keys. Mutually exclusive with `spaces.accessKey` and `spaces.secretKey`. |
| spaces.existingSecretAccessKeyKey | string | `"spaces-access-key"` | Key inside `spaces.existingSecret` that holds the access key. |
| spaces.existingSecretSecretKeyKey | string | `"spaces-secret-key"` | Key inside `spaces.existingSecret` that holds the secret key. |
| spaces.secretKey | string | `""` | Spaces secret key that pairs with `spaces.accessKey`. |
| strategy | object | `{"type":"Recreate"}` | Deployment update strategy. Recreate rather than Kubernetes' RollingUpdate default, because with one replica the rolling surge runs two exporters side by side through every upgrade — and every token rotation, since the Secret checksum annotation rolls the pod — doubling the API spend and reporting the same account twice for as long as both live, which is exactly what running a single replica is meant to avoid. The cost is a metrics gap of one pod start, and a failed scrape or two is the honest picture of an exporter restarting. |
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
