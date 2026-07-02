---
title: Audit remediation — status
date: 2026-07-01
scope: correctness/security audit (docs/audit-2026-06-30) + maintainability audit (docs/maintainability-audit-2026-07-01)
---

# Remediation status

Tracks the fix-everything pass over both audits. Operator-only items live in
[audit-remediation-operator-actions.md](../operations/audit-remediation-operator-actions.md).

> **Resolved (2026-07-01):** wiring the integration suite into CI (CS-070)
> surfaced pre-existing **test rot** — the suite was only ever *compiled*, never
> *run*, so it had drifted. All fixed + the full suite is green + the CI job is
> now a BLOCKING gate: (1) `TestFXQuoteAtOrBefore/FXSources` stale assertion
> (`massive` joined the FX registry); (2) `TestAPI_EndToEnd//v1/markets` +
> `TestTradesInRangeAndMarkets` refreshed only `prices_1m` but `DistinctPairs`
> enumerates pairs from `prices_1d` (the #20 right-granularity rewrite) — now
> refresh both; (3) the Blend round-trip tests substring-matched compact JSON
> against a postgres jsonb column that pretty-prints with spaces — the `contains`
> helper is now whitespace-insensitive.
Each fix landed as its own commit on `main` (see `git log`).

## ✅ Fixed (code/config, this pass)

### Correctness & security (CS-###)
| ID | Sev | What |
|----|-----|------|
| CS-012 | High | SSE `Hub.Publish` send-on-closed-channel panic (process crash) → per-sub mutex; +race test |
| CS-013 | High | SSE FD-exhaustion (cleared write deadline) → rolling per-write deadline + concurrent-stream cap |
| CS-010 | High | XLM supply basis lied (`sdf_reserve_exclusion` with no reserves) → honest `xlm_total_only` (config half = operator) |
| CS-038 | Med | classic/SEP-41 circulating could go negative → clamp at 0 |
| CS-017 | Med | dormant-pair VWAP served `stale=false` forever → freshness-window stale flag |
| CS-124 | High | dashboard CSRF (`SameSite=None`) → `SameSite=Lax` (same-site) |
| CS-071 | Med | User-Agent CR/LF injected into magic-link email → strip control chars |
| CS-009 | High | CF OG edge SSRF (double-decode + unescaped satori markup) → escape + single-decode |
| CS-102 | Med | issuer `home_domain` link unvalidated in AssetSidebar → `isSafeHomeDomain` gate |
| CS-121 | High | alertmanager config world-readable (webhook secrets) → `0640` + service group |
| CS-120 | High | SSH `PasswordAuthentication` gate inverts on string override → `\| bool` |
| CS-052 | Med | OpenAPI route lint missed `mux.Handle(` routes → check both + internal allowlist |
| CS-131 | Low | config round-trip lint skipped digit-bearing tags → `[a-z0-9_]+` |
| CS-083 | High | completeness watermark regressed on smaller re-run (`complete=true` at stale tip) → `WHERE` guard |
| CS-090 | Med | stale-tip verdict invisible → `completeness_watermark_lag_ledgers` gauge in freshness watchdog |
| CS-088 | High | divergence checker went dark silently (all refs fail = `outcome=ok`) → `no_reference` outcome + alert + runbook |
| CS-008-ssrf¹ | Med(sec) | 3 divergent SSRF blocklists (webhook guards missed Oracle metadata IP) → one `internal/nettools` union guard |
| CS-029 | Med | cursor gauge advanced on persist failure (hid stall) → gauge only on success |
| CS-100 | High | issuer detail dropped `org_verified` (impersonation) → thread through API + Verified/Unverified chip |
| CS-055 | Med | webhook HMAC replayable (body-only) → timestamp-bound signature + `X-StellarIndex-Timestamp` |
| CS-040 | Med | USD-volume gate assumed 1e8 (FX is 1e6, ~100× off) → per-source `AmountDecimals` |
| CS-127/007/128 | — | CLAUDE.md false ADR-0035/storage claims, ADR-0003 fictional analyzer, recipe drift |

### Logic / UX / a11y (LC-###)
| ID | Sev | What |
|----|-----|------|
| LC-020 | — | dashboard sidebar linked `/account/*` (pages are `/dashboard/*`) → repointed |
| LC-050 | Serious | RequestReveal dialog no focus-trap/escape/restore → shared `useDialog` hook |
| LC-051 | Serious | mobile nav drawer no focus-trap/restore → `useDialog` |
| LC-052 | Serious | form errors/success not announced → `Callout` role=alert/status + SignInForm role=alert |

### Maintainability (D#)
| Dim | What |
|-----|------|
| D4 | `/CAPABILITY-INVENTORY.md` (intent→symbol index) at repo root |
| D9 | `docs/contributing/` — 6 copy-followable checklists, CLAUDE.md points at them |
| D3 | `internal/nettools` (SSRF union) + `internal/sources/external/scale` (10 dup helper copies → 1, −335 LoC) |

> **Staleness note (2026-07-02):** several rows below shipped in
> v0.6.0 AFTER this file was written: the LC-001 fiat split, the
> exhaustive linter (D7), the import-boundary rules (D8), CS-070's
> blocking integration-CI job, and the ADR-0003 i128/BIGINT lints.
> Cross-check CHANGELOG v0.6.0 before acting on the deferred list.
> Newly DONE 2026-07-02 (this session): CS-084 (per-ledger -ch
> reconcile), CS-089 (chainlink staleness), CS-097/098 (branch
> protection + baseline growth guard), CS-113/114 (DR runbook
> honesty), CS-118/119/122 (ansible non-root), the D3 wsclient/httpx
> extractions, the consumer.Orchestrator retirement, and ADR-0040/
> 0041/0042/0043. CS-026 has a design + implementation plan
> (ADR-0040, board #32).

## ⏭ Deferred — need @ash direction or are large/design-gated

These are NOT operator-infra (those are in the operator register); they're code
work I did not do autonomously because they need a product/design decision, are
data-gated, or are large enough to warrant their own focused change + review.

- **LC-001 — split the assets page (fiat/non-Stellar vs Stellar).** Your headline
  logic-audit item. The API already has a `reference_only` flag; the full split
  (a dedicated `/v1/external/*` surface + explorer nav restructure + which assets
  belong where) is a SemVer-affecting product-design change. Needs your call on the
  target IA before I build it. Plan: `docs/audit-2026-06-30/` (Audit-2).
- **CS-026 — decoder contract-gating for phoenix/aquarius/defindex/comet.** Requires
  seeding factory/pool contract IDs (`seed-protocol-contracts`) + per-source WASM
  audits before flipping gates; data-gated, not a pure code change. Comet needs a
  pool allowlist / WASM-hash gate design. Tracked in [[project_decoder_gating_adr0035]].
- **Coin*→Asset* rename (D2 M0-1)** — zero wire impact but wide mechanical rename;
  own PR to keep the diff reviewable.
- **`stellarindex-ops` CLI split + `explorer_*.go` extraction (D1)** — large structural moves.
- **Remaining D3 extractions** — `external/wsclient` (WS reconnect/backoff/jitter ×4),
  `httpx` writeJSON/writeProblem, `ratelimit.FixedWindowCounter`, `canonical.SafeUnixSeconds`.
- **Enable `exhaustive` linter (D7) + import-boundary/acyclicity rules (D8)** — high-value
  regression guards, but enabling `exhaustive` tree-wide surfaces a cleanup wave that
  should be triaged deliberately (default-signifies-exhaustive config choice).
- **CS-070 — wire a Docker-enabled `make test-integration` CI job.** Needs CI runner
  with Docker; mechanical once that's decided.
- **i128 truncation analyzer + migration BIGINT lint (ADR-0003)** — the guards ADR-0003
  claimed but never had; tree is clean today so no live bug, but building them closes
  the gap (launch-todo P4-6).

## ⏭ Deferred — lower-value / non-issue
- **CS-032** — defindex factory path already returns `(nil, nil)` (recognize-and-drop);
  the `ErrUnknownEvent` is a defensive fallback `Matches` filters. No change needed.
- **CS-021/022 (ClickHouse `ledger_entries_current` versioning), CS-036 (SEP-41 amount
  decouple), CS-072 (signup enumeration), CS-041/042 (outlier/MEV heuristics)** — Medium/Low,
  no live-safety impact; next-wave candidates.

## Dependabot PR triage (2026-07-01)

19 open Dependabot PRs, all 9+ days stale (2 recurring red checks —
`govulncheck+gitleaks`, `prometheus rule validation` — were stale artifacts that
pass on current main). Triaged:

- **Merged (safe):** GitHub Actions minors (setup-go, pnpm-action-setup,
  golangci-lint-action, buildx); Go modules (google-api, aws-config, aws-s3,
  coder/websocket); web bumps incl. **tailwind-merge v3** (major — explorer
  verified clean), **next group → Next 16** (explorer+status), date-fns,
  lucide/prettier. Lockfile-conflict cascades resolved via `@dependabot rebase`;
  the lucide-react ^1.23 explorer bump (#1370) was applied manually after its
  siblings merged. Main rebuilt + `go mod verify` clean after the Go bumps.
- **Follow-up caught + fixed:** the merged **Next 16** bump REMOVED `next lint`, so
  `pnpm lint` failed and the `web/status` CI job went red on main. Next 16 itself
  builds+typechecks+lints fine — migrated both apps' `lint` scripts to the ESLint
  CLI (commit `ff729b29`). Stopgap uses `ESLINT_USE_FLAT_CONFIG=false`; the flat-
  config migration rides with the deferred tooling-group below.
- **Deferred #1347 — go-stellar-sdk v0.5→v0.6 (HELD).** VERSIONS.md mandates a
  compat pass; v0.6 breaks `datastore.GetFile` (now returns file size). Contained
  adaptation (`internal/ledgerstream/tiered.go` `GetFile`/`coldGetFile`,
  `tiered_test.go:43`, `cmd/stellarindex-ops/rehydrate_galexie_archive.go:157`) +
  VERSIONS.md bump + r1 ingest smoke — its own reviewed change. PR open with note.
- **Deferred #1368 + #1369 — tooling groups (HELD).** Coordinated dev-tooling
  **majors**: `tailwindcss v3→v4` (ground-up rewrite, CSS-first config migration),
  `typescript 5→6`, `eslint 9→10`, `eslint-config-next 15→16`, `@types/node 22→26`.
  Needs its own migration + visual QA + the eslint flat-config move. PRs open with note.
- **#1353 — actions/checkout v6→v7 (major).** build/lint/unit-tests pass with v7
  (all use checkout → runner-compatible); earlier `web/status` failure was the
  Next-16 lint issue (now fixed on main). Rebased onto fixed main — merge once green.

## Dependency upgrades — COMPLETE (2026-07-01, follow-up pass)

The deferred deps + a full outdated-sweep, done as individual verified commits:
- **go-stellar-sdk v0.5→v0.6** (the Stellar one) — compat pass done: adapted the
  `datastore.GetFile` size-return across ledgerstream + ops; VERSIONS.md pin bumped.
- **Safe Go deps** — clickhouse, go-redis, aws, google-api, testcontainers, miniredis.
- **TypeScript 5→6** (both apps; dropped deprecated `baseUrl`).
- **ESLint → flat config + eslint-config-next 16** (both apps; removed the
  `next lint` stopgap; new React-Compiler react-hooks rules adopted as advisory).
- **Tailwind v3→v4** (both apps; official codemod, design system verified via a
  styleguide render — every token/type/button correct).
- **Safe web deps** (react-query, tailwind-merge v3 for status, postcss, etc.);
  **removed the unused `zod` dep**.
- Final `verify.sh` green; **0 open Dependabot PRs**.

**Still deferred (ecosystem-blocked / follow-up):**
- **ESLint 9→10** — `eslint-config-next 16` + `typescript-eslint` peer-cap at
  eslint `^9`; revisit when they ship eslint-10 support.
- **React-Compiler react-hooks rules** — ~21 sites flagged (set-state-in-effect
  etc.), set to `warn`; promoting to error + fixing is a code-quality pass.
- **Tailwind v4 visual QA + browser-baseline** — build + styleguide render verified;
  a full visual pass across pages + the Safari 16.4+/Chrome 111+ baseline decision
  are yours.

## Verification
Every code fix built + its package tests passed at commit time; `bash
scripts/dev/verify.sh` run before the batch pushes. Explorer changes `tsc`-clean.
Tailwind v4 additionally spot-checked by rendering the homepage + styleguide.

¹ Register note (2026-07-02): this row was originally logged as CS-008, colliding with the cold-audit's CS-008 (tenant isolation in handlers, Low — 01-cold-system-findings.md). Re-IDed here as CS-008-ssrf; the findings doc keeps the original.
