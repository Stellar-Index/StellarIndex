---
title: CCTP WASM-history audit
last_verified: 2026-05-26
status: "approved — see Audit decision"
source: cctp
backfill_safe: true
---

# CCTP WASM audit

Audit log for the `cctp` source's `BackfillSafe` flag. See
[`README.md`](README.md) for the full procedure.

## Status

**Approved (2026-05-26).** Decoder + wiring landed in commit `1b9a594b4`; the
wasm-history walk (§"WASM timeline") found zero upgrades across all 3 mainnet
contracts, and the registry entry was flipped to `BackfillSafe: true` in the
same commit as the audit decision below.

CCTP is Circle's cross-chain transfer protocol (Stellar is one chain in the v2
deployment). The contracts are bridge infrastructure (mint / burn / wire
envelope) and never emit trades. The source is `ClassBridge` with
`DefaultWeight: 0` and `IncludeInVWAP: false` in
`internal/sources/external/registry.go`; `BackfillSafe` gates only the
operator-triggered backfill path.

## Source identity

| field | value |
| --- | --- |
| Source name (registry key) | `cctp` |
| Registry class | `ClassBridge` |
| Decoder file | [`internal/sources/cctp/decode.go`](../../../internal/sources/cctp/decode.go) |
| Dispatcher hook | event-based `Decoder` (topic[0] classify; one of 26 `Event*` symbols) |
| Package README | [`internal/sources/cctp/README.md`](../../../internal/sources/cctp/README.md) |
| Wiring commit | `1b9a594b4` |

## Mainnet contracts

Verbatim from [`internal/sources/cctp/events.go`](../../../internal/sources/cctp/events.go)
`Mainnet*` constants (verified 2026-05-20 against
<https://developers.circle.com/cctp/references/stellar-contracts>
+ <https://github.com/circlefin/stellar-cctp>):

| role | constant | contract address |
| --- | --- | --- |
| TokenMessengerMinter | `MainnetTokenMessengerMinter` | `CAE2G5Z77UP7GYPYGFOWFGW7C7J6I4YP2AFGSADRKQY62SYUFLPNFTXL` |
| MessageTransmitter   | `MainnetMessageTransmitter`   | `CACMENFFJPJMSDAJQLX4R7K3SFZIW2LJSE3R2UMLGSWHFHS353FVXAZV` |
| CctpForwarder        | `MainnetCctpForwarder`        | `CBZL2IH7F6BIDAA3WBNXYKIXSATJGMSW7K5P5MJ6STX5RXN47TZJDF5T` |

Stellar's CCTP domain ID is `27` (`StellarDomainID` in
`events.go`); other notable CCTP domains: Ethereum=0,
Avalanche=1, Arbitrum=3, Solana=7.

## Decoder expectations

From `internal/sources/cctp/{events,decode}.go` at HEAD 2026-05-24. Four
canonical events matched on `topic[0]` via pre-encoded `ScSymbol` constants (one
string-equal comparison, no full SCVal decode in the hot path).

| event constant | topic[0] symbol | emitting contract | wire shape |
| --- | --- | --- | --- |
| `EventDepositForBurn`  | `"deposit_for_burn"`  | TokenMessengerMinter | 4-element topic + `ScMap` body |
| `EventMintAndWithdraw` | `"mint_and_withdraw"` | TokenMessengerMinter | 3-element topic + `ScMap` body |
| `EventMessageSent`     | `"message_sent"`      | MessageTransmitter   | 1-element topic + raw `Bytes` body |
| `EventMessageReceived` | `"message_received"`  | MessageTransmitter   | 4-element topic + `ScMap` body |

### Topic + body details

Schemas pinned in `events.go` (from
`contracts/{token-messenger-minter-v2,message-transmitter-v2}/src/lib.rs` in
`github.com/circlefin/stellar-cctp`):

- **`deposit_for_burn`** (outbound transfer).
  `topics = ["deposit_for_burn", burn_token, depositor, min_finality_threshold]`;
  body `ScMap { amount, mint_recipient, destination_domain,
  destination_token_messenger, destination_caller, max_fee,
  hook_data }`. `mint_recipient` /
  `destination_token_messenger` / `destination_caller` are `BytesN<32>` (lowercase hex, no `0x`; for an EVM
  destination the trailing 20 bytes are the address, leading 12 zero padding).
