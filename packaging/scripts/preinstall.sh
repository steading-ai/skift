#!/bin/sh
# Runs before the package's files are laid down. $1 is 1 on first install
# and 2 or more on upgrade.
#
# It creates the skift user and group, so /etc/skift/env can be owned by
# the group. The fragment is inlined because packaging/sysusers.d/skift.conf
# is not installed yet at this point; --replace= makes systemd-sysusers
# treat the two as one.
#
# On upgrade it then has the installed, older binary snapshot the database
# before this release's binary can migrate it, and fails when the snapshot
# fails, so that nothing of this release is installed and the older one
# keeps running. See docs/adrs/018-unattended-upgrades.md.
#
# The build renders this file into dist/ with the two values below written
# in; see the Makefile. They are this package's version and the newest
# migration its binary carries.
set -e
printf '%s\n' 'u skift - "skift service" /run/skift /usr/sbin/nologin' |
  systemd-sysusers --replace=/usr/lib/sysusers.d/skift.conf - >/dev/null 2>&1 || :

[ "$1" -ge 2 ] || exit 0

version=@VERSION@
schema=@SCHEMA_VERSION@
skift=/usr/bin/skift
settings=/etc/skift/env

# say prints a line for whoever runs dnf and also writes it to the journal
# as skift-upgrade, since dnf5-automatic's report leaves scriptlet output
# out. $1 is the journal priority; an error goes to stderr.
say() {
  priority=$1
  shift
  if [ "$priority" = err ]; then
    printf 'skift: %s\n' "$*" >&2
  else
    printf 'skift: %s\n' "$*"
  fi
  printf '%s\n' "$*" | systemd-cat -t skift-upgrade -p "$priority" >/dev/null 2>&1 || :
}

# Releases up to 0.4.0 have no snapshot command, and nothing to take one.
if ! "$skift" admin snapshot --help 2>/dev/null | grep -q -e '--if-behind'; then
  say notice "no snapshot taken before upgrading to $version: the installed skift has no admin snapshot command"
  exit 0
fi

# The settings file holds KEY=value lines for systemd's EnvironmentFile=.
# It is read here as text and never run, since a value may hold characters
# a shell would expand. The last DATABASE_URL wins, as it does for
# systemd, and one pair of quotes around the value is dropped.
url=
if [ -r "$settings" ]; then
  url=$(sed -n 's/^[[:space:]]*DATABASE_URL[[:space:]]*=[[:space:]]*//p' "$settings" | tail -n 1 | sed 's/[[:space:]]*$//')
fi
case $url in
  \"*\")
    url=${url#\"}
    url=${url%\"}
    ;;
  \'*\')
    url=${url#\'}
    url=${url%\'}
    ;;
esac
if [ -z "$url" ]; then
  say notice "no snapshot taken before upgrading to $version: $settings sets no DATABASE_URL"
  exit 0
fi

installed=$("$skift" version 2>/dev/null) || installed="the installed skift"

# The service's StateDirectory=, which systemd creates as this does on the
# service's first start; the snapshots go in it.
[ -d /var/lib/skift ] || install -d -o skift -g skift -m 0700 /var/lib/skift

# As skift rather than root, so that the service can prune what this
# writes, with no environment but PATH and the URL. The URL goes in on
# stdin, from the shell's own printf, because a process's arguments and
# environment are readable by every user on the host, and it carries the
# database's password. The inner shell expands what is single-quoted.
# shellcheck disable=SC2016
if out=$(printf '%s\n' "$url" |
  setpriv --reuid=skift --regid=skift --init-groups -- env -i PATH=/usr/sbin:/usr/bin \
    sh -c 'IFS= read -r DATABASE_URL && export DATABASE_URL && exec "$0" admin snapshot --dir /var/lib/skift/backups --if-behind "$1"' \
    "$skift" "$schema" 2>&1); then
  say info "before upgrading to $version: $out"
  exit 0
fi
say err "the database snapshot before upgrading to $version failed, so the upgrade stops here and $installed is still running. The next upgrade tries again. The snapshot said: $out"
exit 1
