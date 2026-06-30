---
title: Audit 2026-06-30 — synthesis & executive summary
status: Wave-1 + data-correctness + a11y complete; Wave-2/3/4 surface-mapped (see coverage honesty)
---

# Synthesis — what the two audits found

A new-model cold pass over Stellar Index, commissioned to **exceed** nine prior
audits. The prior audits were thorough on Go correctness, auth primitives, decoder
event-coverage, and aggregation math — so this pass spent its energy where they
*didn't*: the never-audited CF Pages Edge Functions, the SSE subsystem (flagged
"no chaos test" but never exploited), multi-tenant IDOR, live data-correctness,
and the product-coherence/logic layer.

The method earned its keep immediately: the surface mapper's six concrete
"flags" had a **~50% false-positive rate** (the "down-migration re-adds retention"
and "plaintext secrets" flags were both wrong on inspection), so every
Critical/High here carries a named failing scenario that survived an adversarial
refutation pass.

## Headline findings (most severe first)

| ID | Sev | Finding | Status |
|----|-----|---------|--------|
| **CS-012** | High | SSE `Hub.Publish` send-on-closed-channel race → **whole API process crashes** (publisher goroutine, no recover). Repro: client disconnects mid-publish. | New |
| **CS-013** | High | SSE cleared write-deadline + no conn cap → a stalled/non-reading client **leaks goroutine+conn+FD forever**; anon can exhaust FDs (DoS). | New |
| **CS-010** | High | **XLM circulating supply served == total == max (~50B); the `xlm_sdf_reserve_exclusion` basis is a no-op → market cap overstated ~+58%** on the flagship asset. Confirms + worsens prior one-sample +48%. | New/confirmed |
| **CS-009** | High* | CF **OG edge function: unauthenticated blind SSRF + no-timeout DoS** via double-decoded, unescaped path → satori `<img src>` fetch with no allow-list. (*reviewer said Medium — blind/edge-sandboxed; kept High as a real edge SSRF.) | New (never-audited surface) |
| **LC-001** | P0 | **19 fiat currencies listed as browseable Stellar assets, ranked above Stellar tokens by M2** — the flagship product incoherence. Fix spec written. | New (user-flagged) |
| **CS-017** | Med | Pricing: **dormant pairs return months-old VWAP with `stale=false`** (400-day window, stale hardcoded false on the VWAP branch). Same "old served as fresh" class as a prior SEV. | New |
| **CS-021** | Med | CH `ledger_entries_current` versioned only by `ledger_seq` → intra-ledger ordering lost → can **resurrect a deleted offer** / report intermediate balance under cross-part interleaving. | New |
| **CS-022** | Med | CH write-buffer cap is in ledger-units, tuned for empty `Changes`; Phase-C entry-capture (highest-volume table) inflated the real byte ceiling on the shared host. | New |
| **CS-025** | Med | The load-bearing lake hole-heal script `ch-live-catchup.sh` is **not in the repo** (only its systemd unit) → the "100% coverage" backstop is unverifiable from the codebase. | New |
| **LC-050/051/052** | Serious (a11y) | `RequestReveal` dialog (on every panel) + mobile nav drawer lack focus trap/Escape; auth/dashboard form errors not announced. Prior Cmd-K Critical IS fixed (1 of 3 modals). | New |
| CS-014/015 | Med | SSE client-controlled sub-second tick + no concurrency cap → DB-load amplification. | New |
| CS-008 | Low | Multi-tenant isolation enforced in handlers, not SQL (fragile; thin store tests). IDOR surface otherwise **clean** (8 candidates refuted). | New |
| CS-001 | Med | Live GCP service-account key in the working tree (gitignored/untracked footgun). | New |
| CS-016/018/023/024 | Low | redispub no-restart silent-degrade; SEP-40 depeg not flagged; CH non-FINAL chart double-count; CH UInt32 underflow + IN-list concat footgun. | New |
| CS-007 | Low | ADR-0003 claims a golangci i128 analyzer + migration BIGINT lint that don't exist (runtime discipline holds). | Confirms D2-10 |
| CS-003/005/006/011/019/020 | Low | postgresstore ~0 tests; CLAUDE.md 3× package undercount; hashdb dead; `/assets/xlm` omits mcap; 2 plan-time non-sargable queries; batch identity-id aborts whole batch. | New/minor |

## Re-verified GOOD (recorded to prevent re-litigation — the system is largely sound)

The adversarial passes confirmed a lot is right, which is as important as the
findings:
- **Multi-tenant IDOR surface is clean** — 8 cross-tenant attack candidates
  refuted; ownership checked before every mutation; staff flag is a DB column, not
  spoofable; empty-subject fails *closed*.
- **ClickHouse core integrity holds** — deterministic dedup with FINAL on all
  counting paths; **contiguity + hash-chain to genesis is actually checked** (not
  assumed); the F-1349 unbounded-buffer concern is genuinely fixed; `explorer_reader`
  keyset pagination + `>2^53`-as-string + no-i128-truncation all correct.
