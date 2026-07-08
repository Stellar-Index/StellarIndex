# `internal/sources/cctp`

Decoder for **Circle CCTP v2** on Stellar (Soroban).

## Scope

Three on-chain contracts (verified mainnet 2026-05-20):

| Contract | Address |
|---|---|
| TokenMessengerMinter (v2) | `CAE2G5Z77UP7GYPYGFOWFGW7C7J6I4YP2AFGSADRKQY62SYUFLPNFTXL` |
| MessageTransmitter (v2) | `CACMENFFJPJMSDAJQLX4R7K3SFZIW2LJSE3R2UMLGSWHFHS353FVXAZV` |
| CctpForwarder | `CBZL2IH7F6BIDAA3WBNXYKIXSATJGMSW7K5P5MJ6STX5RXN47TZJDF5T` |

Transfer-flow events:

- **`deposit_for_burn`** (TokenMessengerMinter) — outbound USDC
  transfer. Topics include `burn_token`, `depositor`,
  `min_finality_threshold`.
- **`mint_and_withdraw`** (TokenMessengerMinter) — inbound mint
  after attestation. Topics include `mint_recipient`,
  `mint_token`.
- **`message_sent`** (MessageTransmitter) — wire envelope; paired
  with `deposit_for_burn` in the same tx.
- **`message_received`** (MessageTransmitter) — wire envelope;
  paired with `mint_and_withdraw` in the same tx.
- **`mint_and_forward`** (CctpForwarder) — inbound mint relayed
  onward to the final recipient. Body map `{amount: i128,
  forward_recipient: Address, token: Address}`.

Governance/admin events (emitted by all three contracts; verified
against real mainnet lake events 2026-07-08, ROADMAP #89b decoder
topic-match audit):

- **`ownership_transfer`** — 2-step ownership transfer initiated.
  Body `{live_until_ledger: u32, new_owner: Address, old_owner:
  Address}`.
- **`ownership_transfer_completed`** — the new owner accepted a
  pending transfer. Body `{new_owner: Address}`.
- **`admin_changed`** — the operational admin role was reassigned.
  Body `{new_admin: Address, old_admin: Address | Void}` —
  `old_admin` is `Void` on the bootstrap instance of this event
  (no previous admin); the decoder type-tests it rather than
  assuming an Address.
- **`remote_token_messenger_added`** (TokenMessengerMinter only) —
  a counterpart TokenMessenger registered on a remote CCTP domain.
  Body `{domain: u32, token_messenger: BytesN<32>}`.
- **`token_pair_linked`** (TokenMessengerMinter only) — a local
  Stellar token linked to its counterpart token on a remote
  domain. Body `{local_token: Address, remote_domain: u32,
  remote_token: BytesN<32>}`.

Stellar's CCTP domain ID is `27`. Other notable CCTP domains
(referenced by `destination_domain` / `source_domain` fields):
Ethereum=0, Avalanche=1, Arbitrum=3, Solana=7.

## What this package emits

