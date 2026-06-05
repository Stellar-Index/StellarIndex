---
title: Supply derivation from the ClickHouse lake (every token)
last_verified: 2026-06-06
status: design
---

# Supply from ClickHouse — baseline snapshot + forward flows

**Status: design.** How to serve circulating/total supply for **every token**
when the served tier is (re)built from the ClickHouse Tier-1 lake (ADR-0034),
given the lake captures *activity* (events/ops) but not full ledger-entry
*state*. The answer: supply is a mint/burn calculation, and the flows we need
are in the lake for the common cases; the rest is closed with one cheap
baseline snapshot — NOT the full state-capture.

## 1. Supply is a flow, not (necessarily) a state scan

`supply(asset, t) = baseline(asset, t0) + Σ mints(t0..t) − Σ burns(t0..t)`

If we have a starting balance and every issuance/destruction flow afterward,
we never need to scan full state. Whether the flows are in the lake depends on
the token class.

## 2. By token class

| class | supply formula | flows in the lake? |
|---|---|---|
| **SEP-41 / Soroban tokens** | Σ`mint` − Σ`burn` − Σ`clawback` from contract events | **✅ fully** — `contract_events`, all history. `baseline = 0` at contract genesis. |
| **Classic assets, ≥ P23** (Whisk, mainnet 2025-09-03 ≈ ledger 62M) | Σ`mint` − Σ`burn` (CAP-67 unified asset events, `sep0011_asset` topic) | **✅** — these are real `Type=Contract` events the lake captures (verified: `mint`/`burn`/`transfer` topics present at ledger 62.7M). |
| **Classic assets, < P23** | issuance = issuer→holder outflows; destruction = holder→issuer inflows; pre-CAP-67 there are NO events | **⚠️ partial** — operation bodies are in the lake, but the authoritative balance deltas are effects (`ledger_entry_changes`, deferred). |
| **XLM (native)** | total = ledger header `total_coins`; circulating = total − locked/non-circulating account balances | total **✅ `ledgers`**; circulating needs an account-balance snapshot of the locked set. |

## 3. The cheap closure: one baseline snapshot + forward flows

For classic assets that predate P23 (most major ones), don't reconstruct
pre-P23 history. Instead:

1. **Take a one-time baseline** of each asset's circulating supply at a cut
   ledger `t0` (choose P23 activation, or simply "now"):
   - source: the **live Postgres supply tier** we already maintain (it has
     current classic supply), or a one-time **bucket-list / checkpoint state
     read** (sum of non-issuer `TrustLineEntry` balances). Either is a single
     read, not a per-ledger walk.
2. **Roll forward from `t0` using lake flows**: CAP-67 `mint`/`burn` events for
   classic assets (≥ P23), SEP-41 `mint`/`burn`/`clawback` for Soroban tokens.
3. **XLM**: `total_coins` from the header (exact, per ledger); circulating =
   total − a curated locked-account set whose balances come from the same
   baseline snapshot rolled forward by the relevant account-debit/credit flows
   (or simply refreshed from the live tier).

This gives **current + forward supply for every token from the lake + one
snapshot**, with no dependency on `ledger_entry_changes` history.

## 4. What still needs full state (deferred)

- **Full historical supply time-series before P23** for classic assets (supply
  as it was on an arbitrary past date in 2018) — needs pre-CAP-67 balance
  effects, i.e. `ledger_entry_changes` from genesis (or per-checkpoint
  bucket-list snapshots). This is the full-state / full-explorer work, parked.
- The served product does **not** need this: supply endpoints serve *current*
  values + a forward-going series from `t0`.

## 5. Migration-plan correction

ADR-0034 plan §10a ("drop + rebuild supply tables from CH") must be amended:
- **SEP-41 supply** → rebuild from CH (events). ✓
- **Classic + XLM supply** → **do NOT drop the live-computed path**; seed from a
  baseline snapshot and roll forward. Dropping it before the baseline+forward
  mechanism exists would lose working supply for every classic token.

## 6. Build outline (when scheduled)

- `internal/supply` already has the three algorithms (XLM / classic / SEP-41).
- Add a `baseline(asset, ledger)` reader (from live tier or checkpoint state).
- Point the SEP-41 + CAP-67 mint/burn observers at `clickhouse.StreamContractEvents`
  (the Phase-4 adapter) instead of the live dispatcher for re-derivation.
- Reconcile the rolled-forward number against the live tier at cutover (they
  must agree at `t0`), then serve.