- **`mint_and_withdraw`** (inbound mint).
  `topics = ["mint_and_withdraw", mint_recipient, mint_token]`;
  body `ScMap { amount, fee_collected }`.
- **`message_sent`**: wire envelope, emitted alongside `deposit_for_burn`
  (correlate by `(ledger, tx_hash)`). Single-topic; body is raw `Bytes` (the
  serialised envelope, preserved as hex).
- **`message_received`**: wire envelope, emitted alongside `mint_and_withdraw`.
  `topics = ["message_received", caller, nonce, finality_threshold_executed]`;
  body `ScMap { source_domain, sender, message_body }`.

### Correlation invariants

- One outbound `deposit_for_burn` emits BOTH a `DepositForBurn` and a
  `MessageSent` event in the same transaction; inbound likewise
  (`MessageReceived` + `MintAndWithdraw`). Correlate by `(ledger, tx_hash)`.
- All amounts are i128 decimal strings per ADR-0003 (`Amount`, `MaxFee`, `FeeCollected`).
- `CctpForwarder` (`CBZL2IH...`) emits `mint_and_forward` (inbound mint relayed
  onward, decoded by `DecodeMintAndForward` in `internal/sources/cctp/decode.go`)
  plus the shared ownership/admin events. See the per-WASM review below.

## WASM timeline

**Walked 2026-05-26**: `stellarindex-ops wasm-history` over `[60000000, 62642779]`
with `-parallel 4`, all 3 mainnet contracts; 5h02m, 2,642,780 ledgers scanned
across 4 workers. **Zero WASM upgrades for any of the 3**: output JSON has
`ranges: null` per contract, consistent with stellar.expert (all 3 deployed
2026-04-16 within ~3 min, one deploy event each).

| Contract | Deploy ledger | Deploy timestamp | Upgrades observed |
| --- | --- | --- | --- |
| `CAE2G5Z7…UFLPNFTXL` (TokenMessengerMinter) | one-time | 2026-04-16 15:43:48 UTC | 0 |
| `CACMENFF…3FVXAZV` (MessageTransmitter)    | one-time | 2026-04-16 15:43:48 UTC | 0 |
| `CBZL2IH7…N47TZJDF5T` (CctpForwarder)      | one-time | 2026-04-16 15:46:33 UTC | 0 |

Walk evidence: `/tmp/wasm-history-bridges.json` on r1 (kept until the next
bootstrap; copy to `evidence/` for a permanent artefact). Per-worker JSONL
transition logs are empty.

## Per-WASM decoder review

Three distinct WASM hashes (one per contract; different codebases, not
factory-deployed variants):

- **TokenMessengerMinter** `a6c1acc6e367e465…` (32-byte hash from
  stellar.expert). Events: `deposit_for_burn`, `mint_and_withdraw`
  (decoder side: `internal/sources/cctp/decode.go`). Single deploy
  with no upgrade; decoder body-shape assumptions stable.
- **MessageTransmitter** `99bd0ddc506ee13f…`. Events:
  `message_sent`, `message_received`. Same decoder. Single deploy.
- **CctpForwarder**
  `00b1b70550f887bd87835270ddee76307ced7a00aa6ad354ed744bc0544a03ac`
  (the only WASM the contract has run: `stellar.contract_instance_changes`
  FINAL holds that hash alone over ledgers 62,146,669–62,225,207, read
  2026-10-05). It emits `mint_and_forward` (inbound mint relayed onward)
  plus the shared ownership/admin events; the same decoder handles both,
  and the ROADMAP #89b/89c topic-match audits below cover its topics.

Decoder coverage matches the full event set the contracts emit, verified against
the Rust source ([`docs/architecture/cctp-stellar-coverage.md`](../../architecture/cctp-stellar-coverage.md)),
which `internal/sources/cctp/events.go`'s 26 `Event*` constants mirror; the
ROADMAP #89b/89c topic-match audits (2026-07-08/09) cross-checked every
`topic_0_sym` the 3 contracts ever emitted on mainnet against that set and found
none missing. No i128 scale drift: no upgrades to drift through.

