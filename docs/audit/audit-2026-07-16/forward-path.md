# Stellar Index — concrete path forward (2026-07-16)

> Written mid-audit (money surface + recon complete; ingest/api/infra/plans chunks in flight). The prioritisation is grounded in confirmed findings + the operational reality (`recon/operational-reality.md`) and will be finalised at audit synthesis. Central goal: **get off the re-backfill treadmill** and onto a sequenced, verifiable forward path.

## The problem, named: why you constantly re-backfill

Three compounding causes, in leverage order:

1. **The re-derive trap (finding M1 / INV-3) is the keystone.** Served-tier derived values — `trades.usd_volume`, `asset_supply_history.*`, `oracle_updates.price`, `sep41_supply_events.amount`, ~15 protocol tables — are written with `ON CONFLICT (identity) DO NOTHING`, and the value is NOT in the identity. So a *corrected* re-derive silently no-ops (`rowsInserted=0`). The ONLY way to fix a wrong value is DELETE/TRUNCATE + re-derive from source. **Every data correction is therefore a full heavy backfill.** This is the mechanism behind the Phoenix base==quote re-derive, the reflector-dex quote fix, the redstone MXNe fix, the sep41 re-projection — all of ROADMAP §0.
2. **Latent correctness bugs keep surfacing → each triggers a re-derive.** The non-7dp decimals gap (M2, ~10 serve paths), the inverted FX snap (M3), the wall-clock supply timestamp (M4), CS-040 (G1). Each is "fine until noticed, then needs a re-derive to correct served history." The set is finite — fix them once and the discovery→re-derive cycle stops.
3. **The completeness mission inherently backfills, on a single contended host.** Genesis extension of `ledger_entry_changes` [2, 38.1M], movements [2, 25.1M], participant index, Phase-C tails — multi-day heavy jobs, one-at-a-time on R1 (and `run-heavy-job.sh` enforces the resource cap but NOT the singleton — a convention). Wrong-bucket aborts (the Phase-0 galexie-live vs -archive relaunch) add re-runs.

## The cure, in leverage order

### Keystone: make re-derives idempotent-corrective (kills the treadmill)
**Fix INV-3 so re-running a decoder over a range UPDATES wrong values in place instead of no-op'ing.** The ClickHouse lake already has this (ReplacingMergeTree version column); the defect is specifically the **Postgres served-tier writers**. Two viable shapes (a batched money decision — see below):
- **DO UPDATE on the derived columns** keyed on the existing identity (simplest; a re-project overwrites value columns). Risk: a re-project with a REGRESSED decoder would overwrite good values with bad — mitigate with a `derived_at`/`generation` guard (`DO UPDATE ... WHERE EXCLUDED.generation >= existing`), so only an intentional, newer re-derive wins.
- **Generation-stamped upsert** — add a monotonic `derive_generation` to the writers; re-derive bumps it; readers/CAGGs unaffected.
This converts "DELETE 237k Phoenix trades + re-derive from genesis (heavy, destructive)" into "re-project the affected range, corrected in place (bounded, safe)." It is the single highest-leverage change in the whole backlog. **Money + migration class → strongest verification panel, proven-red DB-backed test (insert wrong value → re-derive → assert corrected).**

### Then: land the known correctness fixes as ONE batch, re-derive ONCE
Fix the money-value findings at a chokepoint, not per-site, so you never re-discover them:
- **Decimals normalization at the reader chokepoint** (M2) — one place applies `AdjustPrice`, covering all ~10 serve paths, instead of per-handler. Also removes the `10^7` market-cap hardcodes.
- **FX-leg snap inversion** (M3), **supply snapshot ledger-close timestamp** (M4), **`/v1/changes` money-as-string** (M7), **CS-040 decimals unification + delete the dead `windowUSDVolume`** (G1).
- With INV-3 fixed, the one-time re-derive that lands these corrections is *incremental*, not a from-genesis rebuild.

### Then: finish completeness with a definition-of-done, switch to continuous verification
- Sequence the remaining heavy backfills as a finite, resumable plan (Phase 0 → genesis-extend `ledger_entry_changes` → movements → participant/Phase-C), each idempotent + windowed under `run-heavy-job.sh`.
- Stand up the **`verify-lake` timer** (the composed contiguity+hashchain+substrate gate already exists, proven on the live zone) so completeness is *continuously proven*, not re-established each session. The two-axis verdict (lake vs served) is already wired API + explorer — surface it and let it be the standing signal.
- **Definition of done for backfills:** `verify-lake` green over [2, tip] + `completeness_snapshots` `lake_complete=true` for all sources. After that, backfills are exceptional, not routine.

### Structural: stop heavy jobs starving live ingest
- The parked **#69** structural pair — a Postgres **read replica** (or per-sink write prioritisation) so a re-derive can't starve live ingest — is what makes re-derives *safe to run anytime* instead of a scheduled-around-Phase-0 event. Pair with an actual **singleton lock** in `run-heavy-job.sh` (today it's convention only — a real gap given Phase 0 is running now).

## The sequence (operational, respects the deploy freeze)

1. **NOW — finish the audit** (chunks 2–4 + synthesis): full findings, coverage ledger, executive summary.
2. **Fix CI baseline** (folded into remediation): `make docs-postman` regen commit (clears openapi lint); the two GH secrets are [OP] for you (`PROM_TARBALL_SHA256` / `GITLEAKS_TARBALL_SHA256` — values in `operational-reality.md`). Then main CI is green and remediation PRs are judged cleanly.
3. **Remediation campaign** off current main (`4d034432`), commit-merge-repeat, **INV-3 fix FIRST** (keystone), then the money-correctness batch, then the rest — each panel-verified with proven-red DB tests, migrations serialised one-owner-per-wave. NO deploy/re-derive on R1 (Phase-0 freeze) — migration *files* are written+tested+committed; *running* them is the post-freeze step.
4. **Business-value decisions** batched for you with options + a recommendation (below) — not guessed.
5. **When Phase 0 finishes:** cut ONE release (v0.17.0) with the verifiers + audit fixes → deploy → run the corrected re-derives ONCE (now incremental, thanks to step 3) → verify-lake green → enable the standing timer.
6. **Forward:** the read-replica / anti-starvation structural fix, then re-backfills are the exception.

## Money / business-VALUE decisions to bring to you (batched, not guessed)
These change served numbers or ops posture and have no defensible auto-default — I'll collect the full set at synthesis, but the shape:
- **INV-3 fix mechanism:** DO-UPDATE-with-generation-guard vs versioned-table. (Recommendation: DO UPDATE + `derive_generation` guard — least schema churn, safe against regressed re-derives.)
- **`min_usd_volume`:** currently 0 on R1 (on-chain-era stop-gap). Restore to 10000 once CEX volume is sustained? And the CS-040 decimals fix is a prerequisite before raising it.
- **Retention / serve-window policy** for the served tier vs the genesis-complete lake claim (the two-axis verdict makes this honest; do you want a served-tier deep-history backfill or lake-backed deep reads?).
- **Genesis backfill scope** — extend `ledger_entry_changes`/movements to genesis [2, …] now, or bound to the AMM/P18 era? (Cost vs completeness-claim tradeoff.)
- **Peg set + stablecoin proxy** thresholds (the aggregator maps that drive served fiat prices).

## What this buys you
Once INV-3 is fixed and the known corrections are batched-and-landed, a data bug becomes a *bounded incremental re-project*, not a from-genesis rebuild — and continuous `verify-lake` means you *know* you're complete instead of re-checking by re-backfilling. That is the exit from the treadmill.
