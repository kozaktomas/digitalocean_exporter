# Debian package

For a plain VM, a Raspberry Pi, or anything else with systemd and `dpkg`. Packages are
built for `amd64` and `arm64`.

!!! note "The package name has a hyphen"

    The binary and the repository use an underscore (`digitalocean_exporter`); the Debian
    package is `digitalocean-exporter`, because Debian rejects underscores in package
    names. Both spellings are correct, in their own place.

## Install

Download the `.deb` for your architecture from the
[releases page](https://github.com/kozaktomas/digitalocean_exporter/releases), then:

```bash
sudo dpkg -i digitalocean-exporter_0.1.0_linux_arm64.deb
sudoedit /etc/digitalocean-exporter/digitalocean-exporter.env   # set DIGITALOCEAN_TOKEN
sudo systemctl start digitalocean-exporter
curl -s localhost:9212/metrics | head
```

The package creates a dedicated unprivileged system user, `digitalocean-exporter`, enables
the unit, and does **not** start it — there is no token yet at that point.

## What it puts where

| Path | What |
|---|---|
| `/usr/bin/digitalocean_exporter` | The binary |
| `/etc/digitalocean-exporter/digitalocean-exporter.env` | Configuration, `root:digitalocean-exporter`, mode `0640` |
| `/lib/systemd/system/digitalocean-exporter.service` | The unit |

The env file is a config file marked `noreplace`, so an upgrade keeps your edits and leaves
a `.dpkg-dist` next to it if the shipped default changed.

## Configuring it

Everything is set through environment variables in that one file — every flag has an
equivalent, listed in [configuration](../configuration/index.md).

```bash
# /etc/digitalocean-exporter/digitalocean-exporter.env
DIGITALOCEAN_TOKEN=dop_v1_...
WEB_LISTEN_ADDRESS=:9212
LOG_LEVEL=info
LOG_FORMAT=logfmt

# This token has no billing scope.
COLLECTOR_BALANCE=false
COLLECTOR_DROPLETS_INTERVAL=10m
```

Environment variables take a value, so a collector is turned off with
`COLLECTOR_BALANCE=false`. The flag form is different — `--no-collector.balance` — and
`--collector.balance=false` is a parse error that stops the process at startup.

Apply changes with `sudo systemctl restart digitalocean-exporter`.

### Keeping the token out of the env file

`DIGITALOCEAN_TOKEN_FILE` points at a file instead:

```bash
sudo install -o root -g digitalocean-exporter -m 0640 /dev/null /etc/digitalocean-exporter/token
sudoedit /etc/digitalocean-exporter/token
```

```bash
DIGITALOCEAN_TOKEN_FILE=/etc/digitalocean-exporter/token
```

The unit runs with `ProtectSystem=strict` and `ProtectHome=true`, so the file has to live
somewhere the service can still read — `/etc/digitalocean-exporter/` is fine.

## The unit is locked down

The shipped service runs as a non-root user with `NoNewPrivileges`, `ProtectSystem=strict`,
`PrivateTmp`, a `@system-service` syscall filter, and `RestrictAddressFamilies` limited to
IPv4 and IPv6. The exporter is stateless — it writes nothing and needs no filesystem access
beyond reading its own configuration — so none of that gets in the way.

If you override the unit with `systemctl edit`, keep those directives. They are the reason
a credential-holding daemon exposed on a port is not much of a risk.

## Logs and status

```bash
systemctl status digitalocean-exporter
journalctl -u digitalocean-exporter -f
```

## Upgrading and removing

```bash
sudo dpkg -i digitalocean-exporter_0.2.0_linux_arm64.deb   # keeps the env file
sudo systemctl restart digitalocean-exporter
```

```bash
sudo apt remove digitalocean-exporter    # leaves /etc/digitalocean-exporter
sudo apt purge digitalocean-exporter     # removes it, and the system user
```
