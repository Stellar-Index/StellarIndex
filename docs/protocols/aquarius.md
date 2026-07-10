# Aquarius — contract & event verification

> **For the Aquarius team:** this is how Stellar Index attributes Aquarius
> pools and the router. The pool set is now pinned (router-anchored,
> cross-checked against your registry API); the remaining asks are
> ratification + one identification question — see the bottom of
> "Verification 2026-07-05".
>
> - **Enumeration method:** router-anchored (the router's own events
>   announce every pool in their data payload), cross-checked against the
>   protocol's public registry API and per-contract WASM hashes — see
>   "Verification 2026-07-05" below.
> - **Last verified:** 2026-07-05 (r1 lake, tip ledger 63,343,398).
> - **Gate status:** ✅ GATED (ADR-0035/0040, shipped 2026-07-05): trust
>   root = the router; in-code seed = the 332 registry pools; live
>   `add_pool` events self-register new pools. Deploy precondition: the
>   re-derive + foreign-row cleanup below.

## Router

| Role | Contract | Lake events |
|---|---|---|
| Liquidity-pool router | `CBQDHNBFBZYE4MKPWBSJOPIYLW4SFSXAXUTSXJN76GNKYVYPCKWC6QUK` | `swap`, `deposit`, `withdraw`, `config_rewards` ×52,492, `add_pool` ×338, `commit_upgrade`, `apply_upgrade`, `set_protocol_fee`, … (counts re-verified 2026-07-05; the 2026-06-12 pass saw only 6 `add_pool` because the lake's event backfill was still partial) |

The router is the user entry point; it proxies to the correct pool, which
emits the trade event.

## Pools

Pinned: **332 pools** — `aquarius.MainnetPools` in code, reproducible from
the lake (decode `data.vec[0].address` of the canonical router's
`add_pool` events) and byte-identical to the protocol's registry API. See
"Verification 2026-07-05" for the census + flagged non-registry emitters.

## ✅ Open question (2026-06-12) — RESOLVED 2026-07-05

The two questions this page used to carry are answered by the lake itself:

1. **Authoritative enumeration** = the router's `add_pool` events. The
   2026-06-12 pass saw only 6 because the lake's event backfill was
   partial; the full lake holds 338 announcing exactly the registry set.
2. **Is `trade` aquarius-only?** NO — the lake contains a parallel
   non-registry router deployment, a foreign-WASM look-alike fork, and a
   3-topic `trade` emitter. Topic-only matching was injectable; hence the
   identity gate.

## Events decoded

| Source (topic[0]) | Where it lands |
|---|---|
| pool `trade` | `trades` (source=aquarius) |
| pool `deposit` / `withdraw` / `update_reserves` / `reserves_sync` | liquidity / reserves tracking |
| router `add_pool` | pool registration (gate fan-out — registers the pool in the contract-identity registry; emits no flow/trade row) |
| pool rewards-gauge (12 kinds, incl. router-side `config_rewards`) | `aquarius_rewards_events` (migration 0099) |
| router/pool governance + upgrade (8 kinds) | `aquarius_admin` (migration 0100) |

## ✅ Rewards-gauge + governance topics — decoded (ROADMAP #89, closed 2026-07-10)

A full topic census against the gated set (332 pools + router) found
**20 real, distinct topics with no decoder** — a rewards-gauge
subsystem (`pool_state` 339,712 · `claim_reward` 263,673 ·
`set_rewards_config` 47,530 · `position_update` 12,403 · bare
`deposit` 7,213 · `claim_fees` 5,056 · `rewards_gauge_claim` 1,121 ·
bare `claim` 168 · `rewards_gauge_schedule_reward` 64 ·
`set_rewards_state` 25 · `rewards_gauge_add` 12) and a governance
surface (`apply_upgrade` 706 · `commit_upgrade` 705 ·
`set_privileged_addrs` 173 · `apply_transfer_ownership` 48 ·
`commit_transfer_ownership` 48 · `enable_emergency_mode` 35 ·
`disable_emergency_mode` 35 · `pool_gauge_switch_token` 31). `transfer`
/ `approve` / `mint` / `burn` are the SEP-41 token layer, correctly
out of scope. All 20 (plus a 12th rewards-family bonus, the
router-side `config_rewards` this page already listed at line 23
above but which had no decoder either) are now decoded into two new
hypertables, `aquarius_rewards_events` (0099) and `aquarius_admin`
(0100). Full per-topic wire shapes, gating, and provenance caveats
(AquaToken's contract-source repo is no longer public — every field
below is reverse-engineered from real lake bytes, not a cloned Rust
source): `internal/sources/aquarius/README.md`.

## ✅ Pool enumeration — answered (2026-06-12, lake-derived)

The Dune community dashboard (claw, "Aquarius Base Metrics") revealed the
derivation: **the router's own `swap`/`deposit`/`withdraw`/`add_pool`
events carry the pool address in the event data** (`data.vec[0]`). The
router is the factory-equivalent trust root — fan-out via event *data*
rather than creation events. Replaying that derivation over our lake:

- **174 distinct pools** from 56,769 router events (zero parse errors);
  **all 174 also emit pool-level `trade` events** (174/174 overlap with
  the trade-emitter set).
- **4 additional contracts** emit the full Aquarius pool signature
  (`trade` + `update_reserves` + `pool_state` + `rewards_gauge_add` +
  `claim_fees`, all from ledger ~61.9M+) but haven't appeared in router
  event data yet — recent pools, pending first routed swap.
- **Authoritative set: 178 pools** (list reproducible from the lake:
  decode `data.vec[0].address` of router-emitted events from
  `CBQDHNBFBZYE4MKPWBSJOPIYLW4SFSXAXUTSXJN76GNKYVYPCKWC6QUK`).

**Gate design (ADR-0035 variant):** trust root = the router; a pool is
registered when the ROUTER's event data announces it (+ the 4
signature-matched recents pending router confirmation). Remaining
question for the team narrows to: ① confirm the router is the sole entry
point that announces every pool (incl. how pools are created — we see
only 6 `add_pool` events vs 178 pools, so most predate our lake or
register otherwise), and ② is the `trade` topic emitted by anything that
is NOT an Aquarius pool?

