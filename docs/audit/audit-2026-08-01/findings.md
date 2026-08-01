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

## WAVE-2 (api/auth/platform) findings

### W2-auth — mutating+auth surface (finder adba) — NO crit/high exploitable; 1 MED (gated)
- **W2-auth-1 — MED, CONFIRMED-by-finder, GATED on auth_backend=postgres (NOT live on
  r1's redis backend): account kill-switch bypassed by the Postgres validator's
  read-through cache.** apikey_postgres.go:221-287 cache-HIT re-checks only RevokedAt
  (:239) + ExpiresAt (:242), NEVER account Status; cache-MISS DOES (:138-140 rejects
  !=AccountActive) — the two paths disagree. admin_accounts.go:209-234 kill-switch calls
  Update + clampKeysAfterTierChange, which early-returns unless the TIER ceiling drops
  (:268-278) — a status-only change invalidates nothing. So on postgres backend, suspend/
  close lets a recently-cached key authenticate for up to cacheTTL (1h) after the switch;
  operator + audit believe it was immediate. Same root: quota/rate override lowering not
  propagated to cached Subjects. r1 runs redis (accountActive fresh per-request,
  apikey_redis.go:330) → not live. FIX before postgres cutover: invalidate the key cache
  on status/override change, or re-check Status on the cache-hit path.
- W2-auth-2 LOW: /v1/signup mints a usable 1000/min key for an UNOWNED email;
  require_email_verification=FALSE by default (signup.go:216, require_email_verified.go:41).
  Acknowledged in code, per-IP throttled, read-only API → accepted upstream. Bounded.
- W2-auth-3 INFO: 6-digit login code c%10 over 32-symbol base32 → digits 0/5 at 4/32 vs
  3/32 (auth.go:102). Negligible (5 attempts + durable lockout + constant-time compare).
- EXAMINED-SOUND: IDOR on every {id} route (dashboardkeys/webhooks/pricealerts +
  account self-service all scope id→account.ID, 404-on-mismatch); scope-prefix trap
  (only mutating routes outside the 3 scoped prefixes are HMAC/anon/cookie, none
  key-scoped user mutations); Stripe (HMAC over body, drift replay window, hmac.Equal,
  empty-secret 503, dedupe+deadletter); SEP-10 (header pinned vs alg:none, iss+exp,
  SETNX replay on canonical hash, fail-closed); session (login-intent browser-bind,
  single-use Consume, HttpOnly+Secure+SameSite=Lax, next= open-redirect guarded,
  suspended-session denial); CSRF RequireSameSiteWrite fail-closed; admin operator-gated
  + X-Reason audited, closed PATCH struct (no mass-assignment); rate-limit fail posture.
- NOT-EXAMINED / RE-QUEUE: internal/ratelimit/{bucket,fixedwindow,inprocess}.go dwell/
  fail-closed math + Redis atomicity (every fail-closed claim above depends on the
  sustained-outage→ErrThrottleUnavailable transition being correct); signup throttle
  internals; streaming/{hub,periplimit,redispub} SSE caps. → Wave-4 small-pkgs finder.

### W2-platform — billing/webhook/usage/notify (finder a98f) — NO crit/high; 1 MED
- **W2-plat-1 — MED (data-integrity, permanent): usage_daily per-endpoint analytics
  permanently over-counted by a Redis SCAN duplicate.** ScanDetail (counter.go:182-208)
  appends readDetailHash for EVERY key SCAN yields, no visited-set; Redis SCAN may return
  a key twice during rehash. groupDetails (rollup.go:147-169) folds dups with OK+=Count
  (sum). usage_daily upsert is GREATEST(existing, new) (usage_daily.go:30-35) → an inflated
  sweep locks in forever; once the day leaves the today/yesterday window it's never
  re-swept → /v1/account/usage permanently ~2× for that endpoint/day. NOT the billing/quota
  counter (MonthlyQuota uses separate legacy usage:<sub>:<day> keys, unaffected). No
  adversary. FIX: visited-set dedup in ScanDetail (SCAN semantics), or a per-key idempotent
  merge that survives duplicate emission.
- W2-plat-2 LOW: customer-webhook at-most-one-in-flight violated under load — batch of 100
  claimed once with a 5-min lease (webhook_store.go:297), processed serially at ≤10s each
  (>16min worst) → during a 2-worker deploy instance B re-claims expired-lease tail rows A
  hasn't delivered → double POST. Stable X-StellarIndex-Delivery-Id lets an idempotent
  receiver dedup; the doc over-promises "never." FIX: per-row re-lease or shorter batch.
- W2-plat-3 LOW: price-alert cooldown bypass — fanOut aborts mid-loop on an EnqueueDelivery
  error after enqueuing earlier webhooks; evaluateOne skips MarkPriceAlertFired (only on
  err==nil, worker.go:186) → next 30s sweep re-fires to already-served webhooks (cooldown
  never stamped); dups carry DIFFERENT delivery-ids → evade receiver dedup. Requires an
  enqueue failure; best-effort alerting. FIX: mark-fired for the webhooks that succeeded.
- EXAMINED-SOUND: SSRF guard THOROUGH (169.254.169.254 via IsLinkLocalUnicast incl
  IPv4-mapped, RFC1918/ULA, CGNAT 100.64, Oracle 192.0.0, NAT64 64:ff9b::/96 + ::1:/48,
  dial-time resolve + pin ips[0] closes DNS-rebind, redirects disabled); HMAC (plaintext
  256-bit secret shared signer/verifier, over ts.body); Stripe claim/lease/advisory-lock
  dedupe (ErrEventInFlight not dup-acked → no money-in-nothing-provisioned); quota INCR/
  HINCRBY int64 monotonic, key lock-step, fail-open intentional+counted; cross-account
  isolation (fanout lists only ListWebhooksForAccount, payload own-fields only; global
  fanout = public events only); email injection none (Resend JSON not raw SMTP,
  html/template escaping); atomic token consume + per-email lockout; audit List parameterized.
  GetSession revoked-not-expired gap is EXAMINED-SOUND (middleware.go:123 enforces
  ExpiresAt.After(now) one layer up — verified by orchestrator).
- NOT-EXAMINED: platform/postgresstore/{store.go,status_notice_store.go,invites}, platform
  type-defs, apikey_postgres quota-cascade unmetered-default (by-design business-logic gap:
  quota=0 + override=0 = unmetered, no per-tier default). Note: internal/platform/usage.go
  = confirmed DEAD CODE (no writer).

### W2-explorer — catalogue/explorer handlers (finder a6b7) — 1 MED SECURITY + 1 MED
- **W2-explorer-1 — MED SECURITY, CONFIRMED-by-inspection (orchestrator verified the
  asymmetry): /v1/accounts/{g}/movements can impersonate a trusted asset.**
  resolveSEP41MovementAsset (movements.go:331-339) returns SACAssetFromEvents VERBATIM
  with NO cross-check, while the sibling wasm_view.go:201-202 does
  `derived,_:=asset.SacContractID(); if derived != contractID { return "",false }` and
  the reader's own docstring (wasm_lake_reader.go:417-426) says "The caller MUST
  cross-check … the topic is attacker-influenceable on non-SAC contracts." SEP-41
  transfers are ingested from ANY token contract (not identity-gated). Attack: deploy a
  non-SAC token emitting a 4-topic CAP-67 transfer with sep0011_asset ScString =
  "USDC:GA5ZSEJYB…" (Circle USDC), airdrop 1 unit to a victim → victim's public unauth
  movements render asset:"USDC:GA5Z…" impersonating Circle. Display-layer (not
  money-moving, not cross-account leak); the "any contract can emit any topic" trap
  CLAUDE.md warns about. Secondary LOW: events path returns colon-form CODE:GISSUER vs
  canonical dash CODE-GISSUER (inconsistent, breaks ?asset= filter). FIX: mirror
  wasm_view's SacContractID cross-check in resolveSEP41MovementAsset.
- **W2-explorer-2 — MED: movements ?asset= filter applied in Go AFTER the SQL LIMIT.**
  fetchSEP41MovementsTail → ListSEP41TransfersByAddress takes NO asset param
  (sep41_transfers.go:389); the filter runs post-fetch (movements.go:289 `if asset !=
  assetFilter continue`). Matching rows beyond page 1 unreachable; a page whose newest
  25 transfers are a different asset → merged=[], no next_cursor, no coverage_note
  (set only on nil-reader/error) → "no USDC movements" when there are many. Violates the
  endpoint's own honest-degrade contract. FIX: push the asset filter into SQL, or emit
  coverage_note + next_cursor on an underfilled filtered page.
- EXAMINED-SOUND: /v1/assets/{slug} two-shape dispatch (GlobalAssetView vs AssetDetail,
  Kind at single funnel); DEX TVL big.Rat lower-bound labeled (UnpricedPools/Basis);
  analytics status ok/stale/unavailable degraded≠zero; coverage two-axis; oracle
  scaledDecimalString big.Int; explorer ids validated (IsContractID/IsAccountID/txHashRe);
  keyset cursors on full pages only (no tie-drop); search.go pure classification no
  injection; issuers scam-suppression; NetworkThroughput stroops-as-strings.
- Notes: changes.go:67 moneyStr(float64)+assets_global FormatFloat(InverseUSD) — display-
  grade prices/FX <2^53, not ADR-0003 violations (trace if ever fed i128-scale).
- NOT-EXAMINED: status.go/status_notices.go analytics-honesty, incidents/methodology/
  currencies, assets_f2 populateMarketCap decimals-overlay, asset_catalogue caches.
  → Wave-4/dry-wave.

### W2-pricing — pricing/history handlers (finder ab49) — money discipline SOLID; DoS REFUTED
- Pricing-F1 DoS amplification REFUTED (orchestrator): the finder's ">60s empty-pair
  scan" relies on a STALE measurement predating migration 0037's composite index
  `trades_pair_source_ts_idx (base_asset,quote_asset,source,ts DESC,ledger DESC)` — which
  exactly matches LatestTradePerSource's WHERE base=$1 AND quote=$2 ORDER BY source,ts
  DESC,ledger DESC → an empty pair is a fast index SEEK to zero rows, not a scan (code
  comment observations.go:115 confirms "index-covered"). No connection-pool amplification.
  → moved to reviewed-not-carried.
- **W2-pricing-1 — LOW (merges pricing-F2 + markets_cache): history_cache + markets_cache
  grow WITHOUT bound** — keyed on user-controllable asset/cursor/params, eviction ONLY on
  cold-fill error (history_cache.go:200, markets_cache.go:242), no size cap/TTL sweep →
  slow memory growth over process lifetime under distinct-key churn. FIX: LRU cap or TTL
  eviction (the explorer caches already have maxEntries; apply the same here).
- W2-pricing-2 LOW: /v1/observations/stream omits the fiat:USD short-circuit the sync
  handler has (observations_stream.go:114 vs observations.go:116) → cold-key stream hits
  the (now index-covered, 8s-bounded) scan. Minor.
- W2-pricing-3 LOW: /v1/history/since-inception doesn't loop base aliases native↔crypto:XLM
  (history.go:630,704 use literal pair.Base; contrast chart.go:646 loops assetAliases) →
  ?asset=crypto:XLM returns empty where ?asset=native returns years. XLM-dual-form trap.
  FIX: loop assetAliases like the other price surfaces.
- EXAMINED-SOUND: ALL money/amount/OHLC/VWAP/TWAP/market-cap/pct-change on the wire are
  decimal STRINGS with big.Rat/big.Int math (floats are confidence/z_score/share_pct
  only); closed-bucket serving (ADR-0015) enforced; alias loops present on price/price_at/
  price_changes/chart/ohlc; cursor full-PK; limits bounded. Sargability trap CLEARED (the
  bucket+INTERVAL predicates are paired with selective equality — filter the in-progress
  bucket off an index-pruned set, not range-defining).

## WAVE-2 COMPLETE (4/4). Net: 0 crit/high; MED: W2-auth-1 (postgres kill-switch cache,
GATED off-r1), W2-plat-1 (usage analytics over-count), W2-explorer-1 (SAC label spoof,
SECURITY), W2-explorer-2 (movements asset-filter-after-limit). Pricing DoS refuted. The
money-on-the-wire discipline + auth/billing/SSRF core are SOLID. Headliner = the SAC
label spoof (public unauth trusted-asset impersonation).

