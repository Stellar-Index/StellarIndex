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

## WAVE-1 (decoders/sources) findings

### W1-external — CEX/FX connectors (finder a4f1)
- **DOUBLE-FIND of M-C** (raises confidence): FX AmountScaleDecimals default-8 with
  DEX-ONLY guard (framework.go:200-205; onchain_decimals_test.go:30-34 covers only
  SubclassDEX). No current FX source mis-scaled (massive/polygon/exchangeratesapi/ecb
  all correctly 6) — the missing LOCK is the finding, CS-040 re-openable. MED-latent.
  FIX: add a test asserting SubclassFX⇒6 ∧ SubclassCEX⇒8, or make AmountDecimals
  mandatory (0 → registry-load panic).
- W1-ext-2 LOW: massive forex client errIsEOF matches substring "EOF"
  (forex/client.go:269) → io.ErrUnexpectedEOF (mid-body truncation) treated as clean
  EOF, returns partial body. Bounded: truncated JSON fails unmarshal → stale cache
  kept, no corruption. FIX: errors.Is(err, io.EOF) not substring.
- W1-ext-3 LOW/INFO: Chainlink inverted-feed reciprocal uses truncating big.Int.Quo
  (chainlink/poller.go:239) not round-half-up scale.InvertScaled → 1-ulp downward
  bias per round. Cosmetic (Chainlink is ClassOracle, IncludeInVWAP:false,
  divergence-panel only). FIX: use InvertScaled.
- EXAMINED-SOUND: CEX money math (binance/kraken/coinbase/bitstamp parse+backfill all
  big.Int quote=base*price/1e8, dust-filtered, no float on integer path); registry
  VWAP gating fail-closed (only ClassExchange IncludeInVWAP:true); reconnect/backpressure
  (ping-watchdog, jittered backoff, ctx teardown, bounded channels); synthetic-txhash
  truncation harmless (ts in the ON CONFLICT unique key). Observation: forex/fx_quotes
  stores FX RATES as float64 (ADR-0003-adjacent, pre-framework architectural choice).
- NOT-EXAMINED (finder-declared): chainlink/{client,backfill,defaults,events}.go,
  frankfurter/client.go, forex/circulation.go, cmc/cryptocompare pollers (spot-checked
  scaling+class only), streamer.go internals (wiring only), ~40 test fixtures.
  → re-queue chainlink/frankfurter in a dry-wave.

### W1-amm — Soroswap/Aquarius/Phoenix/Comet decoders (finder a2ee) — no crit/high; well-defended
- W1-amm-1 MED-latent: phoenix pool_stable 6-field swap silently orphaned if such a
  pool is gated without the 6-field decoder (decode.go:43-52 Complete() requires 8/8;
  README:164-172 pool_stable emits 6). Not reachable (no mainnet pool_stable),
  documented. FIX: ship the 6-field variant before gating any pool_stable.
- W1-amm-2 LOW: soroswap_router CallSig omits op/event position (events.go:175-182) →
  two identical-param router calls in one op hash-collide, one dropped. Bounded
  (log-only intent record, not IsProjectedEvent; per-pair Trade rows still land).
- W1-amm-3 LOW: soroswap gates trade on a trailing Sync it then DISCARDS (consumer.go:29
  requires Swap+Sync; decode.go never reads r.Sync reserves) → a swap ever lacking its
  Sync is dropped with zero benefit. Safe only via the Uniswap-v2 _update→Sync invariant.
