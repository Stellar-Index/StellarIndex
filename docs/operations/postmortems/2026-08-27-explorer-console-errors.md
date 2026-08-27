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

**Root cause (sharpened after research + reading the deploy):** the deploy
workflow *already* prunes the per-segment files (`.github/workflows/
explorer-deploy.yml`, "Prune Next 16 segment-cache prefetch files", site-audit
S-024) to fit the 20k cap — but it deleted `__next._tree.txt` **too**. That tree
file is the ONE segment file Next 16's client router PREFETCHES on every
`<Link>` (hover/viewport). Deleting it 404'd every prefetch on routes without a
`functions/*/[[path]].js` shell fallback — the "tons of console errors." (A
`next.config` flag to stop the emission does **not** exist in Next 16.2/16.3 —
verified; the files are emitted unconditionally.)

**Fix (applied + verified):** keep `__next._tree.txt` in the prune
(`! -name "__next._tree.txt"`); the bulkier `_head`/`_full`/`_index`/`__PAGE__`/
layout payloads stay pruned (fetched only on real navigation, where the client
falls back to `index.txt`). This is **SEO-neutral** — no `generateStaticParams`
trim needed — and holds the mainnet build at **7,244 files**, far under the
18,500 ceiling. Verified on testnet + futurenet: every previously-404ing tree
file (`/`, `/sources/`, `/diagnostics/`, `/pricing/`, `/network/`) now returns
200 (CLI with fresh cache keys). ADR-0044 (Workers SSR) remains the strategic
end-state that removes the static file limit entirely.

**Mainnet still needs this fix.** Mainnet has the identical 404s; the workflow
change must reach `main` (branch merge or a cherry-pick of just the workflow
step) + a mainnet explorer redeploy. It re-deploys `main`'s existing code — it
does not touch r1.

## Still to sweep

Per-page test-net walk for empty/mainnet-only surfaces and any other failing
requests (ledgers / transactions / accounts / contracts / sdex / status /
network), plus the price-404 residuals already fixed earlier.
