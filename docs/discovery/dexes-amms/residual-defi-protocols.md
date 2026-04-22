# Residual Soroban DeFi protocols

**Status:** ⚠️ Surveyed, **not deep-audited.** These six protocols are
listed in the Stellar oracle-providers doc as Reflector consumers,
and our adversarial audit flagged them as "events may add niche
signal." This doc closes the gap by documenting each one's role and
explaining why **we do not need a per-protocol indexer for any of
them.**

## TL;DR

None of these are spot-trading venues. All six are either:

- **Synthetic stablecoin issuers** (Orbit CDP, FxDAO) — they mint
  pegged tokens and the tokens trade on Soroswap/Aquarius/Phoenix.
  We price the issued tokens via the normal AMM-indexer path.
- **Lending protocols** (Laina, Slender) — they facilitate borrowing
  against collateral. Like Blend, they emit position-change events,
  not trade events. Potential auction/liquidation signal but not
  VWAP contribution.
- **Yield aggregators / synthetics** (DeFindex, EquitX) — they rebase
  user deposits across strategies. No direct price output.

**Conclusion:** our existing SDEX/Soroswap/Aquarius/Phoenix/Comet/
Blend indexer set plus SEP-41 token-transfer events **already covers
everything these protocols contribute to price discovery.** What we
gain per-protocol is:

1. Recognition of their **issued asset contract addresses** so we
   can label them correctly in the API response (brand, home domain,
   protocol association).
2. Liquidation / auction signals where the protocol exposes them.

## Per-protocol notes

### Orbit CDP — `orbitcdp.finance`

**Role:** Collateralized Debt Protocol. Users lock XLM (and other
collateral, incl. Etherfuse tokenized-bond tokens) in a Blend
lending pool; Orbit's treasury mints pegged stablecoins:

- `oUSD` — US dollar peg.
- `oEUR` — euro peg.
- `oMXN` — Mexican peso peg.

**Docs:** <https://docs.orbitcdp.finance/>
**Source code:** ✅ **FOUND** (2026-04-22, follow-up) —
`github.com/zenith-protocols/orbit-contracts` (Rust, Apache-2.0,
last updated Jan 2026). User directive pointed to the right org.
Note: the `orbit-protocol` GitHub org is a *different* Blast-L2
project — unrelated.

**Sibling zenith-protocols repos** (audited 2026-04-22):

- **MaxFX** — "Leverage Trade Currencies on Stellar." ⚠️
  **Leverage wrapper, not a new venue.** Per their README, MaxFX
  orchestrates **Blend (borrow) + Soroswap (convert) + Orbit
  (currency access)** to deliver a leveraged-trade UX. All
  execution happens on venues **we already index**. Zero extra
  indexer code. We see MaxFX-originated trades as normal Soroswap
  trades in our stream; user attribution to MaxFX is optional.
  Not yet on mainnet at audit time.
- **hermes** — "decentralized perpetual exchange", AGPL-3.0, 8
  commits. **Spot-trading: none.** Purely perpetual futures with
  "up to 100x leverage, zero slippage/spread", LP-to-trader model,
  inspired by Jupiter on Solana. Perp mark prices are not spot
  prices and are **out of scope** for our pricing API. Not yet on
  mainnet. **Re-audit if Hermes adds spot trading** or if their
  perp events expose oracle-read values useful as a secondary
  price signal.
- **orbit-swap** — swap UI for oUSD/oEUR/oMXN (front-end; nothing
  for us).
- **soroban-vault** — generic vault primitive.
- **zenith-sdk** — TS SDK.

### Orbit event schema (verified from source)

#### Treasury (`treasury/src/contract.rs`)

All events use 2-tuple topics `("Treasury", Symbol::new(…, "<name>"))`:

```
("Treasury", "initialize")       body: (admin,)
("Treasury", "add_stablecoin")   body: (token, blend_pool)   ← asset-discovery signal
("Treasury", "increase_supply")  body: (token, amount)       ← stablecoin minted
("Treasury", "decrease_supply")  body: (token, amount)       ← stablecoin burned
("Treasury", "claim")            body: (reserve_address, to, interest)
("Treasury", "set_admin")        body: new_admin
```

