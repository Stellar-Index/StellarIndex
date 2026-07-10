---
title: Residual DeFi protocols — mainnet + lake census (2026-07)
last_verified: 2026-07-10
status: point-in-time audit
---

# Residual DeFi protocols — mainnet + lake census (2026-07)

**Closes BACKLOG #63 / ROADMAP §2 Tier-2 "Residual DeFi re-eval" — Phase 1
(investigation only).** No code changes in this doc's PR. It re-ranks
Orbit CDP, FxDAO, Laina, Slender, EquitX against the r1 ClickHouse lake
(ADR-0034) now that addresses (or their absence) are known, replacing
the pricing-scoped "decoder not needed" verdict from
`docs/archive/discovery/dexes-amms/residual-defi-protocols.md`
(2026-04-22, local-only — `docs/archive/` is gitignored, see
`.gitignore`) — that verdict predates the granular
every-event-for-every-major-protocol mission and was always meant to
be revisited with real numbers (see
[`notes/DECISION-BRIEF-2026-07-06.md`](../../notes/DECISION-BRIEF-2026-07-06.md)
§2D, decision **(iii) hold now, re-rank after v1.0**). This doc is that
re-rank.

## TL;DR verdict

**None of the five clear the bar for a native six-file decoder today.**
Two protocols aren't deployed to mainnet at all; the three that are
either have near-zero protocol-specific event volume or are dormant.
The one immediately actionable, high-value, *zero-code* finding: FxDAO's
four SEP-41 tokens (USDx/EURx/GBPx/FXG) are genuinely active
(~51.6k events/30d, ~$2.3M USDx supply) and captured by **nothing**
today — they aren't in `watched_sep41_contracts` or the verified-currency
catalogue. Adding them is an operator-config change, not a decoder.

| Rank | Protocol | Mainnet? | 30d events (all contracts) | Lifetime events | Gate mechanism (ADR-0035/0040) | Native-decoder verdict |
|---|---|---|---|---|---|---|
| 1 | **FxDAO** | ✅ live | ~51,585 (token layer only) | ~1,739,082 | Curated-set (single fixed contract per role) | **Hold** — token layer covered by adding to `watched_sep41_contracts` (config, not decoder); CDP/Vaults contract emits ~0 events, nothing to decode natively |
| 2 | **Slender** | ✅ live, **dormant** | 0 | ~377 (7 contracts) | Curated-set (single global pool) | **Hold** — clean schema, easy template, but zero real-world activity since ~ledger 60.75M (~6 months) |
| 3 | **EquitX** | ✅ live, **near-dead** | 0 | 4 (2 contracts) | Factory-anchored (orchestrator → per-symbol xAsset) | **Hold** — clean Map-named schema, but only 4 CDP events ever captured, and the addresses in the public repo's deploy artifacts show *zero* lake activity (see caveat below) |
| 4 | **Orbit CDP** | ❌ not deployed | — | — | N/A (would be curated-set; no factory in source) | **No action** — testnet/pre-audit only as of this census; re-check on mainnet-launch announcement |
| 5 | **Laina** | ❌ not deployed | — | — | N/A (would be factory-anchored; confirmed in source) | **No action** — frontend + CI hardcoded to testnet; re-check on mainnet-launch announcement |

