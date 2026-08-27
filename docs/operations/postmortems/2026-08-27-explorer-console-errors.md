---
title: Investigation — explorer console errors ("tons of errors while navigating")
date: 2026-08-27
status: in-progress
severity: P3 (no data loss; failed prefetch requests + one auth probe)
author: ops (Claude-assisted)
---

# Explorer console errors — investigation

User report: "tons of browser console errors as I navigate around the site …
there shouldn't be these console errors happening at all." Investigated with a
live browser sweep (mainnet + testnet). The console-panel red errors are almost
all **failed network requests** (the browser logs a `Failed to load resource`
per non-2xx), not `console.error()` calls — so `read_console_messages` shows a
clean console while the Network panel is full of 404/503. Two root causes:

## 1. `/v1/account/me` 503 on the lean test nets — FIXED

`useMe()` fires an unconditional credentialed `GET /v1/account/me` on **every**
page (it drives the navbar session widget). On mainnet a signed-out visitor
gets a clean **401 → null**. On testnet/futurenet the accounts/API-key SaaS
backend isn't deployed, so it returns **503** → the query throws → a console
error on every single page load.

**Fix (committed):** added `CURRENT_NETWORK.accounts` (mainnet-only). `useMe`
skips the probe where it's false; the sidebar `AccountCard` and the footer
Sign in / Create account / Your account links are hidden there too. Accounts are
a mainnet-only feature — genuinely not on the free public test nets (this also
satisfies Goal 1's "omit what's genuinely not on testnet").

## 2. Site-wide prefetch 404s — Cloudflare Pages 20k-file limit exceeded

**This is the big one, and it affects mainnet too** (not test-net-specific).
As you navigate, Next.js `<Link>` prefetch requests fetch per-route RSC/segment
files:

- `GET /<route>/__next._tree.txt?_rsc=…` → **404** for many routes
  (`/sources/`, `/diagnostics/`, `/pricing/`, `/signup/`, `/methodology/`, …)
  while others serve 200 (`/assets/`, `/markets/`). The inconsistency is the
  tell.
- Prefetch `HEAD /<route>/` → 503 in the browser (Cloudflare bot-challenging
  rapid prefetch HEADs; `curl -I` returns 200, so it's edge-side, not the file).

**Root cause:** the static-export build (`out/`) contains **22,283 files**,
over **Cloudflare Pages' 20,000-file-per-deployment limit**. Files past the
limit are dropped, so their prefetch requests 404. The bulk is **19,712 `.txt`
files** — Next 16 emits ~7 per route (`__next._tree.txt`, `_head`, `_full`,
`_index`, `__PAGE__`, the layout segment, `index.txt`) as its per-segment
prefetch cache, across ~2,337 routes (incl. 513 pre-rendered `/assets/[slug]`).

Evidence:
- `find out -type f | wc -l` → 22,283 (`.txt` = 19,712; `.html` = 2,337).
- The tree file that 404s live (`out/sources/__next._tree.txt`) **exists in the
  local build** → it's being dropped at deploy, not missing from the export.
- `curl` confirms `/sources/__next._tree.txt` 404 vs `/assets/__next._tree.txt`
  200 on the same live deployment.

**Fix direction (not yet applied — being researched for correctness, then a
careful mainnet deploy):** get the file count under 20k. Primary lever = reduce
Next 16's per-segment prefetch `.txt` emission (a `next.config` flag if one
exists in 16.3.1) so client nav still works but each route emits ~1–2 RSC files
instead of ~7. Secondary lever = trim `generateStaticParams` (e.g. `/assets/[slug]`
513 → top-N) **only if** unknown slugs still resolve client-side rather than
404. Strategic option = the ADR-0044 Workers-SSR path, which removes the static
file limit entirely. Not hot-patched — this is a framework-level change touching
every page on both mainnet and the test nets.

## Still to sweep

Per-page test-net walk for empty/mainnet-only surfaces and any other failing
requests (ledgers / transactions / accounts / contracts / sdex / status /
network), plus the price-404 residuals already fixed earlier.
