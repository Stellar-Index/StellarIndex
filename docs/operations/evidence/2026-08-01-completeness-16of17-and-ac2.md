---
title: Completeness 16/17 (redstone + soroswap FLIPPED GREEN); AC2 load evidence final framing
captured: 2026-08-01 ~02:35Z (v0.21.8 post-deploy chain)
verdict: 🟢 redstone complete=t (projection verified full range) · 🟢 soroswap complete=t · 🔴 aquarius complete=f — its FIRST full-range reconcile surfaced long-standing artifacts (next unit)
---

# The two targets flipped

- **redstone**: `complete=true`, projection verified [58,849,942 → tip] —
  the state-write attribution closed the final 15 provably-ambiguous
  ledgers. Blind ladder final: **1,626 → 170 → 15 → 0**.
- **soroswap**: `complete=true`, projection verified [52,020,138 → tip] —
  non-directional-swap recognition + the 20-twin skim dedup. Taker
  coverage 100.00% (554,221/554,221).

# Aquarius — newly surfaced, not a regression

Its scoped verify was the FIRST full-range reconcile (prior verdicts
carried an incremental claim). Over [52.7M → tip] it found:
41 undecodable-but-matched events on 38 ledgers (first 53,626,410 —
old-WASM-era schema variants, the same class redstone/soroswap just
cleared) and `trades` over-projection: 207 mismatched ledgers,
Σ|Δ|=482, served>expected (first 61,510,608: expected 0, served 3) —
rows projected by an older decoder era that the current decoder
derives differently. Queued as the next per-source unit (probe →
classify → decode arms or scoped cleanup → replay → verify), the
proven playbook. All 16 other sources: `complete=t`.

# AC2 load evidence — final honest framing

- **SLA-shaped acceptance scenario** (00-acceptance-contract-rate):
  **30,601 requests, 0 failures, med 8.5ms, p95 37.1ms** — PASS with
  huge margin. This is the scenario the SLA targets are defined on.
- **Stress scenario** (06-mixed-realistic, 400 VUs): mixed3 p95 5.1s /
  4.5% fails (post-replay churn) and mixed4 p95 5.0s / 4.4% fails
  (quiet-er box) — two consistent runs under different conditions.
  CONCLUSION: this is the genuine single-box ceiling under 400-VU
  mixed load hammering the heaviest endpoints, not measurement
  contamination. It exceeds rated load (the public rate limits keep
  real traffic far below it); filed as a capacity datum for the
  warm-standby/HA decision, not an SLA failure. Side effect: each
  mixed run self-trips the SLO burn-rate pages (fail% + latency over
  the burn windows) — they roll off; expect the same if re-run.
