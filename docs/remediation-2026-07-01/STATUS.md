---
title: Audit remediation — status
date: 2026-07-01
scope: correctness/security audit (docs/audit-2026-06-30) + maintainability audit (docs/maintainability-audit-2026-07-01)
---

# Remediation status

Tracks the fix-everything pass over both audits. Operator-only items live in
[audit-remediation-operator-actions.md](../operations/audit-remediation-operator-actions.md).
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
| CS-008 | Med(sec) | 3 divergent SSRF blocklists (webhook guards missed Oracle metadata IP) → one `internal/nettools` union guard |
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

## Verification
Every code fix built + its package tests passed at commit time; `bash
scripts/dev/verify.sh` run before the batch pushes. Explorer changes `tsc`-clean.
