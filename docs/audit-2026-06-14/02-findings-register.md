# 02 — Consolidated findings register (whole-system audit)

**Audit date:** 2026-06-14 · **Synthesized:** 2026-06-15
**Method:** 24-area read-only fan-out (A01–A22 per-area + X1/X3-X5/X4 cross-cutting
seams), each area read against dimensions D1–D9 + X-codes. Every High/Critical was
adversarially re-checked.
**Headline:** **0 Critical · 0 confirmed committed secrets.** 25 High, 60 Medium,
117 Low, 57 Info — **259 findings** as itemised in this register. (That is the
24-planned-area census of 257 + the 2 Highs the X6/X9 remediation passes
surfaced, folded into the High table below. X6/X9 also produced 14 sub-High
findings, itemised in their own area files; the **full 26-area census is 273** —
see `03-coverage-matrix.md`.) One real GCP key exists on disk but is correctly
`.gitignore`d and untracked (R-A21-A22-5).

**Post-remediation (2026-06-15): all 25 Highs addressed — 22 FIXED + pushed, 3
DOCUMENTED with rationale, 0 unaddressed.** See `04-verdict.md`.

## Severity rollup (sum of all per-area `Severity counts`)

| Severity | Count |
|---|---|
| Critical | 0 |
| High | 25 |
| Medium | 60 |
| Low | 117 |
| Info | 57 |
| **Total** | **259** |

After the full 2026-06-15 remediation pass: of the 25 Highs, **22 are FIXED +
pushed, 3 are DOCUMENTED** (R-A12-1 anon-rate-limit-is-the-ceiling, R-A15-2/3
immutable-already-applied-migrations), **0 unaddressed.** (R-A11-4/5/6 — three
A11 Mediums — were also FIXED-2026-06-15.) The earlier "9 fixed / 14 open"
snapshot was taken mid-remediation; this is the final state.

> Reconciliation note: a few area files' own `## Severity counts` headers round their
> lowest tier ("Low / Info") together or are off by one vs their findings table (e.g.
> A03 declares Medium 3 but its table has 2 Medium + 5 Low; A04 declares "Low/Info 6"
> for 7 rows). This register + the coverage matrix both use the **actual finding rows**;
> the column sums of `03-coverage-matrix.md` equal this rollup exactly (257).

> ID scheme: `R-<area>-<ordinal>` where ordinal = the finding's position in that
> area file's `## Findings` table (top-to-bottom). `status` is one of
> `FIXED-2026-06-15` or `open` per the resolution facts at the foot of this doc.

---

## Master register (sorted Critical→High→Medium→Low→Info, then by area)

### Critical

_None._

### High (25)

> Post-remediation status (2026-06-15): **22 FIXED, 3 DOCUMENTED** (accepted with
> rationale), **0 unaddressed.** Two new Highs (R-X6-1, R-X9-1) were found by the
> X6/X9 cross-cutting passes commissioned during remediation and are both FIXED.
> DOCUMENTED = a deliberate decision not to code-change: R-A12-1 (the global
> anon rate-limit is the intended ceiling — see verdict), R-A15-2/3 (rewriting
> already-applied immutable migrations is riskier than the fresh-install-only
> hazard they carry).

| # | area | sev | file:line | one-line issue | status |
|---|---|---|---|---|---|
| R-A04-1 | A04 | High | projector/projector.go:253 | Idle guard `tip <= fromLedger` skips the durable tip ledger, so every source sits one ledger behind tip and the final ledger is lost if ingest halts. | FIXED-2026-06-15 |
| R-A05-1 | A05 | High | sep41_supply/decode.go:75-97 | `decodeCounterparty` hard-codes legacy-SAC topic indices, so a spec/CAP-67-shaped mint/clawback errors out and the whole supply event is dropped. | FIXED-2026-06-15 |
| R-A07-1 | A07 | High | sep41_supply/decode.go:75-97 | Same sep41 mint/clawback counterparty topic-position bug (corroborated from the SEP-41 v0.4.1 + CAP-67 primary docs): drops events → `total_supply` undercounts. | FIXED-2026-06-15 |
| R-A11-1 | A11 | High | explorer_contracts.go:67-69 / explorer_reader.go:337-365 | Contract-events keyset cursor is `ledger_seq`-only; a ledger with >limit events silently drops the rest across the page boundary. | FIXED-2026-06-15 |
| R-A11-2 | A11 | High | explorer_accounts.go:77-79,116-118 / explorer_reader.go:218-259 | Same ledger-only cursor bug for `/accounts/{g}/transactions` + `/operations`: an account with >limit ops in one ledger loses the remainder. | FIXED-2026-06-15 |
| R-A11-3 | A11 | High | openapi/stellar-index.v1.yaml vs server.go:1032-1033 | `/accounts/{g}/transactions` + `/operations` are shipped but entirely absent from OpenAPI (shapes/codes undocumented). | FIXED-2026-06-15 |
| R-A12-1 | A12 | High | api/v1/auth_sep10.go:124 | SEP-10 token endpoint runs Ed25519 verify + XDR parse on anonymous input with no dedicated cost-aware throttle (expensive-crypto DoS surface). | DOCUMENTED-2026-06-15 |
| R-A12-2 | A12 | High | dashboardauth/handlers.go:169 | Magic-link login sends an email per request with no per-email throttle → victim-inbox email-bombing + sender-reputation burn. | FIXED-2026-06-15 |
| R-A14-1 | A14 | High | pkg/client/types.go:16 | `Envelope.Pagination` is a value type with `omitempty` (no-op on a struct) where the server uses `*Pagination`; SDK re-encode emits `"pagination":{}` → round-trip drift. | FIXED-2026-06-15 |
| R-A15-1 | A15 | High | 0031_remove_trades_retention.down.sql:7 (+0040 down) | The `down` migrations re-add the 90d retention policy on `trades`/`oracle_updates` — one `migrate down` re-arms the ADR-0034-forbidden data-loss worker. | FIXED-2026-06-15 |
| R-A15-2 | A15 | High | 0053–0060 *.up.sql (all 8) | event_index/PK-recovery migrations run multi-statement DDL with no `BEGIN/COMMIT`; a mid-file failure leaves a dirty, partially-applied schema (write-path outage). | DOCUMENTED-2026-06-15 |
| R-A15-3 | A15 | High | 0053–0060 *.up/.down.sql | Bare `DROP CONSTRAINT <t>_pkey` (no `IF EXISTS`) makes the obvious retry-after-partial-failure fail instead of converging. | DOCUMENTED-2026-06-15 |
| R-A15-4 | A15 | High | 0001_create_trades_hypertable.up.sql:64 | `trades` is created at `chunk_time_interval = 1 day` and no migration ever widens it → a fresh region re-accretes the lock-table-sizing crisis. | FIXED-2026-06-15 |
| R-A16-1 | A16 | High (latent) | config/load.go:75-79 ↔ trim_galexie_archive.go:305-319 | S3 access/secret env fields have two contradictory consumers (name vs value); the env override silently breaks explicit S3 creds → silent fallback to the default chain. | FIXED-2026-06-15 |
| R-A17-1 | A17 | High | explorer-shared.tsx:55-59 + tx/TxView.tsx / ledger/LedgerView.tsx | `result_code` typed `string` in the UI but emitted as an XDR `int32`; `/success/i.test(0)` → successful ops render a red "0" badge. | FIXED-2026-06-15 |
| R-A17-2 | A17 | High | tx/TxView.tsx:175-181, ledger/LedgerView.tsx:314-320, SearchModal.tsx:80 | Source-account / account links point at `/issuers/{account}`, which static-export 404s for every non-top-100-issuer account (the common case). | FIXED-2026-06-15 |
| R-A18-1 | A18 | High | /ratesengine-api, /ratesengine-indexer (repo root, tracked) | 136 MB of stale pre-rebrand macOS build binaries are committed and would ship into the public OSS export. | FIXED-2026-06-15 |
| R-A19-1 | A19 | High | docs/reference/api/stellar-index.v1.yaml | Generated API reference is stale (66 vs 73 paths) — missing the entire ADR-0038 explorer section; `make docs-api` was not re-run. | FIXED-2026-06-15 |
| R-A19-2 | A19 | High | docs/reference/api/index.html + README.md | Published rendered reference serves the stale 66-path spec, and the "CI verifies in sync" claim is false in practice (desync exists on main). | FIXED-2026-06-15 |
| R-A19-3 | A19 | High | openapi/stellar-index.v1.yaml vs explorer_accounts.go | `/accounts/{g}/transactions` + `/operations` implemented but undocumented in OpenAPI, contradicting ADR-0038's "all explorer endpoints documented" claim. | FIXED-2026-06-15 |
| R-A20-1 | A20 | High | test/load/scenarios/lib/alertmanager.js:21 | k6 99-spike silence matchers (`APIHighLatencyP95`/`APIHighErrorRate`) match no deployed alert name → silence is a no-op and on-call pages during the planned burst. | FIXED-2026-06-15 |
| R-A21-A22-1 | A21-A22 | High | scripts/ci/lint-imports.sh + internal/xdrjson | Import-boundary lint fails on main (xdrjson imports go-stellar-sdk/xdr without a rule-B allowlist entry) → `make verify` is red on a clean checkout. | FIXED-2026-06-15 |
| R-X3X5-1 | X3-X5 | High | tx/TxView.tsx:264-272 + explorer-shared.tsx:56,76,95 | `result_code` int-vs-string contract break (same as A17-1, traced across lake→reader→handler→OpenAPI→UI): success `0` falsy hides the badge; failures mis-colour. | FIXED-2026-06-15 |
| R-X6-1 | X6 | High | cmd/stellarindex-api/main.go:358-360 vs buildAPIKeyValidator | Under `auth_backend=postgres` the Redis-backed `/v1/account/keys` is disjoint from the Postgres validator → a DELETE there leaves the live Postgres key authenticating (revocation silently no-ops). Latent on r1 (default redis). | FIXED-2026-06-15 |
| R-X9-1 | X9 | High | projector/projector.go:281-293 + 186-189 | The projector per-source goroutine runs decoders on raw lake rows with no recover (the dispatcher has one); a panic on one poison/upgraded-WASM row crashes the live indexer → cursor-stuck crash-loop. | FIXED-2026-06-15 |

