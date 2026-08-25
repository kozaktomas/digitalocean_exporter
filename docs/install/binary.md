# Binary

A static binary with no runtime dependencies. Use this when the
[Debian package](debian.md) does not fit — another distribution, a container base image of
your own, or a supervisor that is not systemd.

## Download

Tarballs for `linux/amd64` and `linux/arm64` are on the
[releases page](https://github.com/kozaktomas/digitalocean_exporter/releases), together
with a `checksums.txt`.

```bash
VERSION=0.1.0
ARCH=arm64   # or amd64

curl -sSLO "https://github.com/kozaktomas/digitalocean_exporter/releases/download/v${VERSION}/digitalocean_exporter_${VERSION}_linux_${ARCH}.tar.gz"
curl -sSLO "https://github.com/kozaktomas/digitalocean_exporter/releases/download/v${VERSION}/digitalocean_exporter_${VERSION}_checksums.txt"
sha256sum --check --ignore-missing digitalocean_exporter_${VERSION}_checksums.txt

tar xzf "digitalocean_exporter_${VERSION}_linux_${ARCH}.tar.gz"
sudo install -m 0755 digitalocean_exporter /usr/local/bin/
```

Verify the checksum before installing. It is one command and it is the only thing standing
between you and a truncated download.

## Run it

```bash
digitalocean_exporter --do.token-file=/etc/digitalocean-exporter/token
digitalocean_exporter --help
```

`--help` lists every flag with its default and its environment variable.

## Building from source

Needs Go 1.26 or newer.

```bash
git clone https://github.com/kozaktomas/digitalocean_exporter.git
cd digitalocean_exporter
make build
./digitalocean_exporter --version
```

```
digitalocean_exporter, version 0.1.0 (commit e67249b, go1.26.1)
```

`make build` stamps the version and commit into the binary. A plain `go build` produces a
working exporter that reports `dev` as its version and `none` as its commit.

The same build metadata is in the first log line at startup and in a metric, which is how
you check what a running exporter is:

```
digitalocean_exporter_build_info{commit="e67249b",goversion="go1.26.1",version="0.1.0"} 1
```

## Supervising it yourself

Whatever runs it should:

- run it as an **unprivileged user** — it needs no privileges at all;
- pass the token through `--do.token-file` or `DIGITALOCEAN_TOKEN_FILE`, so it does not
  appear in `ps`;
- **restart it on exit**. The exporter exits on a configuration error and keeps running
  through API failures, so an exit means something needs fixing, not retrying — but a
  restart loop with a delay is still the right default.

The [systemd unit](https://github.com/kozaktomas/digitalocean_exporter/blob/main/deb/digitalocean-exporter.service)
shipped in the Debian package is a reasonable model to copy, hardening included.
