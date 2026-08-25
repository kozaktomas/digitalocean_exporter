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
| `0.1.0` | An exact release. Use this in production. |
| `latest` | Whatever `main` last built. Moves without warning. |

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
    image: ghcr.io/kozaktomas/digitalocean_exporter:0.1.0
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
  ghcr.io/kozaktomas/digitalocean_exporter:0.1.0 \
  --no-collector.balance --log.format=json --collector.droplets.interval=10m
```
