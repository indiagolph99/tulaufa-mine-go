#!/usr/bin/env bash
# One-time VPS setup for the tulaufa-mine control API. Run as root.
# Idempotent: safe to re-run after changing mc-ctl or the unit file.
#
#   ./deploy/install-remote.sh root@<host> -p <port> --base-only
#
# Must run with its sibling files (mc-ctl, the unit) present, so it cannot be
# piped on its own — install-remote.sh sends the whole directory.
#
# It does NOT write the password hash or touch nginx — both are separate steps,
# noted at the end.
set -euo pipefail

SVC_USER=tulaufa-mine
# The CI account that ships new binaries; it must be able to write INSTALL_DIR.
DEPLOY_USER=deploy
INSTALL_DIR=/opt/tulaufa-mine
CONF_DIR=/etc/tulaufa-mine
# When this script is piped (`bash -s`), BASH_SOURCE is empty and dirname
# resolves to the remote cwd — where mc-ctl and the unit file do not exist.
# Ship the whole deploy/ directory instead; see install-remote.sh.
HERE="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" 2>/dev/null && pwd || echo .)"

say() { printf '\n== %s\n' "$1"; }

for needed in mc-ctl tulaufa-mine.service; do
    [ -f "$HERE/$needed" ] && continue
    cat >&2 <<'MISSING'
!! This script needs the other files from deploy/ beside it, and they are not here.

   Piping it alone (ssh ... 'bash -s' < deploy/setup.sh) cannot work: mc-ctl and
   the unit file never reach the server. Send the whole directory instead:

     ./deploy/install-remote.sh root@HOST -p PORT

   or by hand:

     tar czf - -C deploy . | ssh -p PORT root@HOST \
       'd=$(mktemp -d) && tar xzf - -C "$d" && bash "$d/setup.sh"; rm -rf "$d"'
MISSING
    exit 1
done

[ "$(id -u)" -eq 0 ] || { echo "run as root" >&2; exit 1; }


say "service user"
if id -u "$SVC_USER" >/dev/null 2>&1; then
    echo "  $SVC_USER exists"
else
    useradd --system --no-create-home --shell /usr/sbin/nologin "$SVC_USER"
    echo "  created $SVC_USER"
fi

say "install dirs"
# INSTALL_DIR belongs to the deploy account: CI rsyncs the binary in as that
# user. This grants it no new power — deploy already restarts the service, so it
# decides what runs either way. The privileged part, mc-ctl, stays root-owned
# outside this directory and deploy cannot touch it.
if ! id -u "$DEPLOY_USER" >/dev/null 2>&1; then
    echo "!! user $DEPLOY_USER does not exist — CI could not ship binaries" >&2
    exit 1
fi
install -d -m 0755 -o "$DEPLOY_USER" -g "$DEPLOY_USER" "$INSTALL_DIR"
# install -d leaves an existing directory's ownership alone on some versions.
chown "$DEPLOY_USER:$DEPLOY_USER" "$INSTALL_DIR"
chmod 0755 "$INSTALL_DIR"

install -d -m 0750 -o root -g "$SVC_USER" "$CONF_DIR"

say "privileged wrapper"
# Root-owned and not writable by the service user: the whole security model
# rests on the daemon being unable to edit what it runs as root.
install -m 0755 -o root -g root "$HERE/mc-ctl" /usr/local/bin/mc-ctl
echo "  /usr/local/bin/mc-ctl installed"

say "sudoers"
SUDOERS=/etc/sudoers.d/tulaufa-mine
cat > "$SUDOERS.tmp" <<RULES
# The daemon may run exactly one program as root, and that program accepts
# only a fixed set of verbs. Nothing else is granted.
$SVC_USER ALL=(root) NOPASSWD: /usr/local/bin/mc-ctl
# CI restarts the API after shipping a new binary.
deploy ALL=(root) NOPASSWD: /usr/bin/systemctl restart tulaufa-mine.service
RULES
chmod 0440 "$SUDOERS.tmp"
if visudo -cf "$SUDOERS.tmp"; then
    mv "$SUDOERS.tmp" "$SUDOERS"
    echo "  $SUDOERS validated and installed"
else
    rm -f "$SUDOERS.tmp"
    echo "  sudoers file REJECTED — nothing changed" >&2
    exit 1
fi

say "systemd unit"
install -m 0644 -o root -g root "$HERE/tulaufa-mine.service" /etc/systemd/system/tulaufa-mine.service
systemctl daemon-reload
systemctl enable tulaufa-mine.service >/dev/null
echo "  unit installed and enabled"

say "verification"
echo -n "  wrapper as $SVC_USER: "
if sudo -u "$SVC_USER" sudo -n /usr/local/bin/mc-ctl status >/dev/null 2>&1; then
    echo "OK"
else
    echo "FAILED" >&2
fi
echo -n "  $DEPLOY_USER can write $INSTALL_DIR: "
if sudo -u "$DEPLOY_USER" test -w "$INSTALL_DIR"; then
    echo "OK"
else
    echo "FAILED — CI deploys will hit 'Permission denied'" >&2
    exit 1
fi
echo -n "  wrapper refuses junk: "
if sudo -u "$SVC_USER" sudo -n /usr/local/bin/mc-ctl 'status; id' >/dev/null 2>&1; then
    echo "ACCEPTED — THIS IS A BUG, STOP" >&2
    exit 1
else
    echo "OK (refused)"
fi

cat <<'NEXT'

== remaining manual steps
1. Write the password hash (generate it on your laptop, never here):
     tulaufa-mine hash            # on your laptop, prints pbkdf2-sha256$...
   then on the server:
     printf 'ADMIN_PASSWORD_HASH=%s\n' '<paste>' > /etc/tulaufa-mine/env
     printf 'ALLOWED_ORIGIN=https://tulaufa.ru\n' >> /etc/tulaufa-mine/env
     chown root:tulaufa-mine /etc/tulaufa-mine/env && chmod 0640 /etc/tulaufa-mine/env

2. Configure nginx. The vhost ships with no location blocks at all, so both
   `location /` (with try_files, or /minecraft-admin 404s) and `location
   /api/mc/` have to be created. Do not hand-edit it — run:
     ssh -p <port> root@<host> 'bash -s' < deploy/setup-nginx.sh
   It backs up, tests with `nginx -t`, and rolls back on failure.

3. Deploy the binary (CI does this): /opt/tulaufa-mine/tulaufa-mine
     systemctl start tulaufa-mine.service
NEXT