Ten canonical Go types, one per event kind — transfer-flow:
`DepositForBurn`, `MintAndWithdraw`, `MessageSent`,
`MessageReceived`, `MintAndForward`; governance/admin:
`OwnershipTransfer`, `OwnershipTransferCompleted`, `AdminChanged`,
`RemoteTokenMessengerAdded`, `TokenPairLinked` — corresponding 1:1
to the `#[contractevent]` types in
[`circlefin/stellar-cctp/contracts/{token-messenger-minter-v2,message-transmitter-v2}/src/lib.rs`](https://github.com/circlefin/stellar-cctp).
All ten project into the same `cctp_events` row shape (`Event` in
`consumer.go`), discriminated by `event_type`.

BytesN<32> fields (`mint_recipient`, `destination_token_messenger`,
`destination_caller`, `nonce`, `sender`, etc.) are emitted as
lowercase hex (no `0x` prefix). The decoder doesn't try to
re-format for the destination chain's address shape — that's a
downstream concern (EVM destinations would drop the leading 12
zero-bytes; Solana keeps the full 32).

i128 amounts (`amount`, `max_fee`, `fee_collected`) round-trip
through `*big.Int` per ADR-0003 and are emitted as decimal
strings.

## Pairing semantics

A single `deposit_for_burn` call emits **both** a `DepositForBurn`
event (TokenMessengerMinter) **and** a `MessageSent` event
(MessageTransmitter) in the same transaction. Same for inbound
(`MessageReceived` + `MintAndWithdraw`).

A future consumer can correlate the pair by `(ledger, tx_hash)`
and surface them as one logical "outbound USDC transfer" record.
The decoder doesn't do the pairing — that's a sink-layer
concern.

## Wiring

This package is **wired into the ingest pipeline** (#40):

- `dispatcher_adapter.go` — `Decoder`, a stateless topic Decoder
  gated on the three known CCTP contracts (`Matches` checks
  topic[0] **and** `IsCCTPContract`).
- `consumer.go` — the `cctp.Event` `consumer.Event`, plus the
  projections from each `Decode*` struct into the `cctp_events`
  row shape. The decoder does **not** pair `DepositForBurn` with
  `MessageSent`; each event is its own row, correlatable later by
  `(ledger, tx_hash)`.
- `internal/pipeline/dispatcher.go` — `BuildDispatcher` registers
  `cctp.NewDecoder()` when `"cctp"` is in `ingestion.enabled_sources`.
- `internal/pipeline/sink.go` — `persistCCTPEvent` writes each
  event via `Store.InsertCCTPEvent` and bumps the entry counter.
- Storage: `cctp_events` hypertable, migration
  [`0038_create_cctp_events`](../../../migrations/0038_create_cctp_events.up.sql).
- Registry: `internal/sources/external/registry.go` —
  `Class: ClassBridge, IncludeInVWAP: false, DefaultWeight: 0,
  BackfillAvailable: true, BackfillSafe: false`.

**Operator steps to turn it on:**

1. Apply migration 0038 (`stellarindex-migrate up` after the SCP —
   migrations are not auto-deployed).
2. Add `"cctp"` to `ingestion.enabled_sources` in the region TOML.
3. `BackfillSafe` stays `false` until a WASM-history audit lands
   at `docs/operations/wasm-audits/cctp.md`. The contracts are
   brand new (a single WASM hash is expected) but the audit is
   required program work before `stellarindex-ops backfill` will
   run CCTP against historical ranges. Live ingest works without
   it — per the user's direction CCTP needs little/no history.

## Tests

`decode_test.go` — transfer-flow decode coverage:

- `Classify` — all four transfer-flow event types + unknown-symbol +
  empty-topic.
- `DecodeDepositForBurn` — happy path (covers BytesN<32> roundtrip
  for `mint_recipient` / `destination_token_messenger` /
  `destination_caller` and `hook_data`); ADR-0003 large-i128
  guard (`> 2^99`); short-topic surfaces `ErrMalformedTopic`;
  missing body field surfaces `ErrMalformedBody`.
- `DecodeMintAndWithdraw` — happy path + short-topic.
- `DecodeMessageSent` — Map-body path (`MessageSent { message:
  Bytes }` ScMap form) AND raw-Bytes fallback (forward-compat
  guard if the Soroban macro shifts).
- `DecodeMessageReceived` — happy path + short-topic.
- Topic-symbol encoding stability — re-encoded bytes match
  package-init constants. Drift here would silently break
  `Classify`.
- `ErrUnknownEvent` sentinel availability for downstream consumers.

`source_test.go` — `DecodeMintAndForward` real-mainnet golden fixture
(ledger 63098002).

`governance_test.go` — the 5 governance/admin events, each with a
real-mainnet golden fixture (ledger + tx_hash cited in the test) plus
a malformed-body reject test:

- `DecodeOwnershipTransfer` / `DecodeOwnershipTransferCompleted` —
  the 2-step transfer pair, ledgers 62211157/62146641.
- `DecodeAdminChanged` — BOTH shapes observed on mainnet: the
  bootstrap instance (`old_admin = Void`, ledger 62146641) and the
  later real reassignment (`old_admin` populated, ledger 62225106) —
  the schema-evolution trap this decoder type-tests against.
- `DecodeRemoteTokenMessengerAdded` / `DecodeTokenPairLinked` —
  TokenMessengerMinter-only, ledgers 62146653/62146739; the
  `token_pair_linked` fixture's `remote_token` decodes to Ethereum
  USDC's real contract address as a cross-check.
- `TestDecoder_Decode_GovernanceEvents` — end-to-end `Matches` +
  `Decode` through `dispatcher_adapter.go` for all 5 topics.

`dispatcher_adapter_test.go` — `Decoder.Matches` / `Decode` for the
transfer-flow events, including the CS-026-style "same topic bytes,
foreign contract" rejection test.

## References

- Architecture doc: [`docs/architecture/cctp-stellar-coverage.md`](../../../docs/architecture/cctp-stellar-coverage.md)
- Upstream source: https://github.com/circlefin/stellar-cctp
- Circle developer docs: https://developers.circle.com/cctp/references/stellar-contracts
- Class taxonomy: `internal/sources/external/framework.go` `ClassBridge`
- Sister bridge: `internal/sources/rozo/` (Rozo v1 Payment)
