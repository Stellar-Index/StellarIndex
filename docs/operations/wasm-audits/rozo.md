---
title: Rozo WASM-history audit
last_verified: 2026-07-09
status: "approved — 4 contracts (see 2026-07-09 addendum)"
source: rozo
backfill_safe: true
---

# Rozo WASM audit

Audit log for the `rozo` source's `BackfillSafe` flag. See
[`README.md`](README.md) for the full procedure.

## Status

**Skeleton (2026-05-24)**, since approved (2026-05-26, 2026-07-09
addendum). Decoder + wiring landed in commit `46e0087e8` with registry `BackfillSafe: false`; the walk was gated on r1's
verify-archive bootstrap (see README.md §2 "Where to run wasm-history").

Rozo is an intent-bridge: users invoke `pay(from, amount, memo)`
on a v1 Payment contract; an off-chain relayer fulfils it on the
destination chain. Scope is v1 Payment only (the only mainnet-live surface at
2026-05-20); v2 Forwarder / IntentBridge are pre-mainnet, deferred per
`internal/sources/rozo/events.go`. The source is `ClassBridge` with `DefaultWeight: 0` and
`IncludeInVWAP: false` in
`internal/sources/external/registry.go`; `BackfillSafe` gates
the operator-triggered backfill path only; aggregator output is unaffected.

## Source identity

| field | value |
| --- | --- |
| Source name (registry key) | `rozo` |
| Registry class | `ClassBridge` |
| Decoder file | [`internal/sources/rozo/decode.go`](../../../internal/sources/rozo/decode.go) |
| Dispatcher hook | event-based `Decoder` (topic[0] classify; one of two `Event*` symbols) |
| Package README | [`internal/sources/rozo/README.md`](../../../internal/sources/rozo/README.md) |
| Wiring commit | `46e0087e8` |

## Mainnet contracts

Verbatim from
[`internal/sources/rozo/events.go`](../../../internal/sources/rozo/events.go)
`MainnetPaymentContracts` (confirmed by RozoAI 2026-05-21 — all
three emit the same `PaymentEvent` / `FlushEvent` schemas):

| # | role | contract address |
| --- | --- | --- |
| 1 | v1 Payment (original deployment, `MainnetPaymentContract`) | `CAC5SKP5FJT2ZZ7YLV4UCOM6Z5SQCCVPZWHLLLVQNQG2RWWOOSP3IYRL` |
| 2 | v1 Payment (additional bridge-out C wallet)               | `CCRLTS3CMJHYHFD7MYRBJPNW6R3LCXNDO2B6TK6AS6FSXAHR6GBMGLRE` |
| 3 | v1 Payment (additional bridge-out C wallet)               | `CAQPKW5AUPEA4C7OERZRUCBWT5RZDSETO4PR5REVRC5MT4CF3PBSKXQC` |

**Out of audit scope** — `MainnetRelayerAccounts`
(`GADDIYCVR2Z6H46YWZE53LICP56ZBNEUUT2QAG4QHSWVIYE44HS7W3XY`,
`GB4CLV3UMXDPFP5OQJQKUCWPRJXPXPJSHTUKZEJLAIZFZR7UHYAQ6EB4`) are
classic G-strkey accounts: no WASM, no Soroban events (tracked in
the `MainnetRelayerAccounts` block in `events.go`).

The decoder matches `payment` / `flush` by `topic[0]`, so
extending `MainnetPaymentContracts` is a watchlist concern, not a decoder-shape change;
each new contract must still join the wasm-history walk's `-contracts` list.

## Decoder expectations

Captured from `internal/sources/rozo/{events,decode}.go` at HEAD
on 2026-05-24. Two canonical events matched on `topic[0]` via
pre-encoded `ScSymbol` constants (`symbol_short!` form — both
event names are ≤ 9 chars).

| event constant | topic[0] symbol | wire shape |
| --- | --- | --- |
| `EventPayment` | `"payment"` | 2-element topic + `ScMap` body |
| `EventFlush`   | `"flush"`   | 1-element topic + `ScMap` body |

### Topic + body details

Per the schemas pinned in `events.go` (extracted from
`v1/stellar/payment/src/lib.rs` in
`github.com/RozoAI/rozo-intents-contracts`):

- **`payment`** — user-initiated bridge-out via
  `pay(from, amount, memo)`.
  `topics = (symbol_short!("payment"), from: Address)`;
  body `ScMap` with the `PaymentEvent` struct fields:

  ```text
  pub struct PaymentEvent {
      pub from:        Address,
      pub destination: Address,
      pub amount:      i128,
      pub memo:        String,
  }
  ```

  USDC is the only token v1 handles (hardcoded `USDC_CONTRACT` at init; `pay` transfers via the USDC token client);
  no `token` field on the v1 event (v2 will add one).
