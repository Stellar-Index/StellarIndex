# Audit №3 — remediation tracker

The actionable board for every CONFIRMED finding. Status: ✅ fixed (committed) ·
🔧 open (code) · 📋 operator/product · 💤 accepted-low (post-launch OK).
Ordered by the executive-summary launch priority, HIGH first. LOW/INFO tail is
`💤` unless it rides along with a nearby fix.

## HIGH (fix before announcement)

| ID | Finding | File | Status |
|----|---------|------|--------|
| W4-obs-1 | Unauth metric-cardinality DoS — unknown HTTP methods = unbounded Prometheus label | internal/obs/http_middleware.go | ✅ `ce1bef9f` (collapse to "other" + cardinality-bound test) |
| W3-freeze-1 | Freeze sibling-window release publishes a manipulated price (pair-keyed marker, per-window lifecycle) | internal/aggregate/orchestrator/phase2_freeze.go + internal/cachekeys/keys.go:400 | 🔧 open — window-scope the marker/ladder/looker OR skip Clear while a sibling window is Active |

## Verification-layer cluster (theme A — the first-24h watch depends on these)

| ID | Finding | Status |
|----|---------|--------|
| W5-mon-1/2/3 | 3 dead alerts — supply_refresh_stalled + config_assertions_stale use `timestamp()` (scrape freshness, never fire); ledgerstream both_missing nil-in-prod + routes to no-op receiver | 🔧 open |
| W5-ci-1..5 | 5 false-green gates — SQL money-lint blind to integer/smallint/OHLC/vwap names; gitleaks self-bypassable; route-sweep counts 000 as ok; reconcile-vs-horizon passes on a Horizon outage; lint-i128 not base-restored + misses int32/16/8 | 🔧 open |
| DEP-1 | pnpm-audit gate treats `ERR_PNPM_AUDIT_BAD_RESPONSE` as pass; endpoint down since 2026-07-15 → frontend HIGH-advisory gate is a no-op | 🔧 open — needs an alternative advisory source or a loud-fail + tracking (endpoint genuinely down) |
| W5-ci-6 | verify.sh green ≠ CI green (no main-CI-health check) | 🔧 open |

## Other HIGH-blast / security MED

| ID | Finding | File | Status |
|----|---------|------|--------|
| W2-explorer-1 | SAC label spoofing — hostile token impersonates Circle USDC on public /movements (missing the SacContractID cross-check its sibling wasm_view.go:201 has) | internal/api/v1/explorer/movements.go:331 | 🔧 open |
| W3-guards-1 | decimalsguard latches "fired" before the DB write → a non-7dp token serves a 100×-wrong price until restart | internal/decimalsguard/guard.go:378 | 🔧 open |
| W4-cmd-1 | Aggregator has ZERO panic isolation (API isolates every worker) → one worker panic crash-loops the price pipeline | cmd/stellarindex-aggregator/main.go | 🔧 open |

## Wave-6 MED (new)

| ID | Finding | File | Status |
|----|---------|------|--------|
| W6-tst-2 | i128-truncation guard had no positive control — a rotted detector = a clean tree | internal/canonical/i128_truncation_guard_test.go | ✅ `765cdcf9` (self-contained positive control, probe-verified) |
| W6-perf-1 | assetDetailResponseCache unbounded — crawler enumerating real asset_ids → slow OOM | internal/api/v1/asset_detail_cache.go | ✅ `d0301058` (cap 4096 + expired-purge/oldest-evict + tests) |
| W6-fresh-1 | /v1/price empty-baseline fail-open — a pair's first-ever served bucket is unbounded (guard IS wired; band is [median/3,×3]) | internal/aggregate/served_guard.go:147 | 🔧 open — serve first-ever buckets stale=true + min served-bucket USD-volume floor |
| W6-fresh-4 | Completeness-verdict staleness const hand-calibrated to the daily timer, NO CI lint linking them → silent flag rot if cadence changes | internal/api/v1 coverage const ↔ stellarindex-completeness.timer | 🔧 open — add the const↔timer CI lint (negative-space e) |
| W6-tst-1 | runVerifyChunks (archive hash-chain verify) has a test no CI job compiles (build-tagged, package not in INT_TEST_PKGS) | internal/ops/archive/ + Makefile:41 | 🔧 open — add the package to INT_TEST_PKGS, confirm it runs |
| W6-prv-1 | Session/magic-link PII (IP/UA/geo/email) retained indefinitely — no purge, no retention | migrations/0027 + postgresstore/{user,token}_store.go | 📋 product/legal + a small retention task |
| W6-prv-2 | No data-subject deletion path (GDPR/CCPA erasure absent) | internal/api/v1/server.go + internal/platform/ | 📋 product/legal + a delete path |
| W6-acc-1 | `--color-ink-faint` fails WCAG-AA contrast (2.89–3.29:1), used as text 254× | web/explorer/src/app/globals.css:44 | 🔧 open — stop using ink-faint for text (ink-muted passes) |
| M-B / W6-derive-1 | Two-sided TWAP combine trade-count-weighted (its VWAP twin was fixed) | internal/storage/timescale/aggregates.go:577 | 🔧 open — volume-weight the TWAP combine (mirror combineDirVWAP) |

## Negative-space (launch-relevant gaps)

| Gap | Status |
|----|--------|
| (b) No operator global kill-switch on the price path (only per-pair freeze / aggregator restart) | 📋 consider before launch |
| (e) No CI lint linking the completeness staleness const to its timer cadence | 🔧 = W6-fresh-4 |

## LOW/INFO tail (post-launch OK unless riding along)

- ✅ W6-go-2 (frankfurter treats truncated stream as clean EOF) — internal/sources/external/frankfurter/client.go
- 🔧 W6-sweep-1 — storage_classic_reader.go:124 promises a WARN it can't emit (no logger field) → silent fail-permissive
- 💤 W6-go-1 (WASM export prealloc from LEB128 count — not reachable today, MED if any un-validated WASM path ever feeds it; cheap defensive cap worth adding)
- 💤 W6-web-1 (oracle chip dark-on-dark), W6-web-2 (entries_24h deploy-skew render-throw), W6-acc-2 (Sparkline role=img), W6-acc-3 (hairline contrast), W6-i18n-1 (en-US pin vs bare toLocaleString), W6-prv-3 (staff email unmasked), W6-perf-2 (fan-out 16/25), W6-tst-3 (sdex one-leg-zero untested), W6-tst-4 (webhook SSRF-rebind untested), W6-dom-2 (XLM hardcoded supply), DEP-2/3 (GHSA suppression, galexie tag), W6-derive-2 residual (TolerateTrailingMissing caller-wrapper fragility)
- Waves 1-5 C/D/E residue (W1-supply-1, W1-defi-1, W4-storage-1, W2-explorer-2, W2-plat-1, W5-web-1, W3-guards-2, W3-freeze-3, W1-sub-1, recon R3/R4, W2-auth-1) — see findings.md

## Operator/product (not code — plan OPERATOR INBOX)
DEPLOY_APPROVAL_RELAXED=true (gate no-op); main zero branch protection; GH Actions crons dead since org
transfer (red-main tripwire + weekly security/drift never run); SEP-41 genesis-seed ordering; C2-11
>4-topic truncation (schema + re-ingest); privacy policy/terms/export-delete (= W6-prv-1/2).

---
_This tracker is the single remediation board for audit №3. Update the Status column as fixes land._
