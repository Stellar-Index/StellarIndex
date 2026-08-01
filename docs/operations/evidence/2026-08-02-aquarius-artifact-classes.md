---
title: Aquarius completeness backlog — both artifact classes root-caused (probe evidence)
captured: 2026-08-02 (r1 lake read-only probe; local gated-decoder re-derive over an SSH tunnel)
verdict: class (a) = zero-amount dust swaps (decode fix on main) · class (b) = pre-gating CA7RQDMM parallel-router rows (operator DELETE staged)
---

# Scope

The 2026-08-01 FIRST full-range `compute-completeness -source aquarius`
([52,728,375 → tip]) surfaced two long-standing artifact classes
(evidence/2026-08-01-completeness-16of17-and-ac2.md). Both are now
root-caused with lake-byte evidence, on the same probe → classify → fix
playbook that cleared redstone (1,626 → 0) and soroswap.

# Class (a) — 41 undecodable-but-matched events = zero-amount dust swaps

Probe: the production gated Decoder (`aquarius.NewDecoder()`, in-code
332-pool seed + router add_pool fan-out — 336 gated contracts including
3 pools announced after the seed snapshot) re-run over EVERY lake event
emitted by a gated contract, [52,728,375 → 63,743,183], windowed
500k-ledger streams, adjacent-duplicate dedup (the reconcile's own
algorithm).

Result: **exactly 41 refusals across exactly 38 ledgers — the finding
reproduced byte-for-byte** — and they split into two sub-classes:

- **40 zero-amount dust swaps** (ledgers 53,626,410 → 57,323,650; 32 of
  them from one pool `CD4ASKG2…`, repeatedly `sold=2`): well-formed
  4-topic `trade` events whose body `(sold, bought, fee)` 3-tuple
  carries `bought=0` — genuine swaps whose output rounded to zero.
  First exemplar: ledger 53,626,410, tx 3870e2fc…, event 2, registered
  pool `CCY2PXGM…`, body `(sold=2, bought=0, fee=0)`. NOT schema
  variants: topics, arity and types decode cleanly; only the value is
  zero. Fix (this commit): `ErrZeroAmountTrade` recognized no-op —
  `canonical.Trade.Validate` forbids non-positive amounts, so these can
  never be served rows; decode now succeeds with zero outputs and the
  reconcile sees expected == served == 0. Negative amounts remain
  `ErrMalformedPayload`. Same pattern as redstone's empty
  `write_prices` batch (78486ae6).
- **1 real old-vs-new WASM schema variant**: the canonical router's
  single `set_privileged_addrs` event with a FIVE-element body (ledger
  57,711,797, closed 2025-06-25). Full-history shape census: every
  event ≤ 57,604,772 is `Vec[Address×3, Vec[Address]]` (4 elements, the
  shape the 2026-07-10 audit pinned); every event from 57,697,794
  onward carries the same four plus ONE trailing role Address. Fix
  (this commit): arity-branch decode-by-evidence — the v2 trailing
  address lands in `Attributes["addr_3"]`; other arities still fail
  closed.

Real-lake-bytes golden tests pin all three behaviors (zero-amount
no-op, negative refusal, v2 5-element body).

# Class (b) — 482 over-projected trade rows = pre-gating parallel-router pools

- The lake holds **484** (deduped) 4-topic `Symbol("trade")` events from
  NON-gated contracts at ledgers ≥ 61,510,608 (208 ledgers, 31 emitting
  contracts). **All 484 attribute to pools announced by the parallel
  router deployment `CA7RQDMMV6E53P5EDZA5GPWBZ33AMW2ZNO42XLI2RGRIAP4QXIARUOJQ`**
  — the set deliberately EXCLUDED from the ADR-0035/0040 gate
  (same router WASM, zero overlap with the protocol's registry API;
  docs/protocols/aquarius.md "Flagged").
- **480 of them join a served `trades` row exactly** by
  `(ledger, tx_hash, FanoutOpIndex(op, event))`, source='aquarius' —
  spanning ledgers 61,510,608 → 62,875,592 (2026-03-05 → ~2026-06-12,
  the pre-gating live-projection window). The 4 non-joining events sit at
  3 post-gating ledgers (63,580,260/63,580,265/63,591,082) and were
  correctly never projected.
- Root cause: the 2026-07-05 gating rollout's step 2 ("ALSO delete the
  flagged rows" — the replay is upsert-only) was never executed; the
  gated re-derive added nothing for these events, leaving the old
  pre-gating rows stranded as phantoms.
- **Full independent reconcile** (SQL-derived expected: distinct gated
  4-topic non-zero-amount trade events per ledger, vs PG served counts,
  both capped at tip 63,743,183): **207 mismatched ledgers, Σ|Δ|=482 —
  the 08-01 finding reproduced exactly** — decomposing as:
  - 205 over-projected ledgers, surplus **480** — every ledger's surplus
    EXACTLY equals its CA7RQDMM phantom-row count (205/205);
  - 2 UNDER-projected ledgers (63,400,605 and 63,400,614, one missing
    trade each): normal-amount trades from REGISTERED pools (`CBBMQBNH…`,
    `CCNXGPE4…`), present and successful in the lake but absent from the
    served tier. Both closed 2026-07-09 15:16–15:17Z — inside the
    Galexie P27-decode SEV window (zero lake loss, but the live sink
    path dropped these two rows during the crash-loop churn).
- Fix: decode-side stays as-is (the gate is correct; the phantoms are
  misattributed data, the two drops are a projection gap). Operator
  loop: (1) staged scoped DELETE of the 480 exact keys + verify SELECT
  (expect 480 rows / 205 ledgers); (2)
  `stellarindex-ops projector-replay -source aquarius -from 63400600`
  (tiny window; upsert-only re-derive restores the two dropped rows, and
  the v0.21.9 dirty-window mechanism forces re-reconciliation of the
  rewound region). Post-delete, the affected `prices_*` continuous
  aggregate buckets (2026-03-05 → 2026-06-13) need a refresh pass.

# Post-deploy sequence to flip aquarius complete=t

1. Deploy the build carrying the zero-amount no-op + set_privileged_addrs
   v2 fixes (blind 41 → 0; no served rows change from the decode fixes).
2. Operator: run the staged verify SELECT, then the scoped DELETE
   (480 rows; under a transaction with the decompression cap lifted).
3. Operator: `projector-replay -source aquarius -from 63400600` (under
   `run-heavy-job.sh`; restores the two P27-window drops; the dirty-window
   mechanism forces re-reconciliation of the rewound region).
4. `compute-completeness -source aquarius` full-range re-verify →
   expect blind 41 → 0 and projection mismatches 207 → 0 → complete=t
   (17/17).
5. Optional: refresh `prices_*` continuous aggregates over
   2026-03-05 → 2026-06-13 (the deleted phantoms' buckets).