#### BridgeOracle (`bridge-oracle/src/contract.rs`)

```
("BridgeOracle", "init")      body: (admin, stellar_oracle, other_oracle)
("BridgeOracle", "add_asset") body: (asset, to)              ← asset → fiat-price mapping
("BridgeOracle", "set_admin") body: (new_admin,)
```

**For our pricing pipeline:**

1. `Treasury:add_stablecoin` is a **free asset-discovery signal** —
   walk the treasury's event log and we get every oUSD/oEUR/oMXN
   contract ID without manual tracking. Plus the Blend pool each
   is backed by, which tells us which Blend pool's auction events
   correspond to which Orbit stablecoin.
2. `Treasury:increase_supply` / `decrease_supply` give us the
   **protocol-level** mint/burn volumes, complementing the per-
   token SEP-41 events. Useful as a cross-check (should match).
3. `BridgeOracle:add_asset` tells us which underlying oracle Orbit
   relies on (likely Reflector). Confirms our divergence-detector
   should compare our aggregated price against the same oracle
   feed Orbit uses.

No `trade` / `swap` events in either contract — consistent with
Orbit being a **stablecoin issuer, not a DEX**.

### Orbit deployment architecture

`orbit-contracts/wasm/` contains compiled WASMs for:
- `orbit` — Orbit's own contracts.
- `soroswap` — vendored Soroswap (Orbit integrates with Soroswap
  for stablecoin liquidity).
- `blend` — vendored Blend (Orbit uses Blend for collateral
  custody, as the docs describe).

So Orbit's full dependency chain on-chain is:
`Orbit Treasury → Blend Pool (collateral custody) + Soroswap (peg
arbitrage) + BridgeOracle → Reflector`.

**Architecture (per their docs):**

```
XLM / Etherfuse bond  ──► Blend lending pool
                             │
                             ▼
                       Orbit Treasury  ──► mints oUSD / oEUR / oMXN
                             │
                             ▼
                       Pegkeeper (liquidation + arbitrage)
                             │
                             ▼
                       Stellar AMMs (Soroswap / Aquarius for
                       stablecoin swaps)
```

**What we ingest:**

- oUSD / oEUR / oMXN **trades** in Soroswap/Aquarius pools that list
  them — via our existing AMM indexer. Zero extra code.
- oUSD / oEUR / oMXN **supply** (mint/burn events) via our SEP-41
  event decoder — zero extra code (see
  [../notes/sep-41-token-events.md](../notes/sep-41-token-events.md)).
- **Optionally**: liquidation / Pegkeeper events if their contract
  exposes them — flagged as useful but not required.

**Open items:**

- [ ] Identify `oUSD` / `oEUR` / `oMXN` contract addresses on
      mainnet via stellar.expert once we confirm the exact SAC
      or SEP-41 deployment approach.
- [ ] Once asset contract IDs are known, tag them in our
      `asset_metadata` table with `protocol: "orbit-cdp"`.
- [ ] Watch for open-sourcing of Orbit's contracts; re-audit if
      the source becomes available.

### FxDAO — `fxdao.io`

**Role:** DAO-governed stablecoin issuer on Stellar Soroban. Issues
`XOV` (a USD stablecoin) backed by XLM via a CDP mechanism —
similar to Orbit CDP but with a community-governance layer.

**Community fund page:**
<https://communityfund.stellar.org/project/fxdao-xov>

**Source code:** not locatable at audit time. Searched for
`fxdao-org`, `fxdao-xov`, `fxdao`, etc. — no clear public
repository of the Soroban contracts at the time of this audit.

**What we ingest:** same pattern as Orbit CDP — their issued token
(`XOV`?) trades on Soroban AMMs we already cover.

**Open items:**

- [ ] Confirm XOV contract address once the protocol is deployed /
      re-deployed (community-fund page suggests early stage).
- [ ] Re-audit when source becomes public.

### Laina — `laina-de.fi`

**Role:** Lending protocol on Soroban. Users supply assets to
liquidity pools, borrow against collateral. Competitor to Blend.

