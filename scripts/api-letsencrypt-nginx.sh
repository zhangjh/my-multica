#!/usr/bin/env bash
# One-time backend API exposure for the Cloudflare web deploy.
#
# Turns api.tx-multica.zhangjh.cn (grey DNS-only record -> this VPS) into the
# TLS front for the multica backend on 127.0.0.1:8080:
#   1. adds an nginx port-80 server (webroot) for certbot
#   2. issues a Let's Encrypt cert for the api hostname
#   3. installs the 443 server block (WS-aware proxy to :8080) and reloads
#
# Run AFTER adding the DNS record; run again to renew/repair. Idempotent.
set -euo pipefail

API_HOST="api.tx-multica.zhangjh.cn"
ACME_ROOT="/var/www/acme"
SSL_CONF="/etc/nginx/conf.d/multica-api-ssl.conf"
ACME_CONF="/etc/nginx/conf.d/multica-api-acme.conf"

[[ $EUID -eq 0 ]] || { echo "run as root (sudo -i)"; exit 1; }

echo "==> Preflight: DNS for $API_HOST"
VPS_IP="$(curl -s4 ifconfig.me || true)"
API_IP="$(dig +short A "$API_HOST" | head -1 || true)"
echo "    VPS ip = $VPS_IP"
echo "    $API_HOST -> ${API_IP:-<none>}"
if [[ -z "$API_IP" ]]; then
  echo "ERROR: no A record yet. Add a grey (DNS-only) A record in Cloudflare:"
  echo "       $API_HOST A $VPS_IP"
  exit 1
fi
if [[ -n "$VPS_IP" && "$API_IP" != "$VPS_IP" ]]; then
  echo "WARNING: $API_HOST resolves to $API_IP, not this VPS ($VPS_IP)."
  echo "         Continue anyway? [y/N]"; read -r ans || true
  [[ "${ans:-N}" =~ ^[yY] ]] || exit 1
fi

mkdir -p "$ACME_ROOT"

echo "==> Installing nginx port-80 webroot for certbot"
cat > "$ACME_CONF" <<EOF
server {
    listen 80;
    server_name $API_HOST;
    location /.well-known/acme-challenge/ {
        root $ACME_ROOT;
    }
    location / {
        return 301 https://\$host\$request_uri;
    }
}
EOF
nginx -t && systemctl reload nginx || true

if [[ ! -f "/etc/letsencrypt/live/$API_HOST/fullchain.pem" ]]; then
  echo "==> Issuing Let's Encrypt cert for $API_HOST"
  certbot certonly --webroot -w "$ACME_ROOT" -d "$API_HOST"
else
  echo "==> Cert already exists, attempting renewal (harmless if not due)"
  certbot certonly --webroot -w "$ACME_ROOT" -d "$API_HOST"
fi

echo "==> Installing nginx 443 server for $API_HOST -> 127.0.0.1:8080"
cat > "$SSL_CONF" <<EOF
server {
    client_max_body_size 100M;
    listen 443 ssl;
    server_name $API_HOST;

    ssl_certificate /etc/letsencrypt/live/$API_HOST/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/$API_HOST/privkey.pem;

    proxy_buffering off;
    proxy_read_timeout 86400s;
    proxy_cache off;
    proxy_http_version 1.1;

    # CLI reachability probe: \`multica setup\` GETs /health and expects 200
    location = /health {
        proxy_pass http://127.0.0.1:8080;
    }

    # realtime WebSocket via browser and daemon
    location = /ws {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Upgrade \$http_upgrade;
        proxy_set_header Connection "upgrade";
    }
    location = /api/daemon/ws {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Upgrade \$http_upgrade;
        proxy_set_header Connection "upgrade";
    }

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host \$host;
        proxy_set_header X-Real-IP \$remote_addr;
        proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto \$scheme;
    }
}
EOF
nginx -t && systemctl reload nginx

echo ""
echo "==> Done. https://$API_HOST/health should return 200."
echo "    Add COOKIE_DOMAIN=.zhangjh.cn + CORS_ALLOWED_ORIGINS in VPS .env and restart backend."
echo "    Auto-renewal: certbot renew handles it via the same webroot; both confs persist."