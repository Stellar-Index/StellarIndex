# Audit №3 — findings (HEAD f8c099ee, 2026-08-01)

Skeptic-verified findings, exposure-then-severity ranked. Each carries a
verdict (CONFIRMED = an independent skeptic traced the failure by static
reasoning; PLAUSIBLE = needs runtime state; REFUTED → reviewed-not-carried.md).
CONFIRMED here means corroborated-static, NOT executed. Findings accumulate as
skeptic verdicts arrive.

---

## CONFIRMED

### R5 — 46 web/explorer vitest files never execute in any gate — MED, LIVE (dead gate)
Skeptic aae/dead-gate: CONFIRMED (tried to find the gate anywhere; none exists).
- No workflow invokes them: `grep -rniE 'vitest|pnpm .*test|npm .*test|run test'`
  over all of .github/workflows/ → NO MATCHES. web-explorer CI job (ci.yml:580-649)
  = install/generate:api-diff/typecheck/lint/build/audit, no `pnpm test`.
  explorer-deploy.yml + CF Pages build cmd = build only. Makefile has no web-test
  (390-442). verify.sh (114-118) typecheck/lint/build only. No husky
  (core.hooksPath points at a defunct /Users/ash/code/ratesengine path, not committed).
- 46 *.test.* files under web/explorer/{src,functions}. Dead security-regression
  gates: functions/og/og.test.js (OG-SSRF on a live CF edge Function),
  src/lib/safe-domain.test.ts (phishing/image-SSRF), no-duplicated-price-formatters.test.ts.
- Severity MED: no evidence of a current regression (tests presumably pass), but
  zero CI signal on security-relevant runtime code.
- FIX: add a `web-test` Make target running `pnpm test`, wire into ci.yml + verify.sh.

### R6 — no gate enforces the serializeJsonLd XSS chokepoint; CSP unsafe-inline = no backstop — MED, LIVE (missing gate, no live XSS)
Skeptic aae/dead-gate: CONFIRMED as missing-gate/defense-in-depth; NO live XSS
(all 14 dangerouslySetInnerHTML sites currently route through serializeJsonLd;
`grep '__html' | grep -v serializeJsonLd` → none).
- react/no-danger NOT enabled in either eslint.config.mjs (proven: 14 danger sites
  + green lint). No seo.test.* source-scan. CSP `script-src 'self' 'unsafe-inline'`
  verbatim in both public/_headers; no Trusted-Types, no nonce/sha256 → an injected
  inline <script> executes. serializeJsonLd (seo.ts:59-72) is the ONLY defense.
- Proven incident class: cc9fe451 (stored-XSS via hostile SEP-1 ORG_NAME in
  build-time JSON-LD). Today 100% compliant; risk is a 15th block with raw
  JSON.stringify shipping green.
- R5↔R6 INTERLOCK: closing R5 (run vitest) is the prerequisite for a future
  seo.test.ts source-scan to enforce R6 — and simultaneously revives the OG-SSRF
  + safe-domain security tests. FIX both together.
- FIX: (a) close R5; (b) add a seo source-scan test; (c) consider react/no-danger
  as error with an allowlist; (d) longer-term nonce-based CSP dropping unsafe-inline.

### R4 — spoofable synthetic-UA erases traffic from all HTTP metrics + SLO — MED, LIVE-remote
Skeptic a58/config: CONFIRMED (recalibrated HIGH→MED).
- http_middleware.go:98 isSyntheticUA = bare strings.HasPrefix on client UA
  (:152-168), NO peer-IP/loopback guard; `return` at :99 skips HTTPRequestsTotal
  + Duration + SuccessDuration (SLO numerator). Caddy forwards raw UA
  (Caddyfile.api:122). RateLimit is inside HTTPMetrics and UA-independent → throttle
  intact (evasion-of-DETECTION only). MED not HIGH: Caddy JSON access log
  (Caddyfile.api:147-152) captures every request → parallel Loki/journald surface
  the attacker can't erase. Real risk: a latency/error probe never lands in the SLO
  histogram → SLO-burn alert never fires on it.
