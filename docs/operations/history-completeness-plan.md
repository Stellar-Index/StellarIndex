---
title: History completeness — SDEX trade backfill and XLM/USD price depth
last_verified: 2026-10-05
status: DESIGN — Project A (steps 5-6) not started, open as INV-1214; steps 2-3 may be partly run (see §3), verify before repeating; the pre-2017 span (step 8) is blocked on a licensing decision (INV-1215).
---

# History completeness plan

Two gaps, measured against r1 on 2026-09-05 (figures are that day's; re-measure before relying on one):

- **Project A (#349)** — served SDEX trade history begins at ledger 61,609,957 (2026-03-12). Everything below is absent from the served tier.
- **Project B** — daily XLM/USD (`crypto:XLM`/`fiat:USD`) candles run 2018-07-01 to 2021-01-31, stop for 1,919 days, and resume 2026-05-05.

## 0. Findings that shape the work

1. **Project A needs no archive fill.** Served `sdex` trades derive from operation results, and the ClickHouse lake holds every operation from genesis (`stellar.ledgers` ledgers 2..64,276,863, contiguous: `max - 2 + 1` equals the row count). `ch-rebuild -sdex` reads `stellar.operations` joined to `stellar.operation_results` on `(ledger_seq, tx_hash, op_index)`, filtered to the five trade-bearing op types and `stellar.transactions.successful = 1` (`internal/storage/clickhouse/sdex_op_reader.go`), and writes via `BatchInsertTrades`. No MinIO rehydration, no AWS read; #349's 2.3 TiB rehydrate and "fill-then-re-trim" steps do not apply.
2. **Project B's hole is missing trades, not missing materialisation** (no `trades` rows for `crypto:XLM` between 2021-01-31 23:59:59 and 2026-05-05; the hypertable has zero chunks in 2022 and 2023). A `refresh_continuous_aggregate` alone is a no-op: repair is an ingest, then a refresh.
3. **Kraken reaches 2017-01-17**, below the 2018-07-01 floor already ingested (that floor is the `-from` of the earlier run, not a venue limit). Probe: `GET https://api.kraken.com/0/public/Trades?pair=XLMUSD&since=0&count=5` returns `XXLMZUSD`, first fill 2017-01-17T20:17:52Z. The vendor-only span is therefore 475 days (2015-09-30 to 2017-01-16).
4. **No vendor permits serving its data through our public API on a self-serve tier** (§9). The pre-2017 span is a commercial decision before it is a technical one; steps 2, 3 and 6 of §6 do not wait on it.

## 1. Project A — size

Measured in the lake (`clickhouse-client --port 9300`):

```sql
select count(), sum(op_count), sum(classic_trade_effect_count), min(close_time), max(close_time)
from stellar.ledgers where ledger_seq < 61609957
```

| quantity | value |
|---|---|
| ledgers below the floor | 61,609,955 (2015-09-30 to 2026-03-11, 3,816 days) |
| operations below the floor | 22,268,018,473 |
| served `trades` rows to create | 2,654,748,145 (`classic_trade_effect_count`; #349 extrapolated ~2.1B) |

`classic_trade_effect_count` predicts served `sdex` rows to 0.0017% on a 10,000-ledger window (1,058,858 lake vs 1,058,840 served; the delta is the ts clamp and the `CHECK (base_amount > 0)` one-side-zero fills). It is a sizing estimate, not a reconcile oracle.

Distribution is skewed: **2022 and 2023 hold 1.77B of the 2.65B rows (67%)**; 2015-2017 together are 139,614 rows (0.005%). Never treat years as comparable chunks.

The destination is almost empty: `trades` has no chunks in 2015-2017, 2022 or 2023, and 13 MB across 136 seven-day chunks in 2018-2021; only 2024 to 2026-03 holds meaningful compressed data (~8 GB). So the compressed-chunk upsert penalty (620 rows/s, ~47 h for 72 days; ~100x faster after decompress) applies to under 10% of the span by rows. `-sdex-gaps` restricts to served-empty ranges and does a pure insert with no `ON CONFLICT` walk.

## 2. Project A — mechanism and cost

### 2.1 The tool

| tool | verdict for this job |
|---|---|
| `ch-backfill` | Not this job: the lake is complete. It also has no cold-tier fallback and defaults to the LIVE bucket, which does not hold historic ranges. |
| `projector-replay` | Cannot touch it: `sdex` is not a projected source (`IsProjectedEvent`'s default arm, ADR-0032). |
| **`ch-rebuild -sdex -write`** | This one: non-projected lake re-derive, per `docs/architecture/ingest-pipeline.md` (replay decision rule). Reads ClickHouse, takes no bucket flag. |

### 2.2 Invocation

```
run-heavy-job.sh sdex-hist \
  stellarindex-ops ch-rebuild -config /etc/stellarindex.toml \
    -sources sdex -sdex -sdex-gaps -from <lo> -to <hi> -write
```

- `-sdex-gaps` only narrows the SDEX op pass, which does not run without `-sdex`; both flags are load-bearing.
- `-sources sdex` is required under `-write`: with no `-sources` the run selects the whole catalogue, which `ch-rebuild -write` refuses because it contains decoders that are not `BackfillSafe` (`checkCHRebuildBackfillSafe`, the F050 gate). `internal/ops/chops/ch_rebuild_backfillsafe_callsite_test.go` pins this block.
- **Windows of at most 50,000 ledgers** (the SDEX op read OOMs the 10 GiB client pin above that): 61,609,955 ledgers is 1,233 windows.
- **One job name for every window and attempt.** `run-heavy-job.sh` locks per name and host-wide; a held lock refuses with exit 75 (`fuser -v` on the lock names the holder).
- `ch-rebuild` stamps `derive_generation` from the wall clock and mandates `timescale.InstallUSDVolumeResolution`, so `usd_volume` is populated, not NULLed. See [usd-volume-rederive-2026-08.md](usd-volume-rederive-2026-08.md) for the run discipline (env file, decompress-first, CAGG order).

### 2.3 Wall clock (unresolved)

Two production `ch-rebuild -sdex -write` runs exist, both on 2026 data, and projecting them two ways disagrees by 2x:

| anchor | per row | per window |
|---|---|---|
| populated compressed chunks: 47 h / ~105M rows / ~22 windows | 620 rows/s: 49.6 d | ~2.1 h/window: 108 d |
| decompress-first: 1h44m / ~21.7M rows / 5 windows | ~3,474 rows/s: 8.8 d | ~21 min/window: 18 d |

Range: **9 to 108 days**. Per-row tracks the Postgres write (history is half as dense, 43.1 vs 86.7 trade effects per ledger, and mostly lands in fresh chunks, so faster); per-window tracks the ClickHouse read of operations scanned (2022 ran 908 ops/ledger vs 2026's 759, so not faster). Expect 20-40 days; commit to no number until step 5 of §6 measures one 2022 window.

### 2.4 Storage

111.2 bytes/row (compressed payload, SDEX-dominated era, 54 chunks) to 214.5 bytes/row (whole hypertable all-in, 154,585,718,784 B / 720,555,018 rows). For 2,654,748,145 rows: **plan 300-570 GB**. Plan against ZFS `AVAIL` (4.71 TiB on 2026-09-05), not zpool `FREE` (which ignores parity). pgBackRest and off-site copies grow by the same amount.

### 2.5 After the rows land

Every trades-rooted CAGG must be re-materialised over the new days; the policies cannot (`prices_1d` `start_offset` 7 days, widest `prices_1mo` three months). Twelve views, `prices_1m` first (`twap_1h`/`twap_1d` build on it):

`prices_1m`, `prices_15m`, `prices_1h`, `prices_4h`, `prices_1d`, `prices_1w`, `prices_1mo`, `dex_volume_by_pair_1d`, `source_volume_1h`, `pools_per_source_1h`, `twap_1h`, `twap_1d`

Only the seven `prices_*` are in the Go allow-list (`allowedCAGGViews`); the rest are psql-only. [cagg-broad-recompute.md](cagg-broad-recompute.md) covers eight of the twelve and omits `dex_volume_by_pair_1d`, `source_volume_1h`, `twap_1h`, `twap_1d`; add them before using it here. Its 4-8 h estimate is for today's volume and will not hold after a 4.7x increase in `trades` (§7).

## 3. Project B — what exists

XLM has three disjoint identities (`internal/canonical/alias.go`; SAC form deliberately last so a thin Soroban pool never becomes the served XLM price): `native` (SDEX), `crypto:XLM` (every CEX), and the SAC `CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA` (Soroban AMMs). In `prices_1d` on 2026-09-03, `native` ran 2026-03-12 to date (the SDEX floor) and `crypto:XLM` ran 2018-07-01 to date.

The hole, from `prices_1d` for `crypto:XLM`/`fiat:USD`:

| span | days | state |
|---|---|---|
| 2015-09-30 to 2018-06-30 | 1,004 | nothing |
| 2018-07-01 to 2021-01-31 | 946 | held, contiguous (`{kraken}`) |
| **2021-02-01 to 2026-05-04** | **1,919** | **hole** |
| 2026-05-05 to date | 122+ | held (`{bitstamp,coinbase,kraken}`) |

`/v1/history/since-inception?asset=crypto:XLM&quote=fiat:USD` returned 1,068 points under one `price_type: "vwap"` with the 1,919-day gap unmarked between two adjacent array elements. (Live 2026-10-02 it reported 3,364 points from 2017-01-17 and flagged one 178-day gap, 2017-08-22 to 2018-02-16 as discontinuous, so a later partial fill and gap flag have landed; re-measure.) Why the Kraken run stopped at 2021-01-31 is not recoverable from the data.

## 4. Project B — the on-chain floor

- No USD-pegged asset existed on Stellar before 2016-10-18 (`USD-GBUYUAI75XXWDZEKLY66CFYKQPET5JR4EENXZBUZ3YXZ7DS56Z4OKOFU`, ledger 7,004,658). Network-wide classic trade effects were 29 in 2015, 135 in 2016, 139,450 in 2017: a daily VWAP is meaningless there.
- AnchorUSD (`USD-GDUKMGUGDZQK6YHYA5Z6AY2G4XDSZPSZ3SW5UN3ARVMO6QSRDWP5YLEX`) is materially active from 2018-10-28; Centre USDC from 2021-02-26 with immediate depth (an asset appearing in an operation result is necessary, not sufficient, for a trade).
- `internal/storage/timescale/usd_fx_resolver.go` records the served-tier floor: on-chain peg-quoted XLM markets begin 2024-03-12 (SAC x SAC USDC) and 2026-03-12 (`native` x classic USDC).

| method | earliest defensible |
|---|---|
| served tier today | 2024-03-12 |
| on-chain after Project A | ~2021-02-26 (USDC depth); arguably ~2018-10 via AnchorUSD, thin |
| on-chain before 2016-10-18 | impossible |
| CEX `crypto:XLM`, Kraken | 2017-01-17, reachable now, not ingested |

"To genesis" cannot honestly mean an on-chain USD price: 2015-09-30 to 2017-01-16 is external-only. Pinning the on-chain pair floor exactly needs the decoded claim atoms, i.e. Project A.

## 5. Project B — mechanism and cost

Measured venue depth (2026-09-05): **Kraken 2017-01-17** (`/0/public/Trades?pair=XLMUSD&since=0`); Coinbase `/products/XLM-USD/candles` populated from 2019-06 (empty 2019-01); Bitstamp `/api/v2/ohlc/xlmusd/` first bar **2020-06-17** (the `bitstamp/backfill.go` comment claiming "to 2017" is wrong by three years). Kraken is the only free source below 2019.

The tool:

```
stellarindex-ops backfill-external -config PATH -source kraken -pair XLM/USD \
  -from RFC3339 -to RFC3339 -raw-trades -write
```

`-raw-trades` selects `/0/public/Trades` (1,000 fills per page, paced 1,100 ms) rather than `/OHLC` (only the latest 720 intervals). Fill density sampled on single days (fills/day): 2021-06 12,610; 2022-06 1,466; 2023-06 647; 2024-06 374; 2025-06 2,354; 2026-02 6,726; the hole is about 6.8M fills (point samples, could be off 2x either way). Venue fetch about 2.1 h (909 fills/s); insert about 3.6 h (per-row `InsertTrade` at ~520 rows/s, the long pole; batching it onto `BatchInsertTrades` is open); extending to 2017-01-17 adds under 0.9M fills (under an hour). **Project B is hours, not days.**

Safety properties of a long walk: `-raw-trades` uses `walkWindowed` (one 24 h window fetched and written at a time, counted from `-from`, so memory is bounded). The fetch walk and the DB write have separate budgets (`externalInsertBudget`). Any stop (context expiry, venue HTTP error, infra write fault) exits non-zero printing `-from <cursor>` at the start of the unfinished window; earlier windows are written, inserts are idempotent (`ON CONFLICT`), a rerun costs at most one duplicate page.

Operating it, one quarter per slice (21 slices over the hole, ~460k fills each):

```
run-heavy-job.sh xlm-usd-<q> \
  stellarindex-ops backfill-external -config /etc/stellarindex.toml \
    -source kraken -pair XLM/USD -raw-trades \
    -from 2021-02-01T00:00:00Z -to 2021-04-01T00:00:00Z -write
```

- With `-write` the command refuses a window that already holds `trades` rows for the source and pair, naming the earliest as the `-to` to use (a backfilled row never carries the live streamer's `tx_hash`, so overlap double-counts volume). Resuming a slice this command wrote needs `-allow-overlap`, printed with `-from <cursor>` on a truncated walk.
- It refreshes no aggregate. After each slice or once at the end, run the twelve-view refresh in the §2.5 order, in weekly or monthly windows, never concurrent with another backfill (Timescale rejects the loser with `55P03`; the backfill's ~3.0 s retry budget runs out; see [backfill-procedure.md](backfill-procedure.md)).

## 6. Recommended sequence

| # | step | cost | unlocks |
|---|---|---|---|
| 1 | `backfill-external` salvage/windowed-walk fix (§5) | done | safe unattended runs |
| 2 | Kraken `-raw-trades` over **2021-02-01 to 2026-05-04**, 21 quarterly slices, refresh after each | ~6 h + refresh | closes the 1,919-day hole |
| 3 | Kraken `-raw-trades` over **2017-01-17 to 2018-06-30** | under 1 h + refresh | moves the CEX floor 530 days (see §9.1 on Kraken's terms) |
| 4 | Batch the `insertBackfilledTrades` loop onto `BatchInsertTrades` | small change | removes the long pole |
| 5 | **Project A chunk 1**: `ch-rebuild -sdex-gaps` over one 50k-ledger window in **2022**, timed (r1, under `run-heavy-job.sh`; record wall time and rows written) | ~1 h | the only honest input to §2.3 |
| 6 | Project A, reverse-chronological: 2024 to 2026-03, then 2021 to 2024, then genesis to 2021 | 9-108 days, unresolved | full on-chain history; pins the on-chain XLM/USD floor |
| 7 | Provenance work (§8) before any vendor data lands | n/a | a vendor bar is never indistinguishable from a VWAP |
| 8 | Pre-2017 span (475 days): `backfill-external -source poloniex_via_btc -pair XLM/USD -granularity 24h` (Poloniex `XLM_BTC` daily x Bitstamp `BTC/USD` daily, stamped `poloniex_via_btc`, `IncludeInVWAP: false`, named per point in `sources`; refuses a window outside the span). **Code ready, run blocked**: Poloniex terms unread and the Bitstamp BTC leg needs a signed DLA to redistribute ([data-licensing-register.md](data-licensing-register.md)). Unblock = a recorded owner decision in the register | n/a | the last gap |

Steps 2 and 3 are independent of Project A. Step 5 is in 2022 on purpose: 2022-2023 is 67% of Project A, so a timing from a sparse year is meaningless. Reverse-chronological chunks, measure chunk 1 first, never one job (from #349), are retained.

## 7. Not determined

1. `ch-rebuild -sdex` throughput on the historical era (§2.3); 2022-2023 has never been walked.
2. CAGG re-materialisation cost over 3,816 days (the runbook's 4-8 h is for current volume; `prices_1m` accrues ~390k rows/day at today's density; no historical figure).
3. Exact fill count in the 1,919-day hole (about 6.8M, off up to 2x).
4. Whether an XLM to USD-pegged pair traded on chain before 2021-02 (needs Project A's decoded claim atoms).
5. Why the Kraken series stopped at 2021-01-31 (a context expiry that discarded all fetched fills is a plausible, unproven mechanism).
6. `trades` storage accounting does not reconcile (96.6 GB index vs 35.8 GB compressed payload; hence the 111-215 B/row band).
7. Coinbase's exact listing date (bounded 2019-01-01 to 2019-06-01).
8. Vendor terms never read: CoinMarketCap's commercial API terms (the "one product, up to 100k users" reading is from the pricing FAQ); CoinGecko's exact earliest XLM row and clause numbering (cite text, not numbers); CCData's XLM floor and all pricing (free tier retired 2026-05-21); whether CMC full-history daily OHLCV starts at Startup ($79) or Professional ($699); CMC attribution wording; terms for Poloniex, Bitstamp, CoinAPI, Stellar Expert (SPA-rendered or 403).
9. Whether Kraken's bulk OHLCVT dumps are offered (two Kraken pages contradict; no licence statement on either).

## 8. Provenance — required before any external data lands

An external daily close is not an on-chain or CEX-fill VWAP and must never merge silently into one series.

- `sources` is already the carrier: `prices_1d` has `array_agg(DISTINCT source) AS sources` (migration 0147), and `/v1/price` surfaces it beside `data`.
- **Gap:** `/v1/ohlc` intervals carry OHLCV and `n` but no `sources`; `/v1/history/since-inception` points carry `t`, `p`, `v_usd` under one response-level `price_type`. Required change before any vendor row is written: add `sources` to each element of `intervals` (and per point on since-inception), populated from the existing column.
- A vendor series enters as its own `source` in `internal/sources/external/registry.go` with `IncludeInVWAP: false` (the posture the CoinGecko/CoinMarketCap/CryptoCompare reference adapters hold); `price_type` is where the distinction belongs on the wire.
- **Coverage must not absorb it.** `/v1/coverage` reports `complete`/`coverage_pct` for the lake axis while served `sdex` `trades` begin at 61,609,957 (projection floor 63,636,711 in `completeness_target_floors`), and ADR-0033 has no notion of CEX coverage (a 1,919-day hole on the flagship pair passed 20/20). A vendor span is neither `lake_complete` nor `projection_ok` and needs its own axis, never a reused one.
- A pending CoinGecko Pro purchase is the dependency for the CoinGecko row; it would not cover serving through our API.
- Cheapest first: steps 2-3 shrink the vendor question from 2,923 days to 475, a span with no on-chain USD price at all. They are not term-free (§9.1); the 946 days already served were taken the same way.

## 9. Sources for the pre-2017 span — findings (read 2026-09-05; no recommendation, nothing agreed to)

### 9.1 Kraken is not term-free

Kraken Global Terms (updated 2026-09-04) §9 bar "web scraping, web harvesting, or data extraction methods" and third-party applications that interact with Our Content without prior written consent; §8 grants only a "limited, revocable permission". The 946 served days were obtained the same way (pre-existing exposure; steps 2-3 extend it by 2,449 days). Kraken's bulk OHLCVT dumps would avoid the scraping clause but one Kraken page says no bulk dump exists (§7.9).

### 9.2 Venue listing dates (earliest XLM daily bar)

Poloniex `XLM_BTC` 2014-08-11 (BTC-quoted); Kraken `XXLMZUSD` 2017-01-17; Yahoo `XLM-USD` 2017-11-06; Bitfinex `tXLMUSD` 2018-05-01 (never had an STR symbol); Bitstamp `xlmusd` 2020-06-17. Nothing USD-quoted and free reaches below 2017-01-17.

### 9.3 The only genuine 2014 series is BTC-quoted

Poloniex `XLM_BTC` is a real daily series from 2014-08-11 (pubnet genesis is 2015-09-30, so the honest floor is genesis). A USD cross needs Bitstamp `BTC_USD` (verified back to 2014-08-30; Poloniex `BTC_USDT` is empty before 2014-09-01 and `XLM_USDT` is unusable: zero volume, ~10x off price in 2015). Poloniex terms were NOT FOUND (page renders no text); context as fact: under Justin Sun's control since 2019-10, no US users since 2019-12, on the UK FCA warning list.

### 9.4 Stellar-native cannot supply it

The reference public Horizon returns oldest ledger 57,969,361 (2025-07-12): about 14 months of retention (ADR-0001: nothing here depends on it; the lake is the deeper source). No USD asset before 2016-10-18 (§4). `api.stellar.expert` returns 403.

### 9.5 Free and open sources — every one fails on terms

| source | depth | operative clause |
|---|---|---|
| Kraken | 2017-01-17 | Global Terms §8, §9 (§9.1) |
| Bitfinex | 2018-05-01 | Market Data Terms (2021-01-04) §1 "solely for your internal purposes"; §2.1.1 no distribute/disclose/resell (read from Bitfinex's GitHub org, not byte-matched) |
| Yahoo Finance | 2017-11-06 | Terms (2025-05-06) §2.4(j): no database/archive/data feed that competes with or substitutes the Services |
| DefiLlama | to 2014-10-01 (CoinGecko upstream) | Terms (2025-06-24) §7 personal/non-commercial; §8(7) no republication; §14 liquidated damages up to USD 100,000 per violation; two licences to clear |
| CryptoDataDownload | no XLM pairs | (2023-09-20) non-commercial use only |
| CoinPaprika | 1 year free | site terms §4.1(c)(d)(e) bar aggregating for third parties, licensing, scraping |
| Investing.com | n/a | no use/store/display/distribute without prior written permission |
| CoinCap | gone (NXDOMAIN) | n/a |
| CoinLore | 365 days | terms NOT FOUND |
| Kaggle dataset | claims 2014-09-17 to 2021-11-29 | licence label unread; the uploader scraped an aggregator and cannot grant rights never held |
| Nasdaq Data Link / Quandl | no free XLM table | NOT FOUND |
| CoinAPI | no free plan | terms 403, NOT FOUND |

Zero free sources have both genuine pre-2017 XLM/USD daily history and terms permitting commercial redistribution.

### 9.6 The paid aggregators

Display (vendor bars on charts here) and serve onward (the same bars via `/v1/ohlc`, `/v1/history/since-inception`) are different acts.

| | CoinGecko | CoinMarketCap | CCData (CoinDesk Data) |
|---|---|---|---|
| earliest XLM daily | 2015-03-04 proven (ATL date); "from 2014" is a vendor claim | 2014-08-05 measured (via their site's internal endpoint, not the licensed API) | not verified (key-gated) |
| bar shape | close only before 2018-02-09; OHLC from 2018-02-09 | OHLC from day one | claimed OHLCV |
| granularity trap | ">90 days = daily"; `/coins/{id}/ohlc` `interval=daily` caps at 180 days | none found; 365 points/request on `/v2/cryptocurrency/ohlcv/historical` | 2,000 points/request |
| cheapest tier with full history | Analyst $129/mo (Basic $35 caps at 2 years) | $79 or $699, unresolved | no public pricing |
| display in a commercial product | yes, paid tiers, with attribution ("Powered by CoinGecko", font 10+) | yes, one product, up to 100k users | no, at any price |
| serve onward via our API | no, Enterprise only | no, Enterprise only | no, negotiated licence only |

**No self-serve tier of any vendor permits serving their data through this product's API.** Operative clauses: CoinGecko API Terms (latest 5 Sept 2025) cl. 4.1.6 (no sell/sub-license/re-distribute/syndicate access unless under an Executed Agreement; charging for products that incorporate the API is allowed); CoinMarketCap pricing FAQ (own product yes; no redistribution or resale as a standalone service, including through your own API); CCData API Licence Agreement (effective 2026-04-22) cl. 2.1 internal use only, cl. 2.4.1 no Display, cl. 2.4.2 no Products (no free/paid split).

Easy to miss:

1. **CoinGecko's storage clauses sit badly against this plan**: cl. 6.1.1 refresh cache at least every 24 h; cl. 6.1.4 on termination delete all Data without keeping any copy; cl. 6.2 no storing or deriving from Data. This plan would persist their bars indefinitely in `trades` and every aggregate. "Should", not "shall", and 6.2 is qualified, so ambiguous: a decision, not an assumption.
2. **A $620/month question is unresolved on CoinMarketCap** (pricing matrix says Startup $79 includes full history; their own resources article says Professional $699).
3. **CMC's 2014-08-05 floor came from their internal site endpoint**; their site terms (2025-11-24) §5.1(d) prohibit scraping, so it is evidence the data exists, not a usable source.
4. CoinGecko's website terms (2025-08-12) cl. 4.5 (personal, non-commercial use) govern the site, not the API; read the API terms for an integration (the website-terms text was second-hand; re-read before relying on it).
