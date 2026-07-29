---
title: Evidence — re-derive determinism (E3)
last_verified: 2026-07-29
status: final — byte-identical
---

# Re-derive determinism proof (campaign E3) — 2026-07-29 ~09:55Z

**Claim:** an UNCHANGED re-derive is byte-identical — replaying a range
through the projector with the same code produces exactly the same
served rows (determinism + idempotence). Together with its converse —
a CORRECTED re-derive changes exactly the wrong values (demonstrated in
production by the phoenix base==quote fix, ~237k rows, verified
2026-07-16) — this is the E3 pair.

**Method:** phoenix `trades` over ledgers [63,640,000, 63,690,000]
(v0.21.2, post-D3 lake):

1. Hash before: `SELECT count(*), md5(string_agg(ledger|tx_hash|
   op_index|base_amount|quote_amount ORDER BY …))` →
   **239 rows, `481ae86740fe88afb8184da8b111edbd`**
2. `projector-replay -source phoenix -from 63640000` → live projector
   re-projected the window from the ClickHouse lake to tip.
3. Hash after → **239 rows, `481ae86740fe88afb8184da8b111edbd`** —
   IDENTICAL.

**What this does NOT prove:** determinism of other sources' decoders
(same projector machinery, but per-decoder logic varies — the phoenix
window exercised the 8-events-per-swap reconstruction, a demanding
case); nor anything about ranges whose lake content changed between
derives (that is the corrected-re-derive case, which SHOULD differ).