- FIX: gate isSyntheticUA on a loopback/trusted_proxy peer (config already has
  trusted_proxy_cidrs=["127.0.0.1/32"]).

### R8 — DEPLOY_APPROVAL_RELAXED=true makes the deploy approval gate a no-op — LOW/MED, LIVE-in-CI
Skeptic a58/config: CONFIRMED. gh variable = true (2026-07-26). deploy.yml:132-135
`exit 0` bypasses the required-reviewers check; deploy-protection.yml:65-68 mirrors
(no alarm). Fail-closed on all other values incl. unset. Intentional pre-prod
(R1 no traffic); actionable = re-arm before launch (gh variable delete + set r1
env Required-reviewers). Already tracked in the launch plan's operator list.

### R3 — /v1/accounts/{g} returns 500 (not 503) on refresh-gate saturation — MED, LIVE
Skeptic aae/DoS: CONFIRMED (recalibrated HIGH→MED; a status-code correctness bug,
NOT the DoS the framing implied — the gate is doing its job, capping detached scans
at 4). Full trace: first-request fabricated key + saturated gate → refreshAccountState
closes ch empty (account_state_cache.go:202-210), no scan launched → AccountStateCached
returns errAccountStateRefreshFailed (:135) → handler (account_state.go:285-299):
ClientAborted false, readTimedOut false (plain sentinel, deadline never hit) →
Logger.Error + 500. Siblings map saturation→503 via errRefreshSaturated +
retryableColdMiss (reader.go:70), but errAccountStateRefreshFailed is in pkg clickhouse
— no errors.Is arm crosses the boundary. Reachable: ~4 concurrent cold keys saturate
the shared 4-slot gate; further cold keys 500. Also mislabels a LEGIT fresh-account
lookup during a 4-whale-scan window. Pollutes 5xx SLA + error logs.
FIX: export/translate the sentinel → route through retryableColdMiss → 503 (one arm).

