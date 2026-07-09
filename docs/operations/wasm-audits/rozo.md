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

**Skeleton (2026-05-24).** Source decoder + wiring landed in #41
(commit `1170cd99`); registry entry sits at `BackfillSafe: false`
pending the wasm-history walk. The walk itself is gated on r1's
verify-archive bootstrap finishing (ZFS-ARC + MinIO I/O
contention — see README.md §2 "Where to run wasm-history") so
this doc captures the per-contract / per-event expectations now,
and the operator fills in the timeline + per-hash review
sections once the walk lands.

Rozo is an intent-bridge — users invoke `pay(from, amount, memo)`
on a v1 Payment contract; the protocol's off-chain relayer
fulfils the intent on the destination chain. Audit scope here is
v1 Payment only (the only mainnet-live Rozo surface at
2026-05-20); v2 Forwarder / IntentBridge are pre-mainnet and
deferred per `internal/sources/rozo/events.go`'s package comment.
The source is `ClassBridge` with `DefaultWeight: 0` and
`IncludeInVWAP: false` in
`internal/sources/external/registry.go` — `BackfillSafe` gates
the operator-triggered backfill path only; aggregator output is
unaffected either way.

## Source identity

| field | value |
| --- | --- |
| Source name (registry key) | `rozo` |
| Registry class | `ClassBridge` |
| Decoder file | [`internal/sources/rozo/decode.go`](../../../internal/sources/rozo/decode.go) |
| Dispatcher hook | event-based `Decoder` (topic[0] classify; one of two `Event*` symbols) |
| Package README | [`internal/sources/rozo/README.md`](../../../internal/sources/rozo/README.md) |
| Wiring PR | #41 (commit `1170cd99`) |

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
classic G-strkey accounts, not contracts; they don't run WASM
and emit no Soroban events. They appear on classic `payment`
operations as source/destination and are tracked separately (see
the `MainnetRelayerAccounts` block in `events.go`).

The decoder matches `payment` / `flush` by `topic[0]`, so
extending `MainnetPaymentContracts` is a watchlist concern
(cross-validation + scoping), not a decoder-shape change — but
each new contract still needs to land in the wasm-history walk's
`-contracts` list to be covered by this audit.

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

  USDC is the only token v1 handles — the contract hardcodes
  `USDC_CONTRACT` at init and `pay` transfers via the USDC token
  client. No `token` field on the v1 event (v2 will add one when
  it lands).
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

- `from` (topic[1] on `payment`) duplicates the `from` field
  inside the body — they are the same address by construction.
- `amount` is i128 carried as decimal string per ADR-0003.
- `memo` is free-form Soroban `String` (often a Binance /
  Coinbase deposit address tag or a merchant order ID); no hard
  length cap stated by the contract.

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

All three deploys point to the **same WASM bytes** (per
stellar.expert: `b56aedeaf80c3d4b7c4c2ddf3893ac47c3ecff1a0a6f19152ca993e5bb294414`).
Rozo's pattern is one shared template per-contract-address, not
factory-instanced parameterisation — so the decoder validates
against a single WASM regardless of which payment contract
emitted the event.