**Recommended order if/when native decoders are ever justified:**
Slender → EquitX → FxDAO-Vaults (see [§6](#6-recommended-order--per-protocol-scope-for-a-later-wave)).
That order is *decode-feasibility* first because *activity* is ~zero
for all three today — there's no activity signal to rank by.

## 1 — Methodology

1. **Mainnet address discovery**: parallel web research per protocol
   (project docs, GitHub source + deploy-artifact JSON/TOML, Stellar
   Community Fund pages) cross-checked against `stellar.expert`'s
   public API for each candidate address (creation date, event/
   invocation counts, verified-source status).
2. **Lake census** (r1, read-only): ClickHouse HTTP interface on
   `:8123` (not the native-protocol port `9300`, and not `9000` which
   is MinIO) against `stellar.contract_events`
   (12.39B rows, ledger range `[3, 63407342]` at census time — lake tip
   ≈ 2026-07-10). Per candidate contract: `GROUP BY topic_0_sym` for
   lifetime counts + first/last `ledger_seq`, a second pass with
   `close_time >= now() - INTERVAL 30 DAY` for current activity, and a
   small `LIMIT`-bounded sample of `topics_xdr`/`data_xdr` per topic for
   shape inspection. All queries were written to a local `.sql` file
   and `scp`'d to r1 rather than inlined over `ssh` (inline `$$`/
   heredocs get mangled by the remote shell and can silently corrupt
   a query into a false-empty result); `/tmp/*.sql` scratch files were
   deleted after each query, both locally and on r1.
3. **Cross-validation**: where web research and the lake disagreed
   (EquitX — see §5), the lake was treated as authoritative per the
   ADR-0035 precedent (Blend's undocumented second factory was only
   found by lake verification, not docs).

No code was changed. No `protocol_contracts` rows were seeded. No
config was touched on r1.

## 2 — FxDAO

**Status:** ✅ **live on mainnet**, confirmed independently three ways
(FxDAO's own docs at `fxdao.io/docs/addresses/`, `stellar.expert`
creation dates + activity, and the r1 lake). Not "XOV" — that ticker
from the 2024 Stellar Community Fund listing was never shipped. Live
product issues three currency-pegged stablecoins (**USDx**, **EURx**,
**GBPx**) plus a governance token (**FXG**), all via one shared
`Vaults` CDP contract with a `denomination` parameter (not one Treasury
per currency). Source: `github.com/FxDAO/FxDAO-SC` (Rust, no LICENSE
file on record — flag for legal review if a native decoder is ever
built), last push 2025-06-16.

### Contracts

| Role | Address | Lake-verified |
|---|---|---|
| USDx (SAC/SEP-41) | `CDIKURWHYS4FFTR5KOQK6MBFZA2K3E26WGBQI6PXBYWZ4XIOPJHDFJKP` | ✅ 936,112 lifetime events |
| EURx (SAC/SEP-41) | `CBN3NCJSMOQTC6SPEYK3A44NU4VS3IPKTARJLI3Y77OH27EWBY36TP7U` | ✅ 302,360 lifetime events |
| GBPx (SAC/SEP-41) | `CBCO65UOWXY2GR66GOCMCN6IU3Y45TXCPBY3FLUNL4AOUMOCKVIVV6JC` | ✅ 239,932 lifetime events |
| FXG (governance, SAC/SEP-41) | `CDBR4FMYL5WPUDBIXTBEBU2AFEYTDLXVOTRZHXS3JC575C7ZQRKYZQ55` | ✅ 260,678 lifetime events |
| Vaults (CDP core) | `CCUN4RXU5VNDHSF4S4RKV4ZJYMX2YWKOH6L4AKEKVNVDQ7HY5QIAO4UB` | ⚠️ **0 events** in our lake (stellar.expert shows ~10 lifetime) |
| Oracle | `CB5OTV4GV24T5USEZHFVYGC3F4A4MPUQ3LN56E76UK2IT7MJ6QXW4TFS` | 0 events (expected — SEP-40-style read-only price fetch, no push events) |
| Locking Pool | `CDCART6WRSM2K4CKOAOB5YKUVBSJ6KLOVS7ZEJHA4OAQ2FXX7JOHLXIP` | 0 events (feature unused) |
| Treasury (classic G-account, reserve custody) | `GB4KOTOYRZA32BRBJUOYDCAUJNPG6RPNOZ7QYDC2WLPNM4KML4475CIV` | not a Soroban contract — classic account, out of `contract_events` scope |

### Token-layer census (the four SAC/SEP-41 contracts)

| Contract | Lifetime events | 30d events | First → last ledger |
|---|---:|---:|---|
| USDx | 936,112 | 20,166 (`transfer` 20,165 + `burn` 1) | 51,850,945 → 63,407,284 |
| EURx | 302,360 | 4,933 (`transfer`) | 51,851,060 → 63,407,218 |
| GBPx | 239,932 | 8,261 (`transfer` 8,260 + `burn` 1) | 51,851,072 → 63,407,291 |
| FXG | 260,678 | 18,225 (`transfer`) | 53,290,760 → 63,407,334 |

Shape: standard `transfer`/`mint`/`burn`/`approve`/`set_admin` SEP-41
events, 4-topic CAP-67 form (sample decoded: `["transfer", from, to,
"USDx:GAVH5ZWA…"]`, amount in `data_xdr`) — **identical wire shape to
every other SEP-41 token we already decode generically.** Zero new
decode logic needed.

### Vaults (CDP core) — the actual protocol-specific logic

`0` events captured in our lake against `10` reported by stellar.expert
— both numbers say the same thing: **this contract almost never emits
events.** Vault open/close/liquidation appear to be driven by storage
writes + cross-contract calls to the classic-account Treasury, not by
a topic-based event stream. A native "FxDAO CDP" decoder would have
next to nothing to decode from `contract_events` — it would need the
Band-pattern `ContractCallDecoder` (match on `(contract_id,
function_name)`, decode from op args) or periodic contract-state reads,
which is materially more expensive than a normal six-file source and
currently unjustified by ~10 lifetime events.

### Gating (ADR-0035/0040 doctrine)

**Curated-set, not factory.** One fixed Vaults contract handles all
three currencies via `denomination`; each currency's SAC is also a
single pre-deployed address (verified in source: `create_currency()` /
`toggle_currency()` are admin-invoked config, not per-currency
contract deploys). No registry-anchored gating needed if ever
implemented — same shape as `sorocredit`.

### Recommendation

1. **Now (config, not code):** add the four SAC addresses to
   `watched_sep41_contracts` in `/etc/stellarindex.toml` (r1) and to
   `internal/currency/data/seed.yaml` (verified-currency catalogue) —
   this alone gets real transfer/mint/burn/supply tracking for a
   genuinely active ~$2.3M-supply multi-currency stablecoin family at
   zero decoder cost. `Class: ClassLending` (or a new stablecoin-issuer
   class if the team wants finer taxonomy), `IncludeInVWAP: false`
   (these are pegged assets, not price-discovery venues — their peg
   trades on Soroswap/Aquarius/Phoenix, which we already index).
2. **Later, only if Vaults activity grows:** revisit with a
   `ContractCallDecoder` (Band template), not a topic-event decoder.

## 3 — Slender

**Status:** ✅ live but **dormant**. Single global lending pool (Aave-
style: `sTokens` + `debtTokens` per reserve), 3 reserves configured
(XLM, XRP, USDC; base asset USDC). Source: `github.com/eq-lab/slender`
(MIT), last push 2025-10-03.

### Contracts

| Role | Address |
|---|---|
| Pool | `CCL2KTHYOVMNNOFDT7PEAHACUBYVFLRH2LYWVQB6IPMHHAVUBC7ZUUC2` |
| Deployer (one-time setup, not a live factory) | `CCDZI7OYKBLKSDZ3IGDSPMQPNOAYSPEONPOPZGLAVCVPZGQNUTPB7WCA` |
| sToken (XLM / XRP / USDC) | `CAUE3RVG6QPXZJHHI6VW24SCCRA2DIYEDAAPSUGZ2PRPCF6EM74U3CUU` / `CD677VJOOQY5SMNQND7NYL64K4ZQYO24PXQSZKGTKSHXSGR2DHXWM2Q7` / `CA5RXZCRGH7HCACUBO6M57E2CMEF35JVFPQWJ6LBA336WQUSFH7YFTT6` |
| debtToken (XLM / XRP / USDC) | `CDIYQMQGHX7GSTF2I46K7SDNM5XXDH4PVVKXD37EXP7WNOT4D3SRYPNV` / `CB7NKQGNOY2CHE4UXHULVEWXN64WKP3H4EUOCXEV3YD2M7SBFO2MLECV` / `CCN2XWUKHWMWCBNZXZRV4WADAVBHX2EMMHEGVAH2NV2DMZ3IH3K7FITL` |
| Price feed (Reflector-shaped) | `CALI2BYU2JE6WVRUFYTS6MSBNEHGJ35P4AVCZYF3B6QOE3QKOB2PLE6M` |

### Lake census — Pool contract (lifetime, 30d = 0 across all 7 contracts)

| topic_0_sym | n | first ledger | last ledger |
|---|---:|---|---|
| `deposit` | 78 | 52,820,145 | 60,749,975 |
| `withdraw` | 55 | 52,820,634 | 59,605,065 |
| `reserve_used_as_coll_enabled` | 44 | 52,820,145 | 60,749,975 |
| `reserve_used_as_coll_disabled` | 29 | 52,999,651 | 59,605,065 |
| `borrow` | 7 | 53,206,643 | 59,616,793 |
| `repay` | 5 | 53,223,160 | 59,605,542 |
| `collat_config_change` | 3 | 52,819,318 | 52,819,325 |
| `borrowing_enabled` | 3 | 52,819,329 | 52,819,331 |
| `initialize` | 1 | 52,819,278 | 52,819,278 |

**226 lifetime events, 0 in the last 30 days.** Last activity across
*every* Slender contract (pool + all 6 token contracts) was ledger
60,749,975 — roughly 2.66M ledgers / ~6 months behind the current tip
(63,407,342). The pool is not accumulating positions; it is not
accruing new borrow/repay traffic; it reads as an abandoned pilot, not
an active lending market. Never observed a `liquidation` or
`flash_loan` event despite both being in the documented schema
(`contracts/pool/src/event.rs`) — no liquidations have ever fired.

Sample shape (`deposit`): Map-named body (`amount`, `asset` fields
visible in the XDR), clean and simple to decode — this is the easiest
of the three live protocols to build a decoder for, if it were ever
worth building.

### Gating

**Curated-set, single hardcoded pool address** (Comet-allowlist style,
not Blend-style childgate) — the `Deployer` is a one-shot admin tool,
not a live factory; gate `Matches()` on the one Pool contract ID +
its 6 token contracts.

### Recommendation

**Hold.** Clean schema + trivial gating, but zero current activity —
there's nothing to gain from decoding a dormant pool. Re-audit trigger:
any new `deposit`/`borrow` activity resuming (would show up in the
existing gap-detector/discovery sweep as a new emitter, or via a repeat
of this census).

## 4 — EquitX

**Status:** ✅ live since 2025-12-11 (GitHub PR #171, "Mainnet
deployment"), but **effectively no real usage.** Synthetics protocol
(Synthetix-style CDP), org `EquitXCompany/equitx-project` (Apache-2.0),
last push 2026-03-09. Three synthetic assets shipped: **xBTC, xETH,
xUSDT** (tracking BTC/ETH/USDT spot — no synthetic equities/xTSLA-style
tickers exist despite older promotional copy suggesting them). Price
source: Reflector SEP-40 (`CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6JLN34DLN`,
a shared multi-protocol oracle contract, not EquitX-exclusive), read
live per-call — no price-push events.

### ⚠️ Address discrepancy — lake vs. public repo artifacts

The addresses in the repo's `scripts/existing_contracts.production.txt`
/ `environments.toml` (orchestrator `CCU3FICCTH56KER3YR75NSLXC2BM24RSKNCW6ZT4JGRYYLO5K4FUP24I`
+ xBTC/xETH/xUSDT `CDXLRXM5…`/`CDGMUH4M…`/`CDA4KYX6…`) show **zero**
`contract_events` in our lake at any ledger. A **topic-symbol sweep**
across the whole lake for EquitX's documented event vocabulary
(`CDP`, `Liquidation`, `mintx`, `burnx`, `StakePosition`) instead found
real, schema-matching `CDP` events (Map-named: `accrued_interest`,
`asset_lent`, `id`, `interest_paid`, `last_interest_time`, `ledger`,
`status` [`Open`/`Frozen`], `timestamp`, `xlm_deposited` — an exact
match for the source-documented `CDP` struct) on **two different
addresses**:

| Address | `CDP` events | First → last ledger |
|---|---:|---|
| `CCAKOTMHZ63UZIFRCWWABLIX5VP4DST2JANHFJLUR7CK4SGIKUPUDBA6` | 3 | 59,107,932 → 59,480,808 |
| `CA3BB35F2EK4ADN4SI3QJAKWG2OQ3S5NKLFRV4H24FEHZ3CA2GHNDGAK` | 1 | 59,480,724 |

That's **4 total protocol-specific events, ever**, dated roughly
Oct–Nov 2025 (~2 months before the repo's documented "mainnet
deployment" PR merged 2025-12-11) — consistent with the research
agent's independent finding of "a stale orphaned xBTC address
… from a prior deploy attempt" in `environments.toml`. No `mintx`,
`burnx`, or `StakePosition` topic was found anywhere in the lake under
any contract. **Reading:** EquitX ran (or attempted to run) an earlier
pilot deployment around Oct–Nov 2025 that opened exactly 3–4 CDPs and
was then abandoned/redeployed; the "current" addresses documented in
the public repo have never been used on the mainnet our lake covers.
**This needs a live RPC/stellar.expert re-check before any
implementation** — do not hard-code either address set without
resolving which (if either) is the one actually in current use.

### Gating (if resolved)

**Factory-anchored** — confirmed in source
(`contracts/xasset-orchestrator/src/orchestrator.rs::deploy_asset_contract`
does `deployer().with_current_contract(salt).deploy_v2(...)`, salt =
`sha256(symbol)`), structurally identical to how `soroswap`/`aquarius`
are already gated (ADR-0035 factory-descended registry): trust-root =
the orchestrator, children = each xAsset it deploys via its own
`Map<String, Address>` registry, gate `Matches()` on `IsFactory` for
orchestrator events and registered-child membership for xAsset events.

### Recommendation

**Hold**, and flag the address discrepancy for operator/RPC
confirmation before any further work — 4 lifetime events is not enough
signal to justify a decoder regardless of which address set is live.
Re-audit trigger: real CDP-open volume (the live EquitX API shows
hourly price updates, i.e. the *product* is alive even though on-chain
CDP usage isn't).

## 5 — Orbit CDP and Laina: not deployed to mainnet

Both were the subject of the deepest per-protocol web research (source
code, deploy-artifact repos, CI configs) and **neither has a mainnet
deployment today.** No lake census is possible without contract
addresses — a topic-symbol sweep for Orbit's documented vocabulary
(`Treasury`, `BridgeOracle`, and near-miss variants like `TREASURY`/
`treasury_sent`/`set_treasury_event`) also came back empty/irrelevant
(the few "treasury"-adjacent hits in the lake are unrelated one-off
contracts with 1–5 events each, not Orbit).

### Orbit CDP

- `github.com/zenith-protocols/orbit-contracts`: real source, but
  frozen since 2026-01-23 (5.5 months, no commits) — reads as
  "awaiting SDF-supported audit."
- `github.com/zenith-protocols/orbit-utils` (the team's own deploy-CLI
  repo) carries `testnet.contracts.json` / `futurenet.contracts.json`
  on every branch but **no `mainnet.contracts.json` on any branch** —
  strong first-party negative evidence.
- A 2026-04-16 Stellar dev-meeting reference put mainnet launch at
  "2-3 months out," gated on audit — consistent with still-pending as
  of this census.
- Architecture confirmed from source: single shared Treasury +
  BridgeOracle + Pegkeeper (no `TreasuryFactory`) — if/when it
  launches, it's curated-set-anchorable like FxDAO, not
  factory-anchorable.
- Testnet-only reference addresses are recorded in the local-only
  `docs/archive/discovery/dexes-amms/residual-defi-protocols.md`
  and in this census's research trail — **do not use them for
  anything**, they are not mainnet.

### Laina

- `github.com/laina-defi/laina` (GPL-3.0): actively maintained (699
  commits, pushed 2026-06-28), but the production frontend
  (`laina-de.fi`) and its only CI deploy workflow are hardcoded to
  `soroban-testnet.stellar.org` / the `Test SDF Network` passphrase.
  Every candidate contract address (`loan_manager` factory + 3
  `loan_pool` + 3 `insurance_pool` + a faucet) returns **404 on
  stellar.expert's public-network API** — testnet only.
- An SCF #34 grant ("Laina to mainnet!", ~$115K, awarded ~April 2025)
  funded a mainnet push, but the grant's own project description still
  says "We are also live on testnet but still developing" — no
  contradicting mainnet announcement found.
- Architecture confirmed from source: genuine factory pattern
  (`loan_manager::deploy_pool()` → `deploy_v2` + `pool_address_added`
  event), directly analogous to Aquarius's router-anchored gating —
  **if/when it launches, factory-anchored gating applies cleanly.**
  Event vocabulary is original (not a Blend fork): `positions_updated`,
  `total_balance_changed`, `available_balance_changed`,
  `accrual_changed`, `pool_status_updated`, `loan_created/updated/
  deleted`, `pool_address_added`, `bad_debt_auction_created/deleted`.

### Recommendation

**No action possible.** Both are real, maintained projects worth
re-checking — add a periodic "residual DeFi mainnet-launch watch" to
the recurring protocol-discovery sweep, or simply re-run this census
next time BACKLOG is groomed. Re-audit trigger: a `mainnet.contracts.json`
appearing in `orbit-utils`, or a Laina CI workflow / frontend pointing
at `Public Global Stellar Network`.

## 6 — Recommended order + per-protocol scope (for a later wave)

Restating the TL;DR: **nothing here justifies a native decoder right
now.** The table below is the answer to "if we had to pick an order
anyway" (per the task brief), ranked by decode-feasibility since
activity is ~0 for all three live-but-quiet protocols — to be used
*only* once a re-audit trigger fires.

| Order | Protocol | Best template | New hypertables (sketch) | `Class` / `IncludeInVWAP` | Why this slot |
|---|---|---|---|---|---|
| 1 | **Slender** | `internal/sources/sorocredit` (single hardcoded trust-root contract, curated-set, Map-named events, no registry needed) | `slender_positions` (deposit/withdraw/borrow/repay), `slender_liquidations` — 2 tables, same shape as `credit_positions`/`credit_settlements` | `ClassLending`, `IncludeInVWAP: false` | Cleanest schema, trivial gate, zero collision risk. Only blocker is that the pool is dormant — cheapest to build whenever it's worth it, but building it today would ingest 226 rows and then sit idle. |
| 2 | **EquitX** | `internal/sources/soroswap` or `internal/sources/aquarius` (factory-descended registry: orchestrator = trust root, xAssets = children) | `equitx_cdp_events` (Map-named CDP struct, tracks status Open/Frozen/Liquidated), `equitx_liquidations`, optionally `equitx_stake_events` | `ClassLending` (or a new synthetics class if the team wants xBTC/xETH/xUSDT to read distinctly from classic CDP debt — not required) | Factory pattern this codebase already knows how to gate; blocked on resolving the address discrepancy in §4 first (do that as a 30-minute RPC check, not part of a six-file PR). |
| 3 | **FxDAO (Vaults only** — token layer is a config change, not this list) | `internal/sources/band` (`ContractCallDecoder`, match on `(contract_id, function_name)` since the contract emits ~0 topic events) | Likely none needed if built at all — near-zero volume means a dedicated table isn't justified; if built, one `fxdao_vault_events` table | `ClassLending`, `IncludeInVWAP: false` | Highest-activity protocol overall, but the CDP-specific signal (the only reason to write a *native* decoder) is the thinnest of the three — the token layer, which IS worth capturing, needs no decoder at all. |
| — | Orbit CDP, Laina | N/A — not deployed | N/A | N/A | Can't scope a decoder for a contract that doesn't exist yet. |

**None of the three above should jump the queue ahead of anything in
ROADMAP §1** (the currently-open engineering tasks) — this whole
category remains explicitly parked per the 2026-07-06 decision brief.

## 7 — Housekeeping

- This doc supersedes the "decoder NOT needed" framing in
  `docs/archive/discovery/dexes-amms/residual-defi-protocols.md` for
  these five protocols specifically — that doc's broader "SEP-41 +
  AMM indexer already covers the issued-asset trading side" claim
  still holds for FxDAO's tokens *once* they're added to
  `watched_sep41_contracts` (they are not covered automatically —
  that list is operator-curated, not universal; see
  `internal/sources/sep41_transfers/dispatcher_adapter.go`).
- BACKLOG #63 / ROADMAP §2 Tier-2 "Residual DeFi re-eval" should be
  updated to point at this census with the verdict: **hold all five
  at asset-level (FxDAO only, via config); zero native decoders this
  wave.**
- No `protocol_contracts` rows, no config changes, no migrations were
  made as part of this investigation.
