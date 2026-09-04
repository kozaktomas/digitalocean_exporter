# Docker

The image is published to GitHub Container Registry for `linux/amd64` and `linux/arm64`.

```bash
docker run --rm -p 9212:9212 \
  -e DIGITALOCEAN_TOKEN=dop_v1_... \
  ghcr.io/kozaktomas/digitalocean_exporter:latest
```

## Tags

| Tag | What it is |
|---|---|
| `0.4.0` | An exact release. Use this in production. |
| `latest` | Whatever `main` last built. Moves without warning. |

## Verify the image

Every image is signed by digest with [cosign](https://docs.sigstore.dev/) keyless
signing. The signature is bound to the identity of the GitHub Actions workflow that
built and pushed the image, and logged in the public Rekor transparency log — there is
no maintainer key to trust. Verifying takes cosign 2.x or newer.

For a release tag:

```bash
VERSION=0.4.0

cosign verify \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  --certificate-identity "https://github.com/kozaktomas/digitalocean_exporter/.github/workflows/docker-publish.yml@refs/tags/v${VERSION}" \
  "ghcr.io/kozaktomas/digitalocean_exporter:${VERSION}"
```

The identity flags are what make this a verification rather than a formality: without
them cosign would accept a signature from any GitHub workflow. `latest` is built from
`main`, not from a tag, so its identity ends in `@refs/heads/main` instead.

On success cosign prints the digest it verified. Run the image by that digest rather
than the tag and the deployment pins exactly what was verified:

```bash
docker run --rm -p 9212:9212 \
  -e DIGITALOCEAN_TOKEN=dop_v1_... \
  ghcr.io/kozaktomas/digitalocean_exporter@sha256:...
```

## Image labels

The image carries the standard OCI labels, so `docker inspect` answers what a running
container actually is without guessing from its tag:

| Label | What it holds |
|---|---|
| `org.opencontainers.image.source` | This repository's URL |
| `org.opencontainers.image.revision` | The commit the image was built from |
| `org.opencontainers.image.version` | The release version, or `dev` for a `main` build |
| `org.opencontainers.image.licenses` | `Apache-2.0` |
| `org.opencontainers.image.description` | What the exporter is |

```bash
docker inspect --format '{{ json .Config.Labels }}' \
  ghcr.io/kozaktomas/digitalocean_exporter:0.4.0
```

## Passing the token as a file

`-e DIGITALOCEAN_TOKEN=...` puts the token into the container's environment, where
`docker inspect` will happily show it to anyone on the host. Mounting it as a file avoids
that:

```bash
docker run --rm -p 9212:9212 \
  -v /etc/digitalocean-exporter/token:/run/secrets/do-token:ro \
  -e DIGITALOCEAN_TOKEN_FILE=/run/secrets/do-token \
  ghcr.io/kozaktomas/digitalocean_exporter:latest
```

`--do.token` and `--do.token-file` are mutually exclusive; exactly one must be set.

## Compose

```yaml
services:
  digitalocean-exporter:
    image: ghcr.io/kozaktomas/digitalocean_exporter:0.4.0
    restart: unless-stopped
    ports:
      - "9212:9212"
    environment:
      DIGITALOCEAN_TOKEN_FILE: /run/secrets/do-token
      # The balance collector needs a billing-scoped token. Drop this line if yours has one.
      COLLECTOR_BALANCE: "false"
      LOG_FORMAT: json
    secrets:
      - do-token

secrets:
  do-token:
    file: ./secrets/do-token
```

Note `COLLECTOR_BALANCE: "false"`. Environment variables take a value, but the equivalent
flag does not — a collector is disabled on the command line with `--no-collector.balance`.
See [configuration](../configuration/index.md#turning-a-collector-off).

## Passing flags

Anything after the image name is passed to the exporter:

```bash
docker run --rm -p 9212:9212 \
  -e DIGITALOCEAN_TOKEN=dop_v1_... \
  ghcr.io/kozaktomas/digitalocean_exporter:0.4.0 \
  --no-collector.balance --log.format=json --collector.droplets.interval=10m
```
