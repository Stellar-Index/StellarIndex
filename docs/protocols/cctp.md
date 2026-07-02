---
title: CCTP (Circle) — contract & event verification
last_verified: 2026-07-02
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
| TokenMessengerMinter | `CACMENFFJPJMSDAJQLX4R7K3SFZIW2LJSE3R2UMLGSWHFHS353FVXAZV` |
| MessageTransmitter | `CAE2G5Z77UP7GYPYGFOWFGW7C7J6I4YP2AFGSADRKQY62SYUFLPNFTXL` |
| CctpForwarder | `CBZL2IH7F6BIDAA3WBNXYKIXSATJGMSW7K5P5MJ6STX5RXN47TZJDF5T` |

## Events (5)

| Topic | Emitter | Decoded since |
|---|---|---|
| `deposit_for_burn` | TokenMessengerMinter | integration (2026-05-20) |
| `mint_and_withdraw` | TokenMessengerMinter | integration |
| `message_sent` | MessageTransmitter | integration |
| `message_received` | MessageTransmitter | integration |
| `mint_and_forward` | CctpForwarder | **2026-07-02** — found undecoded in the lake (board #31); schema reverse-engineered from mainnet events (body map: amount i128, forward_recipient Address, token Address). Historical catch-up: `projector-replay -source cctp -from 62403000`. |

**Recognition attribution:** the three contracts are pinned in the
ADR-0033 reconciliation catalogue (`contractIDs`), so any FUTURE
unhandled cctp topic caps this source's completeness verdict instead
of disappearing into the system-wide recognition bucket — the second
half of the board-#31 finding.