**Source code:** no clear public repo located at audit time.

**What we ingest:** if Laina lists synthetic assets or generates
auction events in a similar pattern to Blend, those would be
secondary price signals. Without source we can't pre-audit the
event schema. We treat Laina the same way as Blend in principle:
**validation / stress-price signal only**, not VWAP contributor.

**Open items:**

- [ ] Locate public source (or whitepaper describing event schema).
- [ ] Identify any issued synthetic tokens.

### Slender — `slender.fi`

**Role:** Another Soroban lending protocol.

**Source:** no public repo located at audit time.

**Same treatment as Laina:** future stress-price signal, indirect.

### EquitX — `equitx.com`

**Role:** Synthetics protocol. Issues tokenized equity exposures
backed by over-collateralized deposits. Similar to Synthetix on
EVM chains.

**What we ingest:** EquitX's synthetics (if they launch mainstream
assets) would trade on Soroban AMMs. We pick them up via the normal
AMM indexer plus SEP-41 event decoding.

**Special case:** if EquitX issues a synthetic pegged to a
non-crypto asset (e.g. `sAAPL`, `sTSLA`), pricing that correctly
requires us to know the **external asset's reference price**, not
just the AMM price. Source would ideally be Redstone's RWA feeds
(see [../oracles/redstone.md](../oracles/redstone.md) — they cover
SPXU already; individual equities less clear).

### DeFindex — `defindex.io`

**Role:** Yield aggregator / vault protocol. Users deposit, DeFindex
rebalances across multiple yield strategies (lending pools, LP
positions, etc.). Returns yield-bearing receipt tokens.

**What we ingest:** DeFindex's receipt tokens (yield-bearing wrapper
tokens) if they trade on open markets. Per-token SEP-41 event
decoding same as any other token.

**Pricing challenge:** yield-bearing wrappers have a growing
exchange rate vs. their underlying (e.g. `ydUSDC` grows vs. USDC
over time). Correctly pricing the wrapper requires either:

- Reading the `convertToAssets()` method (ERC-4626-style) on the
  wrapper contract, or
- Tracking the wrapper's underlying + strategy events to compute
  the rate.

Not a Phase-1 concern, but flagged for Phase 3+.

## Common pattern — how we cover all six with zero per-protocol code

```
Soroban AMM events (we already decode)
    +
SEP-41 transfer/mint/burn events (we already decode)
    +
Reflector oracle reads (we already poll)
    =
Coverage for every synthetic / lending / vault asset these
protocols issue, priced against the venues where they trade.
```

Per-protocol code pays off only when:

- The protocol exposes a **unique pricing signal** (e.g. Blend's
  `new_auction` / `fill_auction`). We already picked up Blend.
- The protocol's **issued synthetic has no on-chain AMM
  liquidity** — then we'd need the protocol's internal state
  (convert-to-assets method calls). Edge case.

## What this means for the RFP

[rfp-requirements-matrix.md §A3](../rfp-requirements-matrix.md)
had a line flagging "Other Soroban DeFi (Phoenix-derived synthetics,
FxDAO, OrbitCDP …) — no audit doc, ❌ open." **This doc closes
that row.** Verdict:

- No separate indexer needed.
- Existing DEX/AMM/SEP-41/oracle indexers cover them.
- Open items are mostly contract-ID enumeration for metadata tagging.

Updated [rfp-requirements-matrix.md](../rfp-requirements-matrix.md)
to reflect this.

## References

- Reflector consumers list:
  `stellar-docs/docs/data/oracles/oracle-providers.mdx`
- Individual protocol sites: <https://orbitcdp.finance>,
  <https://fxdao.io>, <https://laina-de.fi>, <https://slender.fi>,
  <https://equitx.com>, <https://www.defindex.io>.
- Stellar Community Fund project pages for community-funded
  status of each (search `communityfund.stellar.org/project/<name>`).
- Related: [blend.md](blend.md) (the primary lending protocol we
  DO index), [../oracles/reflector.md](../oracles/reflector.md)
  (the oracle all six consume).
