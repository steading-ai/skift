#!/bin/sh
# Lets systemd forget removed units, or, on an upgrade ($1 is 1 or more),
# restarts a running service onto the new binary. A stopped service stays
# stopped.
set -e
systemctl daemon-reload >/dev/null 2>&1 || :
if [ "$1" -ge 1 ]; then
  systemctl try-restart dagger.service >/dev/null 2>&1 || :
fi
