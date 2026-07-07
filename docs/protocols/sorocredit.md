---
title: sorocredit (consumer-USDC credit) — contract & event verification
last_verified: 2026-07-07
status: current
---

# sorocredit — contract & event verification

> **For the protocol team:** `sorocredit` is Stellar Index's neutral
> name for an **unbranded consumer-USDC credit / CDP protocol** on
> Soroban. This is the set of contracts and events we ingest — please
> confirm it is correct and complete, and tell us the real product name
> so we can label it.
>
> - **Enumeration method:** the single main contract was surfaced by the
>   undecoded-protocol discovery audit as the largest Soroban protocol we
>   captured **nothing** from (221k events / 30d, 7,086 users). Event
>   schemas reverse-engineered from real r1-lake fixtures (2026-07-07).
> - **Gate status:** ✅ Gated (single hard-coded trust-root contract +
>   childgate; ADR-0035).
> - **BackfillSafe:** ❌ `false` — no WASM-history audit yet
>   (`docs/operations/wasm-audits/sorocredit.md` pending).

## Contracts

| Role | Contract |
|---|---|
| Main contract (trust root) | `CCG5EWFY2KCWWYYEIUMIRG6WSAQFLDR5QE5FMCWY25N36XA5GYTCPQWR` |
| Creator | `GADI6FHS…` |
| WASM | `84a88013…` |

The main contract deploys a per-position `Collateral-<uuid>` **child
contract** on each `NewCollateralContract`. Those children hold
collateral but **emit no events** (verified against the lake) — so they
are seeded into the childgate for forward-compat only. Two **other**
mainnet contracts (`CAYUK5JF…`, `CBNOHFX7…`) emit the same distinctive
topic symbols (~159 events total) and are **deliberately excluded** by
the identity gate — `sorocredit` is scoped to the single main contract.

## Events (7)

| Topic | Body | → served table |
|---|---|---|
| `NewCollateralContract` | `[Address(child)]` + `Vec[String(name), Address(owner)]` | `credit_positions` |
| `StatementPublished` | `[String(stmt_uuid), String(pos_uuid)]` + `Vec[i128(amount), Address(collateral), u64(ts)]` | `credit_statements` |
| `Liquidation` | `[Address(collateral), String(pos_uuid), String(stmt_uuid)]` + `Vec[Address(settler), Vec[Address](assets), Vec[i128](amounts), …]` | `credit_settlements` |
| `Withdrawal` | `[Address(collateral)]` + `Vec[Address(token), Address(recipient), i128(amount)]` | `credit_events` |
| `BeaconUpdated` | `Vec[Void, Address(new_beacon)]` | `credit_events` |
| `SupportedAssetAdded` | `[Address(asset)]` + `Vec[…config…]` | `credit_events` |
| `CollateralHashUpdated` | `Vec[Bytes(old), Bytes(new)]` | `credit_events` |

Every event is decoded (EVERY-event invariant). No event contributes to
pricing / VWAP — this is explorer coverage only.

## ⚠️ `Liquidation` is a SCHEDULED SETTLEMENT, not distress

The on-wire topic `Liquidation` is **misleading**. These events are
**recurring scheduled settlements**, not distressed liquidations:

- a **single keeper** (`GA3PWX3H…`) executes every one;
- ~**1:1** with `StatementPublished` (187,926 statements vs 187,718
  "Liquidation"s over the contract's life);
- ~**14 / user / month, uniformly**.

Stellar Index therefore stores them as **settlements** (table
`credit_settlements`, EventType `settlement`) and **must never** surface
a "221k liquidations" risk signal. Any future `/v1` or explorer surface
must frame these as scheduled settlements.

## Recognition attribution

The main contract is pinned in the ADR-0033 reconciliation catalogue
(`contractIDs`), so any FUTURE unhandled `sorocredit` topic caps this
source's completeness verdict instead of disappearing into the
system-wide recognition bucket. The four tables each reconcile by their
own `sorocredit.<event_type>` kind.

## Coverage

- **Substrate:** 100% — every event is in the ClickHouse lake (ADR-0034).
- **Served tier:** filled from deploy onward by the projector
  (ADR-0031/0032, sole writer). Historical back-window catch-up:
  `stellarindex-ops projector-replay -source sorocredit -from 61620822`
  (under `run-heavy-job.sh`).
- **Decoder / gating / storage:** `internal/sources/sorocredit/README.md`.
