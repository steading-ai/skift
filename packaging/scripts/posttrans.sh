#!/bin/sh
# Puts the operator's settings back where the unit reads them. Releases up
# to 0.4.0 owned /etc/skift/env as a config file, so the upgrade from one
# of them, or removing one before installing this, leaves an edited file
# as env.rpmsave and none at env. This package does not own the file, and
# runs last in the transaction, after rpm has saved it.
set -e
if [ ! -e /etc/skift/env ] && [ ! -L /etc/skift/env ] && [ -f /etc/skift/env.rpmsave ]; then
  mv /etc/skift/env.rpmsave /etc/skift/env
fi
