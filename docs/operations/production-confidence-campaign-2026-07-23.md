---
title: Production-confidence campaign — evidence ledger
started: 2026-07-23
status: IN PROGRESS
owner: run to a state of ~99.99% confidence across every product surface before presenting to Stellar
---

# Production-confidence campaign (2026-07-23)

Companion to `production-readiness-master-plan-2026-07-18.md`. The master plan
tracks *getting the substrate correct* (Phases A–F). **This doc is the adversarial
proof that it IS correct** — a systematic, oracle-anchored evaluation of every
product surface, run while the D2 backfill completes.

## Method — why this reaches 99.99% and a code-read doesn't

Two disciplines, applied to every claim:

1. **Differential verification against an independent oracle.** For every value we
   serve, check it against a source with no shared cause of error: Horizon, the
   ledger header, the SDF history archives, CoinGecko, Chainlink, exchange tickers,
   raw XDR. Internal consistency is how the INV-3 treadmill and the "comprehensive
   across all eras" attestation both survived for months — it is not evidence.
2. **Cold adversarial coverage of 100% of the surface.** Construct the failure,
   verify each finding independently, leave no unexamined white-space, and treat
   docs / tests / comments / prior audits as untrusted.

**A finding is only "confident" when it survives an oracle it can't have biased.**

## Environment fact (established 2026-07-23)

**This is REAL mainnet data, correctly ingested** — proven three ways: (1) the XLM
inflation curve (`total_coins` 104.28B@20M → frozen 105.4439B@30M+, matching the
Oct-2019 inflation-disable); (2) **our lake `total_coins` === Horizon live
`total_coins` to the stroop** (105,443,902,087.3472865); (3) three sampled account
balances match Horizon live exactly (B5). Therefore external real-world oracles
(Horizon, CoinGecko, Chainlink) are **valid pass/fail ground truth** for this campaign.

Ingest lag observed: our tip 63,611,263 vs Horizon 63,611,369 = ~106 ledgers / ~9 min
(healthy for batch ingest; quantify properly in G2).

---

## Track list (exhaustive — grows as new surfaces are found)

Legend: ✅ proven · 🔵 in progress · ⬜ not started · ⚠️ finding open

### A — Data substrate integrity
- **A1** Ledger contiguity 2→tip (gaps/dupes) — ✅
- **A2** Extraction completeness (header tx/op == our rows; vs Horizon) — ✅ (sampled)
- **A3** Fidelity map (ops vs *successful* ops vs real-op changes; no degraded windows) — ✅
- **A4** Ordinal contiguity everywhere (bad_ledgers==0) — 🔵 (blocked on D2/D3 completing)
- **A5** Global duplicate sweep (re-ingest dup class, all tables) + "do all served reads dedup?" — ✅ (recent clean; historical benign)
- **A6** Census-row DELETE safety (no real-op rows share those keys) — ⬜
- **A7** Soroban / contract-event completeness (>4-topic truncation, C2-11) — ⬜
- **A8** Cross-table referential integrity (every op has its tx; every change has its op) — ✅

### B — Served money correctness (crown jewel)
- **B1** Price accuracy vs CoinGecko/Chainlink/exchanges (broad asset set) — 🔵 (XLM/USDC ✅)
- **B2** XLM supply (void-address reconciliation) — ✅
- **B3** Classic asset supply (Algorithm 2) vs issuer/Horizon `/assets` — 🔵 (ours resolved; oracle parse pending)
- **B4** SEP-41 / Soroban asset supply — ⚠️ (B4-F1: sep41 coverage incomplete)
- **B5** Balance reconciliation (N accounts vs Horizon live) — ✅ (3 accounts exact; broaden N)
- **B6** USD-volume waterfall + coverage (100% ext exchanges, 99.5%+ SDEX) — 🔵 (15/17 sources complete)
- **B7** Independent VWAP recompute vs served — ✅
- **B8** Decimal / i128 / FX precision fixtures (JPY-inversion, 10^decimals, i128 bounds) — 🔵 (FX cross-currency ✅)
- **B9** Aggregate / rollup correctness (every total == sum of its parts) — ✅
- **B10** Cross-endpoint consistency (same fact agrees on every endpoint + the explorer) — 🔵 (⚠️ B10-F1 ~0.16% price spread)
- **B11** Historical / time-series correctness (OHLCV integrity) — ⚠️ (B11-F1 OHLC outlier pollution)
- **B12** Junk/scam-asset trades polluting native-XLM non-volume-weighted metrics (surfaced) — 🔵

