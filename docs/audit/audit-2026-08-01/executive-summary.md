# Audit №3 — executive summary (HEAD f8c099ee, 2026-08-01)

Third full cold, adversarial, systematic audit. Recon deep-dive over the highest-risk
surface + a 5-wave file-by-file finder sweep over the remaining ~3,321 files + a **Wave-6
gap-closure exhaustiveness pass** (the dimensions/process-steps/residual-files/prior-audits
the first five waves hadn't dedicated-passed), each finding independently skeptic-verified.
Scale: ~28 finder agents + 11 skeptic agents.

## The single most important number

**Two CONFIRMED HIGH findings. Zero CONFIRMED critical. Zero confirmed fund-loss or
served-data-corruption defect.** Both HIGHs have small, well-scoped fixes. Everything else
is Medium or below. **Wave 6 — which specifically hunted the cross-boundary / protocol-truth /
dependency / privacy / test-integrity surfaces the subsystem sweep structurally misses — added
ZERO new CONFIRMED HIGH.** Its two scariest candidates (VWAP scale-mixing; /v1/price freeze
bypass) were both surfaced by an independent threat-first re-decomposition and then dissolved
under adversarial verification (disjoint asset namespaces; the guard is in fact wired).

## Verdict counts (through Wave 6)
- CONFIRMED HIGH: **2** (metric-cardinality DoS; freeze sibling-release) — unchanged by Wave 6
- CONFIRMED MED: **~34** (waves 1-5 ~26 + Wave-6 +8: PII-retention, no-GDPR-delete, WCAG-contrast,
  staleness-const↔timer, unbounded asset-cache, runVerifyChunks-no-CI-test, i128-guard-no-positive-
  control, /v1/price-empty-baseline-fail-open)
- CONFIRMED LOW/INFO: **~44** (waves 1-5 ~30 + Wave-6 ~14)
- REFUTED / DOWNGRADED by skeptics: **11** (waves 1-5: R2, R10, M-A, F-6, F-4, W2-pricing-F1, +
  freeze-2/resume-stalled; Wave-6: W6-dom-1, W6-derive-2, W6-fresh-3 refuted + W6-fresh-1 downgraded)
- FIXED during the audit: **1** (main CI red since 2026-07-30)

## Wave 6 — what the exhaustiveness pass proved (see wave-6-findings.md)
The gap-closure pass closed every gap vs a full formal audit-suite execution and PROVED SOUND the
highest-anxiety unknowns: the **forged-creation contract-gating bypass does not exist** (all three
attribution surfaces double-gate on factory identity); **i128 discipline + Stellar/Soroban protocol
modeling are correct against the spec** (CAP-67 topics, ClaimAtom variants, SDEX orientation, SEP-40,
all five strkey layouts, two's-complement composition); **all 16 prior HIGH/critical keystones from the
earlier campaigns are still fixed**; the **PERF/CON hot surfaces and every security-critical mechanical
sweep** (authz, secrets, injection, type-assertions, timeouts) are clean; **foreign-contract-rejection
tests exist for all 6 gated protocols**; the **embed money widgets are hardened**. The new MEDs cluster
in the genuinely-unpassed dimensions — privacy/GDPR, accessibility, test-integrity — which is exactly why
the pass was warranted and why "done" should not have stood before it ran.

## The 2 CONFIRMED HIGHs — fix before announcement

1. **Unauthenticated metric-cardinality DoS (W4-obs-1) — LIVE, no auth, no config gate.**
   `normalizeMethod` (obs/http_middleware.go:212) passes unrecognized HTTP methods through
   unchanged as a Prometheus label; `net/http` accepts any token as a method; counter children
   are retained for process life with no cap. `curl -X <random>` in a loop → unbounded series →
   API + Prometheus OOM. **FIX (1 line): map unknown methods to a bounded "other".** The single
   most important item in this report — it's the only confirmed *remotely*-triggerable HIGH.

2. **Freeze sibling-window release publishes a manipulated price (W3-freeze-1) — skeptic-CONFIRMED.**
   The freeze marker + ladder + serving Looker are pair-keyed but the lifecycle is per-window;
   when a short window auto-releases it Clears the shared marker, and still-frozen long windows
   read that as an operator override and publish the manipulated VWAP with flags.frozen=false.
   Defeats anomaly protection for multi-window pairs, doesn't self-heal. **FIX: window-scope the
   marker/ladder/Looker, or skip Clear while a sibling window is still Active.**

## The CONFIRMED MEDs by theme (all fixable; most quick)

**A. The verification/alerting layer has gaps — MOST LAUNCH-RELEVANT (the first-24h watch
depends on these).**
- 3 dead alerts (W5-mon-1/2/3): supply_refresh_stalled + config_assertions_stale use
  `timestamp()` (scrape freshness, not age) so never fire; ledgerstream both_missing's metric
  is nil-in-prod AND routes to a no-op receiver. Root cause: only 5/30 rule files have promtool tests.
- 5 false-green gates/monitors (W5-ci-1..5): SQL money-lint blind to integer/smallint/money +
  OHLC/vwap names; gitleaks self-bypassable; route-sweep counts 000 as ok; reconcile-vs-horizon
  passes on a Horizon outage; lint-i128 not base-restored + misses int32/16/8.
- Dead web tests + no JSON-LD gate (recon R5/R6); verify.sh green ≠ CI green (W5-ci-6).

**B. Freeze/anomaly lifecycle** (beyond the HIGH): decimalsguard latches "fired" before the DB
write and never retries → a non-7dp token can serve a 100×-wrong price until restart (W3-guards-1);
divergence compares 5m-VWAP vs spot → false warnings on fast moves (W3-guards-2); Phase-1 freeze
can get stuck permanent (W3-freeze-3); TWAP two-sided combine trade-count-weighted (recon M-B).

**C. Operator re-derive tooling** (operator-gated): resume-stalled NULLs correct usd_volume
(recoverable, W3-ops-1); the dirty-window/false-certification class spans ≥4 tools — ch-rebuild,
backfill, backfill-router, resume-stalled (recon F-2 + W3-ops-2/3); wasm-extract reports success
on a failed write (W3-archive-1). RECURRING CLASS: "mark/latch before the fallible side-effect" (3×).

**D. Data correctness / display**: SAC label spoofing — a hostile token impersonates Circle USDC
on public /movements (W2-explorer-1, security); sac_balances supply under-report (W1-supply-1);
SDEX zero-leg fill fails a whole batch INSERT + spurious alert (W1-defi-1); duplicate contract
events served during the RMT merge window (W4-storage-1); movements ?asset= filter-after-limit
returns "none" when there are many (W2-explorer-2); usage-analytics permanent over-count (W2-plat-1);
partial-bucket dip on /diagnostics (W5-web-1).

**E. Robustness / hardening**: aggregator has ZERO panic isolation while the API isolates every
worker → one worker panic crash-loops the price pipeline (W4-cmd-1); AsBool panics on a zero ScVal
+ dispatcher has no recover() (W1-sub-1); 500-not-503 on cache-gate saturation (recon R3);
synthetic-UA erases traffic from the SLO (recon R4); postgres kill-switch cache bypass, gated
off-r1 (W2-auth-1).

## What the audit PROVED SOUND (do not re-audit these; they held under adversarial tracing)
- **i128 discipline** never truncates — every decoder, every storage reader, the substrate.
- **Contract-identity gating** is fail-closed everywhere; a hostile look-alike contract is rejected.
- **Money-on-the-wire** is decimal strings with big.Rat/big.Int math throughout the API.
- **Zero SQL injection** anywhere (every query is compile-time identifiers or bound args).
- **Auth/security core**: IDOR scoped on every {id}, Stripe HMAC+replay+dedup, SEP-10 alg:none-safe,
  SSRF guard (metadata IPs/NAT64/DNS-rebind), rate-limit fail-closed math, CSRF, session expiry.
- **Destructive ops**: trim-galexie gated + real upstream-presence check; every verify-* fails
  closed on empty; projector-replay records dirty windows correctly.
- **Migrations DDL set** clean; **pkg/client** money-as-strings + spec-contract test green;
  **completeness reconcile** catches the twin classes (F-4/F-6 refuted on that basis).

## REFUTED (skeptic-killed — the verification was real)
R2/R10 (unauth pool exhaustion — ops_by_source already fixed it); M-A (float wealth —
unobservable); F-6 (redstone two-provenance race — window empty); F-4 (comet/phoenix twins —
strict reconcile catches them); W2-pricing-F1 (observations DoS — 0037 index makes it a seek).
Of findings sent to skeptics, ~40% were refuted or downgraded — the adversarial pass worked.

## Coverage honesty
All 3,321 files enumerated; every unit dispositioned FINDING / EXAMINED-SOUND / NOT-EXAMINED in
coverage-ledger.md. NOT-EXAMINED residual (documented for a follow-up dry-wave, low expected yield):
chainlink/frankfurter connectors, some clickhouse readers (grep-scanned no-trap), dashboard/detail/
embed web views, several ansible sub-roles, ~half the ci-lint internals, the soroswap factory-seed
genesis-walk completeness, and the live/lake state-write parity test. No unexamined *money or
security flow* was left un-reasoned-about; the residual is lower-risk surface.

## Launch-readiness verdict
**No launch-blocking data-corruption or fund-loss defect surfaced in three full audits, now including
the Wave-6 exhaustiveness pass.** The product core is sound. Recommended before announcement, in
priority order:
1. **W4-obs-1** (metric DoS, 1-line) — the only remotely-triggerable HIGH.
2. **The verification-layer cluster** (theme A, now incl. DEP-1 pnpm-audit fail-open) — because the
   first-24h watch you're about to staff literally depends on these alerts firing and these monitors
   not lying. Mostly quick fixes.
3. **W3-freeze-1** (freeze HIGH) + **W2-explorer-1** (SAC spoof) + **W3-guards-1** (decimalsguard
   100× price).
4. **The Wave-6 launch-relevant additions:** the two privacy/GDPR gaps (no data-subject deletion path
   + indefinite session/magic-link PII retention — a product/legal + small-code task before a public EU
   launch); W6-tst-2 (give the i128-truncation guard a positive control — a decorative guard on the #1
   invariant); W6-fresh-4 + negative-space (e) (link the completeness-verdict staleness const to its
   timer, or it silently recreates the "complete forever after the audit died" failure); W6-fresh-1
   (close the /v1/price empty-baseline fail-open); W6-perf-1 (cap the unbounded asset-detail cache).
5. **Negative-space (b): consider an operator global price kill-switch** — for a system whose top asset
   is a served price, the only manipulation-incident lever today is per-pair freeze / aggregator restart.
Everything else (themes C/D/E residue, the ACC/i18n/LOW/INFO tail) is post-launch-acceptable with the
fixes tracked. The refutation rate (11 refuted/downgraded, ~100% of Wave-6's serious candidates), the
breadth of EXAMINED-SOUND, and the fact that a dedicated cross-boundary + protocol-truth + dependency +
privacy + test-integrity pass found **zero new HIGH**, are themselves the strongest evidence that the
system is launch-ready once the short list above is closed.