- W1-amm-4/5 INFO: aquarius classify() recognizes 11 topics Matches() never claims
  (recognition≠claimed set, weakens ADR-0033 gap signal, no data loss); phoenix group
  key omits the pool contract (hardening inconsistency vs soroswap's COR-08 key).
- EXAMINED-SOUND: i128 never truncates (all big.Int via scval→canonical; grep for
  int64()/.Lo/Int64() on value paths = none); gating CLOSED (registry set-membership,
  no wildcard/default-true; hostile look-alike rejected fail-closed); FanoutOpIndex
  PK correct (op<<16|ev, panics >0xFFFF, per-op-unique); direction/price consistent
  (NewPair preserves base/quote); recognized-no-op returns (nil,nil)+counter.
- NOT-EXAMINED: soroswap factory_seed.go genesis-walk completeness (load-bearing for
  the fail-closed gate — does the seeder enumerate every factory child? depends on
  ops code); aquarius decode_admin.go body (governance, non-money). → re-queue seeder
  completeness.

### W1-supply — supply observers (finder a866) — 1 MED CONFIRMED
- **W1-supply-1 — MED, CONFIRMED-by-inspection (orchestrator verified the asymmetry
  directly).** sac_balances observer stamps the asset_key VERBATIM
  (dispatcher_adapter.go:46-61 NewObserver: `cleaned[cid]=ak`, no canonicalization)
  while its 3 sibling classic observers ALL call supply.CanonicalizeWatchedClassic
  (trustlines:42, claimable_balances:65, liquidity_pools:65). The supply reader queries
  COLON form (storage_classic_reader.go:101 → AssetKey → Code+":"+Issuer, key.go:29).
  So a hand-edited `sac_wrappers = {"C…":"USDC-GA…"}` (DASH) stores SAC balances under a
  key SumSACBalancesAtOrBefore("USDC:GA…") never matches → 0 SAC-wrapped component →
  ClassicComputer under-reports total/circulating supply + market cap by the ENTIRE
  contract-held portion, no error, no startup rejection. This is the documented "USDC
  40M vs 266M real, 85% under-read" incident class (key.go:45 comment). Config-gated:
  example.toml:504 uses colon so copy-paste is safe; bites hand-edited dash configs.
  SupplyConfig.Validate (config.go:1417) only checks non-empty. FIX: call
  CanonicalizeWatchedClassic in NewObserver (one line, matches siblings) OR validate
  the sac_wrappers form + that it matches a watched_classic_assets entry.
- Notes (below threshold): sep41_supply couples amount to counterparty metadata
  (decodeCounterparty error drops the whole event — latent, no reachable failing shape);
  classic.go:24 Trustline docstring wrong (behavior correct); classicmovements omits
  Inflation (deliberate, historical explorer feature not supply).
- EXAMINED-SOUND: i128/stroop never truncates (all big.Int; two's-complement negative
  composition verified); i128-OR-map type-test-before-extract in all 3 sites; CAP-67 vs
  legacy topic-index branch correct (all 2/3/4-topic shapes); sum overflow (big.Int) +
  negative/clamp guards (CS-038); removal absorbing-state + latest-wins DISTINCT ON +
  ledger-scoped pre-image memo; classicmovements recognition-guarded op coverage;
  sorobanevents capture/reconstruct/async-sink backpressure.
- NOT-EXAMINED: ClickHouse toInt256 SEP-41 sum widening (outside 9-dir scope — recon
  ingest noted it as the overflow guard, cross-ref sound); dispatcher decode-error
  accounting (whether a dropped mint/burn is counted/alerted). → re-queue.

### W1-substrate — scval/xdrjson/events/contractid/canonical (finder ae6f) — well-hardened, 1 latent-availability pair
- **W1-sub-1 — LOW→MED as a pair (latent availability): scval.AsBool panics on a
  zero-value ScVal + dispatcher has NO recover() around Decode.** ScvBool==0==zero
  value, so AsBool's guard (scval.go:298 `sv.Type != ScvBool`) passes on xdr.ScVal{}
  then nil-derefs *sv.B — the ONLY accessor that does (every other expects nonzero
  type → clean ErrScValType). MapField (scval.go:468) returns exactly xdr.ScVal{},false
  on a miss, so the package hands out the one value that panics its own AsBool. No
  current caller reaches it (all 6 AsBool sites gate on ok). BUT a future
  `v,_:=MapField(...); b,_:=AsBool(v)` (ignoring ok, which the 2-value sig invites) on
  a map missing the key → panic → dispatcher.go:1253-1264 has NO recover() → ingest
  goroutine HALTS. Same root in display.go:74 MustB. FIX: (a) AsBool guard also checks
  sv.B != nil; (b) add recover() around dec.Decode in the dispatch loop (defense in
  depth — makes substrate panic-safety not load-bearing on every decoder).
- W1-sub-2/3/4 INFO: Display renders U256/I256/Error/NonceKey/ContractInstance as bare
  type-name (cosmetic, explorer rows); MapField first-match on dup keys (not exploitable
  — Soroban host enforces map canonicity on emit); ParseContractDataKey drops Durability
  (not exploitable in single-consumer redstone use, set-dedup + fallback).
- EXAMINED-SOUND: every scval accessor panic-safe against PARSEABLE XDR (SDK allocates
  every union arm on decode — verified for ScVal + ScAddress); i128/u128/u256 → big.Int
  no truncation (FromInt128Parts two's-complement -1 case checked); safetime bounds u64
  BEFORE the int64 cast (defeats >MaxInt64 wrap-negative); AsAddressStrkey all 5 CAP-67
  variants SDK-checksummed; xdrjson all Must* under type-switch on unmarshaled data;
  contractid Seed guarded by IsFactory in Matches, production Matches→Decode pairing.
- NOT-EXAMINED / RE-QUEUE: (1) the genesis-seed/ADR-0033 reconcile PRE-SEED walk —
  does it filter creation events by factory EMITTER before Seed? A walk seeding on
  creation-topic alone would let a forged creation inject a gated child. PAIRS with the
  AMM finder's soroswap factory_seed re-queue → ONE targeted gating-seed finder.
  (2) ClickHouse StateWriteKeys/OpArgs second-populator byte-parity vs the live
  dispatcher rule (drift risk documented but not tested — event.go:88-95). [recon F-6
  refuted on runtime-window grounds, but the PARITY-TEST-ABSENCE stands as a separate
  hardening gap.]

### W1-defi — lending/bridge/oracle/sdex decoders (finder a610)
- **W1-defi-1 — MED, CONFIRMED-by-inspection: SDEX one-side-zero fills fail the whole
  batch INSERT.** sdex.decodeClaimAtom drops ONLY both-zero (decode.go:176 `sold<=0 &&
  bought<=0`); a one-side-zero fill is KEPT with BaseAmount=amountFromInt64(0). trades
  CHECK(base_amount>0)/(quote_amount>0) (migration 0001:30-31, active) always rejects
  it. Single-row InsertTrade Validate()s (trades.go:629); the BATCH builder does NOT
  (no per-row Validate/Sign>0 skip; the only Sign() at :1484 is usd_volume). So one
  zero-leg fill in a 200-row batch → 23514 CHECK violation → non-infra "do not retry"
  (errors.go:74) → flushTradeBatch isolates the ENTIRE batch per-row (trade_sink.go:255)
  → 1 INSERT becomes 201 + a Warn + Error log + SourceInsertErrorsTotal{sdex} increment
  (feeds the decode/insert-error runbook → spurious alert on normal SDEX traffic). Valid
  rows recovered (no data loss); the decoder's "captured for completeness" claim is FALSE
  (the row is always dropped). LIVE on live-ingest + ch-rebuild. FIX: drop one-side-zero
  in the decoder (like both-zero) OR add a per-row Validate/skip in the batch builder.
- W1-defi-2 LOW: sorocredit statement ts uses raw time.Unix(int64(tsUnix)) (decode.go:181)
  not canonical.SafeUnixSeconds (the >MaxInt64 wrap-negative guard band/reflector use).
  Gated emitter → not adversarially reachable; consistency/hardening gap.
- EXAMINED-SOUND: CCTP source-chain substr(message_body,33,40) offset math CORRECT
  (hex pos 33 len 40 = low 20 bytes of the right-aligned burn token); mint_and_forward
  EXCLUDED from inbound sums (no 10× double-count); decimal scales CCTP 1e6/Rozo 1e7/
  Band E9/Reflector E14 all consistent; gating on ContractID never topic-alone across
  all 10; i128 no truncation; SDEX direction self-consistent (no cdp-pipeline swap
  inherited).

## WAVE-1 COMPLETE (5/5 finders). Net: 0 crit/high; 1 MED confirmed (W1-supply-1
sac_balances canonicalization), 1 MED-latent pair (W1-sub-1), rest LOW/INFO. The
decoder/source/substrate ingest surface is solid — i128 discipline holds everywhere,
gating is fail-closed. Re-queues: gating-seed-walk (forged-creation), chainlink/
frankfurter, live/lake state-write parity test.

## PLAUSIBLE / PENDING
(skeptic verdicts for the DoS cluster R2/R3/R10, ingest W-1/F-6/F-4, money M-A/M-B/F-2 still in flight)
