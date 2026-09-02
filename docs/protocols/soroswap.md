---
title: Soroswap — contract & event verification
last_verified: 2026-06-12
status: current
---

# Soroswap — contract & event verification

> **For the Soroswap team:** this is the set of Soroswap factories and the
> method we use to enumerate pairs. Please confirm the four factories are
> correct and complete (any factory **not** listed here means we'd miss its
> pairs).
>
> - **Enumeration method:** lake deploy-graph (every
>   `SoroswapFactory:new_pair` event) + the factory's `all_pairs()` view for
>   the live registry, which also carries each pair's token_0 / token_1.
> - **Last verified:** 2026-06-12 (r1 lake).
> - **Gate status:** ✅ enforced (`internal/sources/soroswap`, ADR-0035).

## Factories (trust roots)

A `new_pair` event registers a pair only when emitted by one of these. The
primary factory carries the live pair registry; the three early factories
are launch-era deployments.

| Factory | `new_pair` count | Notes |
|---|---:|---|
| `CA4HEQTL2WPEUYKYKCDOHCDNIV4QHNJ7EL4J4NQ6VADP7SYHVRYZ7AW2` | 194 (`SoroswapFactory` events) | Primary — all pairs with swap activity. |
| `CCIQM2O3YJQEKS7I77AS5IO3CU6UCBAUWHLWRBWVV336ZCSTKRNBKPHW` | 11 pairs | Early/launch-era. **The 21 pairs from these three have zero swaps (defunct).** |
| `CDBRTEJMOUJQHFZCAW4JPXZ75HCRHZAQXG75ZMGQ2LMNXA5ID7RQIFSX` | 6 pairs | Early/launch-era. |
| `CCDATRT2EY6Y2KAZ7HM7BRZVZCB6RHL56PQUBWGBS2ML2JAK7VXFLCJY` | 4 pairs | Early/launch-era. |

Also tracked: **Router** `CAG5LRYQ5JVEUI5TEID72EYOVX44TTUJT5BQR2J6J77FH65PCCFAJDDH`
(orchestration only — its swap calls delegate to the pair contracts, which
emit the events; observed via the router's InvokeContract op for the census).
Router swap-intent calls land in `soroswap_router_swaps` via the dispatcher's
full **auth-tree walk** — the router is captured whether invoked directly or
as a **sub-invocation** of an aggregator contract (the dominant real-world
shape; a top-level-only walk undercounted ~8,729×). Since migration 0101
each captured call records `call_path` (the ordered contract chain from the
top-level invocation down to the router), `call_depth`, and a
`top_level`/`sub_invocation` discriminator (ROADMAP #11;
`internal/sources/soroswap_router/README.md`). Historical rows carry NULL
tree-position columns until the queued r1 re-derive
(`ch-rebuild -sources soroswap-router -contract-calls -write`) runs.

## Pairs

Pairs are **not** hard-coded — they're enumerated from the factories'
`new_pair` events (which carry `pair`, `token_0`, `token_1`) and the
primary factory's `all_pairs()` view, kept in the `soroswap_pairs`
registry. A pair's `swap`/`sync`/`skim` events are attributed to Soroswap
only when the pair is in that registry. The three early factories' 21 pairs
are registered for completeness but have no swap activity.

## Events decoded

Verified against `soroswap-core` `pair/src/event.rs` /
`factory/src/event.rs`.

| Event (topic) | Where it lands |
|---|---|
| `("SoroswapFactory","new_pair")` | registers the pair (token_0/token_1) |
| `("SoroswapPair","swap")` + `("SoroswapPair","sync")` | `trades` (source=soroswap) |
| `("SoroswapPair","skim")` | `soroswap_skim_events` |
| `("SoroswapPair","deposit"/"withdraw")` | `soroswap_liquidity` (LP add / remove; migration 0127, 2026-08-03) |

## Router topics — intentionally not projected (redundant, would double-count)

The Router (`CAG5LRYQ…`) emits `("SoroswapRouter", "swap" | "add" |
"remove")` events (`swap` ≈ 168k, `add` 1,057, `remove` 219) plus
`init` lifecycle events. These are **deliberately not projected** —
they are a router-level VIEW of swaps/LP actions whose underlying data
we ALREADY capture, so projecting them would double-count:

- **Router `swap` event** — body `{ amounts: Vec<i128>, path:
  Vec<Address>, to: Address }` is the realized aggregate of the same
  swap the underlying PAIR contracts emit per-hop. The pair-level
  `swap` events already become `trades` (→ VWAP), and the router
  *call* itself is captured from op-args into `soroswap_router_swaps`
  (path + amount_in/out + recipient). Double-count check (2026-08-03):
  `soroswap_router_swaps` holds **199,931 rows genesis→tip — a
  SUPERSET of the 168,557 router swap events** — so the events add no
  coverage. Turning them into trades would double-count VWAP against
  the pair hops; a new table would duplicate `soroswap_router_swaps`.
- **Router `add` / `remove` events** — the router-level view of LP
  provision whose per-pair `deposit` / `withdraw` events we now
  project to `soroswap_liquidity` (migration 0127). Projecting the
  router view too would double-count LP flow.

Not silently mis-attributed: the pair decoder's `classify()` returns
`""` for the `SoroswapRouter` topic prefix (it isn't a registered
pair), so these are cleanly unmatched, and the router's economic data
is fully served via `trades` + `soroswap_router_swaps` + the new
`soroswap_liquidity`. Supersedes the ROADMAP #89 "known gap" framing —
it was a recognition-accounting artifact, not a data-coverage gap.
