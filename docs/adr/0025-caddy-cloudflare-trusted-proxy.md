---
adr: 0025
title: Caddy trusts Cloudflare for client-IP signal via CIDR-pinned static list
status: Accepted
date: 2026-05-10
supersedes: []
superseded_by: null
---

# ADR-0025: Caddy trusts Cloudflare for client-IP signal via CIDR-pinned static list

## Context

Traffic is Cloudflare edge, then Caddy, then the API. Forwarding the TCP peer as `X-Forwarded-For` made every client look like a Cloudflare POP, which broke per-IP rate limits and access logs. Trusting `CF-Connecting-IP` unconditionally lets anyone who reaches the origin directly spoof their IP.

## Decision

Caddy's global `servers` block sets `trusted_proxies static <Cloudflare IPv4 and IPv6 CIDRs>` and `client_ip_headers CF-Connecting-IP X-Forwarded-For`. `reverse_proxy` sends `X-Forwarded-For {client_ip}`, never `{remote_host}`. The API still trusts only Caddy as a proxy (`api.trusted_proxy_cidrs`, `127.0.0.1/32` on r1).

The CIDR list is committed in the Caddyfile and refreshed by hand each quarter, or when Cloudflare announces a range change. The third-party auto-refresh plugin is not used because it needs an `xcaddy` build.

## Invariant

A listener that is reachable without Cloudflare in front must not carry the `trusted_proxies` block. Cloudflare's published ranges are the only trusted peers. No automated check enforces either rule; `configs/caddy/README.md` documents them.

## Consequences

- Per-IP rate limits and logs use the real client IP; the same Caddyfile shape serves r2 and r3.
- A stale CIDR list degrades rate-limit accuracy for the affected edge only. It breaks nothing else.
- Quarterly operator task: fetch `https://www.cloudflare.com/ips-v4` and `ips-v6`, diff, update, deploy.

## Evidence

- `configs/caddy/Caddyfile.api` (`trusted_proxies`, `client_ip_headers`, `{client_ip}`).
- `configs/caddy/README.md` (refresh procedure); `configs/ansible/roles/archival-node/templates/Caddyfile.j2` carries the same block.
