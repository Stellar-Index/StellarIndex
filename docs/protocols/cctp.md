---
title: CCTP (Circle) — contract & event verification
last_verified: 2026-07-09
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

## Events (26)

Full topic census, ROADMAP #89c (2026-07-09): every `topic_0_sym` the
three CCTP contracts have EVER emitted on mainnet, cross-checked
against `topics_xdr` for the empty-`topic_0_sym` trap (none found —
every CCTP topic is a `Symbol`, none use `ScvString`). 26 distinct
topics, 9,496 total events, exactly reconciled against a plain
`count()` over the same three contract IDs. **Complete — all 26 are
classified and decoded; no known gap remains.**

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
| `admin_change_started` | all three contracts | **2026-07-09** — ROADMAP #89c full census. 2-step admin change initiated (the counterpart to `admin_changed`); body `{new_admin: Address, old_admin: Address \| Void}`. Real fixture: ledger 62211158. |
| `attester_enabled` | MessageTransmitter only | **2026-07-09**. Attester public key enabled; topics `["attester_enabled", attester: BytesN<20>]`, empty body. Real fixture: ledger 62146641. |
| `attester_manager_updated` | MessageTransmitter only | **2026-07-09**. Attester-manager role reassigned; topics `["attester_manager_updated", old_attester_manager: Address \| Void, new_attester_manager: Address]`, empty body — `Void` observed at bootstrap (a TOPIC-field instance of the same trap `admin_changed` guards in its body). Real fixture: ledger 62146641. |
| `denylisted` | TokenMessengerMinter only | **2026-07-09**. Account added to the denylist; topics `["denylisted", account: Address]`, empty body. Real fixture: ledger 62226112. |
| `un_denylisted` | TokenMessengerMinter only | **2026-07-09**. Account removed from the denylist (same account as the `denylisted` fixture); topics `["un_denylisted", account: Address]`, empty body. Real fixture: ledger 62226574. |
| `denylister_changed` | TokenMessengerMinter only | **2026-07-09**. Denylister role reassigned; topics `["denylister_changed", old_denylister: Address \| Void, new_denylister: Address]`, empty body — `Void` observed at bootstrap. Real fixture: ledger 62146653. |
| `fee_recipient_set` | TokenMessengerMinter only | **2026-07-09**. Fee-recipient address changed; body `{fee_recipient: Address}`. Real fixture: ledger 62146653. |
| `min_fee_controller_set` | TokenMessengerMinter only | **2026-07-09**. Min-fee-controller role reassigned; topics `["min_fee_controller_set", min_fee_controller: Address]`, empty body. Real fixture: ledger 62146653. |
| `pauser_changed` | all three contracts | **2026-07-09**. Pause-role address reassigned; body `{new_address: Address}` — NOTE the field is named `new_address`, not `new_pauser`. Real fixture: ledger 62146641. |
| `rescuer_changed` | all three contracts | **2026-07-09**. Rescue-role address reassigned; body `{new_rescuer: Address}`. Real fixture: ledger 62146641. |
| `set_token_controller` | TokenMessengerMinter only | **2026-07-09**. Token-controller role reassigned; body `{token_controller: Address}`. Real fixture: ledger 62146653. |
| `signature_threshold_updated` | MessageTransmitter only | **2026-07-09**. Required attestation signature count changed; body `{new_signature_threshold: u32, old_signature_threshold: u32}`. Real fixture: ledger 62146641 (0 → 2). |
| `max_message_body_size_updated` | MessageTransmitter only | **2026-07-09** (previously design-documented in `docs/architecture/cctp-stellar-coverage.md`, now verified against a real event). Message size ceiling changed; body `{new_max_message_body_size: u32}`. Real fixture: ledger 62146641 (8192 bytes). |
| `set_burn_limit_per_message` | TokenMessengerMinter only | **2026-07-09**. Per-message burn ceiling set for a local token; topics `["set_burn_limit_per_message", token: Address]`, body `{burn_limit_per_message: i128}`. `token` promotes to `Event.Token`; the limit is a policy ceiling and does NOT promote to `Event.Amount`. Real fixture: ledger 62146712. |
| `swap_minter_config_set` | TokenMessengerMinter only | **2026-07-09**. Swap-minter config set for a local token; topics `["swap_minter_config_set", token: Address]`, body `{swap_minter_config: {allow_asset: Address, swap_minter: Address}}` — a NESTED map, flattened by the decoder. Real fixture: ledger 62146806. |
| `token_decimal_config_added` | TokenMessengerMinter only | **2026-07-09**. Canonical/local decimal mapping added for a local token; topics `["token_decimal_config_added", token: Address]`, body `{token_decimal_config: {canonical_decimals: u32, local_decimals: u32}}` — a NESTED map, flattened by the decoder. Real fixture: ledger 62146699 (canonical=6, local=7). |

**Recognition attribution:** the three contracts are pinned in the
ADR-0033 reconciliation catalogue (`contractIDs`), so any FUTURE
unhandled cctp topic caps this source's completeness verdict instead
of disappearing into the system-wide recognition bucket — the second
half of the board-#31 finding.

**Coverage status:** complete as of 2026-07-09 (ROADMAP #89c). Re-run
the same full `topic_0_sym` census periodically (or after any Circle
contract upgrade — CLAUDE.md "Soroban DeFi contracts upgrade in
place") to catch a genuinely new topic; there is no currently-known
gap.
