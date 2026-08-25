# Install

Four ways to run the exporter. They differ only in packaging — it is the same static
binary, the same flags and the same `:9212` in every case.

| Method | Use it when |
|---|---|
| [Docker](docker.md) | Trying it out, or you already run containers |
| [Kubernetes and Helm](kubernetes.md) | You have a cluster and Prometheus in it |
| [Terraform](terraform.md) | Your cluster is declared in Terraform |
| [Debian package](debian.md) | A plain VM or a Raspberry Pi with systemd |
| [Binary](binary.md) | Anything else, or you want to supervise it yourself |

## What you need first

**A DigitalOcean API token.** Create one under *API → Tokens* in the control panel. The
exporter only ever reads, so a token with the single scope `api:read` — every read
permission and no write permission — covers every collector. You can scope it more tightly
than that; [token permissions](../configuration/permissions.md) lists what each collector
needs and how to switch off the ones you scoped the token out of.

**A billing scope, if you want the `balance` collector.** Balance and month-to-date usage
come from `/v2/customers/my/balance`, which needs `billing:read`; a token without it answers
`403 Forbidden`. That scope is not available on every team role. If yours cannot have it,
disable that one collector and nothing else changes.

**A Spaces access key, only for the `spaces` collector.** That is an S3 credential, created
separately under *API → Spaces Keys*, and unrelated to the API token — a Read-only limited
key is enough. See [Spaces](../configuration/spaces.md).

!!! danger "Treat the token as a secret"

    A read-only token still enumerates your whole account. Every install method below has
    a way to pass it as a file or a Secret rather than on a command line, where it would
    show up in `ps`, in shell history, or in `kubectl describe pod`. Use it.

## Verifying an install

Whatever you used, the check is the same:

```bash
curl -s localhost:9212/metrics | grep collector_success
```

Every enabled collector should report `1` within one refresh interval. A `0` means that
collector's last refresh failed — the exporter is running and the others are fine. The
[operations page](../operations.md) explains how to find out why.

Immediately after startup a collector that has never succeeded reports nothing at all,
which is deliberate: an absent series is honest, a zero would not be.