The earliest two contracts deployed (2026-01-18) before the walk's
`-from 60000000` ≈ ledger ~60M (~2026-02-25). The pre-walk history
is implicitly trusted because (a) stellar.expert reports zero
contract-bytes change since deploy and (b) Rozo themselves
confirmed per `internal/sources/rozo/events.go` comment
("Confirmed by RozoAI 2026-05-21 — all three emit the same
PaymentEvent / FlushEvent schemas") that the wire surface is
single-version.

Walk evidence: `/tmp/wasm-history-bridges.json` on r1 (shared with
the CCTP audit — same walk targeted both source sets).

## Per-WASM decoder review

One distinct WASM hash `b56aedeaf80c3d4b…` across all 3 contracts.
Events declared in `internal/sources/rozo/events.go`:

- **`payment`** (topic[0]) — bridge-out send. Body parsed in
  `internal/sources/rozo/decode.go::decodePayment`.
- **`flush`** (topic[0]) — relayer reconciliation. Body parsed in
  `internal/sources/rozo/decode.go::decodeFlush`.

`classify()` matches both, decoder handles both. No WASM upgrades
to drift through. i128 amounts preserved end-to-end (NUMERIC in
postgres, `*big.Int` in Go per ADR-0003).

## Hubble cross-check

Hubble does not index bridge events; cross-check via Circle /
Rozo public stats once live mainnet traffic exists. Bridges emit
no trades — no VWAP cross-check available either, so the
WASM-bytes audit is the load-bearing safety check (per
README.md §4).

## Audit decision

**APPROVED 2026-05-26.** `Registry["rozo"].BackfillSafe` flipped
to `true` in `internal/sources/external/registry.go` in the same
commit as this audit doc update. Single shared WASM hash, no
upgrades, decoder coverage verified. Historical replay via the
`soroban_events` landing zone (ADR-0029) is now unblocked:

```sql
INSERT INTO rozo_events
SELECT … FROM soroban_events
WHERE contract_id IN (
  'CAC5SKP5FJT2ZZ7YLV4UCOM6Z5SQCCVPZWHLLLVQNQG2RWWOOSP3IYRL',
  'CCRLTS3CMJHYHFD7MYRBJPNW6R3LCXNDO2B6TK6AS6FSXAHR6GBMGLRE',
  'CAQPKW5AUPEA4C7OERZRUCBWT5RZDSETO4PR5REVRC5MT4CF3PBSKXQC'
) AND topic_0_sym IN ('payment', 'flush');
```

Re-audit triggers: stellar.expert reports new WASM hash for any
of the 3 payment contracts, OR a new Rozo deploy beyond
`MainnetPaymentContracts`.

## Live-traffic verification notes

Rozo v1 on Stellar is brand-new (per the
`project_protocol_coverage_additions` memory note —
"brand-new on Stellar so short/no historical backfill"); there
is little-to-no on-mainnet bridge traffic to verify against at
audit time. On-mainnet live-traffic verification deferred until
real bridge usage starts.

Because Rozo is `ClassBridge` with `DefaultWeight: 0` and
`IncludeInVWAP: false` in
[`internal/sources/external/registry.go`](../../../internal/sources/external/registry.go),
the source contributes nothing to VWAP regardless of the
`BackfillSafe` flag. The flag gates the operator-triggered
`stellarindex-ops backfill --source=rozo` path only.

## 2026-07-09 addendum — 4th contract admitted + topic-shape correction

Two findings from the §0.7 recognition-audit sweep, both read-only
against the r1 ClickHouse lake (HTTP 8123; no MinIO / port 9000
access, no wasm-history walk run):

**Topic shape correction.** This doc's "Decoder expectations" section
above (written 2026-05-24, pre-dating the 2026-07-07 discovery
recorded in `events.go`) still describes the upstream source's
apparent 2-tuple topic `(symbol_short!("payment"), from: Address)`.
That was already known stale for the SYMBOL (deployed contracts emit
`payment_event`/`flush_event`, not the short form) but the TOPIC
ARITY claim was never re-verified until now. Checked against 3/3 real
lake fixtures (ledgers 61859684, 63147040, 61797898, 2026-07-09):
every `payment_event` has `topic_count=1` — a single Symbol, no
`from` topic element. `from` is body-only. See `events.go`'s
`Payment` doc comment for the corrected wire shape; this doc's
"Decoder expectations" section is left as the historical (now
superseded) pre-walk expectation rather than rewritten, per
docs/engineering-standards.md's "explain why, not what" — the
correction lives where the code decides, in `events.go`.

**4th contract admitted.** `CAFO6OUZAL62SGDVGHHJPSCOOF3HUKXLED3C3FS5RRQI2VBZ4F5HBPXI`
emitted exactly one `payment_event` (ledger 61522543) and was not on
`MainnetPaymentContracts`. Investigated read-only:

- Its contract-instance entry (ledger 61522475, ~68 ledgers before
  the payment) resolves to WASM hash
  `b56aedeaf80c3d4b7c4c2ddf3893ac47c3ecff1a0a6f19152ca993e5bb294414`
  — bytewise IDENTICAL to the hash this audit already covers.
- Instance storage: `{ dest: Address, usdc: Address, init: bool }` —
  the same 3-key init shape as the three audited contracts. `dest` =
  `GB4CLV3UMXDPFP5OQJQKUCWPRJXPXPJSHTUKZEJLAIZFZR7UHYAQ6EB4`, an exact
  match to the second `MainnetRelayerAccounts` entry. `usdc` =
  `CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75`, the
  canonical Circle USDC SAC.
- The one `payment_event`'s `destination` field independently
  resolves to the same relayer account as `dest` above. Memo: "test
  payment 0.01 USDC" — consistent with a deploy-then-smoke-test, not
  an attack (an impersonating contract would route to an attacker
  address, not the real documented relayer).
- Two OTHER contracts turned up in the same lake sweep colliding on
  the legacy `payment` short-form symbol (`CDSXS5GK…`, 33 events;
  `CCP6WOKM…`, 5 events) — both have unrelated body schemas
  (merchant/royalty fields, `admin`/`feebps`/`token` init storage) and
  are correctly NOT admitted. Real-world confirmation of the
  topic-collision risk `Classify`'s doc comment warns about.

Because the 4th contract's WASM hash is bytewise identical to the
already-audited hash, this is the "new Rozo deploy beyond
MainnetPaymentContracts" re-audit trigger this doc names below — and
the trigger resolves by construction (same hash = same audited
decoder behavior), not by re-running a wasm-history walk. `Registry
["rozo"].BackfillSafe` stays `true`; `MainnetPaymentContracts` in
`internal/sources/rozo/events.go` now lists 4 entries. This contract
has NOT been confirmed by RozoAI (unlike the original three) — flag
for operator follow-up if RozoAI disputes it.

## References

- Procedure: [`README.md`](README.md)
- Decoder source: [`internal/sources/rozo/{events,decode}.go`](../../../internal/sources/rozo/)
- Source-package README: [`internal/sources/rozo/README.md`](../../../internal/sources/rozo/README.md)
- Architecture: [`docs/architecture/rozo-stellar-coverage.md`](../../architecture/rozo-stellar-coverage.md)
- Schema-evolution stance: [`docs/architecture/contract-schema-evolution.md`](../../architecture/contract-schema-evolution.md)
- Backfill gate: `internal/sources/external/registry.go` — `Registry["rozo"].BackfillSafe`
- Upstream contracts: <https://github.com/RozoAI/rozo-intents-contracts>