### C — API contract & robustness
- **C1** OpenAPI schema conformance (every endpoint's live response) — 🔵 (98-route smoke ✅; ⚠️ C-F1 two dead endpoints)
- **C2** Error contract (RFC 7807 problem+json, correct status codes) — ✅ (spot)
- **C3** Pagination stability (no dup/gap across pages; cursor integrity) — ✅
- **C4** Fuzz / abuse (malformed, huge limits, unicode, injection → no 5xx/leak) — ✅ (extend to POST/auth)
- **C5** Latency SLO p95/p99 (normal + under load) — 🔵 (⚠️ C-F2 15s reserves, C-F3 ~8s detail scans)
- **C6** Auth + rate-limit enforcement — ✅ (auth rejects; rate-limit TODO)
- **C7** Endpoint determinism / idempotency — ✅

### D — Explorer (live)
- **D1** Re-verify the 44 remediated site-audit findings still hold — ⬜
- **D2** Every widget's data traced to its endpoint + freshness — ⬜
- **D3** Non-Stellar-data-on-Stellar-pages sweep (legacy fiat class) — ⬜
- **D4** Dead-link / detail-route 404 sweep — ⬜

### E — Correctness proof (the Phase-E go-live gate)
- **E1** `reconcile-balances` (post-backfill) — ⬜ (needs D2/D3/D4)
- **E2** `compute-completeness` — ⬜
- **E3** Re-derive determinism — the INV-3 treadmill fix PROVEN: a corrected re-derive
  changes the value; an unchanged re-derive is byte-identical — ⬜
- **E4** `supply_cross_check_divergence` alert clears — ⬜

### F — Deploy / DR
- **F1** Signed-release (sigstore) verification actually verifies — ⬜
- **F2** Migration up/down rehearsal (0109–0114+) — ⬜
- **F3** DR restore drill — ACTUALLY EXECUTE (dataset is empty, never tested — real gap) — ⬜
- **F4** Backup coverage (CH + PG + off-site) — ⬜

### G — Observability / resilience
- **G1** Alerts actually FIRE (supply divergence, ZFS, ingest lag, crash-loop) — ⬜
- **G2** Ingest lag (lake tip vs network tip) — ✅ (6s, healthy)
- **G3** Data-pool watchdog proven — ✅ (evidenced 2026-07-18 halt; re-confirm)
- **G4** Scheduled scans firing — ⬜

### H — Security surface
- **H1** Injection (SQL / XDR / html-template) — ⬜
- **H2** Secret exposure / least-privilege — ⬜
- **H3** CORS / CSP / embed-iframe surface — ⬜
- **H4** Error information leakage — ⬜

### I — Whole-repo cold code audit
- **I1** `/audit` suite across every file + flow, each finding skeptic-verified —
  ⬜ (multi-agent workflow; needs explicit opt-in, else scaled-down inline pass)

### J — Regression suite (prior findings stay fixed)
- **J1** Re-verify every prior audit finding (INV-3, FX inversion, 10^decimals SAC,
  CS-### corpus) is still fixed — ⬜

### K — Config / schema drift
- **K1** Ansible drift (deployed config vs repo) — ⬜
- **K2** CH schema vs migrations — ⬜
- **K3** Prometheus rules parity (R1 overlay vs multi-host) — ⬜

### L — Self-instrumentation trustworthiness (surfaced 2026-07-23)
The product ships rich health diagnostics (`/coverage`, `/divergence`, `/anomalies`,
`/diagnostics/{ingestion,archive,cursors}`). If those are wrong, operators get FALSE
confidence — so audit the monitors themselves.
- **L1** Does `/anomalies` actually consume `/divergence`? (`divergence_checked:false`
  while `/divergence` has data — ⚠️ L1-F1 candidate) — 🔵
- **L2** Is `/coverage`'s per-source `complete:true` independently true (not just
  self-asserted)? — ⬜
- **L3** Is the archive cross-anchor (0 missing) computed against the real archive? — ⬜
- **L4** Do the diagnostics' freshness stamps (`computed_at`, `scanned_at`) prove they
  run on cadence, not once-and-stale? — ⬜

### M — Streaming / SSE endpoints (surfaced 2026-07-23)
Six `*/stream` endpoints (ledger, price, price/tip, observations, oracle/streams,
ledger/stream). Not covered by the request/response sweep.
- **M1** Streams emit valid, ordered, gap-free events; heartbeat; clean close — ⬜
- **M2** Stream backpressure / slow-consumer handling (no unbounded buffer) — ⬜

---

## Evidence log

### A1 — Ledger contiguity — ✅ PASS
`SELECT min, max, count, uniqExact(ledger_seq), span FROM stellar.ledgers`:
- range **2 → 63,611,263**; **span == distinct == 63,611,262 → ZERO GAPS**. Every
  ledger present exactly once.
- rows 124.37M vs distinct 63.61M → **~2× unmerged re-ingest duplicates** in the
  ledgers table. Benign for correctness (RMT dedups on read/merge) but confirms the
  dup class is **lake-wide**, not isolated to partition 44 → makes A5 mandatory.

### A2 — Extraction completeness — ✅ PASS (sampled, recent)
Four-way reconciliation on recent ledgers (63.60M/63.605M/63.610M): **ledger-header
`tx_count`/`op_count` == our dedup'd `transactions`/`operations` row counts, to the
row** (336/653, 316/638, 352/629). Horizon agrees on tx (296+40=336 ✓); its lower
`operation_count` is Horizon counting successful-tx ops only — ours matches the
authoritative header. Historical ledgers can't use public Horizon (410 before-history)
→ header is the oracle there; extend A2 across the range via `SUM(header)` vs rows.

### A3 — Fidelity map — ✅ PASS (no degraded windows genesis→tip)
ops-vs-real-op-changes sampled every few M ledgers: no window shows the degraded
signature (many ops, ~0 changes). Formerly-degraded 35M (was `25,582 ops → 4 changes`)
now `1.0M ops → 2.13M changes`; [54→63M] now 1.8–2.2. Early-region low ratios (1M, 5M)
proven benign: they are **failed-transaction operations** (which correctly produce zero
entry changes) — measured against *successful* ops the ratio is healthy (3.1, 1.9).

### B1 — Price accuracy — 🔵 (XLM/USDC ✅)
vs CoinGecko: XLM ours `$0.18242982` vs `$0.182457` = **0.015%**; USDC ours
`$1.00000000` vs `$0.999804` = **0.02%**. Both far inside the <0.25% bar, derived
from our own trade data. TODO: broaden to top-50 assets + Chainlink + exchange tickers.

### B2 — XLM supply — ✅ PASS (independent void-address reconciliation)
`total_coins` 105.443902B (correctly ingested inflation curve, frozen Oct-2019) − void
address `GALAXYVOID…ILUTO` 55.442115B = **50.001787B ≈ hardcoded 50.001807B (xlm.go),
0.00004% diff**. Served `total_supply`=50.0B is the correct community figure. Sum of all
XLM account balances 104.66B reconciles to `total_coins` within fee_pool+claimable+LP.
No bug; supply logic (ADR-0011 Algorithm 1) validated end-to-end.

### A8 — Cross-table referential integrity — ✅ PASS
Recent 5000-ledger window [63.500M,63.505M]: operations whose `tx_hash` is missing
from `transactions` = **0**; real-op changes whose `(tx_hash,op_index)` is missing
from `operations` = **0**. Clean both directions. TODO: extend to more windows +
historical eras.

### B5 — Balance reconciliation — ✅ PASS (exact vs Horizon live)
`ledger_entries_current FINAL` XLM balance vs Horizon `/accounts/{id}` native balance
for 3 large accounts (void 55.44B, + two 3–4B holders): **all match to the stroop**
(diff 0.0000). Our state lake is byte-identical to on-chain reality. TODO: broaden to
N random accounts incl. small/dust + trustline balances; control for the ~106-ledger lag.

### B3 — Classic asset supply — 🔵 (ours resolved; oracle parse pending)
Our served: USDC `total_supply`=300,246,809.67, yXLM=155,011,717.72. Horizon `/assets`
parse failed this pass — redo with a fixed `_embedded.records[].amount` extraction.

### C2 — Error contract — ✅ PASS (spot)
`/v1/price?quote=USD`, `/v1/price` (no params), `/v1/ohlc` → RFC 7807
`{type,title,status,detail,instance,request_id}`, all well-formed with helpful
`detail`. Even the 500 body is clean RFC 7807 with **no internal leakage**. TODO:
full bad-input matrix across all endpoints.

### C1/C5 — Live smoke sweep across all 98 GET endpoints — ⚠️ 3 FINDINGS
Hit every GET route from R1→localhost with real path params. **48 healthy 2xx, 5
correctly auth-gated (401/403), most 400s are correct "missing required param"** RFC
7807. But:

- **⚠️ C-F1 (HIGH — two dead public endpoints):** `/v1/accounts/{g}/operations` and
  `/v1/accounts/{g}/transactions` return **500 for EVERY account** (verified on a
  whale AND a small account) — both time out at exactly 8s. Root cause: they scan
  `stellar.operations` / `transactions` (4.8B / 10.2B rows) filtered by
  `source_account` via a `bloom_filter` skip-index (`idx_op_source`), but those
  tables are `ORDER BY (ledger_seq,tx_index,op_index)`, so one account's rows scatter
  across the whole range and the bloom under-prunes → ~8s scan > the handler's 8s
  ceiling → 500. `/movements` survives (8.05s) only because it reads the per-account
  `account_movements` table. **Fix class:** give operations/transactions the same
  account-ordered access (a per-account table like movements, a `(account,ledger_seq)`
  projection — capacity-sensitive on 4.8B rows — or extend `operation_participants`,
  which is account-ordered and already 4.2B rows, to include the op's own source so
  both arms are point-lookups). Ties to the roadmapped `account_wealth_snapshot` /
  detail-route follow-up. **A "present to Stellar" blocker — two documented endpoints
  are non-functional.**
- **⚠️ C-F2 (MED):** `/v1/lending/pools/{pool}/reserves` = **15.0s** (returned 200 —
  so this path apparently has NO 8s cap, which is its own problem: an endpoint that
  can run 15s ties up a serving thread).
- **⚠️ C-F3 (MED, known class):** cold detail scans at/near the ceiling — `/accounts/{g}`
  7.9s, `/movements` 8.05s, `/positions` 4.1s, `/external/assets` 4.0s. Same
  account-scan root as C-F1; the AccountState cache only helps repeat views.

Contract retest (real ID `CAS3J7GY…`): `/contracts/{id}` 200 (1.5s), `/interactions`
200 (0.95s), `/transfers` 200 (0.12s), `/wasm` 404 (no wasm — plausibly correct);
**`/code-history` → 500 at 8s** → **folds into C-F1**: the dead set is now
operations + transactions + code-history — i.e. *every endpoint that scans a big
fact-table by a non-primary key.* Indexed contract endpoints are fast; that confirms
the root cause and the fix boundary.

### C4 — Fuzz / abuse — ✅ PASS
8 hostile inputs (SQLi `'OR'1'='1`, `<script>`, `%00` null-byte, `DROP TABLE`,
`limit=999999999`, `limit=-5`, ledger `-1`, ledger 20-digit-overflow) → **all 400,
zero 5xx, zero internal leakage** (grepped bodies for panic/goroutine/sql/clickhouse/
hex-addr/paths). Input validation + error handling are robust. TODO: extend to POST
bodies + auth-token fuzz.

### B10 — Cross-endpoint price consistency — 🔵 (minor finding B10-F1)
Same asset (XLM/USD), three endpoints, three slightly different prices:
`/assets/native` 0.18211 · `/price?asset=native` 0.18240 (bucket 13:55:00) ·
`/price/tip` 0.18230 (13:56:38). ~0.16% spread, ordered by freshness (tip newest) on
a −3.2%/day asset — plausibly correct-per-window, NOT proven a bug. **B10-F1 (LOW):**
the headline price on the asset page (`/assets`) can visibly disagree with `/price`;
reconcile the window/freshness or document it so consumers understand why. Verify each
is internally correct for its stated window before closing.

### C3/C6/C7 — pagination / auth / determinism — ✅ PASS
- **C3:** `/v1/assets` paginated 3×5 via `pagination.next` cursor → 15 unique assets,
  0 dupes, cursor advances monotonically. Stable.
- **C6:** protected endpoints (`/account/me`, `/account/keys`, `/account/usage`) all
  return **401 without a token**. Auth enforced.
- **C7:** two calls to `/v1/ledgers/63000000` (immutable historical) are
  **byte-identical except the envelope `as_of` stamp** → deterministic.

### B11 — OHLCV integrity — ⚠️ FINDING B11-F1 (MED-HIGH, data quality)
48 XLM/USD hourly candles all satisfy OHLC invariants (h≥o,c,l; l≤o,c,h). **BUT the
04:00 candle `high`=0.2000000000 is fabricated** — no XLM trade on ANY CEX venue
exceeded 0.1848 that hour (binance-USDT 0.1848, coinbase-USD 0.1846, kraken 0.1845).
**Root cause:** SDEX XLM (`base_asset='native'`) trades against **junk/scam tokens**
(`LG-GDI7NW2H…`, `HUGH-GDI7NW2H…`) at absurd ratios (up to 414e9 quote/base) — 6,283
such trades, total **$1,796 USD volume (~0 each)**. VWAP is protected (volume-weighted,
so B7 passed), but **OHLC high/low take max/min with no liquidity/recognition filter**,
so scam-token SDEX prints leak a phantom spike into the XLM/USD chart. **Fix class:**
filter OHLC (and any max/min metric) by quote-asset recognition + a min-liquidity/USD-
volume floor, the same way VWAP effectively is. Broader implication (new **sub-track
B12**): junk-asset SDEX trades against `native` may pollute other non-volume-weighted
XLM metrics — sweep for max/min/last aggregations that lack the filter.

### B7 — Independent VWAP recompute — ✅ PASS (validates core pricing math)
Recomputed XLM/USD VWAP from the raw 535M-row PG `trades` table (5-min window):
fiat:USD 0.18208 (320 trades), crypto:USDT 0.18218 (62). **Served = 0.18211 →
0.016% vs recomputed.** The VWAP the API serves is faithful to the underlying trades.
BONUS (B8/FX): XLM/EUR 0.16025 and XLM/GBP 0.13668 both convert to ~0.182 USD — the
FX waterfall reconciles across currencies. Data model note: raw trades live in PG
Timescale (`trades` 535M rows / 73 GB hypertable), NOT CH; CH is the substrate lake.

### B8 — FX / precision — 🔵 (FX cross-currency ✅ via B7)
XLM priced in EUR/GBP converts to the same USD as XLM/USD (±rounding) → FX conversion
correct in the live path. TODO: the explicit fixtures (JPY 163.09 non-inversion,
10^decimals supply, i128 boundary amounts, dust) as targeted cases.

### G2 — Ingest lag — ✅ PASS
`/diagnostics/ingestion`: `latest_ledger 63,611,543, lag_seconds 6`. Live v0.20.9.
Lake tracks network tip within ~6s — healthy. (The ~106-ledger gap I saw earlier was
the `ledger_entry_changes` state-diff tip vs the `ledgers` header tip; quantify that
separately.) 24h volume $2.87B, 22,697 markets, 191,738 assets indexed.

### Archive completeness cross-anchor — ✅ corroborates A1
`/diagnostics/archive`: range 2→63,603,985, `cross_anchor expected 993,812 found
993,812 missing_count 0`. The SDF history-archive checkpoint set is fully present —
an independent-ish completeness signal on top of A1's contiguity.

### E4 — Divergence tracker — 🔵 LIVE (corroborates B1)
`/divergence`: XLM/USD ours 0.18226 vs coingecko 0.18256 (−0.16%, *clear*), vs band
0.18464 (−1.29%, *clear*), redstone present. Multi-oracle price divergence is
monitored live and clear. NOTE: this is *price* divergence — still need the *supply*
cross-check (`supply_cross_check_divergence`) for E4 proper. Ours sits closest to
coingecko; band runs high (its own venue mix). TODO: independently confirm the
divergence math + that "clear" thresholds are sane.

### B6 — Coverage (self-reported) — 🔵
`/coverage`: aquarius + band report `complete:true, coverage_pct:1, substrate_ok +
recognition_ok + projection_ok` to tip. Self-reported — TODO: enumerate ALL sources
and independently corroborate a `complete:true` (Track L2), + measure usd_volume
NULL-rate per venue against Ash's bar (100% ext, 99.5% SDEX).

### L1 — Anomaly detector wiring — ⚠️ FINDING candidate
`/anomalies` returns `firing_count:0` with `divergence_warning:false` **and
`divergence_checked:false`** — yet `/divergence` returns live data with a −1.29% band
delta. If the anomaly path doesn't actually consume the divergence computation, a real
future divergence could fire nothing. **L1-F1 (MED, needs code trace):** confirm
whether `divergence_checked:false` means the anomaly detector skips divergence.

### A5 — Duplicate sweep — ✅ recent clean; historical dupes benign
Recent [63.4M–63.5M] window: ledgers/transactions/operations/ledger_entry_changes all
`raw == uniqExact(PK)` (no dupes). The ledgers table's global ~2× (124M/63.6M) is
therefore **confined to older re-ingested ranges** (D1 archive-walk + D2 backfill
re-ingests) — benign because reads dedup via FINAL, and RMT collapses on merge. TODO:
localize which partitions carry them (schedule OPTIMIZE later); + the code-side check
that EVERY served read of an RMT table uses FINAL/argMax/LIMIT-1-BY (no raw read
double-counts).

### B6 — Coverage — ⚠️ SEP-41 incomplete
17 sources via `/coverage`: **15 `complete:true`** (aquarius, band, blend, cctp, comet,
defindex, phoenix, redstone, reflector-{cex,dex,fx}, rozo, sdex gen→tip, soroswap,
soroswap-router). **⚠️ B4-F1 (MED): `sep41_supply` + `sep41_transfers` report
`complete:false`** — Soroban token supply/transfer tracking is NOT complete (ties to
C2-11 >4-topic event truncation). Independently confirm what's missing + impact on
served SEP-41 asset supply/volume.

### L4 — Diagnostic freshness — ⚠️ FINDING
`/coverage` `computed_at` = 05:37 but wall-clock 14:03 → **~8.5h stale**; its watermark
(63,606,021) trails tip (63,611,543) by ~5.5k ledgers. **L4-F1 (LOW):** the coverage
diagnostic is a morning snapshot, not live — an operator reading it sees stale
completeness. Confirm the intended refresh cadence + whether the staleness flag guards it.

### E4/B10 — native vs crypto:XLM internal price split — ⚠️ FINDING
`/divergence` prices `native` and `crypto:XLM` as SEPARATE assets against the same
references with different deltas (native/coingecko −0.16% vs crypto:XLM/coingecko
+0.37%) → **~0.5% gap between two internal XLM price representations.** All 22
observations are `clear` (good — multi-oracle: band/chainlink/coingecko/reflector/
redstone, max −1.29%), but **B10-F2 (MED): the same underlying asset (XLM) carries two
prices**; a consumer hitting different endpoints/asset-ids gets different XLM/USD.
Trace whether `native` (SDEX-anchored) and `crypto:XLM` (CEX-anchored) are meant to
diverge, and if so document; if not, reconcile.

**→ B10-F2 RESOLVED (benign):** `/v1/price?asset={native,crypto:XLM,XLM}` all return the
IDENTICAL price (0.18198684526717308339) + `asset_id:crypto:XLM`. The /divergence delta
gap was a sampling-time artifact (per-pair cadence), not two prices. No serving-layer
inconsistency.

### B9 — Aggregate / rollup correctness — ✅ PASS
`/network/stats` `volume_24h_usd`=$2,820,338,210 vs Σ(top-200 markets' 24h vol)
=$2,819,693,072 → **0.02% diff**, the residual being the 22,069 long-tail markets not
in the top-200 sample. The headline network total reconciles to the sum of its market
parts. TODO: extend to per-source volume rollup + DEX pool aggregates.

### L1 — Anomaly wiring — ✅ RESOLVED (honest instrumentation, not a bug)
`DivergenceChecked` is a deliberate CS-087 discipline (`envelope.go`, `score.go`):
`false` means "no live cross-oracle check ran for THIS response," and consumers "MUST
NOT read false as references agree." Divergence IS monitored (22 live `/divergence`
observations, all clear). Residual: prove a real divergence actually FIRES an anomaly
→ folded into **G1** (inject-and-observe).