### Medium (60)

| # | area | sev | file:line | one-line issue | status |
|---|---|---|---|---|---|
| R-A01-1 | A01 | Medium | dispatcher.go:728-739 (walkEntryChanges) | The entry-change walk never visits `tx.PostTxApplyFeeChanges` (P23/V4), so a balance change landing there is invisible to supply observers. | open |
| R-A01-2 | A01 | Medium | ledgerstream.go:265-292/300-328 | Trailing-edge tolerance only matches the "missing" SDK error, not the sibling "maximum retries exceeded" wrap, so a tip-race walk can still hard-error with the flag set. | open |
| R-A02-1 | A02 | Medium | clickhouse/sink.go:382-394 (flushChanges docstring) | Docstring still claims the entry-change lake is always empty / a no-op — now false since extract.go:106 populates `ext.Changes`. | open |
| R-A02-2 | A02 | Medium | explorer_reader.go:218-259,337-365,266-282 | Non-FINAL bloom reads can return duplicate rows from un-merged ReplacingMergeTree parts with no read-side dedup; list pagination can repeat/skip a boundary row. | open |
| R-A03-1 | A03 | Medium | timescale/assets.go:145-162 (hasAssetByTradesScan) | Soroban/native/fiat existence falls through to an unbounded `base=$1 OR quote=$1` full-hypertable scan (the F-0157 incident shape, fixed only for classic). | open |
| R-A03-2 | A03 | Medium | timescale/issuers.go:119-152 (ListIssuers) | Issuer directory is a full join+aggregate+top-N sort over ~440K `classic_assets` rows with no recency filter or keyset cursor. | open |
| R-A04-2 | A04 | Medium | projector/projector.go:223,285,294-323 | The 60s per-cycle ctx wraps read+decode+sink; a persistently slow sink cancels mid-stream, the cursor never advances, and the source livelocks retrying the same window. | open |
| R-A04-3 | A04 | Medium | projector/projector.go:334,330-340 | `SinkFunc` returns no error, so a soft sink-failure advances the cursor past undurable events — the documented retry-on-sink-failure property does not hold. | open |
| R-A04-4 | A04 | Medium | archivecompleteness/cross_anchor.go:149-167 | `alignLastCheckpoint` has a tautological condition (`rem>=63 || rem<63`) + an unreachable `return 0`; correct today but unauditable and off-by-one-prone. | open |
| R-A05-2 | A05 | Medium | phoenix/consumer.go:170-198 + decode.go:96-110 | Swap `groupKey` is `(ledger,tx,op)` only; two same-action swaps in one op rely on contiguous emit order — interleaving corrupts/merges into one wrong trade. | open |
| R-A05-3 | A05 | Medium | phoenix/dispatcher_adapter.go:55-58 (Matches) | Phoenix gates on topic[0] strings only, no contract-identity childgate (ADR-0035) → foreign `("swap",…)` emitters mis-attributed (known/tracked). | open |
| R-A05-4 | A05 | Medium | aquarius/comet/defindex dispatcher_adapter.go | Same topic-only gating gap as Phoenix for Aquarius/Comet/DeFindex; none consult childgate (Comet is the genuinely-hard no-factory case). | open |
| R-A07-2 | A07 | Medium | claimable_balances + liquidity_pools dispatcher_adapter.go | Both filter the `Removed` variant at Matches and never emit `is_removal=true`, so the sum monotonically over-counts as claimables are claimed / LPs drained. | open |
| R-A07-3 | A07 | Medium | supply/storage_sep41_reader.go:54-59,113-121 | `AdminBalance` hard-coded 0 so circulating==total stamps `Basis=admin_exclusion` even when no admin exclusion was applied (misleading provenance). | open |
| R-A08-1 | A08 | Medium | canonical/asset.go:266-277 (ParseAsset) | `ParseAsset` accepts G/C issuers but never round-trips M/B/L holder strkeys that `AsAddressStrkey` emits — latent round-trip asymmetry (doc-only fix sufficient). | open |
| R-A09-1 | A09 | Medium | aggregate/orchestrator.go:1242 (windowUSDVolume) | Dead-but-tested function whose stale "CALLER CONTRACT" doc invites re-introducing the F-1213 10× USD-volume mis-statement. | open |
| R-A10-1 | A10 | Medium | api/v1/network_stats.go:~67 | `handleNetworkStats` runs a 24h multi-aggregate over `prices_1m` with bare `r.Context()` — no 8s timeout ceiling like every sibling handler. | open |
| R-A10-2 | A10 | Medium | api/v1/price_tip.go:279-302,314 (tipWindowVWAP) | XLM dual-form `/price/tip` returns an unsorted `sources[]` (alias-iteration-order dependent) → not byte-identical across regions (ADR-0015). | open |
| R-A10-3 | A10 | Medium | api/v1/ohlc_series.go:232 | `Triangulated:true` stamped for ANY fiat-quoted series with bars, even when served from direct fiat:USD CEX bars (over-claims triangulation). | open |
| R-A11-4 | A11 | Medium | xdrjson/operation.go:98,106,111,152,153 | Payment/path-payment/account-merge destinations call `MuxedAccount.Address()` which panics on an unknown type; a crafted body 500s the whole response instead of per-op RawXDR degrade. | FIXED-2026-06-15 |
| R-A11-5 | A11 | Medium | xdrjson/operation.go:162,123,130 | `bump_to` (seq `ledgerSeq<<32`) and `offer_id` emitted as raw JSON numbers → IEEE-754 precision loss past 2^53 (ADR-0003 class). | FIXED-2026-06-15 |
| R-A11-6 | A11 | Medium | xdrjson/participants.go:21-44 | `ParticipantAccounts` only collects G-strkeys, silently missing every muxed (M-address) payment destination from the Phase-B participant index. | FIXED-2026-06-15 |
| R-A12-3 | A12 | Medium | dashboardauth/middleware.go:150-171 | `touchTracker.last` is an unbounded session-id→time map, never evicted → slow memory leak accelerable by minting sessions. | open |
| R-A12-4 | A12 | Medium | auth/{list_keys,store_update,store_mark_email_verified}.go | Every Redis-store mutation/list does a full `SCAN apikey:*` (O(N)) — incl. `MarkEmailVerified` on the verify hot path → self-inflicted DoS at scale. | open |
| R-A12-5 | A12 | Medium | auth/list_keys.go:74 (RevokeKeyByID) | Two different revoke stores (Redis hard-DEL vs Postgres soft-delete) can split-brain so a key revoked via one surface keeps authenticating via the other validator. | open |
| R-A12-6 | A12 | Medium | dashboardauth/handlers.go:303-304 | `CF-IPCountry` geo is trusted verbatim with no trusted-proxy gate → forgeable geo audit data when not actually behind Cloudflare. | open |
| R-A12-7 | A12 | Medium | middleware/keypolicy.go:97-116 (checkRefererAllowlist) | Referer-based access gate is inherently weak (client-controlled); should be documented as defence-in-depth only, not a boundary. | open |
| R-A12-8 | A12 | Medium | ratelimit/bucket.go:153-175 + signup_ip_throttle.go:187-210 | The fail-open/closed dwell clock resets on a single success, so a flapping Redis keeps the limiter fail-open indefinitely (J40 vector only closed for a fully-sustained outage). | open |
| R-A14-7 | A14 | Medium | pkg/client/types.go:805,829,757 | `VerifiedCurrencyListItem`/`GlobalAssetView`/`PerNetworkAssetView` types exist with godoc links to methods (`AssetsVerified`/`AssetByNetwork`) that don't exist — un-callable + dangling refs. | open |
| R-A14-8 | A14 | Medium | pkg/client/doc.go:81-82 | The `# Coverage` doc lists removed `Coins`/`Currencies` methods and claims "35 methods" while omitting the 4 explorer surfaces that exist server-side. | open |
| R-A15-5 | A15 | Medium | migrations/README.md:103-127 | "Current migrations" table is wrong for 0016–0028 (lists entirely different tables) and stops at 0045 (0046–0061 undocumented). | open |
| R-A15-6 | A15 | Medium | migrations/README.md:76-78 | Conventions doc cites `-- migrate:no-transaction` (dbmate/goose syntax) which golang-migrate v4 ignores — mis-teaches the next author. | open |
| R-A15-7 | A15 | Medium | 0037_trades_pair_source_ts_index.up.sql:45 | Plain (non-CONCURRENTLY) `CREATE INDEX` on `trades`; running `up` on a populated table takes a full-table ACCESS EXCLUSIVE lock if the hand-build step is skipped. | open |
| R-A15-8 | A15 | Medium | 0053–0060 *.down.sql | Down files narrow the PK back and will FAIL on duplicates post-re-derive (best-effort, not reliably reversible) — caveat only in 0057. | open |
| R-A15-9 | A15 | Medium | 0002_create_price_aggregates.up.sql:69,102 | prices_1m/15m retention was removed by 0031 (now indefinite per ADR-0034); the audit-prompt's "30d" invariant is the stale pre-0031 state. | open |
| R-A15-10 | A15 | Medium | 0030 / 0004 / 0053–0060 | decompress→DDL→recompress is inherently non-atomic; 0004 + 0053–0060 (unlike 0030) don't wrap the pure constraint swap in an inner txn → crash leaves chunks uncompressed. | open |
| R-A15-11 | A15 | Medium | 0048_source_coverage_snapshots.up.sql:3 | Column named `"table"` (reserved word, quoted) forces every reader to quote it forever. | open |
| R-A16-2 | A16 | Medium | config/config.go:436-437 ↔ pipeline/datastore.go:75-86 | `s3_cold_access_key_env`/`s3_cold_secret_key_env` are orphan config fields (never read) though the docs imply they gate private-bucket access. | open |
| R-A17-3 | A17 | Medium | explorer-shared.tsx:143-159 (stroopsToXlm) | `total_coins` (~1e18 stroops) passes through `Number()` before /1e7 → exact remainder lost (ADR-0003 display-side fidelity loss). | open |
| R-A17-4 | A17 | Medium | contract/ContractView.tsx:170-177 | "Load older" can silently skip events within a saturated ledger — UI symptom of the server-side ledger-only cursor (root cause R-A11-1). | open |
| R-A18-11 | A18 | Medium | ansible/roles/patroni templates (etcd.conf.j2 / patroni.yml.j2) | etcd (the Patroni DCS holding the PG superuser password) runs with no TLS/auth; firewall-gated to internal-only, but add TLS+auth before any multi-host Patroni standup. | open |
| R-A19-4 | A19 | Medium | docs/adr/README.md | ADR-0038 (Accepted) is missing from the ADR README index table (README rule #3 requires every ADR be indexed). | open |
| R-A19-5 | A19 | Medium | CLAUDE.md "Things that will surprise you" | The SEP-41 trap cites `MustI128()`, which is a fictional symbol; the real helper is `scval.AsAmountFromI128` (behaviour described is correct). | open |
| R-A19-6 | A19 | Medium | runbooks/*.yml runbook_url fields | 5 self-named runbooks are dead weight — a live alert by that name points its `runbook_url` at a different (shared) runbook; CI doesn't catch this. | open |
| R-A19-7 | A19 | Medium | runbooks (orphan check) | ~12 alert-shaped runbooks (incl. the 6 SLO-burn ones) are referenced by no alert; the reverse contract is real and unchecked. | open |
| R-A19-8 | A19 | Medium | docs/architecture/coverage-matrix.md:32, archival-node-spec.md:38 | 8 residual `rates-engine`/`RatesEngine` references outside provenance areas; two are stale prose with wrong module/binary names. | open |
| R-A20-2 | A20 | Medium | test/integration/ledgerstream_to_storage_test.go:532-536 | A `ProcessLedger` error is downgraded to `t.Logf`+`return nil` so the load-bearing end-to-end ingest test reports a symptom, not the cause, on a real regression. | open |
| R-A20-3 | A20 | Medium | test/integration/external_fleet_test.go:145-165 | CEX/FX fleet test logs insert failures + asserts lower-bounds only + a 2s sleep; a silent insert regression or a dropped venue still passes. | open |
| R-A20-4 | A20 | Medium | test/integration/api_test.go:76-84,712-729 | Dead scaffolding for an "AllPools" sub-test that no longer exists → reader believes `/v1/pools`/`AllPools` is covered when it isn't. | open |
| R-A20-5 | A20 | Medium | test/integration/doc.go:2 | Suite package doc lists `stellar-rpc` as a required external dependency, contradicting ADR invariant 6 (removed from ingest 2026-04-23). | open |
| R-A20-6 | A20 | Medium | test/load/scenarios/lib/env.js:18-22 | `PROD_HOSTS` lists `api.stellarindex.io` twice and a stale `rates.stellar.org` (pre-rebrand) — guard list never reviewed post-rebrand. | open |
| R-A20-7 | A20 | Medium | test/integration/platform_postgres_stores_test.go:364 + ~10 sites | `_ = CreateInvite(...)` discards the error so a downstream `ErrNotFound` negative assertion passes for the wrong reason; pervasive `got, _ :=` error-discard. | open |
| R-X1-1 | X1 | Medium | extract_entry_changes.go:37-52 + dispatcher.go:728-739 | Two independent hand-walks of the same tx-meta entry-change stream must stay byte-identical but are factored only by comment; drift would silently diverge lake-re-derived supply from live. | open |
| R-X1-2 | X1 | Medium | config/config.go:508-509,1019-1022 + load.go:34-35 | The F-1316 SEP-41 silent-total-loss path is closed by default but re-armable by one `persist_per_source=false` line while a projected source lacks its config (silent domain drop). | open |
| R-X1-3 | X1 | Medium | clickhouse/live_sink.go:127-133 + indexer main.go:536-568 | The ledgerstream cursor advances per-ledger regardless of CH dual-sink acceptance; a dead catch-up timer would let the lake accrue permanent holes the cursor can't re-trigger. | open |
| R-X3X5-2 | X3-X5 | Medium | explorer_reader.go:266-282 (TransactionByHash) | Tie-break on `ingested_at` (1s `DateTime` resolution) can pick an arbitrary row when the same tx is re-ingested within one second. | open |
| R-X3X5-3 | X3-X5 | Medium | openapi ContractEvent vs TxEventView vs ContractEventView | One OpenAPI `ContractEvent` schema documents two structurally different wire shapes and omits `contract_id` (present on the tx-detail wire). | open |
| R-X4-1 | X4 | Medium | config/load.go:75-79 + trim_galexie_archive.go:305-318 | S3 cred name-vs-value confusion (A16 class); masked on Ansible hosts by parallel `AWS_*` vars, High on its own but filed Medium. | open |
| R-X4-2 | X4 | Medium | cmd/stellarindex-api/main.go:131 (ExternalBaseURL) | `api.external_base_url` is effectively dead (log-only) though its doc implies it builds public-facing links. | open |
| R-X4-3 | X4 | Medium | configs/example.toml (whole file) | example.toml drifted from the schema: 15 schema fields undocumented + stale `auth_mode` comment omits `apikey_optional` (the value r1 runs). | open |

### Low (117)

| # | area | sev | file:line | one-line issue | status |
|---|---|---|---|---|---|
| R-A01-3 | A01 | Low | dispatcher.go:559,623,671 | `MuxedAccount.ToAccountId()` panics on an unknown muxed type and aborts the WHOLE ledger (recovered), inconsistent with the per-tx error tolerance everywhere else. | open |
| R-A01-4 | A01 | Low | dispatcher.go:524,690 (ProcessLedger) | A whole ledger's events accumulate into one slice before any is drained — per-ledger-bounded memory, forfeits streaming back-pressure. | open |
| R-A01-5 | A01 | Low | pipeline/sink.go:180-211 (persistWorker shutdown) | On ctx.Done a non-zero worker can still consume from `in` concurrently with worker-0's drain; safe but the "only worker 0 drains" comment overstates the guarantee. | open |
| R-A01-6 | A01 | Low | pipeline/sink.go:269-290 (IsProjectedEvent) | `soroswap_router.Event` is the one non-projected type not pinned in `projected_test.go`, so the projected/non-projected split could drift untested. | open |
| R-A02-3 | A02 | Low | explorer_reader.go:266-268 + tier1_schema.sql:50 | `TransactionByHash` ties on `ingested_at` `DateTime` (1s) so the "latest" of two same-second re-ingests is arbitrary. | open |
| R-A02-4 | A02 | Low | event_reader.go / sdex_op_reader.go / recognition.go (sqlQuoteList) | IN-lists built by string concat with no escaping; safe today (compile-time constants only) but a latent injection sink if a request value is ever threaded in. | open |
| R-A02-5 | A02 | Low | clickhouse/gate.go:60-61 (openRead) | `openRead` sets `max_execution_time=0` for streaming reads but is also used by light point-ops (Ensure*/Write*/MaxLedger) which inherit the unlimited wall-clock + 1h read timeout. | open |
| R-A03-3 | A03 | Low | classic_supply_observations.go (Sum*AtOrBefore) | DISTINCT-ON fan-out is one row per holder with no cap; latent slow-query for the most-held assets (refresh-worker path, not API hot path). | open |
| R-A03-4 | A03 | Low | timescale/markets.go:738-769 (PairMarket) | The one markets surface still touching raw `trades` (14d-bounded); a heavily-traded pair scans a non-trivial recent-chunk slice per `/v1/pairs` hit. | open |
| R-A03-5 | A03 | Low | timescale/coins.go:991-1003 (GetCoinTradeCount24h) | 24h `COUNT(*)` over `trades` with `base=$1 OR quote=$1` can't use one index cleanly; the one asset-detail call still reading raw `trades`. | open |
| R-A03-6 | A03 | Low | timescale/blend_auctions.go:319-330 (ListBlendPools) | `GROUP BY pool` over the whole table with no recency bound/LIMIT — cheap today (sparse table) but unbounded by construction. | open |
| R-A03-7 | A03 | Low | timescale/diagnostics.go:196 (RefreshContinuousAggregate) | View name `fmt.Sprintf`'d into the CALL; guarded by an allow-list check immediately above, but the residual string-built-SQL surface must stay gated. | open |
| R-A04-5 | A04 | Low | projector/projector.go:340 vs :256 | `ProjectorLagLedgers` is set to the post-batch residual and (with R-A04-1) reads 0 while the source is permanently one ledger behind tip. | open |
| R-A04-6 | A04 | Low | completeness/watermark.go:32-41 | Degenerate `tip < genesis` reports `complete=false coverage=0`; a brand-new protocol with no ledgers yet shows a false "not covered" signal. | open |
| R-A04-7 | A04 | Low | completeness/reconcile.go:67,105-150 | The reconcile passes `nil` excludeTopic0Syms (relies on `Matches`) while the projector excludes at SQL — deliberate but undocumented asymmetry. | open |
| R-A04-8 | A04 | Low | archivecompleteness/cross_anchor_fill.go:223 | Each Fill worker seeds `math/rand` from `UnixNano()+workerID`; adjacent IDs in the same nanosecond can produce correlated shuffles (cosmetic load-spread). | open |
| R-A04-9 | A04 | Low | archivecompleteness/metrics.go:35-38,232-244 | `RepairFailures` documented per-source but all failures recorded under one synthetic `multi-source-exhausted` label (no per-source diagnostic). | open |
| R-A05-5 | A05 | Low | soroswap/dispatcher_adapter.go:253-256 (Decode) | A mid-loop `decodeSwap` error discards already-appended TradeEvents for other completed pairs; harmless while one-pair-per-absorb holds. | open |
| R-A05-6 | A05 | Low | soroswap_router/decode.go:120-139 (deadline) | Router decoder parses `deadline:u64` with no upper sanity clamp (unlike band/reflector/redstone); the actual fix lives at the sink, not the decoder. | open |
| R-A05-7 | A05 | Low | cctp/decode.go + dispatcher_adapter.go:71 | CCTP/Rozo compute `observedAt` but the inner decode structs also set `ClosedAt` directly — two timestamp paths that could diverge on a parse edge. | open |
| R-A05-8 | A05 | Low | phoenix/events.go:88 | Phoenix admin/initialize literals are XYK-pool-specific spaced strings; stableswap admin/init spellings may differ (classification-only, no data loss). | open |
| R-A05-9 | A05 | Low | blend/events.go:96-108 (Backstop contracts) | Blend Backstop event surface is deliberately not decoded / kept out of the pool gate — known-uncaptured before claiming 100% Blend coverage. | open |
| R-A06-1 | A06 | Low | binance/streamer.go:258-268 | Dust/unknown-symbol drops bump `SourceDecodeErrorsTotal{binance}`, conflating benign drops with schema drift (the lone inconsistent CEX). | open |
| R-A06-2 | A06 | Low | external/runner.go:123-127,202 | Poller goroutines run under the parent ctx while streamers run under a derived ctx; a `cancelStreamers()`-only teardown wouldn't stop pollers (benign today). | open |
| R-A06-3 | A06 | Low | polygonforex/poller.go:247-267 (midPriceString) | A double 6dp round-trip (scale→format→re-parse) before the final inversion — needless precision erosion vs computing the mid in scaled-int space. | open |
| R-A06-4 | A06 | Low | exchangeratesapi/polygonforex/ecb poller (inversion) | FX inversion `(10^12)/rate` truncates (floor) rather than rounds — a consistent <1e-6 downward bias on every inverted FX quote (immaterial to VWAP). | open |
| R-A07-4 | A07 | Low | supply/refresher.go:279-335 (applyStaleComponentGate) | Per-asset `lastComponentLedger` map read/written with no mutex; safe today (single goroutine) but the "future shared-Refresher" comment invites a race. | open |
| R-A07-5 | A07 | Low | supply/textfile.go:113-119 | `last_success_timestamp` stamps `time.Now()` not `snap.ObservedAt`, so a backfill/replay re-run stamps "now" for a historical snapshot (monitoring-only). | open |
| R-A07-6 | A07 | Low | supply/crosscheck_refresher.go:181 | The cross-check gauge discards the `big.Float` exactness flag; harmless (exact value preserved in the WARN log + result struct). | open |
| R-A07-7 | A07 | Low | supply/storage_classic_reader.go:126-131 | Min-ledger query error swallowed to `_ = err` with a "Log + carry on" comment but no actual log line (freshness gate silently goes permissive). | open |
| R-A08-2 | A08 | Low | canonical/discovery/sink.go:95-99 (AsyncSink.Start) | Docstring claims `Start` is idempotent but the body is a bare `go s.run()` with no `sync.Once`; a second call double-drains. | open |
| R-A08-3 | A08 | Low | canonical/discovery/sink.go:36-49,110-133 (seen) | The in-process dedup set grows once per unique `(contractID,eventType)` and is never pruned (bounded in practice, unbounded in principle). | open |
| R-A08-4 | A08 | Low | scval/scval.go:435-445 (MapField) | `MapField` only matches Symbol-keyed entries; a contract emitting a `Map<String,…>` body silently misses every field as "missing" rather than a distinct error. | open |
| R-A08-5 | A08 | Low | scval/scval.go:128-152 / 96-111 | `EncodeArgsAsScVec` ↔ `DecodeScVecToArgs` (the op_args round-trip for projector-replay) have no unit test — pure coverage gap. | open |
| R-A08-6 | A08 | Low | canonical/oracle.go:46-100 + doc.go:1-2 | `doc.go` names a `canonical.Price` type that doesn't exist; the price-bearing type is `OracleUpdate` (stale package-overview reference). | open |
| R-A09-2 | A09 | Low | aggregate/orchestrator.go:1297-1322 (formatRatFixed) | 12-dp truncate-toward-zero (intentional, spec-mandated); a sub-1e-12 value renders `0.000…` — unreachable for any real price. | open |
| R-A09-3 | A09 | Low | aggregate/global.go:362 (averageAggregatorPrices) | Aggregator-tier mean uses integer division (truncate) at 14dp before render; sub-1e-14 rounding on a tier-2 fallback price only. | open |
| R-A09-4 | A09 | Low | divergence/chainlink.go + coingecko.go + marketcap/refresher.go | Divergence/marketcap outbound URLs have no SSRF dialer guard (unlike metadata); operator-config only, so low real risk (asymmetry note). | open |
| R-A09-5 | A09 | Low | divergence/worker.go:339 + flushObservations | Durable `divergence_observations.firing` (per-reference) can disagree with the cached `WarningFired` (vs median) for the same tick — different questions, undocumented. | open |
| R-A09-6 | A09 | Low | divergence/worker.go:395-460 (LookupCached) | The by-asset divergence lookup is an N+1 Redis round-trip (SMembers + per-quote Get) on the `/v1/price` hot path; bounded by the small operator pair set. | open |
| R-A10-4 | A10 | Low | api/v1/coverage_cache.go:62-66 (Snapshot) | `Snapshot()` returns the internal slice header under RLock; safe today (Refresh swaps the whole slice) but a future in-place sort/append would corrupt it. | open |
| R-A10-5 | A10 | Low | api/v1/ohlc.go:180 / vwap.go:146 / twap.go:101 (Truncated) | `Truncated` is `pre == maxTrades` so the exact-boundary case is ambiguous (was there one more trade or exactly the cap?). | open |
| R-A10-6 | A10 | Low | api/v1/price.go:764-765 (tryFiatCrossRate) | The fiat-vs-fiat cross-rate computes in float64 and renders via FormatFloat — the one float-derived price on the canonical price surface (last-resort fallback). | open |
| R-A10-7 | A10 | Low | api/v1/changes.go:36 (CurrentValue + value fields) | For `entity_type=coin` these `float64` JSON fields carry VWAP prices — the one place a price-derived value ships as a JSON number (ADR-0003 drift, by-design display widget). | open |
| R-A10-8 | A10 | Low | api/v1/markets.go:660 (fanOutAssetMarkets) | `firstErr` only returned when `len(merged)==0`; a partial slug-expansion failure silently under-reports markets with no flag. | open |
| R-A10-9 | A10 | Low | api/v1/diagnostics_ingestion.go:~1132 (parseInt64) | Hand-rolled `n*10+digit` with no overflow check; a >19-digit cursor wraps silently rather than failing (unreachable for ledger numbers). | open |
| R-A11-7 | A11 | Low | xdrjson/participants.go:33 + helpers.go:75-94 | Generic G-strkey extraction is shape-based, correct only because no decoded non-account field currently emits a valid G-strkey — fragile as the decoder grows. | open |
| R-A11-8 | A11 | Low | explorer_ledgers.go:118-120 + contracts/accounts | `next_before` is emitted on the final short page too, forcing one wasted empty-list request to learn pagination is done. | open |
| R-A11-9 | A11 | Low | explorer_ledgers.go:187-201 / explorer_operations.go:107 | Per-ledger/per-tx listings have a LIMIT but no pagination cursor — a ledger with more txs/ops than the cap silently truncates. | open |
| R-A11-10 | A11 | Low | explorer_reader.go:266-282,337 | Non-FINAL re-ingest reads can surface a superseded duplicate row / dup contract events (ReplacingMergeTree pre-merge edge). | open |
| R-A12-9 | A12 | Low | middleware/keypolicy.go:138-149 (permissionMatches) | `EndpointPrefix` uses raw `HasPrefix` with no path-boundary check, so a deny/allow prefix can over/under-match adjacent routes. | open |
| R-A12-10 | A12 | Low | middleware/auth.go:240-260 (bearerOnly) | `Bearer` scheme match is case-sensitive; a spec-compliant `bearer <key>` is treated as no-credential → false-deny (fails closed). | open |
| R-A12-11 | A12 | Low | auth/sep10/jwt.go:71-112 (parseJWT) | `nbf` is written on issue but never enforced on parse (`aud` also unchecked); harmless while `nbf==iat`. | open |
| R-A12-12 | A12 | Low | dashboardauth/auth.go:86-92 (numericFromBase32) | The 6-digit paste code is biased `s[i]%10` over base32 in a ~10^6 space; safe only because consumption requires the full token. | open |
| R-A12-13 | A12 | Low | api/v1/signup.go:291-302 (buildSignupVerifyURL) | The verify URL is built from client-controlled `r.Host`/`X-Forwarded-Proto`; a forged Host could leak the single-use token to an attacker domain (Caddy masks in prod). | open |
| R-A12-14 | A12 | Low | dashboardwebhooks/handlers.go:564-575 + worker | Webhook SSRF defence is solid but `isReservedTLD` lets `.localhost`/`.test` pass registration (delivery-time dial guard is the real boundary). | open |
| R-A12-15 | A12 | Low | usage/counter.go:85-98 (Increment) | `Increment` re-issues EXPIRE on every call (redundant after the first); negligible, INCR is atomic. | open |
| R-A13-1 | A13 | Low | ops/backfill_router.go:232 (signalContext) | Shared `signalContext()` prints a literal "backfill-router: …" message on SIGINT for all 8 caller subcommands (cosmetic mislabel). | open |
| R-A13-2 | A13 | Low | ops/backfill_router.go:232-242 | `signalContext()` leaks its `signal.Notify` goroutine + never calls `signal.Stop`; benign for one-shot CLI but a second SIGINT can't be observed. | open |
| R-A13-3 | A13 | Low | ops/{verify_recognition,verify_reconciliation,compute_completeness}.go | Long-running verify subcommands use `context.WithTimeout(Background())` with no SIGINT handling — can only be OS-killed (read-only, no partial-write risk). | open |
| R-A14-2 | A14 | Low | pkg/client/types.go:411-416 (Account.CreatedAt) | No `omitempty` where the server has it; re-encode of an Account without created_at emits the zero time. | open |
| R-A14-3 | A14 | Low | pkg/client/example_test.go:219,904 | Two examples put `identifier` in the `Account` JSON, but the SDK/server `Account` has no such field (silently dropped) — misleads SDK readers. | open |
| R-A14-4 | A14 | Low | pkg/client/client.go:65-88 (New) | `New()` never validates `BaseURL`; a schemeless/relative URL is accepted and requests silently go to the wrong place. | open |
| R-A14-5 | A14 | Low | pkg/client/client.go:28 (userAgent) | `userAgent = "stellarindex-go-sdk/0.1.0"` is hand-pinned and stale vs the repo tag; server telemetry mis-attributes SDK version. | open |
| R-A14-6 | A14 | Low | pkg/client/errors.go:110-115 | Non-JSON error bodies only land in `Detail` when ≤256 bytes; a 257-byte proxy error is silently discarded (truncate, not truncate-to-256). | open |
| R-A15-12 | A15 | Low | 0031.up.sql / 0040.up.sql | (Positive counterpart to R-A15-1) `up` removes retention with `if_exists => true` — correctly idempotent; only `down` reintroduces the policy. | open |
| R-A15-13 | A15 | Low | cmd/stellarindex-migrate/main.go:37 | `up` applies ALL pending migrations; no `--to <version>`/`goto` to stop before a known-heavy migration. | open |
| R-A15-14 | A15 | Low | cmd/stellarindex-migrate/main.go:88-101 | `force <V>` is DANGEROUS-documented but has no confirmation prompt / env-gate — easy to mask a half-applied schema under pressure. | open |
| R-A15-15 | A15 | Low | 0009:116 / 0003:64 / 0027:242 (+0042-0045) | Several hypertables use a 1-day chunk interval (blend_auctions, oracle_updates, api_usage_events, per-event Soroban tables) — same growth dynamic as trades, lower volume. | open |
| R-A15-16 | A15 | Low | migrations/README.md:38-40 | README rule 4 ("every CAGG also adds retention") is contradicted by the correct files (1h+ are indefinite by design). | open |
| R-A15-17 | A15 | Low | 0046_cursors_add_first_ledger.up.sql | (Checked-and-OK) the `split_part(...)::integer` UPDATE is regex-guarded so the cast can't fail on matched rows. | open |
| R-A16-3 | A16 | Low | config/config.go:636 | Stale prose in `SignupRequireEmailVerification` doc ("Default false…") contradicts the actual default `true` (runtime correct, prose stale). | open |
| R-A16-4 | A16 | Low | obs/metrics.go:701 | `SourceInsertErrorsTotal` Help lists only trade/oracle/panic but the emitted `kind` set also includes unhandled/blend_*/comet_liquidity (Help feeds docs). | open |
| R-A16-5 | A16 | Low | obs/metrics.go:1016 | `CustomerWebhookDeliveryAttemptsTotal` Help is "labelled by outcome" only; the 10-value enum lives only in the Go doc-comment, not the generated docs. | open |
| R-A16-6 | A16 | Low | config/validate.go:341-371 | `interval_seconds`/`divergence_min_interval_seconds`/`max_trades_per_window` have no range validation; a negative value passes Validate (consumer only guards zero). | open |
| R-A17-5 | A17 | Low | explorer-shared.tsx:178-188 (relativeAge) | `relativeAge` uses `Date.now()` in render with no re-render trigger; "Xs ago" freezes between React Query refetches. | open |
| R-A17-6 | A17 | Low | LedgersTable.tsx:171 / contract/ContractView.tsx:175-177 | `next_before` on the final page keeps "Load older" enabled one click past the real end (a dead-end click that blanks the view). | open |
| R-A17-7 | A17 | Low | app/layout.tsx:99,113-118 + next.config.mjs:32 | Residual `re.` (Rates-Engine) localStorage theme key + `re-build-*` meta tags post-rebrand (cosmetic). | open |
| R-A17-8 | A17 | Low | research/architecture/[slug]/page.tsx:16 + lib/architecture.ts:30 | Two stale "Rates-Engine" mentions in code comments. | open |
| R-A18-2 | A18 | Low | scripts/dev/cut-release.sh:21 | Stale comment claims release.yml builds container images (GHCR job dropped 2026-05-11); script behaves correctly. | open |
| R-A18-3 | A18 | Low | ansible/roles/archival-node/tasks/01-preflight.yml | Unlike redis/patroni, archival-node preflight doesn't assert `postgres_pass_*`/`minio_root_password`; a typo'd vault key creates roles with empty passwords. | open |
| R-A18-4 | A18 | Low | ansible/roles/archival-node/defaults/main.yml:475-476 | `allowed_ssh_cidrs: ["0.0.0.0/0"]` — SSH open to the internet (tempered by key-only root + fail2ban + rate-limit; TODO'd to tighten). | open |
| R-A18-5 | A18 | Low | ansible/roles/loki tasks/config | TLS verification skipped for the internal MinIO probe + Loki S3 backend (both internal-only http:// today). | open |
| R-A19-9 | A19 | Low | CLAUDE.md repo map | `internal/xdrjson/` (ADR-0038 XDR→JSON decode) is present but undocumented in the CLAUDE.md repo map. | open |
| R-A19-10 | A19 | Low | CLAUDE.md flag form | `projector-replay -source`/`-from` (single-dash) vs the help text's `--source`/`--from`; functionally harmless (Go flag accepts both). | open |
| R-A19-11 | A19 | Low | CLAUDE.md locality | `completeness_snapshots` table write lives in storage/timescale + ops, not `internal/completeness` as the prose implies (architectural claim holds). | open |
| R-A19-12 | A19 | Low | docs/reference/api/README.md | `last_verified: 2026-05-06` is >30d old (under the 90d warn line) and predates the explorer regen that never happened. | open |
| R-A19-13 | A19 | Low | ADR-0038:25-32 | The ADR embeds a dated row-count snapshot that will silently age (acceptable — ADRs are immutable, so never refreshed). | open |
| R-A19-14 | A19 | Low | CLAUDE.md cmd note | "six in total" binaries is correct today but a hard-coded count that drifts the moment a 7th lands. | open |
| R-A20-8 | A20 | Low | test/load/scenarios/05-streaming.js:30,45-71 | Comment/executor drift + claims `/v1/price/stream` coverage but only hits `/observations/stream`; SSE "first event" is TTFB, not a real `data:` line. | open |
| R-A20-9 | A20 | Low | test/chaos/scenarios/03-redis-network-partition.sh:30 + common.sh:198 | pumba pause hardcoded 60s while `PARTITION_DURATION_SEC` defaults 30; raising the env var past 60 makes the loop measure a healed Redis as "during partition". | open |
| R-A20-10 | A20 | Low | test/load/scenarios/99-spike.js + README | The stated "recovery to baseline p95 within 2 min" pass-criterion is never machine-checked (thresholds assert only `http_req_failed`). | open |
| R-A20-11 | A20 | Low | test/integration/soroban_events_storage_test.go:40-99,127-128 | Count-only test: every row has a unique ledger so columns are never read back and the ON CONFLICT path is never exercised; dead `hex.EncodeToString` keep. | open |
| R-A20-12 | A20 | Low | test/integration/storage_test.go:326-333,397 | Cursor-migration tests re-run an inline COPY of 0046's SQL / hand-run DROP COLUMN instead of driving the checked-in up/down scripts. | open |
| R-A20-13 | A20 | Low | test/integration/issuers_coins_storage_test.go:187 | Asserts `err != sql.ErrNoRows` (direct `!=`) where the contract is `errors.Is`; breaks the moment the error is wrapped. | open |
| R-A20-14 | A20 | Low | test/integration/fx_quote_at_or_before_test.go:147 | `want` pins `FXSources()` to exactly two; CLAUDE.md lists `ecb` too, so a third FX source breaks the golden. | open |
| R-A20-15 | A20 | Low | test/integration/classic_supply_storage_test.go (~15 sites) | ~15 `got, _ =` discard the error from `Sum*AtOrBefore`; a later SQL error slips through silently. | open |
| R-A20-16 | A20 | Low | test/integration/api_registry_cursors_test.go:154-159 | The `lag_seconds` loop asserts only `>= 0` (a near-tautology); no upper/finite bound. | open |
| R-A20-17 | A20 | Low | test/integration/assets_test.go:206 | `mustSorobanTest` is defined but called nowhere — dead helper implying a Soroban-asset case that doesn't exist. | open |
| R-A20-18 | A20 | Low | test/integration/decoders_to_storage_test.go:257-273 | One unreachable nil-check (t.Fatalf already fired) + the `Observer` column is never asserted in any oracle subtest. | open |
| R-A20-19 | A20 | Low | test/integration/migrations_test.go:430-433 | `assertInsertRejected` accepts any CHECK-ish error substring, so a NOT-NULL/type rejection still "passes" the CHECK-constraint guard. | open |
| R-A20-20 | A20 | Low | test/chaos/scenarios/02-timescale-down.sh:79 | The empty-payload guard greps `"data":[]` but the cache-hit branch was never exercised on a real run (only the 5xx branch observed). | open |
| R-A21-A22-2 | A21-A22 | Low | VERSIONS.md | On-chain contract/discovery SHAs "Captured 2026-04-22" with placeholder WASM-hash ellipses; pre-dates ADR-0035/0038 (tool+SDK pins are current). | open |
| R-A21-A22-3 | A21-A22 | Low | scripts/ci/lint-imports.sh | Allowlist predicates are bare substring matches (`/decode.go`, `_test.go`) — over-broad; a future `…decode.go` would be silently exempted from rule A. | open |
| R-X1-4 | X1 | Low | sorobanevents/reconstruct.go:69-84 vs clickhouse/event_reader.go:159-194 | Topic-count divergence: Postgres caps at 4 topics, CH passes all N; the two projector feed sources aren't byte-identical for a hypothetical >4-topic event. | open |
| R-X1-5 | X1 | Low | pipeline/sink.go:269-290 + projected_test.go | `soroswap_router.Event` is the one non-projected type not pinned in the lockstep table (same as R-A01-6, restated at the seam level). | open |
| R-X1-6 | X1 | Low | dispatcher.go:559,623,671 | `MuxedAccount.ToAccountId()` panic aborts the whole ledger (same as R-A01-3, restated as the one spot in the dispatch hop that breaks the per-tx tolerance contract). | open |
| R-X1-7 | X1 | Low | clickhouse/sink.go:382-394 (flushChanges docstring) | Stale doc claims the entry-change lake is always empty (same as R-A02-1, load-bearing for the X1 entry-change read path). | open |
| R-X3X5-4 | X3-X5 | Low | explorer_search.go:51-54 vs ContractView.tsx:43 | Search classifier routes a contract to `/v1/contracts/{q}/transfers` while the UI fetches `/v1/contracts/{id}` (event activity) — same entity, different surface. | open |
| R-X3X5-5 | X3-X5 | Low | explorer_reader.go:218-236,241-259 | Ledger-granular keyset cursor drops the tail of a hot ledger (>limit txs/ops for one account in one ledger) — same class as R-A11-2 at the reader level. | open |
| R-X3X5-6 | X3-X5 | Low | explorer_reader.go:300-318 + extract.go:184-196 | `operation_results.result_xdr` is captured in the lake but never read/surfaced (inner op-result detail) — known coverage stub, not a bug. | open |
| R-X4-4 | X4 | Low | config: Ingestion.BackfillBatchSize | Orphan field — validate-only (rejects 0); no runtime fetch/backfill loop consumes it (only the dead consumer.Orchestrator ever did). | open |
| R-X4-5 | X4 | Low | config: Ingestion.CursorStoreScheme | Orphan field — validate-only enum check; no runtime branch selects a cursor store from it (cursors are postgres-only). | open |
| R-X4-6 | X4 | Low | config: Aggregate.VWAPWindowSeconds + TWAPWindowSeconds | Orphan fields — validate-only; the real windows come from `Aggregate.Windows`, so tuning these two scalars does nothing (yet documented in example.toml). | open |
| R-X4-7 | X4 | Low | config: External.ECB/CoinGecko PollInterval | ECB `PollInterval` never set from config (inert despite a settable poller field); CoinGecko wired in the indexer but dropped in the ops verify path. | open |
| R-X4-8 | X4 | Low | env: COINGECKO_API_KEY / MASSIVE_API_KEY / STELLARINDEX_PROBE_API_KEY | Three secrets read via raw `os.Getenv` with no `env:` struct tag → invisible to `make docs-config` (all reach r1 via Ansible). | open |
| R-X4-9 | X4 | Low | config: S3Cold*KeyEnv / Stellar / Region decorative orphans | Several validate-only/log-only/by-design orphans (cold S3 creds, Region.Name/HomeDomain, Stellar.CoreHTTPEndpoint/HistoryArchiveURL, trace fields). | open |

### Info (57)

| # | area | sev | file:line | one-line issue | status |
|---|---|---|---|---|---|
| R-A02-6 | A02 | Info | extract_entry_changes.go:24-31 + tier1_schema.sql:144 | `change_index` determinism across re-ingest is load-bearing for ReplacingMergeTree dedup (verified CORRECT; documented as a subtle invariant). | open |
| R-A03-8 | A03 | Info | per_source_gaps.go:331 / source_coverage.go:88 / row_counts.go:32 | Gap-detector `statement_timeout` values differ (780000 vs 300000) across one cycle's queries — not a bug but easy to mis-copy; suggest one named const. | open |
| R-A04-10 | A04 | Info | hashdb/hashdb.go (whole pkg) | Confirmed zero production callers (dead-but-safe seam); internally sound; recommend an explicit UNWIRED banner if it survives to v1. | open |
| R-A04-11 | A04 | Info | completeness/reconcile.go:105-150 (ReDerive…FromEvents) | The CH reconcile is O(distinct-ledgers × kinds) not O(events) — the windowing-by-aggregation defense holds against the prior 12 GiB OOM (positive verification). | open |
| R-A06-5 | A06 | Info | external (all FX/aggregator/CEX) | Non-uniform amount scaling VERIFIED correct: CEX/aggregator/chainlink stamp 10^8, FX pollers stamp 10^6, read per-source by the aggregator (no hardcoded 10^8). | open |
| R-A06-6 | A06 | Info | external (chainlink/exchangeratesapi/coingecko/cmc/cryptocompare/polygon) | Vendor-key handling VERIFIED: URL-path/query keys redacted before logging; header-based keys for the rest; all HTTPS/WSS. | open |
| R-A06-7 | A06 | Info | external/registry.go:31-112 | Source-class VWAP gating VERIFIED: only ClassExchange carries IncludeInVWAP:true; `Lookup` fail-closes unknown sources. | open |
| R-A06-8 | A06 | Info | chainlink poller/decode/events | Poller-wedge bug class VERIFIED fixed: phase-rollover (full uint80 *big.Int dedup), all-feeds-failed liveness, CoinGecko 429 self-wedge. | open |
| R-A06-9 | A06 | Info | binance/kraken/bitstamp/coinbase streamer.go | WS lifecycle VERIFIED: bounded backoff + jitter, healthy-connection reset, keepalive dialer, leak-safe teardown on late Start error. | open |
| R-A09-7 | A09 | Info | aggregate vwap/twap/ohlc/triangulate (whole) | All value-serving aggregation math VERIFIED exact (big.Int/big.Rat); no int64(parts.Lo) truncation; refuse to sort internally. | open |
| R-A09-8 | A09 | Info | aggregate/stablecoin.go + canonical/asset_crypto.go | Stablecoin→fiat proxy VERIFIED aggregator-layer quote-side rewrite; all 9 codes present in knownCryptoCodes; depeg signal preserved. | open |
| R-A09-9 | A09 | Info | aggregate/orchestrator.go:1204-1262 (MinUSDVolume gate) | The min-USD gate is correctly post-class/post-outlier (survivor sum) and USD-quote-only; F-1213/1242/1260 chain internally consistent. | open |
| R-A09-10 | A09 | Info | confidence/* + baseline/baseline.go | Confidence factors clamp [0,1], NaN/Inf-safe, log-space weighted geometric mean; baseline z-score MAD==0 split correct. | open |
| R-A09-11 | A09 | Info | anomaly/threshold.go (Evaluate) | Phase-1 anomaly decision table VERIFIED correct (fail-safe on nil prev/curr; big.Rat deviation). | open |
| R-A09-12 | A09 | Info | orchestrator/phase2_freeze.go | The 3-signal AND freeze (confidence<X && z>Y && sources<=Z) VERIFIED correct per ADR-0019. | open |
| R-A09-13 | A09 | Info | metadata/sep1.go (SSRF guard) | Best-in-class SSRF defence on the issuer-controlled stellar.toml fetch (Proxy:nil, post-DNS per-IP block, redirect/host gate, body cap) — exemplary. | open |
| R-A09-14 | A09 | Info | currency/verified.go + data/seed.yaml | Verified-currency catalogue is the hand-vetted trust surface as designed (//go:embed, no auto-populate from CG/CMC). | open |
| R-A09-15 | A09 | Info | marketcap/refresher.go + chainlink/coingecko key handling | CoinGecko key via request header (not URL query), Retry-After backoff, body LimitReader — no secrets in code. | open |
| R-A10-10 | A10 | Info | api/v1/price.go:531-544 (assetAliases) | XLM dual-form alias loop VERIFIED present on every asset-keyed read path on this surface. | open |
| R-A10-11 | A10 | Info | api/v1 (whole area) USDT fabrication | No handler hardcodes a Tether classic USDT-G… into a USD-quote allowlist; the USD-peg set is entirely operator-config. | open |
| R-A10-12 | A10 | Info | api/v1 (whole area) SQL injection | No SQL is built from query params; enum params switch-validated to constants; the one CH supply query uses a `?` bind. | open |
| R-A10-13 | A10 | Info | api/v1/history.go:79-104 (LatestTradePerSource) | The no-time-bound DISTINCT-ON scan is known/tracked (#29), SWR-cache-mitigated; the durable composite-index fix is disk-deferred. | open |
| R-A10-14 | A10 | Info | api/v1/oracle.go:101 (Confidence float64) | Oracle confidence scores ship as float64 — in [0,1], not token amounts, so ADR-0003 doesn't apply. | open |
| R-A10-15 | A10 | Info | api/v1/asset_supply.go:21,71 (NativeTotalCoins int64) | XLM total carried as int64 (ledger-header native int64, ~5e17 « int64 max) and rendered as a string — not a truncation bug. | open |
| R-A10-16 | A10 | Info | api/v1/ohlc_fiat_combine.go (whole) | The fiat-USD combine math VERIFIED correct (big.Rat, dedup across XLM aliases + peg expansion, skip-unparseable). | open |
| R-A10-17 | A10 | Info | api/v1/assets.go:1187-1346 (dual-shape dispatch) | `/v1/assets/{slug}` dual-shape (GlobalAssetView vs AssetDetail) dispatch VERIFIED correct (slug/ticker before canonical-id; route precedence). | open |
| R-A11-11 | A11 | Info | explorer_search.go:51-54 vs 62-65 | A C-strkey classifies as `kind=contract` before the asset branch, so a SAC contract id never resolves to its asset view via search (deliberate precedence). | open |
| R-A11-12 | A11 | Info | explorer_search.go:56-60 | Account search href points at `/v1/issuers/{g}` not the new accounts activity endpoint (UX choice; href is real). | open |
| R-A11-13 | A11 | Info | explorer_operations.go:61-63 (normalizeLakeOpType) | Decode-error fallback lowercases the lake enum without snake_case insertion (e.g. `managesselloffer`); cosmetic, rare path. | open |
| R-A12-16 | A12 | Info | api/v1/signup_verify.go:131 | `MarkEmailVerified(ctx, keyID, time.Time{})` relies on a load-bearing main.go adapter mapping zero-time→now; worth a regression test (verified correct). | open |
| R-A12-17 | A12 | Info | middleware/keypolicy.go:49-75 | Operator-tier subjects bypass per-endpoint permission checks by design (full-access, reserved, never granted to public) — accounts for the "over-permissive" half of the brief. | open |
| R-A12-18 | A12 | Info | auth/sep10/redisreplay.go + validator.go:273-279 | SEP-10 replay guard hashes the full signed XDR, marks after verification, fails-loud at boot if unwired — VERIFIED correct. | open |
| R-A12-19 | A12 | Info | middleware/cors.go:75-79 | CORS constructor panics at boot on `*`+credentials; `Vary: Origin` set — no wildcard-with-credentials cookie-exfil vector. | open |
| R-A13-4 | A13 | Info | api/main.go:629-1062,1074-1081 | The API spawns ~9 background goroutines but shutdown only calls httpSrv.Shutdown + relies on defer-cancel; no WaitGroup join, so in-flight work can be cut mid-flight (by-design). | open |
| R-A13-5 | A13 | Info | aggregator/main.go:551-561 | Shutdown does `refresherWG.Wait()` with no timeout; a wedged Tick that ignores ctx could hang shutdown indefinitely (low likelihood). | open |
| R-A13-6 | A13 | Info | ops/ch_rebuild.go:171,385-401 | `chRebuild` buffers all decoded events for `[from,to]` in memory before writing; a naive too-wide range OOMs (missing guard rail, not a correctness bug). | open |
| R-A13-7 | A13 | Info | sla-probe/main.go:223-227 | `-base-url` has a non-empty default yet the help text + empty-check call it "required" (dead check; doc overstates). | open |
| R-A13-8 | A13 | Info | indexer/main.go:386-393,483-488 | Empty `clickhouse_addr` with the live-sink/projector-source enabled silently falls back to `127.0.0.1:9300` (the API instead 503s) — minor consistency gap. | open |
| R-A14-9 | A14 | Info | pkg/client/endpoints.go:551-561 | Dangling `CoinsOptions` doc block above `type IssuersOptions` with no `CoinsOptions` type beneath (leftover from the Coins deletion). | open |
| R-A14-10 | A14 | Info | pkg/client/errors.go:24 + doc.go | SDK has no retry logic at all — `RetryAfter` is parsed/exposed but never acted on (deliberate "caller owns retry"; recorded for explicitness). | open |
| R-A15-18 | A15 | Info | golang-migrate v4 driver behavior | Confirmed from vendored source: with MultiStatementEnabled=false the driver runs the whole file as one Exec with no transaction of its own (why the no-txn migrations are non-atomic). | open |
| R-A15-19 | A15 | Info | D2 ownership | Projected-source tables are plain tables; the one-writer rule is a Go/convention invariant (no schema constraint prevents a second writer) — consistent with the architecture. | open |
| R-A15-20 | A15 | Info | README rule 7 / source_entry_counts ownership | The 0035 r1 incident (apply-as-postgres made the table superuser-owned → 42501) is an operational apply-as-app-role invariant, correctly not fixed with a GRANT migration. | open |
| R-A16-7 | A16 | Info | customerwebhook/worker.go:274-279 | 3xx responses are classed transient-retry; with CheckRedirect:ErrUseLastResponse a permanent 301/308 retries to exhaustion rather than following/terminating. | open |
| R-A16-8 | A16 | Info | config/config.go:924-925 | `obs.trace_exporter`/`obs.trace_sample` are documented-as-reserved orphans (Validate rejects `otlp` so operators aren't misled) — intentional. | open |
| R-A17-9 | A17 | Info | SearchModal.tsx:81-88 vs explorer_search.go:65 | The `asset`-kind href trusts the backend's `/v1/...` API path; not currently reachable (the search gate never sends a bare asset id) — latent only. | open |
| R-A17-10 | A17 | Info | SearchModal.tsx:54 vs explorer_search.go:77 | Frontend fires `/v1/search` for 1–12-digit inputs; backend rejects >10 digits → harmless wasted request for 11–12-digit numbers. | open |
| R-A18-6 | A18 | Info | scripts/dev/verify.sh | `verify-launch-ready` targets are not in the canonical `verify.sh` pre-push gate (likely intentional release-time gate; expectation note only). | open |
| R-A18-7 | A18 | Info | configs/example.toml:54 + ansible templates | Postgres DSN uses `sslmode=disable` uniformly — acceptable (loopback/unix-socket only, scram-sha-256 pg_hba); worth a one-line "loopback-scoped" note. | open |
| R-A18-8 | A18 | Info | configs/caddy/Caddyfile.api:42-66 | Cloudflare trusted-proxy CIDR list is hand-maintained ("refresh quarterly"); stale CIDRs would drop real-client-IP resolution or trust a reclaimed range. | open |
| R-A18-9 | A18 | Info | ansible/roles/redis-sentinel/defaults/main.yml:74-75 | Comment claims the role "refuses to bind 0.0.0.0" but no `assert` enforces it (harmless — default is `{{ ansible_host }}`, firewall-gated). | open |
| R-A18-10 | A18 | Info | .github/workflows/api-docs.yml:24-27 + release.yml | api-docs retains `id-token: write` (legitimately needed for OIDC Pages deploy) while deploy/release dropped it — asymmetry noted so it isn't "fixed" wrongly. | open |
| R-A20-21 | A20 | Info | test/chaos/reports/2026-05-03-launch-cut/RETRO.md | The only committed chaos run (2026-05-03) predates `04-redis-misconf.sh`; the newest+highest-value scenario has no recorded passing run. | open |
| R-A21-A22-4 | A21-A22 | Info | .golangci.yml gosec excludes | G101 (hardcoded creds) globally excluded (justified — env-var NAME constants), but removes gosec as a secondary secret tripwire; gitleaks is the sole automated gate. | open |
| R-A21-A22-5 | A21-A22 | Info | rates-engine-data-validation-0603331b2417.json (untracked) | A real GCP service-account key exists in the working dir but is correctly `.gitignore`d and NOT tracked (validates `.gitignore` adequacy; never `git add` it). | open |
| R-A21-A22-6 | A21-A22 | Info | internal/** cmd/** pkg/** web/explorer/src/** | Per-file Apache-2.0 headers are sparse (17/942 Go files SPDX'd, 0/150 web) — NOT a compliance gap (root LICENSE suffices), but inconsistent with no documented policy. | open |
| R-X1-8 | X1 | Info | indexer/main.go:455 (events chan cap 256) + dispatcher.go | No within-ledger back-pressure (a whole ledger buffers before enqueue); bounded by one ledger; the only silent-loss point is the 90s drain-deadline, which logs the re-derivable range at ERROR. | open |

---

## High remediation outcomes (2026-06-15 — final)

All 25 Highs were addressed in the remediation pass. Each line: id · disposition ·
what was done (commits on `main`, 368deae6…3c751505 + the X6/X9 commit).

| # | id | disposition | what was done |
|---|---|---|---|
| 1 | R-A04-1 | FIXED | projector idle-guard `tip <= fromLedger` → `tip < fromLedger` (process `[from,tip]` inclusive). |
| 2 | R-A05-1 / R-A07-1 | FIXED | sep41 `decodeCounterparty` made shape-aware (legacy/CAP-67/bare-spec); r1-lake-verified the CAP-67 shape dominates; back-compat fixture replaced with a real shape matrix. |
| 3 | R-A11-1/2 | FIXED | composite keyset cursor (opaque `next_cursor`, ClickHouse tuple compare) for contract-events + account txs/ops. |
| 4 | R-A11-3 / R-A19-3 | FIXED | `/accounts/{g}/transactions` + `/operations` added to OpenAPI (+ `AccountTransactions`/`AccountOperations` schemas); reference regenerated → ADR-0038's claim now true. |
| 5 | R-A12-1 | DOCUMENTED | The global anonymous per-IP rate-limit (60/min) is the intended ceiling for `/v1/auth/sep10/token`: Ed25519 verify is ~µs, ReadChallengeTx (cheap parse) precedes the verify, and a per-IP throttle on the standard wallet-login flow would harm legit NAT/mobile users more than it hardens a non-material DoS. See verdict. |
| 6 | R-A12-2 | FIXED | optional `LoginThrottle` (per-IP + per-email Redis sliding window) on `/v1/auth/login`; over-quota skips the send + returns the generic 200. |
| 7 | R-A14-1 | FIXED | SDK `Envelope.Pagination` → `*Pagination` (matches server); round-trip test + nil-safe example. |
| 8 | R-A15-1 | FIXED | 0031/0040 `down` migrations made documented no-ops (forward-only; retention forbidden per ADR-0034). |
| 9 | R-A15-2 | DOCUMENTED | 0053–0060 are already applied on r1; rewriting applied migrations is riskier than the fresh-install-only non-atomicity hazard. Recorded for any future migration-authoring guideline. |
| 10 | R-A15-3 | DOCUMENTED | Same as R-A15-2 (bare `DROP CONSTRAINT` in the same already-applied files). |
| 11 | R-A15-4 | FIXED | new migration 0062 widens `trades`/`soroban_events`/`blend_auctions`/`phoenix_*` to a 7-day chunk interval (future chunks). |
| 12 | R-A16-1 | FIXED | dropped the `ApplyEnvOverrides` value-override + the `env:` tag on the S3 name fields (field-as-name is the contract); test pins it. |
| 13 | R-A17-1 / R-A17-2 / R-X3X5-1 | FIXED | UI `result_code` typed numeric (success from `=== 0`); account links → `/accounts?id=` page; total_coins BigInt. |
| 14 | R-A18-1 | FIXED | removed the stale `ratesengine-*` root binaries + generalised `.gitignore` to `/ratesengine-*`. |
| 15 | R-A19-1 / R-A19-2 | FIXED | regenerated the API reference (75=75 paths); added a `lint-docs.sh` spec↔reference sync section so `verify.sh` catches drift pre-push (the PR-only CI gate missed direct-to-main pushes). |
| 16 | R-A20-1 | FIXED | k6 silence matchers set to the real `stellarindex_api_*` alert names. |
| 17 | R-A21-A22-1 | FIXED | `internal/xdrjson/` added to import-lint rule-B allowlist; `make verify` green. |
| 18 | R-X6-1 | FIXED | Redis `/v1/account/keys` surface disabled under `auth_backend=postgres` (with a loud log); the Postgres `/v1/dashboard/keys` is the source of truth there. |
| 19 | R-X9-1 | FIXED | per-row `recover` in the projector decode loop (extracted to a unit-tested `processEventSafely`). |

**Net: 22 FIXED + pushed, 3 DOCUMENTED (R-A12-1, R-A15-2, R-A15-3), 0 unaddressed.**
The Medium/Low/Info tail (60/117/57) remains as the captured backlog — triaged,
none launch-blocking; see `04-verdict.md` for the disposition policy.

---

## Resolution facts applied (2026-06-15 remediation pass)

`FIXED-2026-06-15` was assigned to exactly these findings; everything else is `open`:

- **R-A04-1** — projector.go:253 idle-guard off-by-one (`tip <= fromLedger` → `tip < fromLedger`).
- **R-A05-1** / **R-A07-1** — sep41 `decodeCounterparty` (same shape-aware fix in both areas).
- **R-A11-1**, **R-A11-2**, **R-A11-3** — all three A11 Highs (contract-events composite cursor; account txs/ops composite cursor; OpenAPI `/accounts` paths added).
- **R-A17-1**, **R-A17-2** — both A17 Highs (result_code int-vs-string; source_account `/issuers` 404 → accounts page).
- **R-X3X5-1** — result_code int-vs-string High (same fix as R-A17-1).
- **R-A11-4** — MuxedAccount.Address panic (now `GetAddress`/muxedAddr).
- **R-A11-5** — `bump_to`/`offer_id` numeric → `strconv.FormatInt`.
- **R-A11-6** — participants muxed-gap → `muxedToAccountID`.
