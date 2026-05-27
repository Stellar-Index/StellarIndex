# W07 — On-chain source decoders + auxiliary readers

## Scope

Every Soroban + classic source under `internal/sources/` (excluding
`internal/sources/external/*` which is W08, and `forex/` +
`frankfurter/` which are also W08).

23 sources in scope (5 new since 2026-05-12):

- Soroban DEXes: soroswap, soroswap_router (NEW), aquarius,
  phoenix, comet, blend
- Soroban oracles: reflector, redstone, band, defindex (NEW)
- Soroban bridges: cctp (NEW), rozo (NEW)
- Soroban catch-all: sorobanevents (NEW; W27 owns details)
- Soroban supply: sep41_supply
- Classic: sdex
- Classic observers: accounts, trustlines,
  claimable_balances, sac_balances, liquidity_pools

Run the per-decoder loop (`02-protocol.md` §6) for each.

## Inputs

- `inventory/source-decoder-inventory.md`
- CLAUDE.md "Things that will surprise you" (per-source caveat
  list)
- `docs/operations/wasm-audits/<source>.md`
- `internal/sources/external/registry.go` `BackfillSafe` column

## Per-decoder seven-check loop

Fill this table per source:

| Check | Result | Evidence |
| --- | --- | --- |
| 1. Claim surface (event topics / op kinds / contract IDs / methods) | | |
| 2. Decode entry function(s) traced from dispatcher | | |
| 3. Malformed-input handling (no panic, typed error, drop counter) | | |
| 4. Storage / consumer integration (sink + table) | | |
| 5. Fixture realism (real-ledger captures? capture script + mtime) | | |
| 6. Tests vs actual risk (happy-path-only? malformed-input test? WASM-dispatch test?) | | |
| 7. WASM audit status (BackfillSafe flag) | | |
| 8 (NEW). Every-event coverage (W35 cross-ref) | | |
| 9 (NEW). Matching per-source backfill subcommand (W29 cross-ref) | | |

## NEW: per-source notes since baseline

- **soroswap_router**: NEW source — verify event surface, decoder
  exists, persistence path, BackfillSafe flag.
- **cctp**: NEW source — 3 contracts (token-messenger-minter,
  message-transmitter, forwarder); 4 events (deposit_for_burn,
  mint_and_withdraw, message_sent, message_received); BackfillSafe
  true after 2026-05-26 WASM audit; cctp-backfill subcommand
  (W29).
- **rozo**: NEW source — 3 contracts (sharing single WASM hash);
  2 events (payment, flush); BackfillSafe true; rozo-backfill
  (W29).
- **sorobanevents**: NEW catch-all (ADR-0029); W27 owns details.
  W07 verifies it's wired correctly.
- **defindex**: NEW vault source — what events do the vaults
  emit? What persistence path?
- **comet**: now decodes 5/5 events (was 1/5 pre-rc.80); verify
  no regression on the original `swap` path.
- **soroswap**: now decodes 5/5 pair events (was 4/5 pre-rc.78
  — skim was previously dropped); verify.
- **phoenix**: now decodes 4 extra actions (provide_liquidity,
  withdraw_liquidity, bond, unbond) on top of swap;
  per-action correlation buffer.
- **blend**: now decodes all 20 topic kinds across 3 target
  tables (positions, emissions, admin).

## Closure criteria

23 per-source tables filled. Findings on:

- any decoder with happy-path-only tests
- any malformed-input panic
- any silently-dropped event class (cross-ref W35)
- any decoder lacking a wasm-history audit when BackfillSafe is
  true
- any new source missing a per-source-backfill subcommand
