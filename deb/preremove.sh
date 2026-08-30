#!/bin/sh
set -e

# dpkg runs the pre-removal script on an upgrade too, as `upgrade <new-version>`.
# Stopping the service there would take the exporter down for good: the new
# package's post-installation script enables the unit but deliberately does not
# start it, so monitoring would disappear until somebody noticed. Only a real
# removal — `remove`, or the removal half of a purge — takes the service down.
case "$1" in
    remove|purge)
        systemctl stop digitalocean-exporter || true
        systemctl disable digitalocean-exporter || true
        ;;
esac
