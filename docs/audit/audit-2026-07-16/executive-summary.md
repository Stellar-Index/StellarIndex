# Executive summary — Stellar Index audit, 2026-07-16

Cold, adversarial, systematic audit of the whole repo + the plan surface, at commit `4d034432` (engine code byte-identical to the audited `f84e2d0b`; only the two-axis explorer UI + CHANGELOG differ). Four bounded chunks, 627 agents total, every finding skeptic-verified. Findings ranked exposure-then-severity, tagged against the **running R1 config**, not code defaults.

## The one thing to know
**The core data path is sound, but the product's central promise — a *verified* explorer that serves *correct* numbers — is not yet met, on two fronts that reinforce each other:**
1. **Served values can be silently wrong** — a systemic non-7dp decimals gap (~10 serve paths), an inverted FX triangulation leg, wall-clock supply timestamps, and float64 on money boundaries — and
2. **the system largely can't detect or recover from it** — a re-derive trap that turns every correction into a full re-backfill (INV-3, the root of your treadmill), silent write-losses in the projector/sink/backfill, a completeness verdict that fails *open*, and a detection layer where runbook links never render, wrong-number alerts aren't wired, and a page alert literally can't fire.

There is **no CRITICAL** finding — no money-creation/loss exploit, no auth bypass, no injection (i128 discipline, bind-parameterization, contiguity+hash-chain checks all hold). The risk is **correctness + detectability**, exactly where a pricing/explorer product lives.

## Counts (confirmed, skeptic-verified; ~110–120 distinct after cross-chunk dedup of 168 raw)
| Exposure \ Severity | HIGH | MEDIUM | LOW/INFO |
|---|---|---|---|
| **LIVE** (default R1 config today) | ~25 | ~45 | ~30 |
| **GATED** (needs a flag/venue/mode on) | ~5 | ~15 | ~5 |
| **BRANCH / GATE** | — | ~4 | ~6 |

No criticals. The ~25 LIVE HIGHs are the launch/urgency set. (Exact per-finding rows: `findings.md`.)

## The LIVE-HIGH set, grouped
- **Served-value correctness (money):** INV-3 re-derive trap (×3 corroborated, the keystone); non-7dp decimals missing on ~10 serve paths; FX-leg snap inverted; supply snapshots stamped wall-clock not ledger-close; published-VWAP outlier filter is masking-vulnerable σ not MAD; `/v1/changes` serves money as JSON floats.
- **Silent data loss (feeds the re-backfill treadmill):** projector advances its cursor past swallowed sink errors → permanent loss for the sole-writer sep41 supply domain; archive backfill silently skips up to 65k trailing ledgers; before-image current-state reads resurrect deleted entries; 8-worker last-writer-wins persists non-final intra-ledger balances; the per-event sink path has no durability at all.
- **Completeness/verdict honesty:** compute-completeness writes `complete/lake_complete=true` after swallowing a scan error (fail-open); served `complete` can read true while a projection drop below tip−1.5M is invisible.
- **Availability (unauth DoS):** no per-request timeout on `/vwap`,`/twap`,`/ohlc` and the entire explorer package → the shared 8-connection ClickHouse pool is exhaustible by anonymous FINAL-scan floods; no pool-level `statement_timeout`.
- **Detectability (the 3am story):** `runbook_url` is a label not annotation on 266/270 alerts so no page ever shows its runbook; `run-ch-supply.sh` exits 0 on failure; three persist-failure paths emit no metric; a page alert can never fire (nil Registry); no self-check on the SEV-1 paging path; the "served a wrong number" alerts aren't scheduled/wired; no CI tripwire on a red main.

## Two systemic root-causes worth fixing first (highest leverage)
1. **INV-3 — derived money/supply values use `ON CONFLICT DO NOTHING` with the value outside the key.** A corrected re-derive silently no-ops, so *every* data fix requires DELETE + re-backfill. This is the mechanism behind the constant re-backfilling. Fix: idempotent-corrective writers (DO UPDATE + generation guard). See `forward-path.md`.
2. **Durability/detection is fail-open, not fail-closed.** Cursors advance past unpersisted writes; the completeness verdict and the paging path both fail open. Fix: propagate sink errors + advance cursors only to fully-committed ledgers; make the verdict + paging fail-closed and self-checked.

## Plans / CI (you flagged these)
- **CI main has been red 24h+** — root-caused: two missing GH secrets (`PROM_TARBALL_SHA256`, `GITLEAKS_TARBALL_SHA256`) leave the promtool-rule + gitleaks gates *dead at install* (decorative gates that never run — a real security/ops regression), plus a stale generated Postman collection. Secret values are in `operational-reality.md`; the Postman fix is the first remediation commit.
- **Plan validity:** the ROADMAP's "Alg-2 pool-internal readers" work item is **obsolete** (the code's own 2026-07-06 verdict superseded it — the fix is an operator seed, not new readers); #72's PROJECTION fix is **correctly rejected** by perf-todo (ROADMAP stale); #39 decommission **under-enumerates** live `soroban_events` readers; the two-axis explorer UI is **done** (landed 4d034432, reviewed sound). Six CLAUDE.md/arch-doc claims are confirmed stale.

## Launch-readiness verdict
**Not launch-ready as-is** — but the gap is a well-bounded remediation, not a rearchitecture. Priority before any public launch: the ~25 LIVE-HIGHs, led by INV-3 (keystone), the served-value-correctness batch, durability/cursor honesty, the completeness fail-open, the explorer-pool DoS, and the detection layer (so a wrong number or a data loss actually pages someone). The deploy freeze (Phase 0 on R1) means all of this is code-only now; the corrected re-derives run once, post-freeze.

## What this audit did NOT cover (coverage honesty)
Two chunks hit their wave cap while still finding (`converged:false`) — money and ingest/storage are **deep but not exhaustive**; a further pass would likely surface more in the same failure classes. The **`test/` tree** (integration/load/chaos/fixtures, 127 files) was not deep-audited as a unit — "do the harnesses catch these bugs?" is unverified (two test-vacuity findings suggest a dedicated TST pass is warranted). Operational **runbook bodies** and a handful of small utility packages (`stellarrpc`, `xdrjson`, `nettools`, `version`, `httpx`, `sdexclaim`) were not deep-read. Full accounting: `coverage-ledger.md`. Live R1 state (config/data) was inferred from docs — several items are `[OP]`/verify tagged, not code-verified.
