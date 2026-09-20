#!/usr/bin/env bash
# Deploy the multica web app to Cloudflare Workers from a dev machine.
#
# Three things in this script MUST happen on a capable machine (not the small
# VPS): the pnpm install and the Next build are memory-hungry and have
# OOM-killed the self-host host before. Cloudflare only needs the final
# `wrangler deploy` output.
#
# Requirements:
#   - node + pnpm on YOUR machine
#   - either CLOUDFLARE_API_TOKEN + CLOUDFLARE_ACCOUNT_ID env vars, or an
#     active `wrangler login` session
#
# Prereq on the VPS (one time): grey DNS record api.tx-multica.zhangjh.cn -> VPS IP,
# backend env updated (see the printed checklist), scripts/api-letsencrypt-nginx.sh run.
set -euo pipefail

API_BASE="${API_BASE:-https://api.tx-multica.zhangjh.cn}"
REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
cd "$REPO_ROOT"

command -v pnpm >/dev/null 2>&1 || {
  echo "ERROR: pnpm not found. Enable it first with: corepack enable"
  exit 1
}
command -v git >/dev/null 2>&1 || true

echo "==> pnpm install (runs on the dev machine, not the VPS)"
pnpm install

echo "==> Building + deploying to Cloudflare Workers"
# DEPLOY_TARGET switches next.config.ts to images.unoptimized (Cloudflare
# Workers has no sharp). NEXT_PUBLIC_API_URL is inlined by Next at build time
# so the browser talks to the backend directly at the api subdomain (which is
# DNS-only grey -> VPS nginx -> 127.0.0.1:8080, plain https, no CF proxy).
export DEPLOY_TARGET=cloudflare
export NEXT_PUBLIC_API_URL="$API_BASE"

if [[ -n "${CLOUDFLARE_ACCOUNT_ID:-}" ]]; then
  export CLOUDFLARE_ACCOUNT_ID
fi
if [[ -z "${CLOUDFLARE_API_TOKEN:-}" ]]; then
  echo "NOTE: CLOUDFLARE_API_TOKEN unset - wrangler will use interactive login."
fi

pnpm --filter @multica/web run deploy:cf

cat <<'EOF'

==> Deployed. One-time set-up still required:

1. Web custom domain: Cloudflare dashboard -> Workers & Pages -> multica-web
   -> Settings -> Domains -> Add custom domain: tx-multica.zhangjh.cn
   (the zhangjh.cn zone is already on Cloudflare DNS, so it just needs the
   existing grey record switched to "Proxied", or remove the proxy and let
   the custom domain take over.)

2. API record (DNS-only, grey): api.tx-multica.zhangjh.cn  A  <VPS_IP>
   (e.g. 119.28.151.121)

3. On the VPS, run once:
   scripts/api-letsencrypt-nginx.sh     # certbot cert + nginx api server block
   (after step 2's record has propagated)

4. Backend env in VPS .env (then restart backend):
   CORS_ALLOWED_ORIGINS=https://tx-multica.zhangjh.cn
   COOKIE_DOMAIN=.zhangjh.cn
   FRONTEND_ORIGIN=https://tx-multica.zhangjh.cn
   MULTICA_PUBLIC_URL=https://api.tx-multica.zhangjh.cn
EOF