---
adr: 0040
title: Completing contract-identity gating (phoenix, defindex, aquarius, comet)
status: Accepted
date: 2026-07-02
supersedes: []
superseded_by: null
---

# ADR-0040 — Completing contract-identity gating (phoenix, defindex, aquarius, comet)

- **Extends:** [ADR-0035](0035-factory-anchored-contract-gating.md)
- **Closes:** CS-026 (audit 2026-06-30) — the last four topic-shape-gated decoders

## Context

ADR-0035 established that Soroban decoders must gate `Matches()` on
**contract identity**, not topic bytes: any pubnet contract can emit a
colliding topic shape and inject fabricated trades under our source
attribution. `blend` (childgate, factory-descended) and `soroswap`
(pair/factory registry) are gated. Four sources still match on topic
bytes alone: **phoenix, defindex, aquarius, comet**.

The blocker register (audit-remediation-operator-actions.md) framed the
remainder as "waiting on team/operator-confirmed contract data". The
per-protocol verification pages show the picture is better than that:

| Source | Enumeration state (docs/protocols/) | Gate blocker |
|---|---|---|
| phoenix | ✅ 11 pools + factory + multihop + 3 stake contracts, RPC `query_pools()` + lake-verified activity | none — curated set exists |
| defindex | ⚠ REVISED 2026-07-02: lake emitters grew to 88+22 vs the 57 verified 2026-06-12, and `create` events don't carry vault addresses (the page's open question) — the deploy-graph can't verify the growth | §3-style enumeration cross-check |
| aquarius | ⏳ pool set "not yet pinned" | enumeration work |
| comet | ❌ no page; **no factory namespace** (shared Balancer-v1 `("POOL",…)` topics) | gate *design* |

So the work splits into (1) shipping the two implementable gates, (2) an
enumeration procedure for aquarius, (3) a new gate *mechanism* for comet.

## Decision

### 1. Gate taxonomy — three sanctioned mechanisms

1. **Factory-descended registry** (existing; blend/soroswap):
   `childgate.Registry` seeded from `protocol_contracts` +
   hard-coded factory IDs; live `deploy`/`create` events self-register
   new children via the registry hook.
2. **Curated-set registry** (new use of existing machinery): for
   protocols whose creation events PRECEDE the lake's earliest ledger
   (phoenix — factory `create` events are pre-50.46M so live
   self-registration never fires), the same `childgate.Registry` is
   used with `WithSeed(curatedPools)` + `WithFactories(factory)` so a
   *future* creation event still registers. The curated set is the
   protocol page's enumerated list, seeded via
   `seed-protocol-contracts`. Fail-closed: an unlisted pool's events
   are not attributed — and become **recognition gaps** (ADR-0033
   Claim 2a), so a missing pool is *visible*, not silent.
