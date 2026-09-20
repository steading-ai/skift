#!/bin/sh
# Stops and disables the units when the package is removed for good. On an
# upgrade ($1 is 1 or more) the running service is left alone; postremove
# restarts it onto the new binary.
set -e
if [ "$1" -eq 0 ]; then
  systemctl --no-reload disable --now dagger.service dagger-reap.timer dagger-reap.service >/dev/null 2>&1 || :
fi
