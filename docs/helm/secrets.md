# Secrets

The chart handles two independent credentials: the **DigitalOcean API token**, which every
collector needs, and the **Spaces access key pair**, which only the
[spaces collector](../configuration/spaces.md) needs. Each has the same two modes.

Whichever mode you pick, the credential reaches the container **as a mounted file** and is
passed with `--do.token-file`. It is never an environment variable, so it does not show up
in `kubectl describe pod` or in the container's environment.

## The API token

=== "A Secret you manage (recommended)"

    ```bash
    kubectl create secret generic digitalocean-token \
      --namespace monitoring \
      --from-literal=token=dop_v1_...
    ```

    ```yaml
    digitalocean:
      existingSecret: digitalocean-token
      existingSecretKey: token      # the default; set it if your key is named differently
    ```

    The chart creates no Secret of its own and the token never appears in your values, your
    Helm release, or your repository. This is what you want with GitOps, sealed secrets, an
    external secrets operator, or anything else that manages credentials separately.

=== "Let the chart create one"

    ```yaml
    digitalocean:
      token: dop_v1_...
    ```

    The chart creates a Secret named after the release and mounts it. Convenient for a
    quick test — but the token is now in your values file, and in the Helm release stored
    in the cluster. Fine for a scratch cluster, not for anything you keep.

Setting neither fails the render with a clear message rather than deploying a broken pod:

```
Error: execution error at (digitalocean-exporter/templates/secret.yaml:...):
  digitalocean.token or digitalocean.existingSecret is required
```

## The Spaces key pair

Only needed when `collectors.spaces.enabled` is true. Nothing is created otherwise.

=== "A Secret you manage"

    ```bash
    kubectl create secret generic digitalocean-spaces \
      --namespace monitoring \
      --from-literal=spaces-access-key=DO00... \
      --from-literal=spaces-secret-key=...
    ```

    ```yaml
    collectors:
      spaces:
        enabled: true
        region: fra1
        buckets:
          - assets
          - backups@ams3

    spaces:
      existingSecret: digitalocean-spaces
      existingSecretAccessKeyKey: spaces-access-key   # defaults
      existingSecretSecretKeyKey: spaces-secret-key
    ```

    Both keys live in **one** Secret, under two keys. The two `existingSecret*Key` values
    tell the chart what they are called.

=== "Let the chart create one"

    ```yaml
    collectors:
      spaces:
        enabled: true

    spaces:
      accessKey: DO00...
      secretKey: ...
    ```

Enabling the collector without supplying either fails the render:

```
spaces.accessKey or spaces.existingSecret is required when the spaces collector is enabled
```

## Which key to create

A **Limited access** Spaces key with **Read** on the buckets you measure is enough — the
collector asks each bucket for its own usage and never lists or downloads an object.
Discovery mode, where you name no buckets and let the collector find them, needs a
**full-access** key, because listing all buckets is a full-access capability. Naming the
buckets and using a limited key is the better trade.

For the API token, **read-only** covers every collector except
[`balance`](../configuration/collectors.md#balance), which needs the billing scope. If your
token cannot have it, turn that collector off rather than granting more than you meant to:

```yaml
collectors:
  balance:
    enabled: false
```

## Rotating a credential

With an existing Secret, update the Secret and restart the pod — the chart is not involved:

```bash
kubectl create secret generic digitalocean-token --namespace monitoring \
  --from-literal=token=dop_v1_new... --dry-run=client -o yaml | kubectl apply -f -
kubectl rollout restart deployment/digitalocean-exporter --namespace monitoring
```

The token is read at startup, so a rotated Secret does not take effect until the pod
restarts.
