#!/bin/sh
# Creates the dagger user and group before the package's files are laid
# down, so /etc/dagger/env can be owned by the group. The fragment is
# inlined because packaging/sysusers.d/dagger.conf is not installed yet at
# this point; --replace= makes systemd-sysusers treat the two as one.
set -e
printf '%s\n' 'u dagger - "dagger service" /run/dagger /usr/sbin/nologin' |
  systemd-sysusers --replace=/usr/lib/sysusers.d/dagger.conf - >/dev/null 2>&1 || :