- **Pricing precision + the prior perf fix hold** — the exact 50→400ms non-sargable
  bug is fixed/sargable; stale propagates correctly through the fallback chain;
  price strings are big.Int/big.Rat end-to-end; the P2-4b self-peg arm shipped this
  session was reviewed CORRECT (never overrides a real bucket).
- **i128 discipline holds at runtime** (zero truncation sites; `ErrI128Overflow`);
  the **one-writer projector invariant is in sync** (registry ↔ IsProjectedEvent);
  auth primitives sound (per prior audits, re-confirmed via the IDOR pass).
- **Frontend a11y is mostly right** — skip link, semantic tables, real buttons,
  label association, disciplined null rendering, reduced-motion, the prior Cmd-K
  Critical fixed.

So the headline is: **no Critical survived; the architecture and core data paths
are sound.** The Highs cluster in the newest, least-tested surfaces (SSE, CF edge,
live data-correctness) — exactly where a fresh pass adds the most.

## Cross-audit themes

1. **"Code-correct ≠ data-correct" is the biggest real gap.** Prior audits proved
   the *code* sound but sampled served *values* once (and found XLM +48%). CS-010
   shows it's still wrong (+58%) and the `*_exclusion` supply bases need a
   systematic ground-truth reconciliation (Wave 2). A pricing/explorer product
   whose flagship market cap is ~1.6× reality is a credibility risk at launch.
2. **The newest code is the least-audited and where the security bugs are.** The
   CF Edge Functions (shipped after every prior audit) and the SSE subsystem
   ("no chaos test") held the only real security/availability findings (CS-009,
   CS-012, CS-013). Newest-surface-first is the right heuristic.
3. **Defense-in-depth gaps, not active holes, dominate the platform.** Auth/IDOR
   is sound *today* but relies on caller discipline (CS-008) over enforced
   invariants — the same shape as the absent i128 analyzer (CS-007): the guarantee
   exists by convention, not by a guard-rail. A regression wouldn't be caught.
4. **The product still half-reads as the multi-chain "Rates Engine" it was.** Fiat
   ranked by M2 (LC-001), `reference_only` BTC pages (LC-002), `coins/currencies`
   internals (LC-030), unshipped Unit-D wire shapes (LC-031) — the Stellar-focus
   refactor removed cross-chain *plumbing* but left the *presentation* drift.

## Recommended action sequence

**Before launch (correctness/availability/credibility):**
1. **CS-012 + CS-013** — the SSE crash race + FD-exhaustion DoS. Streams are public
   and anon-reachable; one disconnect-timed-right crashes the API. Highest priority.
2. **CS-010** — fix the XLM SDF-reserve exclusion (or stop labeling the basis) and
   add a CI reconciliation vs Stellar Expert. The flagship market cap is wrong.
3. **CS-009** — escape the OG path input + drop the double-decode (small fix).
4. **LC-001 fiat split** — ship the non-breaking parts (the user's explicit ask);
   spec in `fiat-split-implementation.md`.

**Soon after:**
5. CS-008 (query-level tenant scoping + store tests), CS-014/015 (stream caps),
   CS-001 (relocate+rotate the SA key), CS-016 (subscriber supervision).
6. The cheap IA fixes (LC-020 sidebar, LC-014 dead routes, LC-021/022/023 nav).

**Hygiene / guard-rails:**
7. CS-007 (build the i128 analyzer or fix the ADR), CS-005 (CLAUDE.md map),
   CS-003 (store tests), Wave-2 data reconciliation, a11y (pending).

## Coverage honesty (read-fraction)

Like the prior audits, this pass is explicit about depth: **deep-audited this
pass** = CF Edge Functions (A26/A30), SSE subsystem (A21), ClickHouse lake
round-trip + DDL (A11), pricing read-paths (A33), multi-tenant IDOR (A18),
XLM/USDC supply correctness (A6/A8), the projector one-writer invariant (A3), i128
enforcement (A9/A25), and the full product-coherence/IA + a11y layer (Audit 2).
**Surface-mapped, recommended for a next wave** = the 34-area plan's Wave-2/3/4
(systematic data-reconciliation beyond XLM/USDC; per-source Soroban decode fidelity
for the ~11 row-classified sources; ansible-vs-actual-R1 reconciliation;
systemd/Docker hardening; CI supply-chain re-verify; DR-restore drill;
decoder/XDR fuzzing; the W6 onboarding journey). The exclusions register in PLAN-1
lists what is deliberately out of scope (live R2/R3, WASM disassembly, multi-week
fuzz, vendor portals, destructive chaos). **Net: ~25 CS + ~25 LC findings, 0
Critical, ~6 High, the rest Medium/Low — with the Highs concentrated in the
never-before-audited surfaces the gap analysis predicted.**