Disassembly + per-WASM source comparison deferred until (a) Circle ships a v3
upgrade, or (b) decoded events diverge from Circle's public stats once live
bridge traffic begins.

## Hubble cross-check

Hubble does not index bridge events; cross-check via Circle / Rozo public stats
once live traffic exists. Bridges emit no trades, so no VWAP cross-check either:
the WASM-bytes audit is the load-bearing check (README.md §4).

## Audit decision

**APPROVED 2026-05-26.** `Registry["cctp"].BackfillSafe` flipped to `true` in
`internal/sources/external/registry.go` in the same commit as this doc update.
The decoder covers every WASM hash that has ever existed for the 3 contracts
(one each, no upgrades). Historical replay via the `soroban_events` landing zone
(ADR-0029) is unblocked:

```sql
INSERT INTO cctp_events
SELECT … FROM soroban_events
WHERE contract_id IN (
  'CAE2G5Z77UP7GYPYGFOWFGW7C7J6I4YP2AFGSADRKQY62SYUFLPNFTXL',
  'CACMENFFJPJMSDAJQLX4R7K3SFZIW2LJSE3R2UMLGSWHFHS353FVXAZV',
  'CBZL2IH7F6BIDAA3WBNXYKIXSATJGMSW7K5P5MJ6STX5RXN47TZJDF5T'
) AND topic_0_sym IN (
  -- Transfer-flow (original 2026-05-26 approval scope):
  'deposit_for_burn', 'mint_and_withdraw', 'message_sent',
  'message_received', 'mint_and_forward',
  -- Governance/admin — added to the decoder by the ROADMAP #89b/89c
  -- topic-match audits (2026-07-08/09); covered by this same
  -- single-WASM, no-upgrade finding, so no separate re-walk is
  -- needed, but a replay run BEFORE 2026-07-09 must re-run with
  -- this full list to pick these up:
  'ownership_transfer', 'ownership_transfer_completed',
  'admin_changed', 'admin_change_started',
  'remote_token_messenger_added', 'token_pair_linked',
  'attester_enabled', 'attester_manager_updated',
  'signature_threshold_updated', 'max_message_body_size_updated',
  'pauser_changed', 'rescuer_changed', 'denylisted',
  'un_denylisted', 'denylister_changed', 'fee_recipient_set',
  'min_fee_controller_set', 'set_token_controller',
  'set_burn_limit_per_message', 'swap_minter_config_set',
  'token_decimal_config_added'
);
```

Re-audit triggers: Prometheus alerts on `unknown_topic` per source, or a manual
`wasm-history` re-walk if Circle announces a v3.

## Live-traffic verification notes

CCTP v2 on Stellar is brand-new (per the `project_protocol_coverage_additions`
memory note, "brand-new on Stellar so short/no historical backfill"), so there
is little on-mainnet traffic to verify against; live-traffic verification is
deferred until real bridge usage starts.

As `ClassBridge` with `DefaultWeight: 0` and `IncludeInVWAP: false` in
[`internal/sources/external/registry.go`](../../../internal/sources/external/registry.go),
the source contributes nothing to VWAP regardless of `BackfillSafe`, which gates
only `stellarindex-ops backfill --source=cctp`.

## References

- Procedure: [`README.md`](README.md)
- Decoder source: [`internal/sources/cctp/{events,decode}.go`](../../../internal/sources/cctp/)
- Source-package README: [`internal/sources/cctp/README.md`](../../../internal/sources/cctp/README.md)
- Architecture: [`docs/architecture/cctp-stellar-coverage.md`](../../architecture/cctp-stellar-coverage.md)
- Schema-evolution stance: [`docs/architecture/ingest-pipeline.md#contract-schema-evolution`](../../architecture/ingest-pipeline.md#contract-schema-evolution)
- Backfill gate: `internal/sources/external/registry.go` — `Registry["cctp"].BackfillSafe`
- Upstream contracts: <https://github.com/circlefin/stellar-cctp>
- Circle docs: <https://developers.circle.com/cctp/references/stellar-contracts>
