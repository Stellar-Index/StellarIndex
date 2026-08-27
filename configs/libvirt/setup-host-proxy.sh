#!/usr/bin/env bash
#
# setup-host-proxy.sh — install Caddy on the KVM host and deploy the reverse
# proxy that fronts the test-net VMs (see host-Caddyfile for the DNS/TLS split).
#
# Idempotent. Run ON THE HOST (95.217.126.13) from this directory:
#   rsync -a configs/libvirt/ root@95.217.126.13:/root/libvirt/
#   ssh root@95.217.126.13 'cd /root/libvirt && ./setup-host-proxy.sh'
#
# Caddy auto-provisions Let's Encrypt certs for the grey (DNS-only) api.*
# domains via HTTP-01, so :80 and :443 must be reachable from the internet.
set -euo pipefail

CADDYFILE_SRC="${1:-$(dirname "$0")/host-Caddyfile}"

if [[ ! -f "$CADDYFILE_SRC" ]]; then
	echo "host-Caddyfile not found at $CADDYFILE_SRC" >&2
	exit 1
fi

# ── Install Caddy from the official apt repo (idempotent) ──────────────────
if ! command -v caddy >/dev/null 2>&1; then
	echo "== installing Caddy from the official apt repo =="
	export DEBIAN_FRONTEND=noninteractive
	apt-get install -y debian-keyring debian-archive-keyring apt-transport-https curl gnupg
	curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' \
		| gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
	curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' \
		> /etc/apt/sources.list.d/caddy-stable.list
	apt-get update
	apt-get install -y caddy
else
	echo "== Caddy already installed: $(caddy version | head -1) =="
fi

# ── Deploy the Caddyfile ───────────────────────────────────────────────────
install -D -m 0644 "$CADDYFILE_SRC" /etc/caddy/Caddyfile
echo "== validating /etc/caddy/Caddyfile =="
caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile

# ── Enable + (re)start ─────────────────────────────────────────────────────
systemctl enable caddy
systemctl restart caddy
sleep 2
systemctl --no-pager --lines=0 status caddy | head -3
echo "== Caddy deployed. Certs provision on first request to each grey api.* host. =="