3. **WASM-code-hash gate** (new; comet): where no factory namespace
   exists, gate on the contract's *code identity*: `Matches()` accepts
   a contract only if its wasm hash is in the audited set. Resolution
   order:
   - a `protocol_contracts`-seeded allowlist of known comet pools
     (same registry seam as #2), PLUS
   - a `wasm_hashes` column/set for the source: at recognition/re-derive
     time, an unseeded contract emitting comet-shaped topics is checked
     against `ledger_entries_current` (ADR-0039 reader) for its
     `contract_code` hash; a match against the audited Balancer-v1
     Comet hash set (from `docs/operations/wasm-audits/comet.md`)
     auto-registers it (with the registry hook recording provenance
     `wasm-hash`).
   The wasm-hash check runs OFF the hot path (recognition audit +
   an operator sweep), not per-event: live ingest consults only the
   registry; the sweep keeps the registry current. This bounds the
   hot-path cost to a map lookup, identical to childgate today.
   **Caveat named openly:** a fork that deploys byte-identical
   Balancer-v1 code IS the same code — the wasm gate attributes it as
   comet. For a permissionless Balancer clone that is arguably
   correct-by-definition (comet *is* the code, not a brand); the
   protocol page must say so.

### 2. Rollout order and preconditions

Per source, in this order (phoenix → defindex → aquarius → comet):

1. **Seed**: add the enumerated set to `seed-protocol-contracts`
   (idempotent), with factory IDs hard-coded in the source package
   (`phoenix.MainnetPoolFactory`, `defindex.MainnetFactories`, …) the
   way `blend.MainnetPoolFactories` is.
2. **Gate the decoder**: `Matches()` requires `reg.Has(contractID)`
   (or `IsFactory` for creation events). Constructor grows a
   `childgate.Option` variadic exactly like blend's.
3. **Wire all five lockstep sites** — the
   `internal/pipeline/lockstep_ast_test.go` guard plus
   `TestReconciliationCatalogue_OracleSourcesOptOut` already fail CI on
   a missed edit; the reconciliation catalogue entry gains
   `factories`/`creationSym` so the daily verdict preseeds correctly
   (CS-085 note: preseed still reads PG; the CH-native preseed is a
   follow-up there, not here).
4. **Lake re-derive** for the source (`projector-replay` /
   `ch-reproject`) so history is re-attributed under the gate — the
   deploy precondition memorialised in the ADR-0035 rollout: gate code
   must NOT deploy before its seed exists on r1, else live ingest
   fail-closes on every pool.
5. **Verdict watch**: one full `compute-completeness -ch` cycle for the
   source must return `complete=true` before the gate is called done.

### 3. Aquarius enumeration procedure

Aquarius pools all share the aquarius AMM WASM and are deployed by the
protocol's router/factory chain. Enumerate from the lake, not from
docs: every contract that has EVER emitted an aquarius-shaped
`trade`/`deposit`/`withdraw` event that our ungated decoder attributed
(`SELECT DISTINCT contract_id FROM stellar.contract_events WHERE …`),
cross-checked two ways: (a) each candidate's creation op should chain
to the same deployer set; (b) each candidate's wasm hash should fall in
a small set (the aquarius pool code). Candidates failing both checks
are *evidence of the injection risk this ADR closes* and are excluded +
reported. Output: `docs/protocols/aquarius.md` pool table (the page's
missing piece), then mechanism #2.

### 4. What stays out of scope

- Narrow-coverage downstream filtering (the CLAUDE.md workaround) stays
  documented until each gate lands, then is deleted per source.
- sep41 firehose gating — different domain (watched-set, ADR-0031).
- The CH-native childgate preseed (CS-085) — tracked separately.

## Consequences

- The injection vector closes source-by-source with a visible
  audit artifact per step (seed rows, gated decoder tests, re-derive
  logs, verdict green).
- Fail-closed + recognition-gap visibility means an incomplete curated
  set shows up as `complete=false` with named contracts — never as
  silently attributed foreign trades.
- Comet's wasm-hash mechanism adds a new trust-root type; the audited
  hash set lives in `docs/operations/wasm-audits/comet.md` and its
  changes are review-gated like any code change.
- Protocol-team confirmations (phoenix pool list, defindex vault
  enumeration) become *belt-and-braces ratification* of lake-derived
  evidence rather than blocking inputs.

## Implementation tracking

Phoenix gate: shipped 2026-07-02 (curated-set, board #32).
Aquarius gate: shipped 2026-07-05 — the §3 enumeration found the router's
`add_pool` events announce a pool set byte-identical to the protocol's own
registry API (332 pools), so the gate is router-anchored (mechanism 1 fan-out
via event DATA + mechanism 2 in-code seed for history); a parallel
same-WASM router deployment (72 pools), a foreign-WASM look-alike (7
pools), and 8 pre-genesis emitters were excluded + flagged — see
docs/protocols/aquarius.md "Verification 2026-07-05".
Defindex gate: shipped 2026-07-05 via the §3 multi-proof classification
(creation-tx correlation, create-body membership, team-published WASM
hashes, team Dune registry): 101/110 emitters verified → curated seed;
9 no-proof emitters excluded + flagged — see docs/protocols/defindex.md
"Verification 2026-07-05". Both gates await their operator halves
(re-derive + foreign-row cleanup + verdict watch, tracked in
docs/operations/audit-remediation-operator-actions.md).
Comet: still open — needs the wasm-audit hash set confirmed first
(`stellarindex-ops wasm-history` walk exists;
`docs/operations/wasm-audits/`).
