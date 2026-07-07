# Comet — contract & event verification

> **For the Comet / Blend teams:** this documents how Stellar Index
> attributes Comet weighted-AMM trades and liquidity flow, and — bluntly
> — where the attribution is currently **weakest**. Comet is the one
> integrated on-chain source that is **not yet gated on contract
> identity** (see "Gate status" below). If you can send us the complete
> set of live Comet pool contracts (or point us at a factory / WASM-hash
> we can enumerate from), we can close that gap.
>
> - **Enumeration method:** none yet — the decoder matches on the shared
>   `("POOL", …)` topic bytes, not on a pool set. The only pool we have
>   positively identified on mainnet is Blend's BLND/USDC backstop.
> - **Last verified:** 2026-07-06 (source: `internal/sources/comet`;
>   WASM audit `docs/operations/wasm-audits/comet.md`, 2026-05-26).
> - **Gate status:** ❌ **UNGATED (last remaining ungated source, CS-026)**.

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

## Gate status — UNGATED (CS-026)

**This is the one integrated on-chain source with no contract-identity
gate.** `Decoder.Matches` returns true for *any* contract emitting a
recognised `("POOL", <event>)` topic — it is byte-equality on the topic
symbols, with no allowlist and no factory fan-out
(`internal/sources/comet/dispatcher_adapter.go`). Comet has **no factory
namespace** to anchor an ADR-0035 gate on, so the enumeration methods
that gate the other AMMs (Soroswap/Blend `new_pair`/`deploy` graphs,
Aquarius router `add_pool`, Phoenix/DeFindex curated seeds) don't apply.

**The exposure (CS-026):** a look-alike contract that deploys the shared
Balancer-v1 Comet code — or merely emits the `("POOL", "swap")` shape —
can inject **fabricated trades under `Trade.Source = "comet"`**. This is
the exact injection shape ADR-0035 exists to prevent, and Comet is the
last source where it remains open.

**Interim mitigation:** narrow, trustworthy Comet coverage is a
*downstream filter* on `Trade.Source = "comet"` **AND** the contract
address (`CAS3FL6T…`, the Blend backstop) — not a claim that everything
tagged `comet` is genuine. The fix is a pool allowlist or a WASM-hash
gate; tracked in
[ADR-0040](../adr/0040-completing-contract-gating.md) as the remaining
gating work. → also
[ADR-0035](../adr/0035-factory-anchored-contract-gating.md).

## Backfill safety

`BackfillSafe = true` in `internal/sources/external/registry.go` (audited
2026-04-29; the only known mainnet pool is the Blend backstop, WASM
`8abc2891…`, no mid-life upgrade observed). Note the distinction:
**backfill-safety** (the current decoder is trusted against every WASM
version that ran over the replay range) is orthogonal to **gating** (which
contracts are attributed to `comet`). Comet is backfill-safe **and**
ungated — replaying historical ranges faithfully decodes every
`("POOL", …)` emitter, which is precisely why the missing identity gate
matters: an injected historical emitter would replay too.

## Storage

| Event | Hypertable | Migration |
|---|---|---|
| `swap` | `trades` | 0001 |
| `join_pool` / `exit_pool` / `deposit` / `withdraw` | `comet_liquidity` | 0042 |

## Aggregator treatment

Class `Exchange` / `IncludeInVWAP=true` (`external.Registry`) — Comet
swaps are genuine executed trades and contribute to VWAP. Because of the
ungated attribution above, operators concerned about VWAP integrity
should weigh the CS-026 exposure when deciding whether to keep Comet in
the exchange class or scope it to the Blend backstop pool.