### M-B — two-sided TWAP combine is trade-count-weighted (wrong) — MED, LIVE
Skeptic a400/money: CONFIRMED, paper-traced. aggregates.go:576-578 flipped-direction
combine SUM(dir_twap * trade_count)/SUM(trade_count) — the exact weighting migration
0081 says a TWAP must NOT have, and that the VWAP twin (combineDirVWAP) was corrected
away from 2026-07-24. Reachable: trades keep pool orientation (trades.go:94), both
(X,Y)+(Y,X) rows exist; served via /v1/chart?price_type=twap (chart.go:267, NOT
/v1/twap which uses raw-trade compute). Paper trace: 5 large A-dir + 500 dust B-dir →
~2.4% bias vs time-fair. Untested on the flipped path (storage_test.go:541 is
single-direction; chart_test.go:202 stubs the reader). Count is never right for TWAP
(the per-dir value is already time-weighted; correct cross-weight is time-coverage,
for which trade_count isn't even monotone). The 1.0/twap inversion is NUMERIC, not a
precision issue — the weighting is the defect. Bounded by inter-direction divergence →
MED not HIGH. FIX: port combineDirVWAP's reconstruct-the-other-leg time-fair combine
to TWAP + a flipped-row test.

### F-2 — ch-rebuild -write + backfill-router rewrite below-watermark served rows with no dirty-window record — MED (HIGH blast radius), operator-GATED
Skeptic a400/money: CONFIRMED for ch-rebuild (trades/sep41_*/-contract-calls→soroswap-
router+band) + backfill-router; REFUTED for backfill_chainlink + backfill_external
(chainlink/external NOT in buildReconciliationCatalogue — vendor-sourced, no lake to
reconcile, no claim to carry). Only projector-replay (projector.go:111) +
projected-rebuild (projected_rebuild.go:226, refuses -write without it) record. ch_rebuild.go
writes trades (:553), sep41_transfers (:576), sep41_supply (:591) over arbitrary
-from/-to (:118) with a winning derive_generation (:162), NO RecordProjectionDirtyWindow.
Those tables ARE reconcile targets → compute-completeness carries the prior clean
projection claim over [A,B] (dirtyReconcileFloor :914); substrate+recognition are
lake-level, can't catch a served-tier rewrite. Trace: a below-watermark ch-rebuild that
introduces divergence (the file enumerates the ways: pre-0057 event_index residue, KALE
2× rollup, NULL usd_volume overwrite) → next daily incremental runs watermark→tip,
carries stale clean claim over [A,B] → /v1/coverage certifies rows that no longer match
the lake. Exactly the 0125 class, left open on the MOST-USED re-derive tool. Trigger is
conjunctive + operator-gated (a correct ch-rebuild IMPROVES fidelity). FIX: same
RecordProjectionDirtyWindow-before-write guard on ch-rebuild + backfill-router.

---

## CONFIRMED — informational / fact-corrections

### W-1 — projector "sole writer" is CONVENTION (one ansible line) for 15/17 sources — LOW/INFO, LIVE
Skeptic afaa/ingest: CONFIRMED mechanism, LOW. persist_per_source=true (toml.j2:75) →
SinkModeSkipSoleWriter → skipInSink returns IsSoleWriterProjected which is ONLY sep41
(sink.go:501). So 15 sources dual-write (dispatcher + projector), safe only via
ON CONFLICT DO NOTHING. Real + honestly documented (sink.go:78-80 "double-write for the
duplicate-absorbing ON CONFLICT soak") — an intentional Phase-3 soak, NOT corruption:
"≤1 row per PK" == "one writer" for correctness IFF both writers decode deterministically
to byte-identical rows. The only divergence vector is F-6, which is REFUTED. ADR-0032's
"structurally impossible" is Phase-4 end-state aspiration; code is Phase-3 and says so.
Action: none required for launch; ADR-0032 status should note Phase-3-not-4 (doc).

### CI-red — main red since 2026-07-30 on 2 integration tests — FIXED this session
Recon finding (live CI state). TestExplorerScanQueries (synthetic key_xdr can't match
the 85f706e1 PK-prefix reader) + TestClickHouseTxHashIndexProbeFallback (empty index
granted authoritative-miss). Both broken by MY OWN perf work, uncaught because verify.sh
doesn't run the Docker integration suite. FIXED + merged to main (full suite green 753s/
0 fails). LESSON for the recipe: the local gate ≠ CI; a red default branch is invisible
to verify.sh — check `gh run list` as recon.

### R1 — production anon rate limit is 6000/min not 60 — INFO, LIVE
Skeptic a58/config: CONFIRMED fact. toml.j2:238-239 (6000/6000), enforced
main.go:390-399. Caddy has NO rate_limit directive (all 168 lines read); nftables
limits only SSH 30/min + ICMP; public 80/443 bare accept. Not a defect — a recipe
correction: every abuse/capacity estimate that assumed 60 is 100× off.

### R7 — Redis no-AUTH is the canonical tier-bearing key store — LOW/MED, GATED-on-host-foothold
Skeptic a58/config: CONFIRMED mechanism, exposure GATED (recalibrated HIGH→LOW/MED).
- Mechanism end-to-end: auth_backend defaults redis (config.go:1021,1523), prod
  template never overrides; record carries Tier (apikey_redis.go:97,279-286);
  injected {tier:operator} passes requireOperator (admin_accounts.go:110-127,
  admin_keys.go:79) and KeyPolicy short-circuits scope checks for operator
  (keypolicy.go:82-84); account kill-switch doesn't bite (accountActive:330-337).
  No Redis AUTH (16-prometheus-exporters.yml:54).
- BUT GATED: Redis binds loopback + nftables default-drop with internal_allow_ports:[]
  (r1.yml:267); port 6379 nowhere → needs a PRIOR host foothold (SSH/RCE), and
  anyone with redis-cli-to-loopback has already read every secret in the toml. Not
  remote. Defense-in-depth (add Redis AUTH/ACL), Low/Med.

---

## PLAUSIBLE / PENDING
(skeptic verdicts for the DoS cluster R2/R3/R10, ingest W-1/F-6/F-4, money M-A/M-B/F-2 still in flight)
