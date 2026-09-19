#!/usr/bin/env bash
# Installs the tulaufa.ru vhost with the /minecraft-admin and /api/mc bits.
# Run as root:
#
#   ssh -p <port> root@<host> 'bash -s' < deploy/setup-nginx.sh
#
# Safe by construction: the current config is backed up, the new one is tested
# with `nginx -t`, and anything short of success rolls back before reloading.
set -euo pipefail

SITE=tulaufa.ru
AVAILABLE="/etc/nginx/sites-available/$SITE"
ENABLED="/etc/nginx/sites-enabled/$SITE"
STAMP="$(date -u +%Y%m%d-%H%M%S)"
BACKUP="$AVAILABLE.bak-$STAMP"

[ "$(id -u)" -eq 0 ] || { echo "run as root" >&2; exit 1; }

# The desired config is embedded rather than fetched, so this script is the one
# thing you need to pipe over ssh.
read -r -d '' DESIRED <<'CONF' || true
server {
    server_name tulaufa.ru;
    root /var/www/tulaufa.ru;
    index index.html;

    # adapter-static writes /minecraft-admin to disk as minecraft-admin.html,
    # so an extensionless request needs the .html fallback or it 404s.
    location / {
        try_files $uri $uri.html $uri/index.html =404;
    }

    # Control API for /minecraft-admin, served by tulaufa-mine.service.
    location /api/mc/ {
        proxy_pass http://127.0.0.1:8787;
        proxy_http_version 1.1;

        proxy_set_header Host              $host;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;

        # Server-sent events: nothing may buffer the response.
        proxy_buffering off;
        proxy_cache off;
        proxy_set_header Connection '';
        proxy_read_timeout 1h;
    }

    listen 443 ssl; # managed by Certbot
    ssl_certificate /etc/letsencrypt/live/tulaufa.ru-0001/fullchain.pem; # managed by Certbot
    ssl_certificate_key /etc/letsencrypt/live/tulaufa.ru-0001/privkey.pem; # managed by Certbot
    include /etc/letsencrypt/options-ssl-nginx.conf; # managed by Certbot
    ssl_dhparam /etc/letsencrypt/ssl-dhparams.pem; # managed by Certbot

}

server {
    if ($host = tulaufa.ru) {
        return 301 https://$host$request_uri;
    } # managed by Certbot


    listen 80;
    server_name tulaufa.ru;
    return 404; # managed by Certbot


}
CONF

if [ ! -f "$AVAILABLE" ]; then
    echo "!! $AVAILABLE not found — is this the right host?" >&2
    exit 1
fi

# Certificate paths differ per install; refuse to clobber a config that points
# somewhere else rather than silently breaking TLS.
CURRENT_CERT="$(grep -m1 'ssl_certificate ' "$AVAILABLE" | tr -d ' ;' | sed 's/ssl_certificate//')"
DESIRED_CERT="$(printf '%s\n' "$DESIRED" | grep -m1 'ssl_certificate ' | tr -d ' ;' | sed 's/ssl_certificate//')"
if [ "$CURRENT_CERT" != "$DESIRED_CERT" ]; then
    echo "!! certificate path differs — refusing to overwrite" >&2
    echo "   on disk: $CURRENT_CERT" >&2
    echo "   script:  $DESIRED_CERT" >&2
    echo "   Edit the script's embedded config to match, then re-run." >&2
    exit 1
fi

if printf '%s\n' "$DESIRED" | diff -q - "$AVAILABLE" >/dev/null 2>&1; then
    echo "== config already current, nothing to do"
    exit 0
fi

echo "== backing up to $BACKUP"
cp -a "$AVAILABLE" "$BACKUP"

echo "== writing new config"
printf '%s\n' "$DESIRED" > "$AVAILABLE"
ln -sfn "$AVAILABLE" "$ENABLED"

echo "== testing"
if nginx -t; then
    systemctl reload nginx
    echo "== reloaded. backup kept at $BACKUP"
else
    echo "!! nginx -t failed — rolling back" >&2
    cp -a "$BACKUP" "$AVAILABLE"
    nginx -t >/dev/null 2>&1 && echo "   rollback verified" >&2
    exit 1
fi

echo
echo "== check"
echo -n "   /            : "; curl -fsS -o /dev/null -w '%{http_code}\n' https://tulaufa.ru/ || echo FAILED
echo -n "   /api/mc/...  : "; curl -fsS -o /dev/null -w '%{http_code}\n' https://tulaufa.ru/api/mc/session 2>/dev/null \
    || echo "no answer yet (expected until tulaufa-mine.service is running)"
