#!/bin/sh
# Puts the operator's settings back where the unit reads them. Releases up
# to 0.4.0 owned /etc/skift/env as a config file, so the upgrade from one
# of them, or removing one before installing this, leaves an edited file
# as env.rpmsave and none at env. This package does not own the file, and
# runs last in the transaction, after rpm has saved it.
#
# On that upgrade the old release's postremove restarts a running service
# before this runs, so it starts with no settings and fails, and may hit
# its start limit before the settings are back. Once they are, a service
# that is running, waiting to restart or failed starts again with them;
# one the operator stopped stays stopped.
set -e
if [ ! -e /etc/skift/env ] && [ ! -L /etc/skift/env ] && [ -f /etc/skift/env.rpmsave ]; then
  mv /etc/skift/env.rpmsave /etc/skift/env
  state=$(systemctl show --property ActiveState --value skift.service 2>/dev/null) || state=
  case $state in
    active | activating | failed)
      systemctl reset-failed skift.service >/dev/null 2>&1 || :
      systemctl restart skift.service >/dev/null 2>&1 || :
      ;;
  esac
fi
