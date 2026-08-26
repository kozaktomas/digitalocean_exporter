# Token permissions

The exporter only ever reads. It issues `GET` requests and nothing else — no collector
creates, changes or destroys anything — so **no token you give it ever needs a write
scope**.

There are two credentials, and they are unrelated:

- a **DigitalOcean API token**, which every collector but one uses;
- a **Spaces access key**, which only the [spaces collector](spaces.md) uses.

## The simple answer

Create a personal access token with
[custom scopes](https://docs.digitalocean.com/reference/api/create-personal-access-token-with-custom-scopes/)
and grant the single alias scope:

```
api:read
```

That is every read scope at once and nothing else — the token can enumerate the account but
cannot change a thing. Every collector works, and you never revisit this when you enable
another one.

If you also want [Spaces](spaces.md), add a **full-access** Spaces key, or a limited one if
you are willing to name your buckets. The two sections below say when each is needed.

That combination — `api:read` plus a Spaces key — is the sensible default, and it is what
most people should do.

## The minimal answer

If you would rather grant only what is actually used, scope the token per collector. Each
one needs exactly the read scope for the resource it reports:

| Collector | Scopes it needs |
|---|---|
| [`account`](collectors.md#account) | `account:read` |
| [`balance`](collectors.md#balance) | `billing:read` |
| [`databases`](collectors.md#databases) | `database:read` |
| [`droplets`](collectors.md#droplets) | `droplet:read` |
| [`kubernetes`](collectors.md#kubernetes) | `kubernetes:read` |
| [`limits`](collectors.md#limits) | `account:read`, `droplet:read`, `reserved_ip:read`, `block_storage:read` |
| [`registry`](collectors.md#registry) | `registry:read` |
| [`volumes`](collectors.md#volumes) | `block_storage:read` |
| [`loadbalancers`](collectors.md#loadbalancers) | `load_balancer:read` |
| [`cdn`](collectors.md#cdn) | `cdn:read` |
| [`domains`](collectors.md#domains) | `domain:read` |
| [`firewalls`](collectors.md#firewalls) | `firewall:read` |
| [`certificates`](collectors.md#certificates) | `certificate:read` |
| [`dropletmetrics`](monitoring-api.md#dropletmetrics) | `monitoring:read`, `droplet:read` |
| [`loadbalancermetrics`](monitoring-api.md#loadbalancermetrics) | `monitoring:read`, `load_balancer:read` |
| [`spaces`](spaces.md) | none — it uses a Spaces key, not the token |

Scope names follow DigitalOcean's own convention of `<resource>:<action>`, where a `GET`
needs `<resource>:read`. The
[full list of scopes](https://docs.digitalocean.com/reference/api/scopes/) is in their
reference.

## Scoping the token *is* how you scope the exporter

This is the part worth planning deliberately, because the two halves have to agree.

A collector whose scope the token lacks does not fail quietly — **it fails every refresh,
forever**, reporting `digitalocean_exporter_collector_success 0` and logging a `403` each
time. Everything else keeps working, but you have bought yourself a permanently red signal
that means nothing.

So if you deliberately narrow the token, **turn off the collectors you narrowed it out of**:

```bash
digitalocean_exporter \
  --no-collector.balance \
  --no-collector.databases \
  --no-collector.kubernetes \
  --no-collector.registry \
  --no-collector.cdn \
  --no-collector.domains
```

```yaml
# the same thing in the chart
collectors:
  balance: { enabled: false }
  databases: { enabled: false }
  kubernetes: { enabled: false }
  registry: { enabled: false }
  cdn: { enabled: false }
  domains: { enabled: false }
```

A token scoped to `droplet:read`, `block_storage:read`, `reserved_ip:read` and
`account:read`, with everything else switched off, is a perfectly reasonable
"droplets and disks only" deployment. The rule is simply that the set of enabled collectors
and the set of granted scopes must match.

!!! tip "Which way round to decide it"

    Decide what you want to *monitor* first, switch off the rest, and then grant exactly
    those scopes. Deciding scopes first and discovering the failures afterwards is the same
    work in a worse order.

## Billing is the one that catches people

`billing:read` is not part of every token, and on some team roles it cannot be granted at
all. The `balance` collector is the only one that needs it, and against a token without it
the response is unambiguous:

```
GET https://api.digitalocean.com/v2/customers/my/balance: 403
You are not authorized to perform this operation
```

If you cannot grant billing, disable that one collector — `--no-collector.balance`,
`COLLECTOR_BALANCE=false`, or `collectors.balance.enabled: false` — and everything else is
unaffected. This is common enough that it is worth checking before you conclude something is
broken.

## Spaces keys are a different system

A [Spaces access key](https://docs.digitalocean.com/products/spaces/how-to/manage-access/)
is an S3 credential created under *API → Spaces Keys*. It has nothing to do with the API
token: a token grants no access to Spaces, and a Spaces key grants no access to the API.
Neither can be substituted for the other.

| Key | What it allows | When the collector needs it |
|---|---|---|
| **Limited access**, Read on chosen buckets | Reading those buckets, including the usage each one reports | When you name your buckets with `--collector.spaces.bucket` |
| **Full access** | Listing all buckets, and everything else | Only for discovery, when you name no buckets |

The collector never downloads an object, and never lists one either — it asks each bucket
for its own size and object count — so **Read is enough**; there is no case for granting
write.

Prefer the limited key. Discovery needs full access purely because *listing the buckets that
exist* is a full-access capability in Spaces, which is a lot of privilege to buy a
convenience. Naming three buckets costs one flag each.

## Rotating and revoking

Tokens and Spaces keys are both replaced rather than edited: create the new one, put it
where the exporter reads it, restart, then revoke the old one. The exporter reads its
credentials at startup, so nothing takes effect until it restarts. The mechanics for each
install method are in [secrets](../helm/secrets.md) for Kubernetes and on the
[Debian page](../install/debian.md#keeping-the-token-out-of-the-env-file) for a VM.

Because the exporter only reads, a leaked token is an information disclosure rather than a
route to destroying anything — but it enumerates your entire account, so treat it as a
secret and pass it as a file rather than on a command line. See
[credentials](index.md#credentials).
