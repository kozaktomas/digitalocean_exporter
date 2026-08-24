#!/bin/sh
set -e

systemctl stop digitalocean-exporter || true
systemctl disable digitalocean-exporter || true
