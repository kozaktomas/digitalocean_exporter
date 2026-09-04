# Binary

A static binary with no runtime dependencies. Use this when the
[Debian package](debian.md) does not fit — another distribution, a container base image of
your own, or a supervisor that is not systemd.

## Download

Tarballs for `linux/amd64` and `linux/arm64` are on the
[releases page](https://github.com/kozaktomas/digitalocean_exporter/releases), together
with a `checksums.txt`.

```bash
VERSION=0.4.0
ARCH=arm64   # or amd64

curl -sSLO "https://github.com/kozaktomas/digitalocean_exporter/releases/download/v${VERSION}/digitalocean_exporter_${VERSION}_linux_${ARCH}.tar.gz"
curl -sSLO "https://github.com/kozaktomas/digitalocean_exporter/releases/download/v${VERSION}/digitalocean_exporter_${VERSION}_checksums.txt"
sha256sum --check --ignore-missing digitalocean_exporter_${VERSION}_checksums.txt

tar xzf "digitalocean_exporter_${VERSION}_linux_${ARCH}.tar.gz"
sudo install -m 0755 digitalocean_exporter /usr/local/bin/
```

The checksum tells you the download arrived intact — it says nothing about who built it,
because an attacker who can replace the tarball can replace `checksums.txt` beside it.
Authenticity comes from the signature.

## Verify the release

Every release is signed with [cosign](https://docs.sigstore.dev/) keyless signing: there
is no maintainer key to trust or lose. Each signature is bound to the identity of the
GitHub Actions release workflow that built the artifacts, and logged in the public Rekor
transparency log. Verifying takes cosign 2.x or newer.

Verify the signature on `checksums.txt`, then let the checksums vouch for the tarball:

```bash
VERSION=0.4.0

curl -sSLO "https://github.com/kozaktomas/digitalocean_exporter/releases/download/v${VERSION}/digitalocean_exporter_${VERSION}_checksums.txt.sig"
curl -sSLO "https://github.com/kozaktomas/digitalocean_exporter/releases/download/v${VERSION}/digitalocean_exporter_${VERSION}_checksums.txt.pem"

cosign verify-blob \
  --certificate "digitalocean_exporter_${VERSION}_checksums.txt.pem" \
  --signature "digitalocean_exporter_${VERSION}_checksums.txt.sig" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  --certificate-identity "https://github.com/kozaktomas/digitalocean_exporter/.github/workflows/release.yml@refs/tags/v${VERSION}" \
  "digitalocean_exporter_${VERSION}_checksums.txt"

sha256sum --check --ignore-missing digitalocean_exporter_${VERSION}_checksums.txt
```

`Verified OK` means the checksums file was produced by this repository's release workflow
for exactly that tag; a matching checksum then extends that to the tarball you downloaded.
The two identity flags are not decoration — without them cosign would accept a signature
from *any* GitHub workflow, including an attacker's.

Each tarball also carries a `.sig`/`.pem` pair of its own, verifiable the same way, and an
SBOM (`<archive>.sbom.json`, in Syft JSON format) listing every module compiled into the
binary — that is the file to feed a vulnerability scanner or an inventory.

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
digitalocean_exporter, version 0.4.0 (commit 57f5e48, go1.26.1)
```

`make build` stamps the version and commit into the binary, so the three values above are
illustrative — yours are whatever you built. A plain `go build` produces a working exporter
that reports `dev` as its version and `none` as its commit.

The same build metadata is in the first log line at startup and in a metric, which is how
you check what a running exporter is:

```
digitalocean_exporter_build_info{commit="57f5e48",goversion="go1.26.1",version="0.4.0"} 1
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
