# Audit 2026-05-26 — Plan Closure Summary

**Status at plan-completion (2026-05-26):** plan ready; execution
not started.

This summary describes what the plan covers, so a reviewer can
verify the audit is set up to find everything that matters before
any check is run.

## What the plan delivers

- 13 top-level control documents
  ([00..12](.) + [README.md](README.md) + this file)
- **35 workstreams** (vs 26 in the 2026-05-12 baseline)
- 18 inventory artefacts (including `file-coverage.tsv` with one
  row per tracked file — 2104 files, none unassigned, 1
  `unknown` file_kind)
- 7 evidence ledgers + 2 evidence sub-directories
- 40 enumerated journeys (data-plane + operator-plane +
  adversarial)
- ~22 mandatory R1 live-probe surfaces
- ~6200 lines across the 75 markdown documents
- A regeneration script
  ([inventory/generate.sh](inventory/generate.sh)) so a reviewer
  can rebuild the inventory from a fresh clone

## How the plan was hardened (three passes)

### Pass 1 — Framework + inventory

- Read the 2026-05-12 baseline structure (26 workstreams, 13
  control docs).
- Generated current repo inventory: 2104 files, 23 source
  packages, 45 migrations, 29 ADRs, 10 workflows, 11 external
  adapters, 50 API handlers, 40 storage adapters, 387 test
  files, 28 integration tests.
- Identified deltas: 53 commits since baseline. Five new source
  packages (cctp, defindex, rozo, sorobanevents, soroswap_router),
  17 new migrations (0029..0045), three new ADRs (0027 cold-tier,
  0028 RWA, 0029 soroban_events landing zone), six new operator
  subcommands (per-source backfills), one new external adapter
  (chainlink), the back-pressure / cursor-coherence correctness
  fix (rc.80), the trailing-edge tolerance fix (rc.81),
  verify-archive lifecycle changes (rc.75/rc.76/rc.81).

### Pass 2 — Workstream design + coverage matrix

- Re-scoped W01..W26 to fold in all post-baseline files.
- Added W27..W35: nine new workstreams targeting the deltas
  above. Notably:
  - W27 (soroban_events landing zone — ADR-0029)
  - W28 (back-pressure / ctx-shutdown semantics — the audit's
    most critical correctness gate)
  - W29 (per-source backfill subcommands — six)
  - W30 (cold-tier read path — ADR-0027)
  - W31 (RWA asset representation — ADR-0028)
  - W32 (customer webhook fanout — including SSRF)
  - W33 (Stripe billing integration — including replay defence)
  - W34 (verify-archive Type=notify lifecycle)
  - W35 (granular-coverage mission — every event for every
    Soroban source)
- Designed cross-cutting interaction classes (W26): 14 baseline
  classes + 6 new (ADR-0029 seams, backfill seams,
  trailing-edge handling, BackfillSafe gating, Stripe webhook
  chain, customer webhook fanout chain).

### Pass 3 — Document writing

- Wrote all 13 top-level control docs from scratch (no copy
  from baseline — every claim re-derived from current code or
  marked as a check item).
- Wrote 35 workstream sub-plans. Each carries scope, inputs, a
  numbered check list, evidence expectations, and closure
  criteria.
- Wrote the per-loop protocols (per-file, per-decoder,
  per-migration, per-route, per-source, per-alert, per-doc,
  memory-truth) explicitly in [02-protocol.md](02-protocol.md).
- Wrote 7 evidence-ledger documents with stable ID taxonomy.
- Wrote 18 inventory artefacts via `generate.sh`.
- Seeded 3 findings already discovered during the plan-writing:
  - F-0001: r1 root partition 100% full (high)
  - F-0002: memory `feedback_reenable_trades_compression`
    obsolete (low)
  - F-0003: migration deploy operator-manual (medium)
- Cross-referenced every memory entry that affects audit
  discipline: `feedback_r1_sql_quoting`,
  `feedback_no_pipe_through_tail`, `feedback_verify_bg_exit_lies`,
  `feedback_cold_tier_premature_enable`,
  `feedback_prewarm_handler_drift`,
  `feedback_migrations_not_auto_deployed`,
  `feedback_fd2_wrap_drain_on_exit`, etc.

### Pass 4 — Gap-fill review

- Cross-checked the file-coverage TSV: 0 unassigned files (was
  5; fixed by extending generate.sh's classifier).
- Cross-checked `file_kind` assignment: 1 file with `unknown`
  remains (a `.gitkeep` placeholder).
- Workstream coverage by file count:
  W17 204, W07 196, W16 187, W11 186, W18 184, W24 156, W14
  156, W09 149, W08 96, W02 90, W13 88, W19 69, W10 59, W15
  57, W12 46, W06 42, W03 37, W05 31, W25 25, W01 20, W29 9,
  W27 7, W34 5, W32 4, W04 1.

  (The new workstreams W27..W35 have low direct-file counts
  because they target focused new code; their checks span the
  whole codebase via cross-references.)

### Pass 5 — Adversarial sharpening

- Re-read each workstream with adversarial framing: "what would
  a hostile party exploit here?"
- The attack tree ([10-attack-tree.md](10-attack-tree.md)) has
  ~50 leaf nodes across 8 categories (data integrity, identity
  attacks, availability, operability, observability,
  multi-region, granular-coverage, supply-chain) — 12 of which
  are NEW since baseline (CCTP/Rozo malformed inputs, soroban_events
  poisoning, cold-tier read attacks, customer webhook SSRF +
  DNS-rebinding, Stripe webhook replay + refund-bypass,
  granular-coverage gaps, GH Actions CDN flakiness, ZFS pool
  exhaustion as a class of resource attack).
- The severity rubric carries six adversarial multipliers
  (discovery-cost asymmetry, public mention by competitor,
  money-touching, boundary surface, repeat offender,
  cross-cutting). Each tips severity one tier higher.

## What the plan does NOT do

- It does not execute any check. Execution is the next phase.
- It does not collect evidence beyond the three seed findings.
- It does not run any R1 probe (the protocol is documented; the
  transcripts will follow in a separate execution session).
- It does not commit findings beyond the seeds.

## Closure criteria (when can execution stop?)

Per [00-plan.md](00-plan.md) §Closure Criteria:

- every workstream W01..W35 has terminal status in the tracker
- every mandatory pass is terminal
- `inventory/file-coverage.tsv` has no `todo`
- every finding has evidence references and a disposition
- every exclusion is explicit and re-entry-evidence-listed
- the remediation plan maps to every open finding
- the CG/CMC parity matrix is fully filled (no blank rows)
- the Stellar coverage matrix is fully filled
- the granular-coverage register has one row per (source,
  on-chain-event) tuple
- at least one full R1 probe transcript is in
  `evidence/r1-probes/`
- W26 confirms every required cross-file interaction class has
  at least one fully-traced `XFI-####` row

## Next step

Begin execution. Recommended start order:

1. **W21 (r1 live state)** — capture probe transcripts, address
   F-0001 (root disk).
2. **W01 (snapshot + memory truth)** — establishes the baseline
   from which all subsequent evidence is dated.
3. **W27 + W28** — the audit's most critical correctness gate
   (cursor coherence; trailing-edge tolerance).
4. **W35** — the granular-coverage register; this drives many
   downstream findings.
5. **W11 + W19 + W33** — API + auth + Stripe; the launch-blocking
   security surface.
6. Remaining workstreams in any order, with W26 closing last
   (as the gate).
