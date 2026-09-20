#!/bin/sh
# Creates the socket directory now rather than at the next boot, and lets
# systemd see the new units. The service is not enabled or started here:
# a first install has no database configured yet and would fail to become
# active. $1 is 1 on first install and 2 or more on upgrade.
set -e
systemd-tmpfiles --create dagger.conf >/dev/null 2>&1 || :
systemctl daemon-reload >/dev/null 2>&1 || :
if [ "$1" -eq 1 ]; then
  systemctl preset dagger.service dagger-reap.timer >/dev/null 2>&1 || :
fi
