#!/usr/bin/env bash
# Ships deploy/ to the server and runs the setup scripts there.
#
#   ./deploy/install-remote.sh root@HOST [-p PORT] [--nginx-only|--base-only]
#
# The scripts need their sibling files (mc-ctl, the unit), so the directory
# travels as a tarball rather than a single piped script.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" && pwd)"
TARGET=""
PORT=22
WHICH=both

while [ $# -gt 0 ]; do
    case "$1" in
        -p|--port) PORT="$2"; shift 2 ;;
        --nginx-only) WHICH=nginx; shift ;;
        --base-only)  WHICH=base;  shift ;;
        -h|--help) sed -n '2,8p' "$0"; exit 0 ;;
        -*) echo "unknown option: $1" >&2; exit 64 ;;
        *) TARGET="$1"; shift ;;
    esac
done

[ -n "$TARGET" ] || { echo "usage: install-remote.sh root@HOST [-p PORT]" >&2; exit 64; }

# $d is the remote temp dir: these must stay single-quoted so the remote shell
# expands them, not this one.
# shellcheck disable=SC2016
case "$WHICH" in
    both)  remote_cmd='bash "$d/setup.sh" && bash "$d/setup-nginx.sh"' ;;
    base)  remote_cmd='bash "$d/setup.sh"' ;;
    nginx) remote_cmd='bash "$d/setup-nginx.sh"' ;;
esac

echo "== sending deploy/ to $TARGET (port $PORT)"
# ssh reads the password from the terminal, not stdin, so piping the tarball in
# does not stop it prompting.
tar czf - -C "$HERE" . | ssh -p "$PORT" "$TARGET" "
    set -euo pipefail
    d=\$(mktemp -d)
    trap 'rm -rf \"\$d\"' EXIT
    tar xzf - -C \"\$d\"
    $remote_cmd
"
