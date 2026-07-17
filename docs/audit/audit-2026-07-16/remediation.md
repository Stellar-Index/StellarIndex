# Remediation ledger — audit-2026-07-16

Branch `remediation/audit-2026-07-16` off main. Posture: **full-autonomous-commit-all**.
Every fix is an atomic commit at a gate-green tip (build/vet/gofumpt + proven-red test where one applies + the money/integration gate for DB/data fixes) → pushed. "FIXED" = committed + independently verified + gate-green. This ledger reconciles every disposition; it is a living document while the campaign runs.

Two hard limits held in every commit: never force-push/rewrite shared history; never run a live deploy, rotate a secret, or execute a migration against the prod DB. Migration *files* are committed like any code; *running* them is the separate (frozen) deploy step.

---

## REVIEW-AFTER (committed — post-merge spot-check, NOT a gate)

These landed by clearing the strongest verification; they are already committed. Eyeball hardest, in this order:

1. **INV-3 keystone + 24 protocol tables** (`2e5c7c0c`, `a3248bad`) — MIGRATION 0109 + 0110 + every money/derived writer switched to a generation-guarded corrective upsert. Money + migration + data class. Proven-red DB tests; `make test-integration` green. The root cause of the re-backfill treadmill.
2. **C2-4 SAC full-history coherence** (`a9a1c299`) — tuple-argMax fixes a resurrect-deleted before-image feeding served supply. Proven-red (old query resurrects 3/3).
3. **Projector durability cursor** (`6efc8565`) — transient-hold / permanent-skip; changes ingest cursor semantics. Proven-red integration test.
4. **Money serve-value fixes** — M3 FX-leg inversion (`a2c41925`), M12 self-priced oracle reject (`d8a350b6`), M14 issuer max_supply poisoning (`295128cb`), M7 /changes money→string end to end (`b0e01b8b`). M3 orientation independently reconfirmed against the forex source.
5. **Auth hardening** (`de8286ff`) — rate-limit fail-open close + per-IP SSE cap + IPv6 bind warn + failed-auth throttle. Security.
6. **API unauth-DoS** (`fde6727f`) — request-timeout middleware + serving statement_timeout + input validation.
7. **Completeness fail-closed** (`ef0f2466`) — the /coverage verdict no longer fails open on a scan error.

---

## FIXED (committed + verified, 24 commits)

| Finding(s) | Commit | What |
|---|---|---|
| C4-17 | `c69c0415` | regenerate Postman (clears the openapi-lint "in sync" CI red) |
| M1 / INV-3 (keystone) | `2e5c7c0c` | generation-guarded corrective upsert + migration 0109 (3 core money tables) |
| — | `1bd8af63` | R1 disk/storage assessment (RAID/ZFS verification + runway) — docs |
| C3-1,C3-2,P1,P2,R1 | `fde6727f` | request-timeout middleware + serving statement_timeout + id validation (unauth DoS) |
| C4-8,C4-9 | `60997eb9` | correct audit-confirmed false claims in CLAUDE.md + arch docs |
| C4-2 | `8fa8346a` | run-ch-supply.sh reports failure to systemd (stops masking supply-seed failures) |
| C2-1,D1,C4-14 | `6efc8565` | projector durability-honest cursor (transient hold / permanent skip) + metric fix |
| C4-1,C4-11,C4-15 | `a03c3f86` | runbook_url labels→annotations (270 alerts) + YAML guard + inhibit-rule fix |
| C2-5 | `ef0f2466` | completeness fails closed on a recognition-scan error |
| C3-13,C3-22,C3-8,C3-18,C3-5 | `de8286ff` | auth: rate-limit fallback + per-IP SSE cap + IPv6 bind warn + failed-auth throttle |
| C2-4 | `a9a1c299` | tuple-argMax coherent latest-change in the SAC full-history supply seed |
| C4-17b | `c4f9f528` | commit tool-tarball checksums in-repo (removes the [OP] secret dep for promtool+gitleaks) |
| M1 / INV-3 (protocol) | `a3248bad` | extend the generation guard to 24 protocol projector tables + migration 0110 |
| HLT (dead code) | `6a2c546a` | remove windowUSDVolume + web/status dead env (kept VerifiedCurrencyListItem — false positive) |
| C4-15 | `6248454e` | make vacuous obs metric/logger tests assert real behaviour (proven non-vacuous) |
| C4-14,INF-11 | `cbb8780f` | route heavy timers through run-heavy-job.sh + flock singleton (no overlap-stacking on R1) |
| C4-3,C4-4,C4-6 | `366b9271` | persist-failure metrics + nil-Registry gauge/alert + served-insert-frozen tripwire |
| C4-16 | `b5355fb7` | main-CI-red scheduled tripwire (opens/closes a tracking issue) |
| C4-7 | `835265b3` | down-migration pairing/numbering/non-empty lint |
| M3 | `a2c41925` | FX-leg inversion fixed (rate_usd is ticker-per-USD, verified vs source); proven-red exact big.Rat |
| M12 | `d8a350b6` | reject self-priced oracle update (Asset == Quote) |
| M14 | `295128cb` | reject issuer max_supply of 0 or below circulating |
| M4 (partial) | `755ba052` | regression guard: Computers stamp the passed close time; the CALLER fix is deferred (below) |
| M7 | `b0e01b8b` | /v1/changes money → JSON strings across API + SDK + OpenAPI + Postman + docs |
| M3 (test) | `a2c41925*` | corrected the fx_quotes orientation fixture that masked M3 (rate_usd is ticker-per-USD, externally confirmed; the FX leg was genuinely inverted) |
| C2-12 | `3f5693ad` | dedup RMT reads (FINAL / uniqExact) in served analytics counts — a re-ingested row no longer double-counts throughput/op-type/event breakdowns |
| C2-6 | `5e38f54d` | observation writers keep the FINAL intra-ledger change (intra_ledger_seq guard) not last-writer-wins + migration 0111 |
| M2 | `d354243c` | non-7dp decimals normalization applied on the ~8 missing serve paths (per-path; main /price byte-identical, no double-apply) |
| M5,M8,M11 | `867b46e1` | robust manipulation resistance: median+MAD outlier filter (M5), median-filtered global mean (M8), tightened served-VWAP guard (M11) |
| — (CI) | `b2efc14a` | clear golangci-lint (funlen/gocyclo/predeclared/unparam) + regenerate web API types for the M7 OpenAPI change |