## WAVE-3 (aggregate/divergence/ops-cli) findings

### W3-guards — divergence + decimalsguard + guards (finder a6bc) — 1 MED money
- **W3-guards-1 — MED (money path), CONFIRMED-by-inspection: decimalsguard latches the
  dedup flag BEFORE the DB write + never retries → a non-7dp token can serve a 100×-wrong
  price indefinitely.** report() (guard.go:385) sets fired[key] before
  UpsertNonstandardDecimalsAsset (:402); later Sweep/Backfill for the same (source,asset)
  early-returns (:382) → NEVER re-attempts. The nonstandard_decimals_assets table is the
  SOLE input to read-time AdjustPrice (decimals.go:101, feeds /v1/price,vwap,twap,ohlc,
  price/at). A transient Postgres blip at first-detection → the row is never written →
  every price for pairs touching that 9dp asset skewed ×100 with stale=FALSE, until
  aggregator restart or hand-seed. The Warn log MISDIRECTS ("a later sweep will fix it" —
  impossible via the early-return). Mitigated: one-shot detection metric+ERROR alert
  fires (operator alerted), restart recovers, non-7dp assets rare. FIX: set fired[key]
  only AFTER a successful write (or split metric-latch from write-success-latch).
- W3-guards-2 LOW/MED: divergence compares a 5-min CLOSED-bucket VWAP vs reference SPOT
  (divergence_refresh.go:61 shortest window; references use asOf=now) → a fast market
  move produces a false divergence warning + edge-triggered customer webhook driven by
  WINDOW LAG not data error. Transient, customer-visible, webhook-amplified. Same class
  as the recon anomaly-freeze-on-correct-prices [DECIDE]. FIX: compare like-for-like
  (tip price vs spot) or annotate window-lag vs true divergence.
