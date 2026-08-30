#!/bin/sh
set -e

# The dedicated system user is deliberately kept, even on purge. It may still
# own files elsewhere on the host — a token file, a log — and Debian practice
# is to leave such an account behind rather than orphan them to a numeric uid
# that a later package could be given.
systemctl daemon-reload || true