- **`flush`** — admin sweep of accidentally-sent non-USDC
  balances via `flush(token)`.
  `topics = (symbol_short!("flush"),)` (1-element);
  body `ScMap` with the `FlushEvent` struct fields:

  ```text
  pub struct FlushEvent {
      pub token:       Address,
      pub destination: Address,
      pub amount:      i128,
  }
  ```

### Invariants

- `from` (topic[1] on `payment`) duplicates the body's `from`.
- `amount` is i128 carried as decimal string per ADR-0003.
- `memo` is a free-form Soroban `String` (e.g. exchange deposit tag or order ID); no stated length cap.

## WASM timeline

**Walked 2026-05-26** — `stellarindex-ops wasm-history` over
`[60000000, 62642779]` with `-parallel 4` covering all 3 mainnet
`MainnetPaymentContracts` (shared walk with the CCTP audit).
Walk duration: 5h02m. Result: **zero WASM upgrades observed for
any of the 3 contracts** — output JSON shows `ranges: null` per
contract, consistent with stellar.expert reporting a single
shared WASM hash `b56aedeaf80c3d4b…` since each contract's
deploy.

| Contract | Deploy ledger | Deploy timestamp | Upgrades observed |
| --- | --- | --- | --- |
| `CAC5SKP5…OOSP3IYRL` | one-time | 2026-01-18 16:40:53 UTC | 0 |
| `CCRLTS3C…6GBMGLRE`  | one-time | 2026-01-18 16:40:31 UTC | 0 |
| `CAQPKW5A…F3PBSKXQC` | one-time | 2026-03-24 04:51:10 UTC | 0 |

All three point to the **same WASM bytes** (stellar.expert: `b56aedeaf80c3d4b7c4c2ddf3893ac47c3ecff1a0a6f19152ca993e5bb294414`);
one shared template per contract address, so one WASM is validated.

