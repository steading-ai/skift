#!/bin/sh
# Creates the skift user and group before the package's files are laid
# down, so /etc/skift/env can be owned by the group. The fragment is
# inlined because packaging/sysusers.d/skift.conf is not installed yet at
# this point; --replace= makes systemd-sysusers treat the two as one.
set -e
printf '%s\n' 'u skift - "skift service" /run/skift /usr/sbin/nologin' |
  systemd-sysusers --replace=/usr/lib/sysusers.d/skift.conf - >/dev/null 2>&1 || :
