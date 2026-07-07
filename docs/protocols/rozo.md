# Rozo — contract & event verification

> **For the Rozo team:** this is the set of Rozo v1 Payment contracts and
> events Stellar Index ingests. Please confirm the three mainnet Payment
> contracts below are correct and complete, and tell us when v2
> Forwarder / IntentBridge go live (they get their own source entries, not
> a widening of this one).
>
> - **Enumeration method:** hard-coded set of the three known v1 Payment
>   contracts, gated on contract identity.
> - **Last verified:** 2026-07-06 (source: `internal/sources/rozo`; WASM
>   audit `docs/operations/wasm-audits/rozo.md`, 2026-05-26).
> - **Gate status:** ✅ Gated (ADR-0035): `Matches` checks topic[0]
>   **and** `IsRozoContract` — events only attribute when the emitter is
>   one of the three pinned contracts.

## What Rozo is

Rozo is an intent-bridge protocol on Stellar. Coverage is scoped to
**v1 Payment** — the only mainnet-live Rozo contract shape at time of
writing. v2 Forwarder + IntentBridge are pre-mainnet (design-stage) and
documented for follow-up in `docs/architecture/rozo-stellar-coverage.md`.

## Contracts (3 — v1 Payment)

| Contract |
|---|
| `CAC5SKP5FJT2ZZ7YLV4UCOM6Z5SQCCVPZWHLLLVQNQG2RWWOOSP3IYRL` |
| `CCRLTS3CMJHYHFD7MYRBJPNW6R3LCXNDO2B6TK6AS6FSXAHR6GBMGLRE` |
| `CAQPKW5AUPEA4C7OERZRUCBWT5RZDSETO4PR5REVRC5MT4CF3PBSKXQC` |

All three share a single WASM hash (`b56aedeaf80c3d4b…` per the audit),
deployed 2026-01-18 + 2026-03-24. Bridge-out volume without a memo flows
through the C-wallet contracts; memo-bearing flows go via known classic
relayer accounts (`MainnetRelayerAccounts`).

## Events decoded

Two `#[contractevent]` types from
[`v1/stellar/payment/src/lib.rs`](https://github.com/RozoAI/rozo-intents-contracts),
each mapped 1:1 to a Go type, landing in the `rozo_events` hypertable
(migration 0039, fully typed — no JSONB blob):

| Event (topic[0]) | Body | Where it lands |
|---|---|---|
| `payment` (topic `("payment", from: Address)`) | `{ from, destination, amount, memo }` | `rozo_events` |
| `flush` (topic `("flush",)`) | `{ token, destination, amount }` (admin sweep) | `rozo_events` |

Amounts are `i128` → `*big.Int` (ADR-0003; the decoder locks the
large-i128 round-trip in tests against the int64-truncation bug class).

## Aggregator treatment — not counted

Class `Bridge` / `IncludeInVWAP=false`, `DefaultWeight=0`
(`external.Registry`). Bridges move tokens across chains; they publish no
prices and emit no trades, so Rozo never contributes to VWAP. It is
captured for the granular-coverage mission (intent-bridge flow visibility)
and surfaced on `/v1/sources`.

## Backfill safety

`BackfillSafe = true` (audited 2026-05-26 via the bridges WASM-history
walk `[60M, 62.64M]` across all three contracts: zero upgrades observed,
single shared WASM hash). Note: the source package's README still carries
the pre-audit `BackfillSafe = false` language — the authoritative value is
the registry entry (`true` since the 2026-05-26 audit).

## References

- Source package: `internal/sources/rozo/README.md`
- Architecture: `docs/architecture/rozo-stellar-coverage.md`
- Sibling bridge: [cctp.md](cctp.md)
