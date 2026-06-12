---
title: Rozo relayer-account observer (classic-payment bridge path)
last_verified: 2026-06-04
status: proposed
---

# Rozo relayer-account observer (`rozo-relayer`)

**Status: proposed (not yet built).** Spec for capturing Rozo's
real bridge volume — the USDC/EURC *classic-payment* flows through
Rozo's relayer accounts, which the Soroban-event decoder
(`internal/sources/rozo`) is structurally blind to.

## Problem

Rozo's on-chain footprint is split across two paths:

1. **Soroban contract path** — three `C` payment contracts
   (`rozo.MainnetPaymentContracts`) that emit `payment` / `flush`
   contract events. Decoded by `internal/sources/rozo` →
   `rozo_events`. **Currently idle** (`rozo_events` = 0 rows): the
   bridge-out-via-contract path has little/no mainnet use.

2. **Classic relayer path** — two *classic* Stellar accounts
   (`rozo.MainnetRelayerAccounts`) that move USDC/EURC via classic
   `payment` operations. **This carries the bulk of real volume**
   (RozoAI: "those 2 addresses should cover most of the txs on
   usdc/eurc"), and we capture **none** of it: classic payments emit
   no Soroban contract event, so the event decoder never sees them.
   `MainnetRelayerAccounts` is declared in code but wired to nothing.

On-chain confirmation (Stellar Expert / Horizon, 2026-06-04):

| relayer account | payments | trades | assets | activity |
|---|--:|--:|---|---|
| `GADDIYCV…7W3XY` | 4,656 | 10 | XLM, USDC, EURC | monthly: very high |
| `GB4CLV3U…Q6EB4` | 1,880 | 0 | XLM, USDC, EURC | monthly: very high |

Event shape (from a real sample on `…6EB4`):
- **Inbound** (dest = relayer): user deposits, e.g. 1000 / 3000 / 750
  USDC, each carrying a **text memo = the bridge order/intent ref**
  (`27577216`, `46213951`, …).
- **Outbound** (source = relayer): bridge payouts, 0.12 → 1999 USDC +
  EURC, **no memo**.
- All `payment` ops. Assets are USDC/EURC only — the accounts also
  attract heavy native-XLM dust-spam ("claim your VTRX airdrop"),
  which the asset filter naturally excludes.

## Watched set

- Relayer accounts (`rozo.MainnetRelayerAccounts`):
  `GADDIYCVR2Z6H46YWZE53LICP56ZBNEUUT2QAG4QHSWVIYE44HS7W3XY`,
  `GB4CLV3UMXDPFP5OQJQKUCWPRJXPXPJSHTUKZEJLAIZFZR7UHYAQ6EB4`
- Assets: **USDC**
  `GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN`,
  **EURC** `GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2`

## Ingest path — classic `OpDecoder` (non-projected)

Like SDEX (`internal/sources/sdex`), this is a **non-projected**
source: it writes through the dispatcher's classic-operation path,
not `soroban_events`/projector (ADR-0032 — non-projected events write
through the dispatcher's own path). Implement
`dispatcher.OpDecoder` (`Name` / `Matches(op) bool` /
`Decode(OpContext) ([]consumer.Event, error)`):

- **Matches:** `op.Body.Type ∈ {PAYMENT, PATH_PAYMENT_STRICT_SEND,
  PATH_PAYMENT_STRICT_RECEIVE}`. (The sample is plain `payment`;
  match path-payments defensively.)
- **Decode:** keep only ops where asset ∈ {USDC, EURC} **and**
  (`destination` ∈ watched ⇒ **inbound**) or (op/tx source ∈ watched
  ⇒ **outbound**). Emit one `consumer.Event` per matched op.

`OpContext` has no transaction memo (fields: Ledger, ClosedAt,
TxHash, TxSource, OpSource, OpIndex, Op, OpResult). Capturing the
inbound order-ref memo therefore needs a small **`OpContext.Memo` /
`MemoType` addition in the dispatcher** (backward-compatible — existing
OpDecoders ignore it). **v1 ships without the memo; memo capture is
v2** (enables per-order net-flow correlation).

## Storage — `rozo_relayer_payments` hypertable (migration 0053)

Per-source table, partitioned by `ledger_close_time`, registered as an
ADR-0030 gap-detector target.

| column | type | notes |
|---|---|---|
| ledger_close_time | timestamptz | partition key |
| ledger | bigint | |
| tx_hash | text | |
| op_index | int | classic txs have many ops → part of PK |
| relayer_account | text | which watched account |
| counterparty | text | the other side |
| direction | text | `inbound` / `outbound` |
| asset | text | canonical asset_id (USDC / EURC) |
| amount | **NUMERIC** | `*big.Int`, stringified on the wire (ADR-0003 — never int64) |
| memo / memo_type | text | NULL in v1; populated in v2 |

PK `(ledger_close_time, ledger, tx_hash, op_index)`. Written via a
`persistRozoRelayerPayment` arm in `internal/pipeline/sink.go`
(non-projected — NOT `IsProjectedEvent`).

## Source registration + wiring

- New `external.Registry` entry `"rozo-relayer"` — `ClassBridge`,
  `DefaultWeight: 0`, `IncludeInVWAP: false` (volume signal, not a
  price). Increments `stellarindex_source_events_total{source=
  "rozo-relayer"}` → appears in `active_sources` + the **Entries 24h**
  panel.
- Wire `dispatcher.AddOpDecoder(rozorelayer.NewDecoder())` in
  `cmd/stellarindex-indexer/main.go` alongside SDEX.
- Register `rozo_relayer_payments` in
  `internal/storage/timescale/per_source_gaps.go` (ADR-0030 —
  CI fails an unregistered per-source hypertable).

## Backfill + completeness

- **Backfill** the watched accounts deploy→tip via the dispatcher's
  classic-op replay: `…6EB4` from ~2026-02-19 (~ledger 61M),
  `…W3XY` older (pre-P23). Bounded — a few thousand real ops.
- **Substrate**: the same `ledger_ingest_log` continuity (ADR-0033
  Claim 1) covers it — no separate substrate.
- **Reconciliation oracle**: like SDEX, the classic side has no
  `soroban_events` oracle; reconcile against an LCM classic-payment
  census restricted to these accounts+assets, or (v1) rely on
  substrate-continuity + the gap detector.

## Design note — P23 unified events

Post-P23 (CAP-67, mainnet 2025-09-03) every classic USDC/EURC payment
*also* emits a Soroban `transfer` event with a `sep0011_asset` topic,
so `…6EB4` (created post-P23) is theoretically capturable via the
transfer-event path too. But `…W3XY` predates P23, and the classic
`OpDecoder` is uniform across both eras and doesn't depend on
transfer-event capture being wired for classic SACs. **Primary path:
classic `OpDecoder`**; optional post-P23 transfer-event cross-check
later.

## Open questions

1. **USDC/EURC only** (recommended — XLM through these accounts is
   spam), or include XLM bridge flows?
2. **Watchlist freshness** — relayer accounts can rotate. They live in
   `rozo.MainnetRelayerAccounts` (code constant, redeploy to change);
   consider a recognition-style alert when a new high-volume USDC/EURC
   counterparty of the known accounts appears.
3. **Net-flow correlation** (inbound memo ↔ outbound payout) — v2,
   depends on the `OpContext.Memo` addition above.
