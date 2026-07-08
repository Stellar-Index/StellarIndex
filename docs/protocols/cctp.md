---
title: CCTP (Circle) — contract & event verification
last_verified: 2026-07-08
status: current
---

# CCTP — contract & event verification

> **For the Circle/CCTP team:** this is the set of CCTP v2 contracts and
> events Stellar Index ingests. Please confirm it is correct and complete.
>
> - **Enumeration method:** Circle's published deployment (single deploy
>   2026-04-16, confirmed via stellar.expert; WASM-audited 2026-05-26 across
>   all three contracts — zero upgrades observed, see
>   docs/operations/wasm-audits/cctp.md).
> - **Gate status:** ✅ Gated (hard-coded contract set — `IsCCTPContract`;
>   ADR-0035 mechanism since integration).

## Contracts (3)

| Role | Contract |
|---|---|
| TokenMessengerMinter | `CAE2G5Z77UP7GYPYGFOWFGW7C7J6I4YP2AFGSADRKQY62SYUFLPNFTXL` |
| MessageTransmitter | `CACMENFFJPJMSDAJQLX4R7K3SFZIW2LJSE3R2UMLGSWHFHS353FVXAZV` |
| CctpForwarder | `CBZL2IH7F6BIDAA3WBNXYKIXSATJGMSW7K5P5MJ6STX5RXN47TZJDF5T` |

(Corrected 2026-07-08 — this table previously had TokenMessengerMinter
and MessageTransmitter's addresses swapped relative to the source of
truth, `internal/sources/cctp/events.go`'s `Mainnet*` constants.)

## Events (10)

| Topic | Emitter | Decoded since |
|---|---|---|
| `deposit_for_burn` | TokenMessengerMinter | integration (2026-05-20) |
| `mint_and_withdraw` | TokenMessengerMinter | integration |
| `message_sent` | MessageTransmitter | integration |
| `message_received` | MessageTransmitter | integration |
| `mint_and_forward` | CctpForwarder | **2026-07-02** — found undecoded in the lake (board #31); schema reverse-engineered from mainnet events (body map: amount i128, forward_recipient Address, token Address). Historical catch-up: `projector-replay -source cctp -from 62403000`. |
| `ownership_transfer` | all three contracts | **2026-07-08** — ROADMAP #89b topic-match audit. 2-step ownership transfer initiated; body `{live_until_ledger: u32, new_owner: Address, old_owner: Address}`. Real fixture: ledger 62211157. |
| `ownership_transfer_completed` | all three contracts | **2026-07-08**. New owner accepted a pending transfer; body `{new_owner: Address}`. Real fixture: ledger 62146641. |
| `admin_changed` | all three contracts | **2026-07-08**. Admin role reassigned; body `{new_admin: Address, old_admin: Address \| Void}` — `old_admin` is `Void` on the bootstrap instance (type-tested, not assumed). Real fixtures: ledger 62146641 (void) and 62225106 (populated). |
| `remote_token_messenger_added` | TokenMessengerMinter only | **2026-07-08**. Remote-domain TokenMessenger registered; body `{domain: u32, token_messenger: BytesN<32>}`. Real fixture: ledger 62146653. |
| `token_pair_linked` | TokenMessengerMinter only | **2026-07-08**. Local↔remote token link registered; body `{local_token: Address, remote_domain: u32, remote_token: BytesN<32>}`. Real fixture: ledger 62146739 (links Stellar USDC to Ethereum USDC). |

**Recognition attribution:** the three contracts are pinned in the
ADR-0033 reconciliation catalogue (`contractIDs`), so any FUTURE
unhandled cctp topic caps this source's completeness verdict instead
of disappearing into the system-wide recognition bucket — the second
half of the board-#31 finding.

**Known gap:** the 2026-07 topic-match audit (ROADMAP #89b) that
surfaced these 5 governance/admin topics also found further
lower-signal admin topics not yet decoded (`admin_change_started`,
`set_burn_limit_per_message`, `rescuer_changed`,
`set_token_controller`, `pauser_changed`, `attester_enabled`,
`token_decimal_config_added`, `attester_manager_updated`,
`fee_recipient_set`, `swap_minter_config_set`, `denylisted`,
`max_message_body_size_updated` — the last already called out in
`docs/architecture/cctp-stellar-coverage.md`, `denylister_changed`,
`signature_threshold_updated`, `un_denylisted`,
`min_fee_controller_set`; 1-4 lake occurrences each as of
2026-07-08). Per CLAUDE.md's "EVERY event for EVERY Soroban
protocol" binding principle these are a completeness gap, not a
closed decision — tracked as follow-up work, out of scope for this
change.
