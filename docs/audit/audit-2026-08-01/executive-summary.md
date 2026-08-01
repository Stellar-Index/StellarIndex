# Audit №3 — executive summary (HEAD f8c099ee, 2026-08-01)

## The single most important number

**Zero confirmed LIVE critical or high-severity findings.** The third full,
cold, adversarial audit found no launch-blocking correctness or security defect.
Everything that survived independent skeptic refutation is Medium or below, and
each is either a one-line fix or operator-gated.

## LIVE-P0 / P1 counts
- P0 (critical, live): **0**
- P1 (high, live): **0**
- Confirmed Medium: **5** (M-B, F-2, R3, R4, R5+R6)
- Confirmed Low/Info: **4** (W-1, R1, R7, R8)
- Refuted (reviewed-not-carried): **5** (R2, R10, M-A, F-6, F-4)
- Fixed during the audit: **1** (main CI red since 2026-07-30)

## What the audit process did to my own recon
The recon over-claimed. Of the findings I sent to skeptics, **5 of 14 were
refuted** and several more were recalibrated down a full severity level. The
two "launch-critical DoS" findings (R2, R10) were both refuted — the
`ops_by_source` projection we built during the completeness push already closed
that surface — and the scariest correctness cluster (the redstone two-provenance
race F-6, the comet/phoenix latent twins F-4) both proved unreachable because
the completeness reconcile machinery catches exactly those classes. That
inversion is the audit working: adversarial verification is what turns a
plausible-looking recon finding into either a real defect or a documented
non-issue.

## The confirmed findings, ranked by what to do about them

1. **M-B — two-sided TWAP combine is trade-count-weighted (MED, served price).**
   `aggregates.go:576-578`, served via `/v1/chart?price_type=twap`. The exact bug
   fixed on the VWAP twin 2026-07-24, never propagated; ~2.4% bias on a two-sided
   market; untested on the flipped path. The most product-relevant finding. Fix:
   port `combineDirVWAP`'s time-fair combine to TWAP + a flipped-row test.
2. **F-2 — ch-rebuild/backfill-router rewrite below-watermark served rows with no
   dirty-window record (MED, operator-gated).** The `/v1/coverage` verdict can
   carry a stale "clean" claim over a re-derived range — the exact class migration
   0125 closed for `projector-replay`, left open on the most-used re-derive tool.
   Fix: same RecordProjectionDirtyWindow-before-write guard.
3. **R3 — /v1/accounts/{g} returns 500 not 503 on gate saturation (MED).** Error
   sentinel can't cross the clickhouse↔explorer package boundary. Pollutes the 5xx
   SLO + mislabels legit cold-cache misses. Fix: one error arm → retryableColdMiss.
4. **R5 + R6 — 46 web tests never run in CI; no gate on the JSON-LD XSS chokepoint
   (MED, interlocked).** Dead OG-SSRF/phishing/XSS regression gates; a raw
   JSON.stringify in a script tag ships green (CSP is unsafe-inline). Fix: one
   web-test Make target + CI step (revives all 46), then a seo source-scan test.
5. **R4 — synthetic-UA erases traffic from HTTP metrics + SLO (MED, observability).**
   Fix: gate isSyntheticUA on a loopback peer.

Low/Info + operator items: W-1 (intentional Phase-3 dual-write soak — doc ADR-0032
as Phase-3), R1 (6000/min recipe correction), R7 (Redis-no-auth, GATED on host
foothold — add AUTH/ACL), R8 (deploy gate relaxed — the known re-arm-before-launch
item, already on the operator list).

## Recon-surfaced findings NOT sent to a dedicated skeptic (carry as lower-confidence)
Money: M-C (AmountScaleDecimals default-8, DEX-only guard — re-openable CS-040),
M-D (trades.usd_volume no CHECK), M-E (lint-i128 self-weakenable), M-F (min_usd_volume
bypass for unpegged quotes), M-G (bespoke_lending no numeric-safety test).
Ingest: F-3 (dirty windows unobservable), F-5 (oracle op_index fanout twin class, no
purge — the one twin finding NOT refuted; MED), F-7 (redstone feed-dark = watcher-only),
F-8 (ops_by_source guard is existence-not-coverage — silently truncated history if
Step-2 backfill skipped; MED), F-9 (ops_by_source semantic broadening), F-10
(crossed-pairs gauge has no alert), D-1 (ADR-0033 provenance anti-join unimplemented),
N-1/N-2/N-3 (doc drift). Web: R9 (frontend ADR-0003 residuals, ungated).
Doc: CLAUDE.md OpenAPI section factually wrong (generators ARE drift-guarded now).

## What this audit did NOT cover (coverage honesty)
This was a DEEP recon over the highest-risk surface — money/pricing, ingest/storage/
completeness, web/config/trust, entry points, architecture — with adversarial skeptic
verification of the launch-relevant findings. It is NOT yet the complete whole-repo
file-by-file coverage-ledger pass: the finder waves over the ~3,312-file surface's
lower-risk units (the bulk of internal/sources/*, internal/api/v1 handler-by-handler,
the CLI subcommands, test/, scripts/) have not run. The confidence bound: everything
the recon examined is cited to file:line; the negative-space and per-unit finder waves
over the remainder are the outstanding work for a truly exhaustive claim. Given the
deep recon already done, the expected marginal yield there is lower-severity findings.

## Bottom line for launch
No blocker surfaced. The five Mediums are worth fixing — M-B and F-2 for correctness
integrity, R3/R4/R5/R6 as cheap hygiene — but none gate v1. The audit strengthens
rather than delays the launch case: a cold third-party-style pass over the whole
high-risk surface came back with no critical, no high, and a refutation rate that
shows the verification was real.