The earliest two contracts deployed (2026-01-18) before the walk's
`-from 60000000` ≈ ledger ~60M (~2026-02-25). Pre-walk history
is trusted because (a) stellar.expert reports zero
contract-bytes change since deploy and (b) Rozo confirmed in `internal/sources/rozo/events.go`
("Confirmed by RozoAI 2026-05-21 — all three emit the same
PaymentEvent / FlushEvent schemas") a single-version wire surface.

Walk evidence: `/tmp/wasm-history-bridges.json` on r1 (shared with the CCTP audit).

## Per-WASM decoder review

One distinct WASM hash `b56aedeaf80c3d4b…` across all 3 contracts.
Events declared in `internal/sources/rozo/events.go`:

- **`payment`** (topic[0]) — bridge-out send. Body parsed in
  `internal/sources/rozo/decode.go::decodePayment`.
- **`flush`** (topic[0]) — relayer reconciliation. Body parsed in
  `internal/sources/rozo/decode.go::decodeFlush`.

`classify()` matches both. i128 amounts preserved end-to-end (NUMERIC in
postgres, `*big.Int` in Go per ADR-0003).

## Hubble cross-check

Hubble does not index bridge events; cross-check via Circle /
Rozo public stats once live traffic exists. No trades, so no VWAP cross-check:
the WASM-bytes audit is the load-bearing check (README.md §4).

## Audit decision

**APPROVED 2026-05-26.** `Registry["rozo"].BackfillSafe` flipped
to `true` in `internal/sources/external/registry.go` in the same
commit as this audit doc update. Single shared hash, no
upgrades, decoder coverage verified. Historical replay via the
`soroban_events` landing zone (ADR-0029) unblocked:

```sql
INSERT INTO rozo_events
SELECT … FROM soroban_events
WHERE contract_id IN (
  'CAC5SKP5FJT2ZZ7YLV4UCOM6Z5SQCCVPZWHLLLVQNQG2RWWOOSP3IYRL',
  'CCRLTS3CMJHYHFD7MYRBJPNW6R3LCXNDO2B6TK6AS6FSXAHR6GBMGLRE',
  'CAQPKW5AUPEA4C7OERZRUCBWT5RZDSETO4PR5REVRC5MT4CF3PBSKXQC',
  'CAFO6OUZAL62SGDVGHHJPSCOOF3HUKXLED3C3FS5RRQI2VBZ4F5HBPXI'
) AND topic_0_sym IN ('payment_event', 'flush_event', 'payment', 'flush');
```

Per the 2026-07-09 addendum: 4th contract (`CAFO6OUZ…`) and corrected on-wire symbols (`payment_event` /
`flush_event`; the short forms `payment` / `flush` are never emitted, kept for forward-safety per
`events.go`).

Re-audit triggers: stellar.expert reports new WASM hash for any
of the 4 payment contracts, OR a new Rozo deploy beyond
`MainnetPaymentContracts`.

## Live-traffic verification notes

Rozo v1 on Stellar is brand-new ("short/no historical backfill"); little-to-no
mainnet bridge traffic at audit time. Live-traffic verification deferred until
real usage starts.

`ClassBridge` with `DefaultWeight: 0` and
`IncludeInVWAP: false` in
[`internal/sources/external/registry.go`](../../../internal/sources/external/registry.go):
no VWAP contribution regardless of `BackfillSafe`, which gates the operator-triggered
`stellarindex-ops backfill --source=rozo` path only.

## 2026-07-09 addendum — 4th contract admitted + topic-shape correction

Two findings from the §0.7 recognition-audit sweep, read-only
against the r1 ClickHouse lake (HTTP 8123; no MinIO / port 9000
access, no wasm-history walk run):

**Topic shape correction.** "Decoder expectations" above (2026-05-24,
pre-dating the 2026-07-07 discovery in `events.go`) describes the 2-tuple topic
`(symbol_short!("payment"), from: Address)`; that is superseded. Deployed contracts emit
`payment_event`/`flush_event`, and 3/3 real
lake fixtures (ledgers 61859684, 63147040, 61797898, 2026-07-09)
show every `payment_event` has `topic_count=1` — a single Symbol, no
`from` topic element; `from` is body-only. The corrected wire shape lives in `events.go`'s
`Payment` doc comment; the section above is left as the historical expectation.

**4th contract admitted.** `CAFO6OUZAL62SGDVGHHJPSCOOF3HUKXLED3C3FS5RRQI2VBZ4F5HBPXI`
emitted exactly one `payment_event` (ledger 61522543), not on
`MainnetPaymentContracts`. Investigated read-only:

- Its contract-instance entry (ledger 61522475, ~68 ledgers before
  the payment) resolves to WASM hash
  `b56aedeaf80c3d4b7c4c2ddf3893ac47c3ecff1a0a6f19152ca993e5bb294414`
  — bytewise IDENTICAL to the hash this audit already covers.
- Instance storage: `{ dest: Address, usdc: Address, init: bool }` —
  the same 3-key init shape as the three audited contracts. `dest` =
  `GB4CLV3UMXDPFP5OQJQKUCWPRJXPXPJSHTUKZEJLAIZFZR7UHYAQ6EB4` (the second `MainnetRelayerAccounts` entry). `usdc` =
  `CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75`, the
  canonical Circle USDC SAC.
- The payment's `destination` resolves to the same relayer as `dest`.
  Memo: "test payment 0.01 USDC" — a deploy smoke-test, not
  an attack (an impersonator would route to an attacker address).
- Two OTHER contracts collided on the legacy `payment` short-form symbol
  (`CDSXS5GK…`, 33 events; `CCP6WOKM…`, 5 events); unrelated body schemas
  (merchant/royalty fields, `admin`/`feebps`/`token` init storage), correctly NOT admitted
  (the topic-collision risk `Classify`'s doc comment warns about).

The 4th contract's hash is bytewise identical to the audited hash, so the
"new Rozo deploy beyond MainnetPaymentContracts" re-audit trigger
resolves by construction, with no new walk. `Registry
["rozo"].BackfillSafe` stays `true`; `MainnetPaymentContracts` in
`internal/sources/rozo/events.go` now lists 4 entries. This contract
has NOT been confirmed by RozoAI (unlike the original three) — flag
for operator follow-up if RozoAI disputes it.

## References

- Procedure: [`README.md`](README.md)
- Decoder source: [`internal/sources/rozo/{events,decode}.go`](../../../internal/sources/rozo/)
- Source-package README: [`internal/sources/rozo/README.md`](../../../internal/sources/rozo/README.md)
- Architecture: [`docs/architecture/rozo-stellar-coverage.md`](../../architecture/rozo-stellar-coverage.md)
- Schema-evolution stance: [`docs/architecture/ingest-pipeline.md#contract-schema-evolution`](../../architecture/ingest-pipeline.md#contract-schema-evolution)
- Backfill gate: `internal/sources/external/registry.go` — `Registry["rozo"].BackfillSafe`
- Upstream contracts: <https://github.com/RozoAI/rozo-intents-contracts>