- W3-guards-3 LOW: 2-reference median has zero outlier robustness — one flaky vendor
  (>threshold/2 off) fires a divergence warning even when the OTHER reference agrees
  exactly (compare.go:204 + worker.go:305 OR's the median leg with agreement). Reachable
  for pairs with exactly 2 responders (MinSourcesForWarning=2).
- W3-guards-4 LOW: operator Chainlink FX feeds inherit the 3h crypto MaxAge
  (chainlink.go:180) → go dark every weekend (FX heartbeats 24h, ages ~72h by Sunday;
  built-in FX uses 76h but operator additions don't). FIX: default MaxAge by feed class.
- EXAMINED-SOUND: sdexclaim drop-rules verified BYTE-FOR-BYTE vs sdex decodeClaimAtom;
  domain money fields all *big.Int (float64 only on documented cross-check/statistical
  shapes, not ADR-0003 value path); pricingguard exact-rational, empty-baseline fails
  open no panic; coingecko/oracle/supply staleness gates fail-closed; chainlink round
  decode word-offsets + two's-complement + int64 overflow guard correct.

### W3-archive — ops archive/diagnostics/supply CLI (finder a6f3) — destructive ops SOUND
- **W3-archive-1 — MED (audit tooling): extract-wasm-from-galexie reports success on a
  FAILED write.** maybeWriteWasmCode sets found[hash]=outPath (wasm_extract.go:338) BEFORE
  os.WriteFile (:341); on write error (ENOSPC/perms) it only prints stderr + returns, never
  rolls back found → the report counts it extracted → exit 0 + -early-exit fires (:186),
  violating the tool's own "exit non-zero on partial completion" contract (:221). The
  WASM-history audit gate (for flipping BackfillSafe) proceeds believing <hash>.wasm is on
  disk when it isn't. Same MARK-BEFORE-FALLIBLE-OP class as W3-guards-1 (decimalsguard) →
  RECURRING (recipe trap). FIX: write first, mark found only on success.
- W3-archive-2 LOW: hubble-check certifies "OK" exit 0 when it compared NOTHING (both
  sides empty → zero diffs → "every ledger matches", hubble_check.go:191,384). The ONE
  verifier missing the vacuous-run guard every sibling has (verify-rollup checked==0,
  cross-region totalComparable==0, verify-archive verified==0/matches==0, archive-
  completeness Vacuous). Memoried verification-blind-spot class. Bounded (both-empty).
  FIX: add len(ours)+len(theirs)>0 guard.
- EXAMINED-SOUND (high value — the destructive/verify surface): trim-galexie-archive
  delete gate SOUND (dry-run default, --commit exclusive, --max-files cap, cold.Exists
  fail-closed on error+absent, SEPARATE cold-tier upstream so presence-check is genuine);
  supply seed-sac ARCHIVED-entry bug FIXED (both paths route through ClassifyTTLLiveness,
  drop TTLArchived); verify-archive/cross-region/archive-completeness all have vacuous
  guards; all supply seeds big.Int no truncation; rehydrate non-destructive. Notes:
  hubble-soroban COUNT(DISTINCT xdr) undercounts identical events (diagnostic); verify-
  decoders/external no exit semantics (documented dry harness); seed-sep41-genesis allows
  genesis<boundary (deliberate override only).

### W3-freeze — aggregate freeze/anomaly/mev/confidence (finder a490) — 1 HIGH (pending skeptic) + freeze cluster
- **W3-freeze-1 — HIGH candidate (PENDING SKEPTIC): sibling-window auto-release publishes
  a still-frozen window's manipulated price.** Marker key omits window (keys.go:401
  freeze:asset:quote); lifecycle is per-(asset,quote,window) (orchestrator.go:943);
  engage MarkHold + release Clear both by PAIR only (phase2_freeze.go:366,466);
  loadFreezeState (phase2_freeze.go:309) returns overridden=true when the marker's gone.
  So: manipulation freezes 5m+1h; 5m auto-releases ~10min later → Clear() deletes the
  shared marker; next tick the 1h window (still Active, anomaly live) sees marker gone →
  overridden=true → publishes the manipulated 1h VWAP with flags.frozen=FALSE. Auto-release
  of ONE window force-unfreezes ALL windows + corrupts multi-window ladder rehydration on
  restart. Mechanism VERIFIED by orchestrator; REACHABILITY (≥2 windows frozen concurrently
  + release order) → skeptic. FIX: window in the marker key, or per-window markers.
- W3-freeze-2 — MED/HIGH candidate (PENDING SKEPTIC): Phase-2 z-score compares a
  rolling-window-VWAP tick-delta (confidence.go:95, prev=previous TICK's window VWAP) vs a
  baseline calibrated on 1-MINUTE-bucket returns (baseline/refresh.go:138) → z biased LOW →
  z>5 under-fires on 1h/24h → a genuinely manipulated price never Phase-2-freezes on longer
  windows (a MISS, opposite of F1). Population mismatch. → skeptic.
- W3-freeze-3 MED: a Phase-1 freeze with unscored buckets can never satisfy the
  auto-unfreeze streak (lifecycle.go:426 resets on !Scored) → always escalates to
  permanent operator-only even after price recovers; reachable for thin single-source
  pairs with no baseline. Bounded: prod wires Baselines unconditionally (main.go:453).
- W3-freeze-4 LOW: SourceCountFactor(1)=0.119 not the documented ~0.3 (factors.go:187) →
  single-source confidence stricter than spec → compounds freeze-on-correct-single-source.
- These interact with the recon anomaly-freeze [DECIDE] (sources=1 pages on correct prices).
- EXAMINED-SOUND: MEV detectors are labeled candidate-generators (false-positives by
  design, no event-DROP/ordering bug); anomaly decision/threshold guarded; confidence
  score weighted-geomean + bootstrap cap; baseline MinMAD floor + NaN/Inf handling;
  changesummary display-grade.

### W3-ops — ops ingest/chops destructive CLI (finder a9ae) — 1 HIGH (pending skeptic) + F-2 extensions
- **W3-ops-1 (F-A) — HIGH candidate (PENDING SKEPTIC): resume-stalled overwrites correct
  usd_volume with NULL.** backfill() sets SetDeriveGeneration + InstallUSDVolumeResolution
  (backfill.go:204-205); resume_stalled opens a BARE store (resume_stalled.go:478, neither,
  no shared helper) then reuses runBackfillChunk → InsertTrade with gen=0. trades.go:668
  DO UPDATE SET usd_volume=EXCLUDED.usd_volume WHERE gen<=EXCLUDED.gen → at gen 0 (0<=0
  true) overwrites; with no resolver tradeUSDVolume returns nil → usd_volume=NULL. The
  A-CRIT-1 guard (store.go:156) fires only at gen>0 → SILENT (its own comment warns of this
  gen-0 blind spot). resume-stalled (167 cursors × ~100-150k ledgers) re-walks whole ranges
  incl. already-correct rows → NULLs their usd_volume → every DEX-volume/venue/market-share
  surface under-reports until re-derived. FIX: resume-stalled must SetDeriveGeneration +
  InstallUSDVolumeResolution like backfill (or COALESCE usd_volume in the upsert). → skeptic.
- W3-ops-2 (F-B) MED: backfill-router writes soroswap_router_swaps (catalogue source)
  below-watermark with gen>0 but NO RecordProjectionDirtyWindow (backfill_router.go:79,153)
  → F-2 class confirmed in-scope. W3-ops-3 (F-C) MED: main backfill writes sdex trades
  below-watermark SinkModeAll, no dirty-window (backfill.go:204,397) → F-2 extension
  (weaker: needs the re-walk to diverge from the LCM census). resume-stalled inherits F-C.
- W3-ops-4 LOW: verify-recognition scans legacy Postgres soroban_events (verify_recognition.go:68)
  not the CH lake (ch-recognition.go:86) → false-OK on a narrow slice; CH path covers it.
- EXAMINED-SOUND (high value): projector-replay records dirty-window BEFORE rewind fail-closed;
  trim-galexie gated (verified in W3-archive); census-backfill/state-snapshot/ch-backfill/
  reproject/supply/txindex/participant/classic-movements all lake-idempotent (ReplacingMergeTree)
  or gen-guarded; ALL verify-* (reconciliation/served-values/usd-volume/contiguity/hashchain/
  lake/reconcile-balances) fail-closed on empty range/all-skipped; big.Int/big.Rat no truncation.
- RECURRING CLASS (recipe): "mark/latch/count BEFORE the fallible side-effect" now 3×
  (decimalsguard W3-guards-1, wasm-extract W3-archive-1, and the gen-0-blind-spot family).
  "any tool writing a reconcile-target table below the watermark must RecordProjectionDirtyWindow"
  now ≥4 offenders (ch-rebuild, backfill-router, backfill, resume-stalled).

## WAVE-3 COMPLETE (4/4). Net: 2 HIGH candidates (freeze sibling-release, resume-stalled
usd_volume NULL) both PENDING SKEPTIC; freeze cluster (miss + stuck-frozen + spec) + F-2
extensions + decimalsguard money + wasm-extract. Destructive/verify ops surface largely SOUND.

## PLAUSIBLE / PENDING
(skeptic verdicts for the DoS cluster R2/R3/R10, ingest W-1/F-6/F-4, money M-A/M-B/F-2 still in flight)