## ✅ Verification 2026-07-05 — gate evidence (lake tip 63,343,398)

Full re-enumeration of everything emitting the aquarius trade shape, plus
the router fan-out derivation, plus two independent cross-checks (the
protocol's public registry API and per-contract WASM code identity).

### The trade-shape emitter census

- **346 contracts** have EVER emitted a 4-topic `trade` event
  (3,986,241 events total, 52.70M → tip; up from 177 on 2026-06-12 —
  pool creation is continuous).
- **1 contract** (`CDX3TMDOQ66A7UTZWDSEGPS3BVGYWHJRCXVBCMEXKLRF6X3JQNBI3UMN`)
  emits a 3-topic `trade` look-alike (505 events, 60.86M → 61.73M) — never
  decodable (wrong arity), now also identity-rejected.

### The trust root: router `add_pool` events == the protocol registry

The canonical router `CBQDHNBFBZYE4MKPWBSJOPIYLW4SFSXAXUTSXJN76GNKYVYPCKWC6QUK`
has emitted **338 `add_pool` events announcing 332 distinct pools**
(`data.vec[0].address`; zero parse failures). Cross-check:
`https://amm-api.aqua.network/pools/` serves **exactly 332 pools — the two
sets are byte-for-byte identical**. The router IS the protocol's registry,
so the gate anchors on it:

- in-code seed: `aquarius.MainnetPools` (the 332);
- live fan-out: any future `add_pool` from the router registers the
  announced pool before its first trade (blend-style);
- provenance persistence: `protocol_contracts` warm + live-upsert hook.

Of the 346 trade emitters, **338 are router-announced** (283 by the
canonical router; see flagged sets below for the rest) and **73 announced
pools have not yet traded directly** (routed-only or new).

### WASM cross-check

Every pool announced by the canonical router (and its parallel deployment,
below) that still has a live contract instance runs one of exactly three
pool code hashes — the aquarius pool families
(`AE0DA5A8…` ×318, `F1077E0B…` ×55, `12FCA5A7…` ×31, matching the
constant-product / stableswap / concentrated split). The two router-WASM
deployments share code hash `06F4207B…`.

### ⚠️ Flagged — excluded from the gate (NOT silently dropped)

| Set | Contracts | Trades | Evidence |
|---|---:|---:|---|
| Parallel router deployment `CA7RQDMMV6E53P5EDZA5GPWBZ33AMW2ZNO42XLI2RGRIAP4QXIARUOJQ` + its 72 announced pools | 73 | 1,302 (52 pools traded; 52.90M → 62.26M) | Runs the SAME router WASM and its pools run genuine aquarius pool WASM, but **none of its 72 pools appear in the protocol's registry API**. Someone (possibly the team — staging?) operates a second full deployment. Excluded pending team confirmation; ask ③ below. |
| Look-alike router `CCPHUHQYFOJJ6WQUGUYHHPJYQGFLRQHJJTRJNWQG54MHCHPRFLWQI7SE` + its 7 announced pools | 8 | 187 (3 pools traded; 59.50M → 60.67M window) | Pools run FOREIGN WASM (`8E06DDCE…`/`435FD892…`, not any aquarius family); the router's own instance has since been evicted from live state. A short-lived fork — exactly the CS-026 injection shape. |
| Pre-genesis rehearsal pools (8 emitters) | 8 | 35 (all inside 52,702,424 → 52,717,661, BEFORE source genesis 52,728,375 and before the router's first announcement) | Never router-announced; all 8 instances gone from live state. Launch-rehearsal artifacts. |

Post-gate + re-derive, the served `trades` table keeps
**3,984,212 / 3,986,241 = 99.95%** of the raw shape-matched events; the
1,524 excluded rows are the flagged sets above (the 505 3-topic events
never decoded in the first place).

### Operator rollout (ADR-0040 §2 — deploy preconditions)

1. Deploy the gated build (the in-code seed makes `seed-protocol-contracts`
   optional for aquarius — PG `soroban_events` is capture-scoped and only
   holds recent `add_pool` rows anyway; live announcements upsert
   provenance into `protocol_contracts` from day one).
2. Re-derive: `projector-replay -source aquarius -from 52728375` (under
   `run-heavy-job.sh`). Replay is upsert-only, so ALSO delete the flagged
   rows: export the flagged contracts' trade-event identities from
   ClickHouse (`SELECT ledger_seq, tx_hash, op_index, event_index FROM
   stellar.contract_events WHERE contract_id IN (<flagged 88>) AND
   topic_0_sym = 'trade'`), compute the served-tier op index as
   `canonical.FanoutOpIndex(op_index, event_index)`, and delete the
   matching `(ledger, tx_hash, op_index)` rows from `trades WHERE source =
   'aquarius'` via a staged temp table — do NOT delete by `(ledger,
   tx_hash)` alone (an aggregator tx could legitimately touch both a
   registry pool and a foreign pool).
3. Verdict watch: one green `compute-completeness -ch` cycle for aquarius.

### Remaining asks for the Aquarius team (ratification, no longer blockers)

1. Confirm the router (`CBQDHNBF…`) is the sole official deployment and
   that `add_pool` announces every pool (our API cross-check says yes).
2. ③ What is `CA7RQDMMV6E53P5EDZA5GPWBZ33AMW2ZNO42XLI2RGRIAP4QXIARUOJQ`?
   It runs your router WASM, actively receives `config_rewards`, and has
   announced 72 pools that are not in your API. If it's yours (staging /
   legacy), we'll keep it excluded but documented; if not, it's a fork
   worth knowing about.
