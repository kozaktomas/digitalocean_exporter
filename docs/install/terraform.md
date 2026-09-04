# Terraform

The chart repository is a plain HTTP Helm repository, so `helm_release` consumes it
directly — no OCI login, no local checkout.

```hcl
resource "helm_release" "digitalocean_exporter" {
  name       = "digitalocean-exporter"
  namespace  = kubernetes_namespace.monitoring.metadata[0].name
  repository = "https://kozaktomas.github.io/digitalocean_exporter"
  chart      = "digitalocean-exporter"
  version    = "0.4.0"
}
```

Always set `version`. Left out, the provider resolves to whatever is newest at apply time,
so the same configuration produces different infrastructure on different days — and the
diff appears in an unrelated apply.

## With a values template

The usual pattern, and the reason to keep secrets out of the repository:

```hcl
resource "kubernetes_secret" "digitalocean_token" {
  metadata {
    name      = "digitalocean-token"
    namespace = kubernetes_namespace.monitoring.metadata[0].name
  }
  data = {
    token = var.digitalocean_token
  }
}

resource "helm_release" "digitalocean_exporter" {
  name       = "digitalocean-exporter"
  namespace  = kubernetes_namespace.monitoring.metadata[0].name
  repository = "https://kozaktomas.github.io/digitalocean_exporter"
  chart      = "digitalocean-exporter"
  version    = "0.4.0"

  values = [
    templatefile("${path.module}/values/digitalocean-exporter.yaml", {
      existing_secret = kubernetes_secret.digitalocean_token.metadata[0].name
      prometheus_release = "kube-prometheus-stack"
    })
  ]
}
```

```yaml
# values/digitalocean-exporter.yaml
digitalocean:
  existingSecret: ${existing_secret}

collectors:
  balance:
    enabled: false

serviceMonitor:
  enabled: true
  labels:
    release: ${prometheus_release}
```

Passing the token through `set_sensitive` instead of a `kubernetes_secret` also works, but
the value then lives in the Terraform state either way — a Secret at least keeps it out of
the Helm release manifest.

## Watching for drift

`helm_release` stores the resolved chart version in state, so a `terraform plan` after a
new release is published shows nothing until you bump `version` yourself. That is the point
of pinning. To find out what is available:

```bash
helm repo update
helm search repo digitalocean-exporter/digitalocean-exporter --versions
```

## Provider notes

Tested with the [`hashicorp/helm`](https://registry.terraform.io/providers/hashicorp/helm/latest/docs)
provider. `repository` takes the base URL — the provider appends `/index.yaml` itself, so
do not include it. A trailing slash is accepted.
