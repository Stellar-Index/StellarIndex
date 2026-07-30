---
title: Route sweep — 0×5xx across all 94 routes (v0.21.6)
captured: 2026-07-30 ~12:15Z
command: bash scripts/ops/route-sweep.sh (production, api.stellarindex.io)
verdict: ✅ ok=56 client_4xx=39 server_5xx=0 — the §1 route-availability gate CLOSED
---

# Route sweep — zero server errors

The journey: 21×5xx (2026-07-28 baseline) → 10 (serving-auth fix) →
8 (v0.21.4, mid-backfill) → 5 (post-heavy-drain) → 2 → **0** (v0.21.6).

The final reading was taken under adversarial conditions on purpose:
minutes after a full process restart AND while the redstone
full-history replay was saturating the lake — the worst realistic
serving environment short of an outage.

What closed the last five (all v0.21.5/v0.21.6):

- `/v1/contracts` directory — external group-by/sort spill on the
  shared explorer scan settings (hard OOM → 55.7 s disk-backed
  completion, detached).
- Contract detail ×3 (events / interactions / code-history) — the
  shared `contract_detail` SWR cache; cold key waits one request
  budget, retry lands warm, stale serves with `flags.stale`.
- `/v1/accounts/{g}` (whale accounts) — three layers: detached fill
  (cache can actually fill), stale-serve past the 30s TTL (warm
  window → always-answerable), and the **75× PK-prefix read**
  (trustlines/offers pruned by their `key_xdr` byte prefix instead
  of the bloom skip-index: 5.18 s → 0.069 s measured), which makes
  the detail interactive rather than merely available.

Operator-facing acceptance (2026-07-30): every one of the top 20
wealth-ranked accounts' detail pages probed 200 in 0.24–0.28 s
(first-ever cold probe 1.1 s) — the "click any row on /accounts"
user journey is interactive end-to-end.

**What this does NOT prove**: sustained-load latency (that is the k6
suite's job); the 39 client_4xx rows are the sweep's own invalid-input
probes behaving correctly, not failures.
