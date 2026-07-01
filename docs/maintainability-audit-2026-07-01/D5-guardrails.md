---
title: D5 — Guardrail matrix — invariant → is it machine-enforced & firing?
---

# D5 — Guardrails against regression

**Headline:** an unusually rich guard layer (6 CI jobs, ~10 lint scripts, several
drift-tests) — most of it genuinely good — sitting on **three cracked foundations
that make the whole layer advisory**, plus a tier of **prose-only invariants behind
this repo's worst data-loss bugs**. Rating: **M0** = will silently regress + has/will
cause a bug; **M1** = real gap, partial mitigation; **M2** = latent/safe-by-accident.

## M0 — the four that matter (all have caused or will cause a bug)
1. **`main` is unprotected → every gate is advisory (CS-097).** CI runs on push-to-main
   but the commit is already landed; the next push's `cancel-in-progress` kills the red
   run. **This is the root multiplier** — fixing it (branch protection + required checks,
   a repo *setting*) instantly converts the entire solid-guard column from advisory to
   blocking. **Highest-leverage single fix in the whole audit.**
2. **The `-tags=integration` + chaos suites are compiled but NEVER RUN in CI (CS-070).**
   `make test-integration` is in no workflow, no cron; chaos is scripts-only. So the
   retention guard, cascade-503, 503-mapping, and NUMERIC-arithmetic assertions never
   execute → regressions ship green. **Fix:** one Docker-enabled nightly running the suite.
3. **The ADR-0034 "no retention on `trades`" guard lives ONLY in that dead integration
   suite** — a rogue `drop_after` (this repo has had exactly that drift) ships green.
   **Interim fix:** move the retention assertion to a *unit-level SQL grep* (no DB), like
   `lint-pk-discriminators`.
4. **The two prose-ONLY correctness invariants behind the worst bugs have NO guard:**
   (a) **every-emitted-topic-classified** — the class behind Blend 3/21→23, Phoenix 8→1,
   SEP-41 99.96% mint loss, DefIndex recognize-vs-decode (CS-032); (b) **per-source
   `Decimals`** — CS-040's ~100× USD-volume error (kept latent only because
   `min_usd_volume=0` disables the feature on r1). **Fix:** a decoder-completeness test
   (golden topic-set per source vs `classify()`) + scale the volume gate by per-trade
   Decimals with a test.

## The 8 CLAUDE.md invariants — enforcement status
- ①i128-no-truncate: **M1** — ADR claims a golangci analyzer + BIGINT lint that **don't
  exist** (CS-007); runtime discipline real, but a future `int64(parts.Lo)` ships green.
- ②no-Horizon: **solid** (import-lint rule C, empty allowlist).
- ③S3-not-local-FS: **M2** (prose-only, low regression risk).
- ④one-module: **solid** (compiler-enforced).
- ⑤tier-1 validators: N/A (aspirational).
- ⑥Galexie-not-rpc ingest: **M1** — import-lint rule A, but a **blanket `/decode.go`
  exemption** lets any decoder import stellarrpc; baseline bypassable.
- ⑦one-writer-per-domain: **M1** — `TestIsProjectedEvent_TableDriven` exists but is a
  **manual, non-exhaustive** table (a new projected source not added to the test fires
  nothing). *Should be reflection-based like the config drift test.*
- ⑧ClickHouse-lake/no-trades-retention: **M0** (see above — the guard is in the dead suite).

## Meta-guards (the layer that guards the guards)
- **M1:** gates self-editable (violation + allowlist edit in one commit → green, CS-098);
  route lint greps `HandleFunc(` only → misses `mux.Handle()` (CS-052, let the admin-PII
  route slip); CLAUDE.md has no frontmatter → the freshness check **skips it entirely**,
  so its false ADR-0035 claim (CS-127) is unguarded ("checked in CI" is itself partly false).
- **M2:** config round-trip regex skips digit tags (CS-131); actions-pinning hard-fail is
  a no-op on push-to-main; **Postman collection NOT drift-guarded** (types.ts IS).

## Guards that are SOLID (keep + COPY the pattern)
`TestDefault_MatchesStructTags` (reflection-based, self-exhaustive — **the model to copy
for the M1 manual-table guards**); `lint-pk-discriminators` (static coarse-PK, well-
documented allow-map); `lint-metric-refs.sh` (bidirectional dead-alert, F-1329);
import-lint rules B+C; closed-bucket tests (run in *unit* CI, not the dead suite);
metrics round-trip + the whole alert↔runbook↔catalogue integrity cluster; docs-api /
web-generate-api byte-diff gates; the golangci correctness set (errcheck/errorlint/
bodyclose/rowserrcheck/gosec, `tests:true`); SHA-verified tool installs.

## The two highest-leverage fixes
1. **Branch-protect `main`** (M0/CS-097) — the multiplier on every other guard.
2. **Run the integration suite in CI** (M0/CS-070) — resurrects a whole tier of already-
   written tests incl. the retention guard.

Then: make the reflection-guard pattern (`TestDefault_MatchesStructTags`) the template for
the manual-table guards (IsProjectedEvent exhaustiveness, decoder-completeness), and add
the two missing correctness guards (every-topic-classified, per-source Decimals).
