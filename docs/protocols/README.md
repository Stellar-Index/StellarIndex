# Protocol verification pages

One page per on-chain protocol we index, listing **every contract we
attribute to that protocol** — factories, pools/vaults, and the events we
decode from each. For the DEXes/AMMs these pages exist to be **sent to
each protocol team for verification**: "this is the complete set of your
contracts and events we ingest; please confirm it's correct and complete."

The tree also covers the **oracles** (Reflector/RedStone/Band),
**bridges** (CCTP/Rozo), the **classic DEX** (SDEX), and the **supply
observers** (SEP-41 + classic). For those, the page documents coverage +
provenance (and, for third-party oracles/bridges, a contract-set
confirmation) rather than a full pool enumeration.

## Why this matters (ADR-0035)

Soroban topic symbols are not unique across protocols (`swap`, `supply`,
`deploy`, `create`, `claim` are all emitted by multiple protocols and by
SACs). We therefore gate every decoder on **contract identity** — a
contract's events are attributed to a protocol only if the contract is one
of that protocol's factories or a contract a factory created (fan out).
The correctness of that gate depends on having the **complete** factory +
contract set. Discovery docs proved incomplete (e.g. Blend has two pool
factories, only one was documented), so each set is verified **empirically
against the certified lake** and then confirmed by the protocol team via
these pages.

## How the sets were enumerated

The enumeration method differs per protocol because creation events aren't
always in our lake (which starts at ledger 50,457,424):

- **Lake deploy-graph** — decode every creation event (`deploy` /
  `new_pair`) in `contract_events`, build factory → children. Used where
  the factory's creation events fall inside the lake (Blend, Soroswap).
- **RPC view enumeration** — the factory's `query_pools()` / `all_pairs()`
  view returns the current child set. Used where pools predate the lake
  (Phoenix). Snapshot-in-time; re-run to refresh.
- **WASM-hash walk** — contracts sharing the protocol's pool/vault WASM
  hash (the `wasm-history` audit). The fallback discriminator.

Each page states which method produced its set and the `last_verified`
date, so a team can tell us if a contract is missing or mis-attributed.

## Status legend

- ✅ **Gated** — the decoder enforces this set (events from contracts
  outside it are not attributed to this protocol).
- 🔎 **Enumerated, pending gate** — set verified from the lake; decoder
  gate not yet shipped.
- ⏳ **Pending verification** — set not yet enumerated.

### DEXes / AMMs (trades)

| Protocol | Method | Gate status | Page |
|---|---|---|---|
| Soroswap | lake deploy-graph | ✅ Gated (4 factories) | [soroswap.md](soroswap.md) |
| Blend | lake deploy-graph | ✅ Gated (2 factories, 27 pools) — lending, excluded from VWAP | [blend.md](blend.md) |
| SoroCredit | single trust-root | ✅ Gated (1 contract + child collateral positions) — consumer USDC credit/CDP, no pricing signal | [sorocredit.md](sorocredit.md) |
| Aquarius | router-anchored | ✅ Gated (router + 332 pools, 2026-07-05) | [aquarius.md](aquarius.md) |
| Phoenix | RPC view (pre-lake) | ✅ Gated code-side (curated set, 2026-07-02); operator rollout pending | [phoenix.md](phoenix.md) |
| DeFindex | multi-proof classification | ✅ Gated (curated 85 vaults + 16 strategies, 4 factories, 2026-07-05) | [defindex.md](defindex.md) |
| Comet | — (topic-bytes only) | ❌ **UNGATED — last remaining (CS-026)** | [comet.md](comet.md) |
| SDEX (classic) | op-result XDR | N/A — no contracts | [sdex.md](sdex.md) |

> **Gating is now complete except Comet.** ADR-0035 gates every decoder on
> contract identity so a look-alike contract can't inject fabricated
> trades under a protocol's source name. Factory fan-out is clean when the
> creation event **carries the child's address** (Blend `deploy`, Soroswap
> `new_pair` — both lake-verified). Where that signal is absent —
> Phoenix/Aquarius pools predate the lake (50.46M), DeFindex's `create`
> carries the vault *config* but not its address — the gate anchors on a
> curated / registry-cross-checked seed instead (Phoenix curated set,
> Aquarius router `add_pool` == registry API, DeFindex multi-proof
> classification). **Comet is the one source still matching on topic bytes
> alone** — it has no factory namespace and needs a pool allowlist /
> WASM-hash gate (CS-026, [ADR-0040](../adr/0040-completing-contract-gating.md)).

### Oracles (reported on `/v1/sources`, excluded from VWAP)

| Protocol | Gate status | Page |
|---|---|---|
| Reflector (DEX/CEX/FX) | ✅ Gated — 3 pinned contract IDs | [reflector.md](reflector.md) |
| RedStone | ✅ Gated — 1 Adapter contract + 19-feed registry | [redstone.md](redstone.md) |
| Band | ✅ Gated — 1 StandardReference contract (ContractCall, zero events) | [band.md](band.md) |

### Bridges (flow coverage, excluded from VWAP)

| Protocol | Gate status | Page |
|---|---|---|
| CCTP (Circle) | ✅ Gated — 3 pinned contracts | [cctp.md](cctp.md) |
| Rozo | ✅ Gated — 3 v1 Payment contracts | [rozo.md](rozo.md) |

### Supply

| Domain | Gate status | Page |
|---|---|---|
| SEP-41 Soroban tokens (Algorithm 3) | ✅ Gated — operator watched-set | [sep41-supply.md](sep41-supply.md) |
| Classic supply observers (Algorithm 1 + 2) | ✅ Gated — operator watched-set | [supply-observers.md](supply-observers.md) |

## External cross-checks — Dune dashboards

Dune has Stellar datasets and community/team dashboards that serve as an
independent cross-check for our contract enumerations and metrics
([discover: blockchain:Stellar](https://dune.com/discover/content/popular?q=blockchain%3A%27Stellar%27&timeframe=30d&resource-type=dashboards)).
Directly relevant:

| Dashboard | Author | Use for us |
|---|---|---|
| [Soroswap.Finance](https://dune.com/paltalabs/soroswap) | @paltalabs (the team) | pair set + volume cross-check |
| [DeFindex](https://dune.com/paltalabs/defindex) | @paltalabs (the team) | **vault enumeration** (our open Q) + TVL benchmark ($4.02M @ 2026-06-12) |
| [Blend 🧪](https://dune.com/fergmolina/blend) | @fergmolina | pool set cross-check vs our 27-pool/2-factory enumeration |
| [Aquarius ♒️](https://dune.com/fergmolina/aquarius) + [Aquarius Stellar](https://dune.com/claw) | community | **pool enumeration** (our open Q) |
| Soroban AMMs on Stellar | @paltalabs | cross-AMM pool lists (phoenix/comet) |
| Stellar Smart Contract Analysis | @stellar (SDF) | contract activity baseline |

The contract addresses live in each dashboard's query SQL — retrievable
via the Dune API (free tier key) or a logged-in query view, not from the
public page render. When cross-checking metrics, mind the
window-mismatch trap: Dune totals are often lifetime, ours are often
windowed.
