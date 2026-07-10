# Rozo — contract & event verification

> **For the Rozo team:** this is the set of Rozo v1 Payment contracts and
> events Stellar Index ingests. Please confirm the four mainnet Payment
> contracts below are correct and complete (the 4th was admitted
> read-only from lake evidence, not an operator confirmation — see
> below), and tell us when v2 Forwarder / IntentBridge go live (they get
> their own source entries, not a widening of this one).
>
> - **Enumeration method:** hard-coded set of the four known v1 Payment
>   contracts, gated on contract identity.
> - **Last verified:** 2026-07-09 (source: `internal/sources/rozo`; WASM
>   audit `docs/operations/wasm-audits/rozo.md`, 2026-05-26 + 2026-07-09
>   addendum).
> - **Gate status:** ✅ Gated (ADR-0035): `Matches` checks topic[0]
>   **and** `IsRozoContract` — events only attribute when the emitter is
>   one of the four pinned contracts.

## What Rozo is

Rozo is an intent-bridge protocol on Stellar. Coverage is scoped to
**v1 Payment** — the only mainnet-live Rozo contract shape at time of
writing. v2 Forwarder + IntentBridge are pre-mainnet (design-stage) and
documented for follow-up in `docs/architecture/rozo-stellar-coverage.md`.

## Contracts (4 — v1 Payment)

| Contract | Admitted | Evidence |
|---|---|---|
| `CAC5SKP5FJT2ZZ7YLV4UCOM6Z5SQCCVPZWHLLLVQNQG2RWWOOSP3IYRL` | 2026-05-20 | RozoAI-confirmed |
| `CCRLTS3CMJHYHFD7MYRBJPNW6R3LCXNDO2B6TK6AS6FSXAHR6GBMGLRE` | 2026-05-21 | RozoAI-confirmed |
| `CAQPKW5AUPEA4C7OERZRUCBWT5RZDSETO4PR5REVRC5MT4CF3PBSKXQC` | 2026-05-21 | RozoAI-confirmed |
| `CAFO6OUZAL62SGDVGHHJPSCOOF3HUKXLED3C3FS5RRQI2VBZ4F5HBPXI` | 2026-07-09 | lake evidence (see below) — **not yet RozoAI-confirmed** |

All four share a single WASM hash (`b56aedeaf80c3d4b…` per the audit).
The first three deployed 2026-01-18 + 2026-03-24; the 4th deployed +
initialized at ledger 61522475 (2026-03-06) and emitted exactly one
`payment_event` (ledger 61522543, memo "test payment 0.01 USDC").
Admission evidence for the 4th (read-only ClickHouse lake queries on
r1, no MinIO/port-9000 access): bytewise-identical WASM hash; identical
3-key instance-storage init shape (`dest`/`usdc`/`init`); its `dest`
init field AND its one event's `destination` field both resolve to
`GB4CLV3UMXDPFP5OQJQKUCWPRJXPXPJSHTUKZEJLAIZFZR7UHYAQ6EB4` — an exact
match to a documented `MainnetRelayerAccounts` entry; its `usdc` field
resolves to the canonical Circle USDC SAC
(`CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75`). Full
trail: `internal/sources/rozo/events.go`'s `MainnetPaymentContracts`
doc comment and `docs/operations/wasm-audits/rozo.md`. Bridge-out
volume without a memo flows through the C-wallet contracts;
memo-bearing flows go via known classic relayer accounts
(`MainnetRelayerAccounts`).

## Events decoded

Two `#[contractevent]` types from
[`v1/stellar/payment/src/lib.rs`](https://github.com/RozoAI/rozo-intents-contracts),
each mapped 1:1 to a Go type, landing in the `rozo_events` hypertable
(migration 0039, fully typed — no JSONB blob). The deployed contracts
emit the FULL-length symbols below (topic_count=1 for `payment_event` —
verified against 3/3 real lake fixtures 2026-07-09; `from` lives only in
the body map, NOT as a second topic element, despite what the upstream
Rust source's `publish((PAYMENT, from.clone()), …)` call suggests):

| Event (topic[0]) | Body | Where it lands |
|---|---|---|
| `payment_event` (1-element topic `("payment_event",)`) | `{ from, destination, amount, memo }` | `rozo_events` |
| `flush_event` (1-element topic `("flush_event",)`) | `{ token, destination, amount }` (admin sweep) | `rozo_events` |

Amounts are `i128` → `*big.Int` (ADR-0003; the decoder locks the
large-i128 round-trip in tests against the int64-truncation bug class).

**Body verification (ROADMAP #89 residual, 2026-07-10):** the
`payment_event` BODY decode (as opposed to the topic SHAPE, verified
2026-07-09 above) had never been checked against real on-chain bytes.
A read-only ClickHouse-lake pull of 5 real `payment_event` rows across
both the original and 4th-contract deployments confirmed the decoder
is **verified correct**: alphabetical `{amount, destination, from,
memo}` ScMap, `from`/`destination` both ACCOUNT (G-strkey) addresses,
i128 amount, free-form memo. No decoder change required; a real-bytes
golden test now pins this (`internal/sources/rozo/decode_test.go`
`TestGolden_Payment_LakeBytes`). `flush_event` has **zero real
occurrences** across all 4 gated contracts as of the same census —
`DecodeFlush` remains verified only against synthetic fixtures.

## Aggregator treatment — not counted

Class `Bridge` / `IncludeInVWAP=false`, `DefaultWeight=0`
(`external.Registry`). Bridges move tokens across chains; they publish no
prices and emit no trades, so Rozo never contributes to VWAP. It is
captured for the granular-coverage mission (intent-bridge flow visibility)
and surfaced on `/v1/sources`.

## Backfill safety

`BackfillSafe = true` (audited 2026-05-26 via the bridges WASM-history
walk `[60M, 62.64M]` across the original three contracts: zero upgrades
observed, single shared WASM hash). The 4th contract
(`CAFO6OUZ…HBPXI`, admitted 2026-07-09) was NOT in that walk's
`-contracts` list, but its instance's WASM hash — read directly from
the lake — is bytewise identical to the audited hash, so it's covered
by the same finding by construction; see
`docs/operations/wasm-audits/rozo.md`'s 2026-07-09 addendum. Note: the
source package's README still carries the pre-audit `BackfillSafe =
false` language — the authoritative value is the registry entry
(`true` since the 2026-05-26 audit).

## References

- Source package: `internal/sources/rozo/README.md`
- Architecture: `docs/architecture/rozo-stellar-coverage.md`
- Sibling bridge: [cctp.md](cctp.md)