*(C4-14 and C4-15 each appear on two commits — the finding had two aspects, both addressed. The M3 orientation was reconfirmed against Polygon's C:USDEUR convention externally; the integration test that encoded the inverted value was corrected.)*

---

## CI baseline (the operator's "CI fails a lot but the agent doesn't pick it up")

Main was red for 24h+ on three CI-only checks, invisible to local `make verify`:
- **prometheus rule validation** → promtool install fail (secret-gated) — **fixed in code** by `c4f9f528` (checksum committed, secret now optional).
- **govulncheck + gitleaks** → gitleaks install fail (secret-gated) — **fixed in code** by `c4f9f528`.
- **openapi lint** → stale Postman collection — **fixed** by `c69c0415`.

**All three of main's red checks are now addressed in code on this branch.** Once merged, main CI should go green with **no operator secret-set required** — the previously-logged [OP] secrets are no longer a merge blocker.

---

## DEFERRED (dispositioned, not gambled)

- **M4-callers** — `resolveSnapshotLedger` (ops/supply) + `supplyAggregatorLedgers.LatestKnownLedger` (aggregator) stamp `time.Now()`, not the ledger's close time, so a re-derived HISTORICAL supply snapshot is mis-timestamped. The Computers are proven correct (`755ba052`). The fix needs a ledger→close_time resolver — authoritative source ClickHouse `stellar.ledgers.close_time` (aggregator already builds CH readers; the ops supply command is timescale-only → needs CH wiring or a TS observation at-or-before query) + an integration test. A design choice, not rushed into the supply path.
- **C2-4c** — `ledger_entries_current` is `ReplacingMergeTree(ledger_seq)`; its version lacks intra-ledger order, so the FINAL-backed current-state readers (account_state / account_balance / soroswap_pair_state + routine SAC seed) inherit an arbitrary same-ledger present-vs-removed tie. Real fix = composite RMT version + reproject (heavy, [OP], freeze-gated).
- **C2-11** — `soroban_events` stores topics in 4 fixed columns and truncates >4 (Aquarius ≥5-topic multi-token pools lose topics). Needs a schema change (topics array / overflow) + the ingest capture path + a re-ingest to recover historical events. Migration-owner task.
- **C2-18** — migration-0105 `classic_movements` DROP. The table has code refs (writer/reader methods) but the finding is they're caller-less; confirming true deadness needs caller-tracing, and a destructive DROP should be operator-reviewed. LOW housekeeping.

## REVIEW knobs (defensible defaults chosen; operator may tune)

- **M11 `guardRatioBound = 3`** — the served-VWAP manipulation tolerance (rejects a >3× single-bucket deviation). The finding's prose wanted ≥3× rejected; the default admits ≤3× to avoid false-rejecting real extreme moves (depegs/halvings). A one-constant tightening if a stricter posture is wanted. Also `M8 aggregatorMADFactor=5`, `M11 guardThinRatioBound=10`.

## SECOND WAVE — landed (money2 + data2)

- **M9** (staleness gates), **M10** (exact fiat money), **M13** (confidence mixed-scale 10× — the author's "log-saturating doesn't care" rationale was disproved: ΔF=0.5 across the sub-$100K band).
- **C2-13** (cctp/rozo event_index migration 0112 + batch registry hook), **C2-14** (both enqueue-not-persist cursors → persist-confirmed watermarks), **C2-17** (shutdown drain, C2-1 preserved).
- **C2-16 — DEFERRED (rigorous):** the oracle-reconcile window-netting deliberately absorbs a legacy vintage ledger-keying shift (legacy backfills key by oracle-timestamp ledger, live by event ledger); a strict per-ledger compare would re-introduce false positives on every legacy oracle window. A sound fix needs a content-level per-update reconcile on a vintage-stable identity — flagged, not rushed.

**Total: 52 distinct findings addressed across 42 commits. Full local integration suite + golangci-lint green on the merged tree.**

## LOW / INFO / GATED tail — disposition

Many tail items are already CLOSED by the landed waves: C3-13/C3-18 (auth), C4-4/C4-16 (detectability), C4-2 (supply-seed exit), G6/windowUSDVolume + web/status dead env (hygiene), C3-1/C3-2/C3-9 (API DoS/validation). `VerifiedCurrencyListItem` was a FALSE POSITIVE (live API contract — kept).

- **FIXED from this shortlist:** C4-13 (isSafeHref now strips C0 controls before scheme detection — the `java\tscript:` stored-XSS bypass), C3-15 (`*_password_env` docs corrected to match behaviour — the maintainer-footgun), PRV1 (all 11 dashboardauth customer-email log sites masked).
- **Still worth a follow-up (clean, real):** C3-14 (two ops archive commands use bare `config.Load()`, no `ApplyEnvOverrides` → placeholder DSN); C3-20 (`clickhouse_projector_source` requires `clickhouse_live_sink`, unenforced at config-validate); C3-17 (6-digit login-code brute-force leans on the OPTIONAL LoginThrottle).
- **GATED-not-live on R1 (accepted; reactivate-if-enabled, documented):** G1/CS-040 decimals hardcode (polygon-forex/exchangeratesapi disabled, min_usd_volume=0); C3-16 (Stripe lifecycle/downgrade); C3-19 (/metrics colocation); C3-21 (usage rollup 2-day window); C3-13/C3-14/C3-15 are gated but the code fixes above still apply.
- **negspace / operator-safety (N1, C2-19):** no guarded (dry-run/backup/confirm) re-derive command, no dead-letter/quarantine, no `migrate down` destructive guard, no advisory lock coordinating ops re-derives vs the live projector, no single-source kill-switch — these are OPERATOR-CONTROL absences (tooling to build), surfaced for a dedicated hardening pass, not a code bug to patch inline.
- **LOW/INFO accepted (documented, not blocking):** DOC-drift comments, `scval.Display` U256 rendering, ACC aria-live gaps, ISO `coingecko_id` slugs, tag-pinned (not digest) Docker base images, heterogeneous-decimal stablecoin-proxy VWAP fold.

## DEFERRED-STRUCTURAL (schema/reproject/re-ingest — [OP]-coordinated)

- **C2-11, C2-18** — see DEFERRED above (topics >4 schema + re-ingest / classic_movements dead-table DROP).

## [OP] — operator actions (logged, cannot be done here)

- **~~Set PROM_TARBALL_SHA256 / GITLEAKS_TARBALL_SHA256 secrets~~** — obviated by `c4f9f528` (checksums committed). No longer required.
- **R1 live storage** — run `zpool list/status/history`, TimescaleDB compression backlog (job 1034, paused during the 2026-06 load incident); see `r1-storage-assessment.md`. Pool is 4-drive raidz2, ~93% full at the last snapshot; the drive-shuffle pool EXPANSION never completed.
- **C4-5** — SEV-1 paging self-check (needs live Alertmanager/pager wiring).
- **Post-freeze** — run the corrected re-derives ONCE after ADR-0047 Phase-0 finishes (the INV-3 guards make them correct-and-idempotent now); apply migrations 0109 + 0110 via the normal migration path.

## Coordination

Other agent stays PAUSED (this campaign owns the branch). One migration owner per wave (0109 keystone, 0110 protocol; the next migration-owner slot is free for C2-6 / C2-11). Deploy freeze respected throughout — no fix runs infra.
