# Comet — contract & event verification

> **For the Comet / Blend teams:** this documents how Stellar Index
> attributes Comet weighted-AMM trades and liquidity flow. As of
> 2026-07-08 Comet is **gated on contract identity** (a curated
> one-pool allowlist — see "Gate status" below); before that date it
> was the one integrated on-chain source not yet gated, and rows
> captured before the gate shipped were not retroactively re-verified.
> If you can send us further live Comet pool contracts beyond the
> known Blend backstop (or point us at a factory / WASM-hash we can
> enumerate from), we can grow the curated set.
>
> - **Enumeration method:** curated allowlist (ADR-0040 §1 mechanism 3) —
>   the WASM-audit census found exactly ONE Comet pool on mainnet
>   (Blend's BLND/USDC backstop), and the decoder's `MainnetGatedSet`
>   seeds it as the trust root. The WASM-hash sweep is the registered
>   upkeep loop for discovering future byte-identical deployments.
> - **Last verified:** 2026-07-08 (source: `internal/sources/comet`;
>   WASM audit `docs/operations/wasm-audits/comet.md`, 2026-05-26).
> - **Gate status:** ✅ **GATED (curated one-pool allowlist, 2026-07-08 —
>   CS-026 closed; comet was the last ungated source). Rows captured
>   before 2026-07-08 predate the gate and were not retroactively
>   re-verified.**

## What Comet is

[Comet](https://github.com/CometDEX/comet-contracts-v1) is a
Balancer-v1-derived weighted AMM on Soroban. Each token in a pool has a
configurable weight, and the spot price between any two tokens is a
function of both their reserves and their weights (up to 8 tokens per
pool in Balancer-v1; the Stellar-side limit is unconfirmed).

The **only positively-identified Comet pool on mainnet is Blend's
BLND/USDC backstop** — Blend vendors `comet.wasm` and uses a Comet pool
as its backstop LP. That single pool is what gives us BLND pricing.
Whether a standalone Comet DEX with independent public pools exists
beyond Blend's backstop is an open question.

| Role | Contract | WASM hash |
|---|---|---|
| Blend backstop Comet pool (only known mainnet pool) | `CAS3FL6TLZKDGGSISDBWGGPXT3NRR4DYTZD7YOD3HMYO6LTJUVGRVEAM` | `8abc28913035c07411ed5d134e6bfeab4723d97ddd4d1a22a0605d35c94d1a36` |

## Topic shape — shared, not per-protocol

Comet emits **`("POOL", <event_name>)` for every event** — note the
uppercase `POOL` symbol. This is *not* a per-protocol namespace: every
pubnet contract that deploys Balancer-v1 Comet code looks byte-identical
on the wire. Contrast Soroswap (`SoroswapPair`/`SoroswapFactory`),
Phoenix (`XYK Pool: …`), or Aquarius (a `trade` topic anchored to the
router registry) — each carries a discriminator Comet lacks.

## Events decoded

Verified 2026-05-26 against upstream `main`
(`contracts/src/c_pool/{event.rs, call_logic/pool.rs}`). All five events
the Soroban port of Balancer-v1 emits:

| Event (topic) | Body (all `i128` amounts) | Where it lands |
|---|---|---|
| `("POOL","swap")` | caller, token_in, token_out, token_amount_in, token_amount_out | `trades` (source=comet) |
| `("POOL","join_pool")` | caller, token_in, token_amount_in | `comet_liquidity` (kind=join_pool, direction=add) |
| `("POOL","exit_pool")` | caller, token_out, token_amount_out | `comet_liquidity` (kind=exit_pool, direction=remove) |
| `("POOL","deposit")` | caller, token_in, token_amount_in | `comet_liquidity` (kind=deposit, direction=add) |
| `("POOL","withdraw")` | caller, token_out, token_amount_out, pool_amount_in | `comet_liquidity` (kind=withdraw, direction=remove) |

Notes:

- **Token identities are in the event body** — `token_in` / `token_out`
  are `Address` fields, so unlike Soroswap the decoder needs no pool
  registry to name the assets. Trade direction: the trader sold
  `token_in` (base) and bought `token_out` (quote).
- **`join_pool` / `exit_pool` are loop-emitted** — an N-token join emits
  N events from one `(ledger, tx_hash, op_index)`, each carrying one
  token's amount; the `comet_liquidity` PK includes `token` so per-token
  rows don't collide.
- **`pool_amount_in` (BPT burned) is withdraw-only** — NULL on the other
  three liquidity kinds.

## Events NOT decoded (and why)

Verified absent from the Soroban port 2026-05-26:

- `bind` / `rebind` / `unbind` / `finalize` — these functions **do not
  exist** in the Soroban port (the pool is initialised in one shot via
  `init(controller, tokens, weights, balances, swap_fee)`; the token+
  weight set is fixed at deploy).
- `set_swap_fee` / `set_public_swap` — absent from the Soroban port.
- `set_controller` / `gulp` — the functions exist but publish no event.
- BPT (Balancer Pool Token) `transfer` — emitted on the **SEP-41
  standard token-event surface**, claimed by `internal/sources/sep41_supply`
  when the pool is in scope; re-decoding it here would double-count.

A future Comet upgrade that starts emitting any of these routes to
`Decode`, which rejects unknown `("POOL", *)` kinds as `ErrNotCometEvent`
and counts them on `source_orphan_events_total{source="comet"}` — the
signal to extend `classify`.

## Gate status — GATED (curated allowlist, 2026-07-08; CS-026 closed)

`Decoder.Matches` gates on **contract identity** (ADR-0035/0040):
an event is attributed to comet only when it carries a recognised
`("POOL", <event>)` topic **and** was emitted by a pool in the curated
registry — the in-code `comet.MainnetGatedSet()` (today exactly one
pool, Blend's BLND/USDC backstop `CAS3FL6T…`) plus the
`protocol_contracts` DB warm. Comet has **no factory namespace** to
anchor a deploy-graph gate on (the enumeration methods that gate
Soroswap/Blend/Aquarius don't apply), so this is the ADR-0040 §1
curated-set mechanism.

**What closed (CS-026):** before 2026-07-08 a look-alike contract that
deployed the shared Balancer-v1 code — or merely emitted the
`("POOL", "swap")` shape — could inject **fabricated trades under
`Trade.Source = "comet"`**. That event now fails `Matches` and lands in
the recognition audit (ADR-0033 Claim 2a) instead — visible, never
silently attributed.

**Admitting a future pool:** fail-closed by design. A genuinely new
Comet pool must be operator-admitted before its events attribute —
`stellarindex-ops seed-protocol-contracts -source comet` (after adding
it to the curated set) or a direct `protocol_contracts` row. The
WASM-hash sweep (ADR-0040 §1 mechanism 3) is the registered upkeep loop
for spotting byte-identical Balancer-v1 deployments; note the named
caveat — a byte-identical fork *is* the same code and would be
attributed as comet once admitted. → also
[ADR-0035](../adr/0035-factory-anchored-contract-gating.md).

## Backfill safety

`BackfillSafe = true` in `internal/sources/external/registry.go` (audited
2026-04-29; the only known mainnet pool is the Blend backstop, WASM
`8abc2891…`, no mid-life upgrade observed). Note the distinction:
**backfill-safety** (the current decoder is trusted against every WASM
version that ran over the replay range) is orthogonal to **gating** (which
contracts are attributed to `comet`). Comet is backfill-safe **and**
gated (2026-07-08) — a historical replay attributes only curated-registry
emitters; a foreign `("POOL", …)` emitter in the replayed range is
dropped by the gate and surfaced by the recognition audit.

## Storage

| Event | Hypertable | Migration |
|---|---|---|
| `swap` | `trades` | 0001 |
| `join_pool` / `exit_pool` / `deposit` / `withdraw` | `comet_liquidity` | 0042 |

## Aggregator treatment

Class `Exchange` / `IncludeInVWAP=true` (`external.Registry`) — Comet
swaps are genuine executed trades and contribute to VWAP. With the
2026-07-08 identity gate, everything attributed to `comet` comes from
the curated pool set (today: the Blend backstop pool), so the CS-026
integrity caveat that previously applied here is closed.
