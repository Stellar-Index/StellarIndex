# Audit №3 — WAVE-6 gap-closure findings (accumulating)

The exhaustiveness pass: missed dimensions (DEP/DOM/PERF+CON/PRV+ACC+I18N/TST),
skipped process steps (mechanical sweeps, second-decomposition, dry/convergence-skeptic),
residual files, and prior-audit re-derivation. Findings land here as finders return;
serious ones go to skeptics; merged into findings.md + coverage-ledger.md at wave close.

---

## DEP — dependencies / supply-chain (finder a56, DONE)

Headline traps ALL verified clean, multiple ways:
- **cdp-pipeline-workflow** (verified-buggy, must-not-inherit): CLEAN — no Go import, no
  copied code; exists only as gitignored `.discovery-repos/` reference, skipped by lint-imports.
- **stellar/go archived monorepo**: CLEAN — no `github.com/stellar/go/` import; only
  `go-stellar-sdk v0.6.0` + `go-xdr`.
- **mutable-tag pins**: none exploitable — all 19 workflows' actions are 40-hex-SHA pinned;
  Go deps content-pinned (`go mod verify` passes); galexie tag compensated by an ansible
  binary-SHA256 fail-closed assert.
- **govulncheck**: wired + fail-on-finding on every PR + push (pinned v1.1.4). (Current PASS
  state not independently runnable offline — wiring verified, not today's result.)
- **suspicious transitive / npm float**: none — aws-sdk-go v1 CVE path unreachable (indirect,
  not imported); lockfiles frozen + sha512.

Findings (all LOW/INFO — no crit/high):
- **DEP-1 (LOW, false-green class):** the CI `pnpm audit` advisory gate treats
  `ERR_PNPM_AUDIT_BAD_RESPONSE` as a pass, and the repo's own comment says the npm endpoint
  has returned that error continuously since 2026-07-15 → the frontend HIGH-advisory gate is a
  de-facto no-op; only the weekly trivy-fs scan backstops (≤7d window). `.github/workflows/ci.yml:636-649,684-697`.
  **Fits theme A (verification-layer false-green cluster) — same class as W5-ci-1..5.**
- **DEP-2 (INFO):** `web/explorer/package.json:76-80` suppresses `GHSA-52cp-r559-cp3m` with no
  in-repo rationale/expiry; web/status doesn't carry the same suppression (divergent policy).
- **DEP-3 (INFO):** VERSIONS.md pins galexie only to mutable tag `galexie-v27.0.0`, SHA pending,
  contradicting the file's reproducibility contract — but neutralised at deploy by the ansible
  binary-SHA assert, so doc-gap only.

NOT-EXAMINED (honest): current live govulncheck/pnpm-audit PASS result (needs network); full
SPDX license enumeration of the transitive tree (spot-checked permissive, no scanner offline);
upstream CVE status of the pinned runtime SHAs in VERSIONS.md (needs advisory feeds).

Disposition: **dependency surface is unusually well-managed.** DEP-1 folds into theme A;
DEP-2/3 are INFO hygiene. No new crit/high.

---

## PRIOR-AUDIT RE-DERIVATION (finder ae7, DONE)

Re-derived ~20 highest-stakes prior HIGH/CRITICAL across 5 campaigns vs current HEAD. **All 16
money/data/security keystones HELD** (INV-3 generation-guarded upsert, DAT-15 usd_volume resolver,
SEP-41 CAP-67 topic discriminator, projector sink-fault durability, completeness fail-open on scan
error, M3 FX-triangulation, SAC-archived-seed drop, rate-limit in-proc fallback, SSE recover+AST
guard-test, VWAP volume-weight, i128 composition, trades-retention removed, comet gating,
supply-snapshot ledger-time, completeness projection_verified_from floor, price-latency sargable).
Migrations 0116/DAT-15 code present but inert until re-materialized on r1 (operator step).

Current findings from the re-derivation:
- **W6-derive-1 (MED, LIVE) — TWAP two-sided combine STILL trade-count-weighted.**
  `internal/storage/timescale/aggregates.go:577-578` folds flipped-direction TWAP with
  `* trade_count / SUM(trade_count)`; its VWAP twin was fixed (`combineDirVWAP`, 2026-07-24) but the
  TWAP path was left behind. Served via `/v1/chart?price_type=twap`. **CORROBORATES recon M-B
  independently** — the clearest "fix didn't cover the sibling" case. (Already carried as M-B;
  this is second-source confirmation, promotes confidence.)
- **W6-derive-2 (MED cand, operator-GATED) → SKEPTIC:** `TolerateTrailingMissing` interior-chunk.
  07-16 remediation prescribed tolerate=true only on the final chunk whose `to`=live tip;
  `internal/ops/opsutil/opsutil.go:300` (`NewBoundedLedgerStreamConfig`) appears to set it
  UNCONDITIONALLY, and `maybeTolerateTrailingMissing` bounds against the chunk's `to` not the live
  tip. Only the COR-01 single-ledger sub-case was fixed. A chunked/parallel ops backfill could
  silently accept a genuine interior hole ≤64k window. Mitigant: contiguity/substrate reconcile
  surfaces a resulting gap downstream; 08-01 ingest audit didn't re-flag. → skeptic to confirm/refute.
- **W6-derive-3 (ch-rebuild missing dirty-window)** = already carried as recon F-2 (ch-rebuild half).
  Second-source confirmation.

Prior DEFERRED/accepted-risk still open + launch-relevant (operator inbox, not code):
- **C2-11 soroban_events truncates >4 topics** (`sorobanevents/reconstruct.go:94` `if want>4`) —
  Aquarius ≥5-topic multi-token pools silently lose topics. Still open (schema + re-ingest).
- SEP-41 genesis baseline seed ORDER (don't seed before sep41_supply complete:true — bakes wrong
  market-cap); DEPLOY_APPROVAL_RELAXED=true (gate no-op); main zero branch protection;
  stale-data-alert no metric producer + reliable-paging Discord-only no dead-man's-switch;
  privacy policy/terms/export-delete absent; GH Actions crons dead since org transfer (red-main
  tripwire + weekly security/drift never run). → all already in plan OPERATOR INBOX; re-confirmed open.

Honest caveat: site-audit-07-22 S1–S35 are live-site/runtime claims not verifiable from a read-only
checkout; the MED/LOW tail of 07-16/07-23 not re-derived.

---

## MECHANICAL SWEEPS (finder ae3, DONE — 10 named sweeps + 2 repo-specific)

**Security-critical sweeps GENUINELY CLEAN across the whole tree:** authz/IDOR (~15 param routes,
all ownership-scoped or public on-chain data), secret-in-tree (0 prod secrets; vault-encrypted),
SQL-string-build (every interpolated identifier compile-time-constant, values bind as params),
unchecked-type-assertion (0 single-value `.(T)` in sources/scval/events/projector/dispatcher),
missing-timeout (all http.Client carry Timeout; WS dialer correct), unbounded (caches cap+evict,
limits ≤500, ops scans under statement_timeout), money-as-float (~50 hits all NUMERIC-space or
by-design analytics), TODO/FIXME (all substring false-positives).

- **W6-sweep-1 (LOW) — promised-but-absent observability.**
  `internal/supply/storage_classic_reader.go:124-131`: comment says "Operator's signal is the WARN
  log line" but code does `_ = err` with no logger field → on a transient `MinClassicComponentLedger`
  query error, `minLedger` silently defaults 0 → `MinComponentLedger=0` → the Refresher's
  stale-component freshness gate goes fully permissive, no operator signal. Fail-permissive posture
  is deliberate (F-1236); the defect is the missing WARN. **Quick fix: add the logger + WARN line.**
- INFO notes (no finding): account_state_reader.go:357 float `/1e7` on the /v1/accounts wealth
  ranking (= existing M-A class, display-only); stellarrpc panic (diagnostics-only); **goroutine
  recover INCONSISTENT** — `baseline/refresh.go:187`, `external/runner.go:263/283`,
  `streampublish/publisher.go:123` have no `defer recover()` (corroborates fresh-eyes #6 + W4-cmd-1
  aggregator panic-isolation), but no reachable panic traced → latent-trap note.

---

## FRESH-EYES INDEPENDENT DECOMPOSITION + CONVERGENCE-SKEPTIC (finder aa1, DONE)

Extraction-derived, threat-first, no prior plan seen. Counts: 110 mux routes (11 write, 5 SSE,
1 loopback-gated /metrics), 7 CF Pages catch-all Functions + OG generator, 59 ops subcommands,
10 systemd timers, 19 workflows (8 cron), 1 Redis pub/sub channel, ~15 long-running goroutine
workers. 9 assets, 6 adversaries. **The value: 7 CROSS-BOUNDARY blind spots a subsystem sweep
structurally cannot find — exactly the convergence-skeptic's job.**

- **W6-fresh-1 (HIGH cand) → SKEPTIC: `/v1/price` BYPASSES the freeze/outlier/volume guards.**
  `internal/aggregate/served_guard.go` documents it: freeze/σ-outlier/min-USD-volume protect the
  orchestrator→Redis VWAP, but `/v1/price` reads the `prices_1m` continuous-aggregate DIRECTLY,
  never passing through freeze. Compensating `GuardServedVWAP` FAILS OPEN on an empty baseline and
  only catches order-of-magnitude deviation. A single manipulation/fat-finger trade in the served
  minute on a real-`prices_1m` pair → served price corrupted with `stale=false`, no volume floor.
  The freeze audit + the price-handler audit are each sound in isolation; the danger is purely in
  their INTERACTION. → skeptic to confirm reachability + which pairs have prices_1m rows unprotected.
- **W6-fresh-2 (MED cand) → SKEPTIC: aggregator→Redis→API SSE bridge has no payload validation.**
  Producer `orchestrator.StreamPublisher` → Redis `stellarindex:closed-bucket:v1` → `redispub`
  subscriber → SSE fan-out. Neither side validates the serialized payload the API trusts off the
  channel. Redis write access (host-adjacent adversary) → inject arbitrary `price_update` SSE to all
  clients. No schema validation, no producer signing. → skeptic (residual-go a93 also covers redispub).
- **W6-fresh-3 (MED cand) → VERIFY: signupreaper DELETEs accounts on a timer, no request-path authz.**
  API binary runs `signupreaper` (DELETEs orphan `accounts` rows), `logincodereaper`, usage rollups,
  webhook delivery. Check the reaper's orphan predicate — a too-broad `Suspended`/no-key filter could
  delete LIVE accounts — and that every `go worker.Run` is recover-wrapped (a rollup once took the
  whole API down). → verify predicate.
- **W6-fresh-4 (MED, latent) — completeness-verdict staleness const decoupled from its timer.**
  `coverageVerdictStaleLedgers=34560` + `coverageVerdictStaleAge=26h` hand-calibrated to the daily
  completeness.timer ("CALIBRATED TO THE DEPLOYED CADENCE 2026-07-26"), NO CI lint linking them
  (unlike the enforced metric/alert/runbook triad). Change the timer cadence → the served-verdict
  staleness flag silently flaps or never trips → recreates the "served 15/15 complete forever after
  the audit died" failure (MNY-04's class). Real unguarded cross-artifact invariant. → negative-space.
- **W6-fresh-5 (MED, known-but-unowned) — CF Pages Functions are a 2nd non-Go request/proxy trust
  boundary.** 7 catch-alls translate status codes (already bitten: REL-02 soft-200→503 cache-poison
  fix) + the OG generator makes an UNAUTHENTICATED live upstream fetch to the public API on every
  edge request (self-noted R7) = an unauthenticated amplifier against origin, no rate limit. Per-fn
  mitigations exist (MAX_ID_LENGTH, asset-id shape check) but unaudited as a SET. → residual-web scope.
- W6-fresh-6 (config toggles move trust boundaries; prod wires them safely — no live defect, no-owner
  observation) + W6-fresh-7 (SSE per-IP cap degrades to global behind CDN; `warnCollapsedStreamCap`
  already warns) — both NOTES, not findings.

Negative-space gaps (SHOULD exist, don't): (a) no inline lake-vs-archive reconciliation — a rewound
upstream lake has a full inter-audit window to feed bad ledgers into the served price; **(b) no
operator global kill-switch on the price path** — only per-pair freeze / aggregator restart during
an active manipulation incident; (c) no rate-limit on the OG edge→origin fetch; (d) no gap/replay on
the redispub SSE bridge (clients silently miss closed buckets across reconnect); (e) no CI lint for
W6-fresh-4's const↔timer; (f) Audit sink degrades to log-only when nil (prod wires it; silent-degrade
is the gap). Items (b) + (e) are the most launch-relevant negative-space gaps.

**Convergence-skeptic verdict on "we covered everything": the subsystem sweep is deep PER BOX, but
the served price crosses 3 process boundaries + 4 datastores + a JS edge tier a Go audit never
enters. The strongest blind spots (#1 CF Functions, #2 Redis bridge, #3 price-bypasses-freeze,
#4 const↔timer) are all cross-boundary/cross-artifact — undiscoverable by any single subsystem's
files. This is the real residual and it is now surfaced.**

---

## PRV / ACC / I18N (finder a98, DONE)

ACC is NOT absent — real, consistent a11y consideration (sortable tables are real `<button>`+aria-sort,
combobox has full keyboard nav + role=dialog, decorative icons `alt=""`+aria-hidden). Defects are the
contrast token + one unrole'd chart. PRV masking is mostly correct (customer emails masked, poller API
keys kept out of logs, /metrics loopback-only, no PII in metric labels). Findings:
- **W6-prv-1 (MED) — session/magic-link PII retained indefinitely.** `sessions` (ip_first/last_seen,
  user_agent, geo) + `magic_link_tokens` (email, requested_ip) never DELETEd — only soft-revoked;
  the sole retention policy in the tree is on dead-code `api_usage_events`. GDPR Art.5(1)(e)
  data-minimisation. `migrations/0027_platform_v1_schema.up.sql` + `postgresstore/{user,token}_store.go`.
- **W6-prv-2 (MED) — no data-subject deletion path (GDPR/CCPA erasure absent).** No `DELETE /v1/account`,
  no `DeleteUser`/`CloseAccount` store method; `users.account_id` is ON DELETE RESTRICT. A "delete my
  data" request has no code path. `internal/api/v1/server.go` route table + `internal/platform/`.
  **W6-prv-1/2 are launch-relevant compliance gaps → OPERATOR INBOX (product/legal + a small code task).**
- **W6-acc-1 (MED) — `--color-ink-faint #5b6472` fails WCAG AA contrast (3.29:1 canvas / 2.89:1 muted;
  AA needs 4.5:1), used as text 254×** incl. small (10px) rank cells/timestamps/metadata.
  `web/explorer/src/app/globals.css:44`. Fix: stop using ink-faint for text (ink-muted #8a91a0 passes).
- **W6-acc-2 (LOW)** Sparkline SVG has aria-label but no `role="img"` (every other chart pairs them) —
  the most-used chart may be nameless to AT. `primitives/Sparkline.tsx:59-67`.
- W6-acc-3 (INFO) hairline borders 1.32–1.60:1 vs WCAG 1.4.11 3:1 for control boundaries (design-intent).
- **W6-i18n-1 (LOW)** `lib/format.ts` comment claims "locale-respecting" but pins en-US; ~40 bare
  `.toLocaleString()` follow browser locale → mixed separators on one page for non-US users +
  possible hydration mismatch. Cosmetic (monolingual product). RTL/translation N/A by design.
- W6-prv-3 (LOW) staff actor email logged unmasked (`dashboardauth/handlers_admin.go:81,90,171`) —
  lower sensitivity (internal, audit-correlation) but inconsistent with the maskEmail policy.

## RESIDUAL WEB (finder a2a, DONE)

Money-critical web paths heavily hardened post-wave-5. **Embed money widgets SOUND** (all prices are
quote/USD < 2^53, absent→"—" not 0, sparklines guard length<2, no account data → no cross-origin leak);
stroop/i128 all BigInt-divide-first; dashboard CRUD credentialed + server-ownership + full-page reload
clears cache (no cross-account leak). Findings (both LOW):
- **W6-web-1 (LOW)** OraclesView TONE map: `reflector-cex` bg-brand-100/text-brand-800 = dark-navy on
  dark-navy ≈1.4:1 illegible; `reflector-fx` uses stock light-mode violet on dark canvas.
  `oracles/OraclesView.tsx:22-24` (reused in the streams table). Off-theme + illegible chip.
- **W6-web-2 (LOW)** `entries_24h: number` typed required + unguarded `.toLocaleString()`
  (`status/StatusPageClient.tsx:130,1821`; same `SourceHealthPanel.tsx:46`) while sibling evolving
  fields are optional+guarded → on explorer-ahead-of-API deploy skew, the field's absence render-throws
  and the segment error boundary BLANKS the entire customer-facing status page. Latent (API serves it now).
- Notes: LivePrice staleness is hover-only tooltip (inherent to a ticker); embed/pair div-by-zero →
  Infinity trend color (cosmetic, chip correctly hidden).

## RESIDUAL GO + ANSIBLE (finder a93, DONE)

**TOP RE-QUEUE CLEARED — the forged-creation gating BYPASS DOES NOT EXIST.** All three attribution
surfaces double-gate: live (`{blend,defindex,aquarius}/dispatcher_adapter.go` gate deploy/create on
`reg.IsFactory`), ops seed-walk (`seed_protocol_contracts.go:145` → SQL `contract_id IN(factories)` +
`Matches`), reconcile pre-seed (`gated_recon_seed.go:36-44` identical). A forged creation from a
non-factory contract is filtered at SQL AND by Matches. **i128 SOUND** across all residual readers
(comet/phoenix/blend/liquidity pool-state, chainlink int256, token_decimals). opsutil SplitRange
gapless, job_heartbeat atomic-write no race, redispub one-bad-message isolation correct. Ansible
patroni (sync-mode ANY-1-of-2), haproxy (VRRP unicast split-brain mitigation), redis-sentinel
(quorum 2-of-3, requirepass) all SOUND. Findings:
- **W6-go-1 (INFO; MED-if-reachable) — WASM export parser preallocs from attacker-influenced LEB128
  count.** `wasm_export_parse.go:179,298,314` `make([]T,0,count)` where count=uvarint up to 2^64 → a
  ~20-byte body can encode count≈5e9 → ~250GB alloc → OOM before the read loop hits errTruncated. NOT
  reachable today (only fed from on-chain `contract_code`, Soroban-validated at upload); becomes MED
  the instant any un-validated WASM path (operator upload) feeds it. **Cheap defensive cap worth adding.**
- **W6-go-2 (LOW)** frankfurter `client.go:143` breaks the body loop on any error containing "EOF" →
  `io.ErrUnexpectedEOF` (mid-stream drop) treated as clean end; degrades to a decode error not
  corruption, operator fx-backfill only. Use `errors.Is(rerr, io.EOF)`.

NOT-EXAMINED residual (flagged): chainlink client/backfill/defaults/events, cmc/cryptocompare poller
decimal math (spot-checked class only), a few grep-scanned CH readers (tx_hash_index, tx_index,
contract_call_op, sdex_op, entry_change, recognition), ch_gate.go, ansible task/*.yml + redis data-plane
+ loki/prometheus roles. No money/security FLOW left un-reasoned; residual is lower-risk surface.

## DOM — domain-logic vs protocol truth (finder a80, DONE)

**Extensive protocol-truth EXAMINED-SOUND:** CAP-67 4-topic vs legacy 3-topic vs bare mint/burn/clawback
discrimination + i128-or-map body; classicmovements CreateAccount/Payment/PathPayment(both)/CB/AccountMerge
with correct issuer attribution; SDEX ClaimAtom OrderBook/LP/V0 variants + sold=base/bought=quote
orientation + both-zero drop; SEP-40 reflector update Map shape + 14dp; band E9 single-symbol; redstone
signed-payload median + refuse-on-ambiguity; i128/u128/u256 two's-complement composition; all 5 CAP-67
ScAddress strkey byte-layouts; stablecoin fiat-proxy map + exact-rational decimals factor; XLM Alg1 +
SEP-41 Alg3 supply formulas match ADR-0011. The Stellar/Soroban protocol is modeled correctly, not just
self-consistently. Findings:
- **W6-dom-1 (HIGH cand) → SKEPTIC (a00):** real-time VWAP sums raw smallest-unit BaseAmount/QuoteAmount
  across sources with DIFFERENT scales (on-chain 7dp / CEX 8dp / FX 6dp) with no per-trade normalization →
  a CEX trade weighted 10× an equal-volume on-chain trade → flagship price biased toward CEX, worst in
  depeg/arbitrage windows. `aggregate/vwap.go:34-56` (+ SourceContributions:110-150);
  `orchestrator.go:1295-1345` merges expanded source-pairs leaving native scale; the tell:
  `orchestrator.go:1376-1440` (usd-volume gate) DOES normalize per-source scale per-trade (MNY-05/CS-040)
  but the VWAP number doesn't. → skeptic verifying reachability (does the mix reach one Σ; is there a
  correction; is the proxy path enabled).
- **W6-dom-2 (INFO)** XLM total supply is a hardcoded round constant (`supply/xlm.go:38` 50_001_806_812·1e7)
  not the ledger-header `total_coins` — no drift alarm against the authoritative on-chain figure. Low today.

Excursion gaps (declared): SAC G-account vs C-account holder coverage (classic trustline holdings of a
SAC-wrapped asset — downstream reader reliance not traced); the per-protocol event-shape decoders
(SwapEvent/SyncEvent correlation, Phoenix 8-event, Comet POOL topic) not individually re-traced beyond
CLAUDE.md claims; the prices_1m/15m/1h VWAP CAGG SQL arithmetic not read (the scale-mix doesn't reach
them — a stored canonical pair is single-source-scale — but the CAGG math itself unaudited).

## PERF + CON — systematic (finder ad2, DONE)

**PERF/CON hot surfaces exceptionally hardened.** Independently EXAMINED-SOUND: orchestrator single-runner
invariant (working maps touched only inside serialized Tick — no concurrent-map race), freeze recovery
cross-process coordination (Redis/PG, no in-proc race), streaming hub send-on-closed (CS-012 guarded) +
topic-reaper lock ordering, ratelimit + signup-throttle localStore hard-capped + REL-06 dwell, trade_sink
retry buffer, contractid registry, dispatcher single-threaded ProcessLedger, SWR caches (history/markets/
issuers/catalogue/account_state/wealth/ttl) single-flight + LRU-capped, N+1 sweep (no request-path per-row
amplification), SQL non-sargable sweep (the aggregates.go instances outside HistoryPoints carry from/to
bounds). Findings:
- **W6-perf-1 (MED)** `assetDetailResponseCache` is the LONE unbounded non-evicting response cache
  (`asset_detail_cache.go:91-101` — no size cap, no janitor; the code comment admits the gap). Fabricated
  ids 404 before put, so growth is bounded by the REAL-asset universe (10^5+); a crawler walking known
  asset_ids climbs resident memory to hundreds-of-MB–~1GB that TTL-expiry never releases → slow OOM on a
  long-lived instance. Every sibling cache is capped (accountState 4096, holders 512, wealth 1). Fix: the
  janitor/cap the comment anticipates.
- **W6-perf-2 (LOW)** per-request fan-out width `readFanoutConcurrency=16` (+ `priceBatchConcurrency=16`)
  is 64% of the 25-conn serving pool → two concurrent cold fan-out requests (32>25) contend + head-of-line
  block unrelated queries (latency, not deadlock). `bounded.go:17`, `price.go:1220` vs pool 25.
- INFO: HistoryPoints (`aggregates.go:294-301`) carries the non-sargable `bucket+INTERVAL<=now()` tip
  filter uncached — defensible IFF a `(base,quote,bucket)` index exists on each prices_<g> CAGG (couldn't
  verify from source — **worth an operator check**); sdex_orderbook snapshotPair O(live-offers) scan/request.

Excursion gaps (declared): customerwebhook/usage-rollup/pricealerts/logincodereaper/signupreaper/platform
background workers not opened (goroutine/ctx-leak + Redis-TOCTOU territory — note signupreaper is under
skeptic a67); redispub publisher not opened (= fresh-eyes #2); classic_supply DISTINCT ON watermark bounds
unverified; the prices_<g> CAGG index existence.

## TST — test quality (finder af3, DONE)

**Money/gating/completeness test coverage genuinely strong:** foreign-contract-rejection tests
(`Matches=false`) exist for ALL 6 gated protocols (comet/soroswap/phoenix/blend/aquarius/defindex);
contractid fail-closed default asserted; VWAP negative/zero/all-zero/i128-scale boundaries; supply-drift
2-stroop FIRES WithinTolerance=false; completeness reconcile catches missing+phantom+zero-delta-blind-spot;
redstone ambiguous/no-match/bijection refuse; usd_volume asserts a real ~5.00 value + NULL-when-unanchored;
pricing/decimals/alert fat-finger rejection asserted. Findings:
- **W6-tst-1 (MED)** `runVerifyChunks` (archive hash-chain verify orchestration, ADR-0033/0016) has ZERO
  executing test — its only test is `//go:build integration` in `internal/ops/archive/`, a package NOT in
  `INT_TEST_PKGS` (Makefile:41), so no CI job compiles or runs it. A refactor of the chunk-stitch/counter
  could silently report a bad range verified with green CI. `verify_archive_chunks_integration_test.go` +
  `verify_archive.go:487` + `Makefile:41,114`.
- **W6-tst-2 (MED)** the ADR-0003 i128-truncation GUARD has no positive control — `i128_truncation_guard_test.go`
  asserts only ABSENCE of violations, has no synthetic-`int64(p.Lo)` self-test proving `checkConversion`
  still fires, and there are ZERO live `i128:ok` markers → "detector works, tree clean" is
  indistinguishable from "detector silently broken." The decode primitive itself IS boundary-tested; the
  risk is the guard ceasing to catch NEW truncation bugs (the "a guard that never fails is decorative"
  class, verify-done §3). Fix: a positive-control test feeding a known-truncating snippet.
- **W6-tst-3 (LOW)** SDEX one-leg-zero KEPT asymmetry (`sdexclaim.go:82` `sold<=0 && bought<=0`) untested —
  decoder + census lockstep only test both-zero + only OrderBook atoms (not LP/V0) → a `&&`→`||` edit
  breaks the completeness reconcile invisibly.
- **W6-tst-4 (LOW)** customerwebhook SSRF DNS-rebinding guard (`ssrf.go:28` `ssrfGuardedDialContext`, the
  F-1245 anti-rebind control) has no test — only the delegated `nettools.IsBlockedIP` block-list is tested;
  the resolve-and-refuse + dial-by-resolved-IP composition is unexercised.
- Notes: freeze recovery_test `TestRecovery_ListErrorIsNonFatal` assertion-free (didn't-panic only);
  reconcile COUNT-only by design (DAT-15, architecturally acknowledged).

═══ ALL 10 WAVE-6 FINDERS DONE. 4 skeptics in flight (a00 dom-1, a76 price-bypass, a79 tolerate, a67 reaper). ═══

---

## SKEPTIC VERDICTS (appended as they land)

- **W6-fresh-3 signupreaper → REFUTED (a67, positive exoneration).** Predicate ANDs SIX conditions
  (`account_store.go:257-265`): `status='suspended'` AND `suspended_reason LIKE 'signup-race:%'`
  (machine-stamped ONLY on a race-LOSING, user-less orphan at `handlers.go:795`, inside the
  ErrConflict branch) AND `suspended_at < now()-24h` AND `NOT EXISTS users` AND `NOT EXISTS api_keys`.
  Every live-account case the finding raised is provably excluded (email-verified → users row exists →
  fails NOT EXISTS; operator-suspend carries a different reason; mid-flow orphan is <24h). Backstops:
  ON DELETE RESTRICT FKs (migration 0027:89,181) abort loudly on any mislabeled row with a child;
  `defer recoverBackgroundWorker` on the goroutine (main.go:1618) — panic-isolated, contra the prior
  rollup incident. The finding invented a non-existent "activity-timestamp filter." → INFO at most (the
  no-request-path-auth framing is moot given the predicate). Deletes only never-completed placeholder
  shells. **MOVED to reviewed-not-carried.**
- **W6-derive-2 TolerateTrailingMissing → REFUTED (a79), residual LOW latent-note.** The mechanism is
  REAL: `opsutil.go:300` sets `TolerateTrailingMissing:true` unconditionally, and
  `maybeTolerateTrailingMissing` (`ledgerstream.go:316-347`) bounds against the CHUNK's `to` and does
  NOT distinguish interior from tail — so at the ledgerstream layer alone an interior hole IS swallowed
  (the "tolerate is tail-only" refutation does NOT hold). BUT every lake-writing backfill reconciles the
  walk against a persisted required-count contract that fails closed: `backfillCoverage`
  (`ch_backfill.go:224-238`, `want=to-from+1` vs `streamed=prog.total()` delivered-count, not
  chunks-returned-nil) + `censusCoverage` (`census_backfill.go:254-267`) — the "second half of the trap"
  remediation, landed 2026-07-25, AFTER the 07-16 COR-01 fix the finding cited (why it was missed). Short
  count → hard error → non-zero exit → window not appended to resume state → re-runs. The `delivered==0`
  sub-case is caught even harder (`streamed==0` hard error). ADR-0033 substrate/contiguity reconcile is a
  2nd independent defense. **Residual LOW latent-fragility: the safety lives entirely in each caller's
  coverage wrapper — a future lake-writing caller of `NewBoundedLedgerStreamConfig` that forgets its own
  `*Coverage` check reopens the class.** Worth a defensive assertion/lint. Non-writing callers
  (verify_archive, wasm_history, diagnostics) don't create substrate holes → out of scope.
- **W6-fresh-1 /v1/price freeze-bypass → DOWNGRADED HIGH→LOW/MED (a76).** The "bypass" premise is FALSE
  on its load-bearing point: `pricingguard.GuardServedVWAP1m` IS invoked on exactly the direct-CAGG read —
  `main.go:3237` (`served := GuardServedVWAP1m(...); return served`), and the same guard on
  `/v1/assets/{slug}` headline (main.go:2997) + the alert evaluator (aggregator main.go:2067). The
  compensating control is wired, not absent. RESIDUAL (correctly identified, bounded): `GuardServedVWAP`
  (served_guard.go) is a robust-BAND sanity guard, not σ/volume — accepts within [median/3, median×3] on
  populated pairs (a 3× move is DELIBERATELY served; >3× rejected → last-known-good), [median/10, median×10]
  on thin history, and **FAILS OPEN unbounded on an empty baseline (a pair's first-ever served bucket)**
  (served_guard.go:147-149). Deep/headline pairs (crypto:XLM/fiat:USD) effectively immune (minute VWAP is
  volume-weighted → single trade diluted, AND >3× rejected). Exploitable surface = a thin single-source
  pair's near-empty/first minute — already surfaces `flags.single_source=true` (price.go:483). Couldn't
  positively exonerate the first-bucket empty-baseline case → **DOWNGRADED not refuted, corrected severity
  LOW-MED.** Fix: serve first-ever/empty-baseline buckets as `stale=true`/low-confidence + a min
  served-bucket USD-volume floor in GuardServedVWAP1m. SECONDARY note (distinct, minor): the freeze FLAG is
  a separate Redis lookup (`lookupFrozen`, price.go:1030) while the served VALUE is the guard-checked raw
  CAGG → for a monitored frozen pair, /v1/price can serve the raw bucket WITH `flags.frozen=true` (flag/value
  can disagree). Consistency issue, not a manipulation vector. NOTE: does NOT protect against W6-dom-1 —
  a systematic scale-mix bias moves the trailing median identically, so the relative band can't catch it.
- **W6-dom-1 VWAP scale-mixing → REFUTED (a00), positive exoneration.** The 7dp on-chain and 8dp CEX
  trades occupy DISJOINT canonical (base_asset, quote_asset) namespaces, so no single `aggregate.VWAP` Σ
  ever sums across scales. CEX stamps XLM base as `crypto:XLM` (8dp; binance/pairs.yaml:29 +
  externalAmountDecimals=8 across all CEX), quote `crypto:USDT`/`fiat:USD`; on-chain DEX stamps XLM base as
  `native` (SDEX, asset_xdr.go:44) or the XLM SAC C-address (Soroban), 7dp (registry.go:34-38), quote
  classic `USDC-GA5Z…`. The decisive fact: `store.TradesInRange` (trades.go:1241-1249) does
  `WHERE base_asset=$1 AND quote_asset=$2` EXACT match, NO alias resolution — so a `crypto:XLM/USDC-GA5Z`
  fetch returns zero on-chain rows and `native/USDC-GA5Z` returns zero CEX rows. Proxy expansion holds
  target.Base FIXED (stablecoin.go:216,231). Target `crypto:XLM/fiat:USD` → all-8dp single-scale slice;
  target `native/fiat:USD` → all-7dp single-scale slice; SEPARATE Redis keys, never co-mingle. The only
  on-chain packages stamping a `crypto:<ticker>` base are the oracles (IncludeInVWAP=false). The
  "blurs 7-vs-8" comment is about the USD-volume GATE's per-trade attribution (which spans the whole
  expansion incl. FX 6dp), not a VWAP mix; `TestTick_MinUSDVolumeFilter_StablecoinProxyLegs` mixes only
  coinbase+binance-USDT (both 8dp) and publishes correct 0.20. **REFUTED (0 severity). No fix required —
  the per-source scale split is handled structurally by the identity-namespace partition, which is why the
  VWAP needs no per-trade normalization while the cross-leg dollar gate does.** Residual (different topic):
  on-chain XLM/USD and CEX XLM/USD compute as SEPARATE prices under separate keys, reconciled at serve time
  by `readPriceWithAliases` priority order — a consolidation/coverage question (= the XLM dual-form seam
  already known), corrupts no computed number.

═══════════════════════════════════════════════════════════════════════════════════════════════════
## WAVE-6 CLOSE — SYNTHESIS

**All 4 serious candidates REFUTED or DOWNGRADED under adversarial verification. Wave 6 added ZERO new
CONFIRMED HIGH.** The audit's HIGH count stays at 2 (W4-obs-1 metric DoS + W3-freeze-1 freeze sibling).

Wave-6 CONFIRMED additions (all MED or below):
- **MED (8):** W6-prv-1 (PII indefinite retention), W6-prv-2 (no GDPR deletion path), W6-acc-1 (WCAG-AA
  contrast, 254× text), W6-fresh-4 (completeness staleness const↔timer, latent — no CI link), W6-perf-1
  (assetDetailResponseCache unbounded), W6-tst-1 (runVerifyChunks no-CI test), W6-tst-2 (i128 guard no
  positive control), W6-fresh-1 (/v1/price empty-baseline fail-open — downgraded from HIGH). Plus
  W6-derive-1 = second-source confirmation of the already-carried M-B (TWAP count-weighted).
- **LOW/INFO (~14):** DEP-1 (pnpm fail-open → theme A), DEP-2/3, W6-sweep-1 (promised-absent WARN),
  W6-acc-2/3, W6-i18n-1, W6-prv-3 (staff email), W6-web-1/2, W6-go-1 (WASM prealloc — INFO now, MED if any
  un-validated WASM path ever feeds it), W6-go-2 (frankfurter EOF), W6-perf-2 (fan-out 16/25), W6-tst-3/4,
  W6-dom-2 (XLM hardcoded supply). Plus the W6-derive-2 latent-fragility note.
- **Negative-space (most launch-relevant):** (b) no operator global kill-switch on the price path;
  (e) no CI lint linking W6-fresh-4's staleness const to its timer cadence.
- **REFUTED (4):** W6-dom-1, W6-derive-2, W6-fresh-3 (+ W6-fresh-1 downgraded). ~100% of wave-6's serious
  candidates dissolved — the adversarial pass worked exactly as designed on the highest-stakes items.

Wave-6 PROVED SOUND (do not re-audit): the forged-creation gating BYPASS does not exist (triple
double-gate); i128 holds across ALL residual readers + protocol-truth (CAP-67/SDEX/SEP-40/strkey/i128
two's-complement modeled correctly vs spec); all 16 prior HIGH/critical keystones still fixed; PERF/CON
hot surfaces + security-critical mechanical sweeps (authz/secret/injection/type-assertion/timeout) clean;
gating-rejection tests exist for all 6 gated protocols; embed money widgets hardened; ansible HA sub-roles
sound. The convergence-skeptic's cross-boundary blind spots were surfaced AND verified — the real residual
is now known and bounded.
═══════════════════════════════════════════════════════════════════════════════════════════════════

