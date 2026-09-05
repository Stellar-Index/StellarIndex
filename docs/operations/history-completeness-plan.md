---
title: History completeness — SDEX trade backfill and XLM/USD price depth
last_verified: 2026-09-05
status: DESIGN — costed, not started. No backfill has been run against this plan.
---

# History completeness plan

Two related gaps, both measured against r1 on 2026-09-05:

- **Project A (#349)** — served SDEX trade history begins at ledger
  61,609,957 (2026-03-12). Everything below that is absent from the served
  tier.
- **Project B** — daily XLM/USD candles run 2018-07-01 → 2021-01-31, stop
  for 1,919 days, and resume 2026-05-05.

Every figure below carries the query or file citation that produced it.
Numbers with no citation do not appear; where a number could not be
measured, §7 says so instead of estimating one.

---

## 0. The three findings that change the shape of the work

### 0.1 Project A does not need an archive fill

Issue #349 costs the SDEX backfill as a re-ingest and makes it conditional
on first rehydrating ~2.3 TiB of ledger meta from the AWS public dataset,
because r1's local `galexie-archive` was trimmed to a hot floor of
49,984,000 on 2026-07-26 (`docs/operations/galexie-backfill.md:20-37`).

That prerequisite does not apply to this job. The served `trades` row for
`source = 'sdex'` is derived from operation results, and the lake already
holds every operation result from the start of the chain:

```
clickhouse-client --port 9300 -q "
  select 'ledgers' t, min(ledger_seq), max(ledger_seq), count() from stellar.ledgers
  union all select 'operations', min(ledger_seq), max(ledger_seq), count() from stellar.operations
  union all select 'operation_results', min(ledger_seq), max(ledger_seq), count() from stellar.operation_results"

ledgers            2   64276863   64276862
operations         3   64276863   24716353659
operation_results  3   64276863   24716248175
```

`64276863 - 2 + 1 = 64276862`, which is exactly the row count — the ledger
substrate is contiguous from genesis with zero gaps, and `operations` and
`operation_results` span the same range (ledger 2 carries no operations).

`ch-rebuild -sdex` reads precisely these tables. Its SQL
(`internal/storage/clickhouse/sdex_op_reader.go:107-121`) joins
`stellar.operations` to `stellar.operation_results` on
`(ledger_seq, tx_hash, op_index)`, filtered to the five trade-bearing op
types (`sdex_op_reader.go:21-27`) and gated on
`stellar.transactions.successful = 1`. It writes served rows through
`store.BatchInsertTrades` (`internal/ops/chops/ch_rebuild.go:739-748`,
insert SQL at `internal/storage/timescale/trades.go:1209`).

**So Project A is a projection job over data already on disk, not a
re-ingest.** No MinIO rehydration, no AWS read, no pool headroom
surrendered to a temporary archive fill. The ~2.3 TiB prerequisite and the
"gives back the headroom the trim created" objection in #349 both fall
away.

### 0.2 Project B's hole is missing trades, and its floor is a run's floor, not a venue's

The gap in daily XLM/USD candles is not an unmaterialised aggregate. The
underlying trades are absent (§4.2), so no amount of
`refresh_continuous_aggregate` will close it.

Separately, the 2018-07-01 floor of the existing series is the `-from` of
the run that produced it, not the limit of the venue. Kraken's public fills
endpoint, probed 2026-09-05:

```
curl -s "https://api.kraken.com/0/public/Trades?pair=XLMUSD&since=0&count=5"
  → XXLMZUSD, earliest fill 2017-01-17T20:17:52Z, price 0.002251
```

Kraken holds 530 further days below the floor already ingested, reachable
with the tool that is already in the tree and at no cost beyond its rate
limit. That moves the genuinely vendor-only span from 1,004 days down to
**475 days** (2015-09-30 → 2017-01-16).

### 0.3 No vendor sells what serving that span would require

The remaining 475 days can only come from a third party. Of the three
candidates examined, **none permits serving their data through this
product's own public API on any self-serve tier** — CoinGecko, CoinMarketCap
and CCData all route that to a negotiated agreement, and CCData's standard
licence forbids even displaying it (§9.6). Nothing free was found that has
both pre-2017 XLM/USD history and terms allowing commercial redistribution
(§9.5).

So the pre-2017 question is commercial before it is technical, and it is
the smallest of the three spans. Steps 2, 3 and 6 of §6 do not depend on
it and should not wait for it.

---

## 1. Project A — what is missing, measured

### 1.1 Served-tier floors per source

`trades` compresses with `source` as a segmentby column
(`migrations/0001_create_trades_hypertable.up.sql:81-90`), so every
compressed batch carries per-source min/max metadata and the floors can be
read without scanning the 154 GB hypertable. Query shape:

```sql
-- per compressed chunk of `trades`, unioned across all 258:
select source, min(_ts_meta_min_1) mn_ts, min(_ts_meta_min_2) mn_l,
       max(_ts_meta_max_1) mx_ts, max(_ts_meta_max_2) mx_l, sum(_ts_meta_count) n
from _timescaledb_internal.compress_hyper_<id>_chunk group by source
```

| source | first ts | first ledger | last ts | rows |
|---|---|---|---|---|
| kraken | 2018-07-01 00:00:25.764496Z | 0 (off-chain) | 2026-08-26 | 28,126,508 |
| soroswap | 2024-03-11 16:41:49Z | 50,746,445 | 2026-08-26 | 578,516 |
| comet | 2024-05-02 22:30:28Z | 51,500,460 | 2026-08-26 | 50,241 |
| phoenix | 2024-05-07 23:00:06Z | 51,573,544 | 2026-08-26 | 247,457 |
| aquarius | 2024-07-25 08:35:51Z | 52,728,694 | 2026-08-26 | 4,355,601 |
| **sdex** | **2026-03-12 00:00:00Z** | **61,609,957** | 2026-08-26 | **221,867,583** |
| coinbase | 2026-05-05 10:31:01Z | 0 | 2026-08-26 | 163,155,643 |
| bitstamp | 2026-05-05 10:31:03Z | 0 | 2026-08-26 | 15,748,577 |
| binance | 2026-05-05 10:31:03Z | 0 | 2026-08-26 | 286,424,892 |

Total 720,555,018 rows across the 258 compressed chunks. The figures
exclude the two uncompressed tip chunks, which is why every `last ts` reads
2026-08-26. Issue #349's table omits `bitstamp`, `comet` and `phoenix`; the
`sdex` floor it gives is confirmed exactly.

### 1.2 The gap, in ledgers, days and rows

The lake's `stellar.ledgers` carries `classic_trade_effect_count` per
ledger — a direct count of the classic trade effects each ledger produced,
which is what a served `sdex` row is one of.

```
clickhouse-client --port 9300 -q "
  select 'below_floor', count(), sum(op_count), sum(classic_trade_effect_count),
         min(close_time), max(close_time)
  from stellar.ledgers where ledger_seq < 61609957
  union all select 'at_or_above', count(), sum(op_count), sum(classic_trade_effect_count),
         min(close_time), max(close_time)
  from stellar.ledgers where ledger_seq >= 61609957"

below_floor  61609955  22268018473  2654748145  2015-09-30 16:46:54  2026-03-11 23:59:55
at_or_above   2666910   2023569598   231198281  2026-03-12 00:00:00  2026-09-04 23:40:33
```

| quantity | value |
|---|---|
| ledgers below the floor | **61,609,955** |
| days below the floor | **3,816** (2015-09-30 → 2026-03-11) |
| operations below the floor | 22,268,018,473 |
| **served `trades` rows to create** | **2,654,748,145** |

The operation count matches issue #349 exactly (22,268,018,473). The row
count does not: #349 extrapolates ~2.1B from the current yield ratio. The
measured figure is **2.65B, 26% higher**, and it is a count rather than an
extrapolation.

### 1.3 The row count is validated, not assumed

`classic_trade_effect_count` predicts served `sdex` rows one-for-one.
Measured on a bounded window:

```
-- lake
clickhouse-client --port 9300 -q "select min(close_time), max(close_time),
  sum(classic_trade_effect_count) from stellar.ledgers
  where ledger_seq between 63000000 and 63009999"
  → 2026-06-12 18:29:19  2026-06-13 10:32:40  1058858

-- served
psql -c "select count(*) from trades where source='sdex'
  and ts >= '2026-06-12 18:00' and ts <= '2026-06-13 11:00'
  and ledger between 63000000 and 63009999"
  → 1058840
```

18 rows apart on 1,058,858 — a 0.0017% delta, attributable to the ts clamp
on the served side. The 2.65B target is sound to about three decimal
places.

### 1.4 Where the work actually lands

Per-year distribution of the effects to be created
(`select toYear(close_time), count(), sum(op_count),
sum(classic_trade_effect_count) from stellar.ledgers group by 1`):

| year | ledgers | operations | classic trade effects |
|---|---|---|---|
| 2015 | 1,618,656 | 92,677 | **29** |
| 2016 | 6,730,924 | 286,456 | **135** |
| 2017 | 7,034,100 | 2,754,203 | 139,450 |
| 2018 | 6,369,420 | 281,346,206 | 7,572,344 |
| 2019 | 5,784,739 | 596,030,303 | 7,902,013 |
| 2020 | 5,802,223 | 1,174,528,638 | 6,829,765 |
| 2021 | 5,638,129 | 2,855,224,385 | 60,446,863 |
| 2022 | 5,296,202 | 4,813,352,273 | 784,931,763 |
| 2023 | 5,441,316 | 4,420,699,937 | 984,639,849 |
| 2024 | 5,362,841 | 3,893,830,476 | 444,076,130 |
| 2025 | 5,480,892 | 3,579,639,146 | 312,849,138 |
| 2026 | 3,717,427 | 2,673,806,148 | 276,559,365 |

**2022 and 2023 alone are 1.77B of the 2.65B — 67% of the whole job.**
2015 through 2017 together are 139,614 rows, which is 0.005%. Any chunking
plan that treats the years as comparable units will be wrong by three
orders of magnitude.

### 1.5 The destination is almost entirely empty

`select extract(year from range_start), count(*), min(range_end::date -
range_start::date), max(...), pg_size_pretty(sum(pg_total_relation_size(...)))
from timescaledb_information.chunks where hypertable_name='trades' group by 1`:

| year | chunks | chunk width | on disk |
|---|---|---|---|
| 2018 | 27 | 7 d | 2,592 kB |
| 2019 | 52 | 7 d | 4,992 kB |
| 2020 | 53 | 7 d | 5,088 kB |
| 2021 | 4 | 7 d | 384 kB |
| 2022 | **0** | — | — |
| 2023 | **0** | — | — |
| 2024 | 15 | 6–30 d | 650 MB |
| 2025 | 12 | 30 d | 7,332 MB |
| 2026 | 97 | 1–30 d | 97 GB |

This matters more than any other operational fact in Project A. The known
worst case for `ch-rebuild -sdex` is upserting into *populated compressed*
chunks — measured at 620 rows/s, ~47 h for a 72-day span
(`docs/operations/evidence/2026-07-30-verify-usd-volume-30d.md:82-84`), with
a ~100× improvement from decompressing first (52 min → 36 s on the same
2,000 ledgers,
`docs/operations/evidence/2026-08-04-usd-volume-rederive.md:25-32`).

The target range does not look like that. 2015-2017 and 2022-2023 have **no
chunks at all** — those inserts create fresh, uncompressed chunks. 2018-2021
holds 13 MB across 136 seven-day chunks, trivial to decompress. Only 2024
through 2026-03 has meaningful resident compressed data, ~8 GB. So the
compressed-chunk penalty applies to under 10% of the span by row count.

`ch-rebuild` already has the right mode for this: `-sdex-gaps` restricts to
served-empty ranges and does a pure insert with no `ON CONFLICT` walk over
resident rows (`internal/ops/chops/ch_rebuild.go:237-239`).

---

## 2. Project A — mechanism and cost

### 2.1 The tool, and why it is not the other two

| tool | reads | writes | verdict for this job |
|---|---|---|---|
| `ch-backfill` | galexie/MinIO ledger meta | ClickHouse lake | **Not this job.** The lake is already complete. It also has no cold-tier fallback (`internal/ops/opsutil/opsutil.go:350-365` never sets `ColdDataStore`), so below the 49,984,000 hot floor it cannot read at all without a rehydrate. |
| `projector-replay` | rewinds a cursor; the indexer does the work | per-source Postgres tables | **Cannot touch this.** `sdex` is explicitly not a projected source — `internal/pipeline/sink.go:583` names `sdex.TradeEvent` in `IsProjectedEvent`'s default arm, out of scope per ADR-0032. |
| **`ch-rebuild -sdex -write`** | `stellar.operations` + `operation_results` + `transactions` | served `trades` | **This one.** The decision rule is stated at `docs/architecture/ingest-pipeline.md:136-142`: non-projected lake re-derive → `ch-rebuild`. |

The bucket trap recorded in prior operational memory — `ch-backfill`
defaulting to the LIVE bucket, which does not hold historic ranges — is
real (`internal/ops/chops/ch_backfill.go:36`,
`internal/ops/opsutil/opsutil.go:285-286`) but is not on this path.
`ch-rebuild` reads ClickHouse and takes no bucket flag.

### 2.2 Invocation

```
run-heavy-job.sh sdex-hist-<window> \
  stellarindex-ops ch-rebuild -config /etc/stellarindex.toml \
    -sdex-gaps -from <lo> -to <hi> -write
```

Two hard constraints from the record:

- **Windows of ≤50,000 ledgers.** The SDEX op read OOMs the 10 GiB client
  pin above that (`docs/operations/usd-volume-rederive-2026-08.md:135-136`).
  61,609,955 ledgers is **1,233 windows**.
- **A unique job name per attempt.** `run-heavy-job.sh` skips a name whose
  lock is still held, silently and with exit 0
  (`docs/operations/galexie-backfill.md:78-86`).

`ch-rebuild` stamps `derive_generation` from the wall clock
(`ch_rebuild.go:283`) so the rebuild wins the upsert, and mandates
`timescale.InstallUSDVolumeResolution` (`ch_rebuild.go:293-299`) so
`usd_volume` is populated rather than NULLed.

### 2.3 Wall clock

Two production runs of `ch-rebuild -sdex -write` are on the record, and
neither touched the historical era. Worse, they can be projected two ways —
per row written, or per 50k-ledger window — and **the two framings
disagree by a factor of two.** Both are given, because the disagreement is
itself the finding.

| anchor | per row | per window |
|---|---|---|
| upsert into populated compressed chunks — 47 h / ~105M rows / ~22 windows (`evidence/2026-07-30-verify-usd-volume-30d.md:82-84`) | 620 rows/s → **49.6 d** | ~2.1 h/window → **108 d** |
| decompress-first — 1h44m / ~21.7M rows / 5 windows (`evidence/2026-08-04-usd-volume-rederive.md:25-32`) | ≈3,474 rows/s → **8.8 d** | ~21 min/window → **18 d** |

At 1,233 windows (§2.2), the range is **9 to 108 days.**

The two framings measure different bottlenecks and both are real:

- **Per row** tracks the Postgres write. §1.5 says most of the destination
  is empty, so most inserts take the fresh-chunk path, not the
  compressed-upsert path — which argues for the fast end. Historical
  density also helps: 43.1 trade effects per ledger below the floor versus
  86.7 above it (§1.2), so a historical window writes about half the rows
  of a 2026 one.
- **Per window** tracks the ClickHouse read, which scales with *operations
  scanned*, not rows produced — and there density does **not** help. 2022
  ran 908 operations per ledger against 2026's 759 (§1.4). The grace-hash
  join over `operations` × `operation_results` costs the same whether or
  not the window yields many trades.

So the write should be faster than the anchors suggest and the read should
not be. **The honest expectation is the middle of the band, around 20-40
days, and no number in this section should be committed to.** Step 5 in §6
exists to replace all of it with one measured 2022 window.

### 2.4 Storage

Two independent measurements of bytes per served `trades` row:

```sql
-- (a) the SDEX-only era, 2026-03-12 .. 2026-05-05, before CEX ingest began
--     54 chunks, compressed internal relations
select count(*), sum(pg_total_relation_size(format('%I.%I',s,t)::regclass))
  from <compressed chunk relations for that range>
  → 54 chunks, 6,468,665,344 bytes
-- rows in the same 54 chunks, from compressed-batch metadata:
--   sdex 57,750,308 | aquarius 401,143 | soroswap 28,852 | phoenix 8,721 | comet 1,951
--   = 58,190,975 rows  →  111.2 bytes/row

-- (b) the whole hypertable, all-in
select * from hypertable_detailed_size('trades');
  → table 22,239,952,896 | index 96,588,349,440 | toast 35,757,416,448
    | total 154,585,718,784
-- 154,585,718,784 / 720,555,018 rows = 214.5 bytes/row
```

| basis | bytes/row | 2,654,748,145 rows |
|---|---|---|
| compressed payload, SDEX-dominated era | 111.2 | **295 GB** |
| whole hypertable, all-in | 214.5 | **569 GB** |

Issue #349's 340–500 GB estimate is corroborated, but it was reached from a
row count 26% low; the same per-row assumption applied to the measured
2.65B lands at or above the top of that band. **Plan for 300–570 GB.**

Headroom, measured:

```
zfs list -o name,used,avail
  data              13.6T   4.71T
  data/postgres      763G   4.71T
  data/clickhouse    9.28T  4.71T
  data/minio         2.57T  4.71T
zpool list → data 27.7T size, 20.5T alloc, 7.26T free
```

The planning figure is the ZFS `AVAIL` of **4.71 TiB**, not the zpool
`FREE` of 7.26 TiB (which does not account for parity). 570 GB is 12% of
available. Issue #349's "7.63 TB free" is stale and reads against the wrong
column.

Because §0.1 removes the archive fill, this job's whole storage cost is the
Postgres rows. The pgBackRest and off-site consequences #349 raises remain
real and are unchanged in kind — `data/pgbackrest` is already 1,014 GB —
but they now attach to 300-570 GB of new rows rather than to that plus
2.3 TiB of rehydrated ledger meta.

### 2.5 After the rows land

Every trades-rooted continuous aggregate must be re-materialised over
3,816 new days. The policies cannot do it: `prices_1d` has
`start_offset => '7 days'` and the widest of the seven, `prices_1mo`, looks
back three months (`migrations/0147_ohlc_deterministic_tiebreak.up.sql:141-150`).

Twelve aggregates are rooted on `trades`, in this order — `prices_1m` must
lead because `twap_1h`/`twap_1d` are built on it:

`prices_1m` → `prices_15m` → `prices_1h` → `prices_4h` → `prices_1d` →
`prices_1w` → `prices_1mo` → `dex_volume_by_pair_1d` → `source_volume_1h` →
`pools_per_source_1h` → `twap_1h` → `twap_1d`

Only the seven `prices_*` are reachable from Go
(`internal/storage/timescale/diagnostics.go:112-120`, `allowedCAGGViews`);
the other five are psql-only.
`docs/operations/cagg-broad-recompute.md` covers eight of the twelve and
omits `dex_volume_by_pair_1d`, `source_volume_1h`, `twap_1h`, `twap_1d` —
that runbook needs those four added before it is used here.

Its 4–8 hour full-sweep estimate is for today's data volume and will not
survive a 4.7× increase in `trades`. See §7.

---

## 3. Project B — what exists

### 3.1 XLM's identities are disjoint and stay that way

`internal/canonical/alias.go:33-38` defines the family in canonical
priority order, and the SAC form is deliberately last
(`alias.go:277-289`) so a thin Soroban pool can never become the served XLM
price:

| DB string | venue population |
|---|---|
| `native` | SDEX / classic on-chain |
| `crypto:XLM` | every CEX — kraken, binance, coinbase, bitstamp |
| `CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA` | Soroban AMMs |

Measured in `prices_1d`, which confirms they are populated from different
places and different dates:

```sql
select base_asset, count(*), min(bucket)::date, max(bucket)::date
  from prices_1d where base_asset ilike '%XLM%' or base_asset='native'
  group by 1 order by 2 desc;
```

| base_asset | rows | from | to |
|---|---|---|---|
| `native` | 451,861 | 2026-03-12 | 2026-09-03 |
| `crypto:XLM` | 1,452 | **2018-07-01** | 2026-09-03 |

`native` begins on exactly the SDEX served floor. Everything else the
pattern matched — `XLMGOLD-…`, `100XLM-…`, `XLMFISH-…`, `yXLM-…` and 20
more — are separate issuers' tokens, not XLM, and are excluded here.

### 3.2 The hole, exactly

```sql
select quote_asset, count(*), min(bucket)::date, max(bucket)::date
  from prices_1d where base_asset='crypto:XLM' group by 1;
  → fiat:USD 1068  2018-07-01 .. 2026-09-03
    fiat:GBP 122 | crypto:USDT 122 | fiat:EUR 122 | crypto:BTC 18  (all 2026-05-05+)

with d as (select bucket::date b, lead(bucket::date) over (order by bucket) nb
           from prices_1d where base_asset='crypto:XLM' and quote_asset='fiat:USD')
select b, nb, nb-b-1 from d where nb-b > 1 order by 3 desc limit 5;
  → 2021-01-31 | 2026-05-05 | 1919
```

| span | days | state |
|---|---|---|
| 2015-09-30 → 2018-06-30 | 1,004 | nothing |
| 2018-07-01 → 2021-01-31 | 946 | held, contiguous |
| **2021-02-01 → 2026-05-04** | **1,919** | **hole** |
| 2026-05-05 → 2026-09-03 | 122 | held |

One gap, exactly 1,919 days, with no interior fragmentation on either side.

This is what a caller sees today:

```
GET /v1/history/since-inception?asset=crypto:XLM&quote=fiat:USD

price_type: vwap | granularity: 1d | point keys: ['t','p','v_usd']
points: 1068 from 2018-07-01 to 2026-09-03
GAP: 2021-01-31 -> 2026-05-05 = 1919 missing days
```

1,068 points presented as one continuous series, `price_type: "vwap"`
throughout, with a five-year discontinuity between two adjacent array
elements and nothing on the wire marking it. A client charting "all time"
draws a straight line across it. This is the defect, and §8.2 is why the
same surface must not be allowed to absorb vendor data silently.

The brief's figures of 1,865 days and a 2026-03-12 resumption are close but
name a different object: 2026-03-12 is where `native` starts, not where
`crypto:XLM/fiat:USD` resumes. For the flagship CEX series the resumption
is 2026-05-05, the day Binance/Coinbase/Bitstamp ingest was enabled on r1.

### 3.3 It is missing trades, not missing materialisation

Decisive, from the same compressed-batch metadata technique as §1.1,
filtered to `base_asset='crypto:XLM'`:

| source | quote | year | first ts | last ts | rows |
|---|---|---|---|---|---|
| kraken | fiat:USD | 2018 | 2018-07-01 00:00:25Z | 2019-01-02 | 308,468 |
| kraken | fiat:USD | 2019 | 2019-01-03 | 2020-01-01 | 360,943 |
| kraken | fiat:USD | 2020 | 2020-01-02 | 2021-01-06 | 618,170 |
| kraken | fiat:USD | 2021 | 2021-01-07 | **2021-01-31 23:59:59.735541Z** | 251,130 |
| coinbase | fiat:USD | 2026 | 2026-05-05 | 2026-08-26 | 7,401,199 |
| binance | crypto:USDT | 2026 | 2026-05-05 | 2026-08-26 | 5,826,371 |
| kraken | fiat:USD | 2026 | 2026-05-05 | 2026-08-26 | 1,041,933 |
| bitstamp | fiat:USD | 2026 | 2026-05-05 | 2026-08-26 | 588,638 |

Nothing between 2021-01-31 23:59:59 and 2026-05-05 10:31. Corroborated
structurally: the `trades` hypertable has **zero chunks in 2022 and 2023**
(§1.5). The aggregate is faithfully reporting an empty source.

`prices_1d`'s `sources` column shows the same shape from the other side:

```sql
select sources, count(*), min(bucket)::date, max(bucket)::date from prices_1d
  where base_asset='crypto:XLM' and quote_asset='fiat:USD' group by 1;
  → {kraken}                     946  2018-07-01 .. 2021-01-31
    {bitstamp,coinbase,kraken}   122  2026-05-05 .. 2026-09-03
```

The repair is an ingest, then a refresh. A refresh alone is a no-op.

### 3.4 Why the run stopped is not recoverable from the data

The 2018-2021 span exists because a Kraken `/0/public/Trades` walk
completed; the golden fixture for that code path is a captured 2018 frame
at `since=2018-07-01`
(`internal/sources/external/kraken/backfill_trades_test.go:20,48`). Whether
the 2021+ window was never requested, was requested and failed, or was
lost, is not determinable from anything on the box. The prior audit reached
the same dead end (`docs/audit-2026-07-03-rfp-compliance/README.md:150-152`).

One mechanism in the tool makes "requested and silently lost" plausible and
is worth fixing regardless — see §5.

---

## 4. Project B — the on-chain floor

The most important number here is the earliest date at which an on-chain,
USD-quoted XLM price is defensible from the data already held. Everything
earlier is
the external span.

### 4.1 No USD-pegged asset existed before 2016-10-18

`classic_assets` is a complete registry — its earliest first-seen is ledger
864,344 / 2015-11-18, and it holds 198,923 assets.

```sql
select code, count(*), min(first_seen_at)::date
  from classic_assets where code ~ '^(USD|USDC|USDT|USDx|USDV|USDS)$'
  group by 1 order by 3;
```

| code | assets | earliest first seen |
|---|---|---|
| USD | 165 | **2016-10-18** |
| USDT | 195 | 2018-01-09 |
| USDC | 220 | 2019-04-20 |
| USDV | 11 | 2021-03-05 |
| USDS | 13 | 2021-04-13 |
| USDx | 8 | 2021-07-05 |

The named anchors:

| asset | first seen | ledger | observations |
|---|---|---|---|
| `USD-GBUYUAI75XXWDZEKLY66CFYKQPET5JR4EENXZBUZ3YXZ7DS56Z4OKOFU` | 2016-10-18 | 7,004,658 | 1,798 |
| `USD-GDUKMGUGDZQK6YHYA5Z6AY2G4XDSZPSZ3SW5UN3ARVMO6QSRDWP5YLEX` (AnchorUSD) | 2018-08-26 | 19,654,279 | 68,268 |
| `USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN` (Centre) | **2021-02-26** | 34,180,766 | 41,706,192 |

**Nothing USD-denominated existed on Stellar before 2016-10-18.** An
on-chain XLM/USD price for 2015-09-30 → 2016-10-17 is not thin, it is
impossible.

### 4.2 And there was nothing to price it with

From §1.4: the entire Stellar network produced **29** classic trade effects
in 2015, **135** in 2016, and 139,450 in 2017 — across every pair, not just
XLM. A daily VWAP is not a meaningful object over 135 network-wide trades
in a year.

### 4.3 When USD markets actually became active on chain

`operation_results.result_xdr` is base64 XDR, so the asset issuer can be
matched by decoding and searching for the raw 32-byte key. Validated
against a known-good window first (ledgers 63,000,000-63,002,000:
4,324,620 ops, 249,114 Centre-USDC hits), then run over 100,000-ledger
windows sampled once per 1M-ledger partition:

```
clickhouse-client --port 9300 -q "
  select intDiv(ledger_seq,1000000) part, count() ops,
    countIf(position(base64Decode(result_xdr), unhex('E8A6…9FDC'))>0) anchorusd,
    countIf(position(base64Decode(result_xdr), unhex('3B99…15C5'))>0) usdc,
    countIf(position(base64Decode(result_xdr), unhex('698A…78E5'))>0) usd2016
  from stellar.operation_results where <sparse windows> group by part order by part"
```

| partition (≈date) | ops sampled | `USD-GBUY…` | AnchorUSD | Centre USDC |
|---|---|---|---|---|
| 7 (2016-10) | 2,036 | 823 | 0 | 0 |
| 17 (2018-04) | 2,355,564 | 1,550 | 0 | 0 |
| **20 (2018-10-28)** | 8,238,379 | 2,299 | **268,531** | 0 |
| 30 (2020-07) | 19,776,429 | 3,019 | 283,640 | 0 |
| 33 (2021-01) | 19,547,186 | 15,411 | 652,924 | 0 |
| **34 (2021-02)** | 49,676,140 | 4,173 | 186,242 | **790,274** |

A hit means the asset appeared in an operation *result* — an offer entry or
a claim atom. It is a necessary condition for a trade in that asset, not
proof of one, and it does not establish the counter-asset. Read as a
bracket:

- **2016-10** — first USD-pegged SDEX activity of any kind, at a few
  hundred ops per 100k-ledger window, against a network doing ~135 trades a
  year. Not priceable.
- **2018-10-28** — AnchorUSD becomes materially active; 2018 is also the
  first year with a real trade count (7.57M network-wide).
- **2021-02-26** — Centre USDC arrives with immediate depth.

### 4.4 What the served tier can reach today

`internal/storage/timescale/usd_fx_resolver.go:822-838` records the
measurement taken on r1 2026-09-03: on-chain peg-quoted XLM markets bottom
out at **2024-03-12** (SAC × SAC USDC, 291,883 rows) and 2026-03-12
(`native` × classic USDC, 215,790 rows). Every other combination is zero.

So the answer to "what is the earliest defensible on-chain XLM/USD price":

| method | earliest defensible | status |
|---|---|---|
| direct on-chain USD-pegged market, served tier today | 2024-03-12 | held |
| direct on-chain USD-pegged market, after Project A | ~2021-02-26 (USDC depth); arguably ~2018-10 via AnchorUSD, thin | unlocked by Project A |
| on-chain, before 2016-10-18 | — | **impossible; no USD asset existed** |
| CEX (`crypto:XLM`), Kraken's real floor | **2017-01-17** | reachable now, not ingested |

**"To genesis" cannot honestly mean an on-chain USD price.** The defensible
statement is: on-chain XLM/USD begins in 2018 at the earliest and is only
solid from 2021-02; 2015-09-30 → 2017-01-16 has neither an on-chain USD
market nor any reachable CEX series, and is external-only.

Note also that ⚠️ **Project A is a prerequisite for pinning the on-chain
floor exactly.** Attributing a trade to the XLM/USD *pair* requires the
decoded claim atoms, which is what `ch-rebuild -sdex` produces. The bracket
in §4.3 is as precise as this can get without it.

---

## 5. Project B — mechanism and cost

### 5.1 Venue depth, measured 2026-09-05

Probed directly, not taken from the code comments:

| venue | endpoint | measured earliest XLM/USD | repo comment says |
|---|---|---|---|
| **Kraken** | `/0/public/Trades?pair=XLMUSD&since=0` | **2017-01-17T20:17:52Z** | full history (`kraken/backfill_trades.go:20-34`) — correct |
| Coinbase | `/products/XLM-USD/candles` | empty 2019-01-01, populated 2019-06-01 | "since 2019" (`coinbase/backfill.go:40-42`) — roughly right |
| Bitstamp | `/api/v2/ohlc/xlmusd/` | empty 2020-01-01, populated 2020-06-01; first bar **2020-06-17** | ⚠️ "XLMUSD to 2017" (`bitstamp/backfill.go:35-38`) — **wrong by three years** |

Both empty responses were taken with call shapes validated against recent
windows in the same session, so they are absences, not malformed requests.
The Bitstamp floor is corroborated exactly: the first `xlmusd` bar the
endpoint serves is timestamp 1592352000 = **2020-06-17**, which matches
Bitstamp's own launch announcement for the pair. Their endpoint is not
window-truncated — `btcusd` serves 2014 — so this is a listing date, not a
retention limit.

**Kraken is the only free source that reaches below 2019, and it reaches
2017-01-17.** The Bitstamp comment recommending it over Kraken for
multi-year backfill is backwards and should not be relied on.

### 5.2 The tool

`stellarindex-ops backfill-external` (`internal/ops/ingest/backfill_external.go`):

```
-config PATH -source kraken -pair XLM/USD
-from RFC3339 -to RFC3339 -raw-trades -write
```

`-raw-trades` selects `/0/public/Trades` rather than `/OHLC`, which serves
only the most recent 720 intervals. Pagination is 1,000 fills per page
paced at 1,100 ms (`kraken/backfill_trades.go:27-33`).

It is undocumented — there is no runbook for it anywhere under `docs/` —
and it has three properties that matter at this scale (§5.4).

### 5.3 Cost

Fill density inside the hole was measured directly rather than
extrapolated. Each probe requests 1,000 consecutive fills from a date and
reads the span they cover, which gives fills/day at that instant:

```
GET /0/public/Trades?pair=XLMUSD&since=<date>&count=1000
   → (date, fills returned, seconds spanned, implied fills/day)
```

| sample date | seconds for 1,000 fills | fills/day |
|---|---|---|
| 2021-06-01 | 6,852 | **12,610** |
| 2022-06-01 | 58,948 | 1,466 |
| 2023-06-01 | 133,598 | 647 |
| 2024-06-01 | 231,124 | 374 |
| 2025-06-01 | 36,708 | 2,354 |
| 2026-02-01 | 12,846 | 6,726 |

These are single-day point samples, not annual means, and 2021-06 sits at
the cycle peak so applying it across 2021 leans high. Weighting each
calendar segment of the hole by its sample:

| segment | days | fills/day | fills |
|---|---|---|---|
| 2021-02-01 → 2021-12-31 | 334 | 12,610 | 4.21M |
| 2022 | 365 | 1,466 | 0.54M |
| 2023 | 365 | 647 | 0.24M |
| 2024 | 366 | 374 | 0.14M |
| 2025 | 365 | 2,354 | 0.86M |
| 2026-01-01 → 2026-05-04 | 124 | 6,726 | 0.83M |
| **total** | **1,919** | — | **≈6.8M** |

| quantity | value |
|---|---|
| **venue fetch**, 1,000 fills / 1.1 s = 909 fills/s | **≈2.1 h** |
| **insert**, per-row `InsertTrade` at ~520 rows/s (`CHANGELOG.md:10641`) | **≈3.6 h** |
| CAGG refresh over 1,919 days | not measured — §7 |

Pagination cost is proportional to *fills*, not days: a page returns up to
1,000 fills whatever span they cover, so the sparse 2022-2024 years — three
quarters of the calendar — are under a seventh of the work.

The write, not the venue, is the long pole, because
`insertBackfilledTrades` loops `store.InsertTrade` one row at a time
(`backfill_external.go`, insert loop) while `BatchInsertTrades` exists and
is what `ch-rebuild` uses. Batching that loop is the single highest-value
follow-up and is **not** included in this change.

Extending down to Kraken's real floor adds 530 days (2017-01-17 →
2018-06-30). Density there is below the 2018 H2 figure of 1,676/day
already held (§3.3), so under 0.9M further fills — well under an hour of
fetch.

**Project B is hours, not days.** It is the cheaper of the two projects by
two orders of magnitude and closes the more visible defect.

### 5.4 What makes a multi-hour walk unsafe, and what this change fixes

Three properties of `backfill-external`, all confirmed at HEAD:

1. **The whole walk is buffered in RAM** before any insert —
   `BackfillTrades` accumulates `out []canonical.Trade` and returns it
   whole. ~6.8M structs for the hole. Not fixed here; mitigated by slicing.
2. **One context covered both the fetch and the write.** The raw-trades
   budget is 24 h, and the same context was passed to `timescale.Open` and
   the insert loop. A walk that used its budget had nothing left to write
   with.
3. **A context expiry discarded every fetched fill.** `BackfillTrades`
   returns `(out, ctx.Err())`; the caller did
   `if err != nil { return fmt.Errorf("backfill: %w", err) }`, dropping a
   populated slice on the floor. Hours of rate-limited pagination, gone,
   with no resume cursor printed.

Property 3 is a plausible mechanism for §3.4's unexplained stop.

**This plan ships a fix for 2 and 3** (`internal/ops/ingest/backfill_external.go`):

- The venue walk and the database write get separate budgets
  (`externalInsertBudget`), so an expired walk context no longer poisons
  the write.
- `partialFetchResume` recognises a context expiry that still returned
  fills, and yields the high-water venue timestamp as a resume cursor.
  Inserts are idempotent (`ON CONFLICT`), so re-running from it costs at
  most one duplicate page. Any other error is a real fault and is **not**
  salvaged.
- `partialWalkError` makes a truncated range exit non-zero regardless of
  how the writes went, and prints `-from <cursor>` — so a chunked walk can
  be scripted and a truncated slice can never be mistaken for a complete
  one.

### 5.5 Operating it

Slice by quarter — 21 slices over the hole, ~460k fills each:

```
run-heavy-job.sh xlm-usd-<q> \
  stellarindex-ops backfill-external -config /etc/stellarindex.toml \
    -source kraken -pair XLM/USD -raw-trades \
    -from 2021-02-01T00:00:00Z -to 2021-04-01T00:00:00Z -write
```

`backfill-external` does **not** refresh any aggregate — it inserts and
exits. After each slice, or once at the end, run the twelve-view refresh in
the §2.5 order, in weekly or monthly windows, and never concurrently with
another backfill (Timescale rejects the loser with `55P03` and the
backfill's ~3.0 s retry budget is exhausted,
`docs/operations/backfill-procedure.md:534-546`).

---

## 6. Recommended sequence

Ordered by value per unit of risk, not by project number.

| # | step | cost | unlocks |
|---|---|---|---|
| 1 | Land the `backfill-external` salvage fix (§5.4) | done, in this change | Makes every step below safe to run unattended |
| 2 | Kraken `-raw-trades` over **2021-02-01 → 2026-05-04**, 21 quarterly slices, refresh after each | ≈6 h + refresh (§5.3) | Closes the 1,919-day hole on the flagship pair — the worst discoverable defect |
| 3 | Kraken `-raw-trades` over **2017-01-17 → 2018-06-30** | <1 h + refresh | Moves the CEX floor 530 days below where it has ever been; no new vendor, but see §9.1 on Kraken's terms |
| 4 | Batch the `insertBackfilledTrades` loop onto `BatchInsertTrades` | small change | Removes the long pole from steps 2-3 and every future CEX backfill |
| 5 | **Project A chunk 1**: `ch-rebuild -sdex-gaps` over one 50k-ledger window in **2022**, timed | ~1 h | The only honest input to the §2.3 range. Do not commit to the rest without it |
| 6 | Project A, reverse-chronological: 2024→2026-03, then 2021→2024, then genesis→2021 | 9-108 days, unresolved (§2.3) | Full on-chain trade history; pins the on-chain XLM/USD floor exactly (§4.4) |
| 7 | Provenance work (§8) before any vendor data lands | — | A vendor bar must never be indistinguishable from an on-chain VWAP |
| 8 | External span 2015-09-30 → 2017-01-16 (475 days) | blocked — needs a negotiated licence (§9.6) plus §8's provenance work | The only remaining gap |

Steps 2 and 3 are independent of everything in Project A and should not
wait for it. Step 8 is last because it is the only step that cannot be
started at all on this plan's own authority: every vendor examined routes
serving their data through this product's API to a negotiated agreement
(§9.6), and that is a commercial decision, not an engineering one. Step 5 is deliberately in 2022 rather than in a sparse year:
2022-2023 is 67% of Project A (§1.4), so a timing taken on 2016 would be
meaningless.

Issue #349's recommended shape — reverse-chronological chunks, measure
chunk 1 first, never one job — is retained. Its "fill-then-re-trim per
chunk so the pool never surrenders a third of its headroom" step is
**dropped**, because §0.1 removes the archive fill entirely.

---

## 7. What could not be determined

Stated plainly rather than estimated.

1. **`ch-rebuild -sdex` throughput on the historical era.** Both anchors in
   §2.3 were measured on 2026 data, and projecting them per row versus per
   window gives 9-50 days and 18-108 days respectively — a 2× disagreement
   that cannot be resolved from anything on the box, because the two
   framings track different bottlenecks (Postgres write versus ClickHouse
   read) and the historical era moves them in opposite directions. 2022-2023
   is 67% of the job and has never been walked. Step 5 in §6 exists to
   replace the whole band with one measurement.

2. **CAGG re-materialisation cost over 3,816 days.** The runbook's 4-8 hour
   sweep (`docs/operations/cagg-broad-recompute.md:118-130`) is for current
   volume. `trades` would grow 4.7× and `prices_1m` accrues ~390k rows/day
   at today's density
   (`docs/operations/backfill-procedure.md:530-533`). No historical-density
   figure exists. This could plausibly rival the insert cost and is
   unbudgeted.

3. **Exact fill count in the 1,919-day hole.** §5.3 measures density at
   six single days and weights the calendar by them, giving ≈6.8M. Those
   are point samples, not annual means: 2021-06 sits at the cycle peak and
   is applied to 334 days, so the figure leans high and could be off by a
   factor of two either way. The exact count is one full walk away, which
   is the job itself.

4. **Whether an XLM ↔ USD-pegged *pair* traded on chain before 2021-02.**
   §4.3 brackets asset-level SDEX activity, which is a necessary but not
   sufficient condition. Pair-level attribution needs the decoded claim
   atoms — i.e. Project A. Recorded as a dependency, not guessed.

5. **Why the Kraken series stopped at 2021-01-31.** Not recoverable from
   the box (§3.4). §5.4 identifies a mechanism that would produce exactly
   this signature, which is a hypothesis, not a finding.

6. **The `trades` storage accounting does not fully reconcile.**
   `hypertable_detailed_size` reports 96.6 GB of index against 35.8 GB of
   compressed payload, and per-chunk `pg_total_relation_size` over the 258
   compressed chunks sums to 60 GB against 42 GB of compressed internal
   relations. The residue is index and uncompressed remnant that was not
   attributed. §2.4 therefore quotes a 111-215 B/row band from two
   independent measurements rather than a single number.

7. **Coinbase's true listing date.** §5.1 bounds it to 2019-01-01 →
   2019-06-01 by bisection and it was not narrowed further, because it is
   not on the recommended path. Bitstamp's was pinned exactly (2020-06-17).

8. **Vendor terms — the items that stayed NOT FOUND** (§9.6). Each is a gap
   in the record, not an inference:
   - CoinMarketCap's operative API Commercial Terms of Use
     (`pro.coinmarketcap.com/user-agreement-commercial/`) returned only
     marketing markup on every attempt. **The actual contract is unread**;
     the "one product / ≤100k users" reading comes from the pricing-page
     FAQ, not the agreement.
   - CoinGecko's exact earliest XLM row. 2015-03-04 is proven from their
     published all-time-low date; the historical endpoints return 401
     without a key, so "starting from 2014" stays a vendor claim.
   - CCData's XLM-specific floor **and all pricing** — both
     subscription-gated; their free tier was retired 2026-05-21. No
     dollar figure for that vendor exists in this document.
   - Whether CoinMarketCap's full-history daily OHLCV starts at Startup
     ($79) or Professional ($699).
   - CoinMarketCap's attribution wording — required in practice, no clause
     text located.
   - CoinGecko's clause numbering. The wording was stable across repeated
     reads; the numbers (4.1.5 vs 4.1.6, 4.3 vs 4.4) were not, so cite the
     text rather than the number.
   - Terms for Poloniex, Bitstamp, CoinAPI and Stellar Expert — all
     single-page-app rendered or 403 (§9.3, §9.5).

9. **Whether Kraken's bulk OHLCVT dumps are actually offered.** Two Kraken
   pages contradict each other (§9.1) and no licence statement was found on
   either dump page.

---

## 8. Provenance — the requirement any external data must meet first

An externally-sourced daily close is not the same object as an on-chain or
CEX-fill VWAP, and must never be silently merged into one series. This is
the surface class this codebase has been burned by before: a verdict that
reads identical whether it covers everything or a narrow slice.

**The mechanism already exists and should be extended, not duplicated.**

### 8.1 `sources` is already the carrier

`prices_1d` carries `array_agg(DISTINCT source) AS sources`
(`migrations/0147_ohlc_deterministic_tiebreak.up.sql:146,158`), and it is
already discriminating correctly across the hole:

```
{kraken}                    946 rows  2018-07-01 .. 2021-01-31
{bitstamp,coinbase,kraken}  122 rows  2026-05-05 .. 2026-09-03
```

`/v1/price` already surfaces it as a sibling of `data`, alongside
`price_type`:

```json
{"data":{"asset_id":"native","quote":"fiat:USD","price":"0.1794…",
         "price_type":"vwap","confidence":0.5,…},
 "sources":["coinbase","kraken"],
 "flags":{"stale":false,"triangulated":false,…}}
```

### 8.2 The gap: `/v1/ohlc` does not expose it per bar

Measured on the same box:

```
GET /v1/ohlc?base=crypto:XLM&quote=fiat:USD&interval=1d&limit=2
{"data":{…,"intervals":[{"t":…,"o":…,"h":…,"l":…,"c":…,"v_base":…,
                         "v_quote":…,"n":117961}, …]},
 "flags":{"stale":false,"triangulated":true,…}}
```

Each interval carries OHLCV and a trade count. It carries **no `sources`**,
and the only provenance on the response is a single top-level `flags`
object describing the request, not the bar.

`/v1/history/since-inception` is the same story and is the more exposed
surface, because the gap is the first thing it returns: its points carry
`t`, `p`, `v_usd` and a single response-level `price_type: "vwap"` (§3.2).

A caller charting 2015→2026 would receive vendor bars and fill-derived bars
in one array, under one `price_type`, with nothing to tell them apart.

**The required change, before any vendor row is written:** add `sources` to
each element of `intervals`, populated from the column `prices_1d` already
computes. It is a wire addition, backed by existing storage, extending the
mechanism `/v1/price` already uses. No new concept is introduced.

A vendor series should enter as its own `source` name in the existing
registry (`internal/sources/external/registry.go`) with
`IncludeInVWAP: false` — the same posture the CoinGecko/CoinMarketCap/
CryptoCompare reference adapters already hold, where they feed
`oracle_updates` and are never used as a price. A vendor daily close is a
*different kind* of observation from a fill-derived VWAP, and
`price_type` is where that distinction belongs on the wire.

### 8.3 The coverage verdict must not absorb it

`/v1/coverage` today reports 20 of 20 sources complete on the lake axis,
and for `sdex` reports:

```json
{"source":"sdex","complete":true,"lake_complete":true,"genesis_ledger":2,
 "coverage_pct":1,
 "detail":"substrate: verified [2,64265159] — contiguous + hash-chained
   from this source's genesis; projection: verified [64249915,64265159];
   [61609957,64249914] carried from the prior clean verdict …"}
```

The two axes are separated correctly in `detail` and the substrate claim is
true. But the headline fields a consumer reads first —
`complete: true`, `genesis_ledger: 2`, `coverage_pct: 1` — describe the
lake, while served `trades` for `sdex` begin at 61,609,957. The projection
axis's own floor is recorded as 63,636,711 in `completeness_target_floors`,
a full 2.0M ledgers above the data floor.

Two consequences for this plan:

- ADR-0033 completeness has no notion of CEX coverage at all — the
  `crypto:XLM` series is outside the model entirely, which is why a
  1,919-day hole on the flagship pair passed 20/20.
- Any vendor-sourced span must be excluded from `lake_complete` and from
  `projection_ok` by construction. It is neither. It needs its own axis or
  its own verdict, and reusing either existing one would make the coverage
  surface assert something false.

### 8.4 Vendors — findings only

*Reported as findings for the maintainer's decision. Nothing here is a
recommendation to accept any terms, and nothing was signed up to.*

The product would display and serve historical XLM/USD bars through a
public API — that is redistribution, not internal use, and it is the term
that governs whether a vendor is usable at all, independent of data quality
or price.

**Dependency already on the books:** a CoinGecko Pro purchase is pending
and `COINGECKO_API_KEY` is set nowhere on r1; the feed has been dead since
2026-06-19 (`docs/operations/v1-launch-plan.md:3481`, with the purchase
tracked at `:4141` and `:4211`). If CoinGecko is the answer, that pending
purchase becomes this plan's blocking dependency.

*Vendor comparison — depth, granularity, pricing and the redistribution
clauses — is recorded in §9.*

**The cheapest option should be exhausted first.** Steps 2 and 3 in §6 use
Kraken's own endpoint, cover 2017-01-17 onward, add no new counterparty,
and cost hours. They are not, however, free of terms — Kraken's Global
Terms restrict use of their content, and the 946 days already served were
taken the same way (§9.1). That is a pre-existing exposure this plan would
extend, not one it creates, and it belongs in the same decision. Without them a vendor would have to cover **2,923 days** — the
1,004-day pre-2018 span plus the 1,919-day hole. With them the vendor
question shrinks to **475 days** before 2017-01-17: a span in which, per
§4.1-4.2, no on-chain USD price existed at all and a vendor number would be
the only number available. That is a much smaller thing to buy, and a much
easier thing to label honestly.

---

## 9. Sources for the pre-2017 span — findings

All probes and clause reads below are dated **2026-09-05**. This section
reports what each source holds and what its terms say. It contains no
recommendation, and nothing was agreed to.

### 9.1 The exchange route already in use is not term-free

Steps 2 and 3 of §6 read Kraken's public endpoints. Kraken's Global Terms
(updated 2026-09-04) restrict that:

> §9 — "use any web scraping, web harvesting, or data extraction methods to
> extract any data from Our Content"; "develop any third-party applications
> that interact with Our Content without our prior written consent"
>
> §8 — "You do not have or acquire any rights to Our Content beyond the
> limited, revocable permission in the previous sentence."

Two things follow, and both are for the maintainer rather than for this
plan to settle:

- The 946 days of Kraken fills already served (§3.2) were obtained the same
  way. This is a **pre-existing** position, not one steps 2-3 would create.
  They would extend it by 2,449 days.
- Kraken also publishes bulk OHLCVT dumps covering each market "from the
  beginning of each market up to the present"
  (support article, updated 2026-04-26), which would avoid the scraping
  clause specifically. ⚠️ A second Kraken page states the opposite —
  "Kraken does not provide a bulk historical data dump or websocket replay
  service" — so the two disagree and neither was reconciled. **No licence
  statement was found on either dump page**; they carry risk disclaimers
  only.

### 9.2 Venue listing dates — the ceiling on any exchange-native route

| source | earliest XLM daily bar | quote |
|---|---|---|
| Poloniex `XLM_BTC` (ex-`STR_BTC`) | **2014-08-11** | BTC |
| Kraken `XXLMZUSD` | **2017-01-17** (trade id 1, $0.002251) | USD |
| Yahoo Finance `XLM-USD` | 2017-11-06 | USD |
| Bitfinex `tXLMUSD` | 2018-05-01 | USD |
| Bitstamp `xlmusd` | 2020-06-17 | USD |

Two premises worth correcting: **Bitfinex never carried an STR symbol**
(`tSTRUSD` returns `[]`), and its XLM listing is 2018, not early. **Nothing
USD-quoted and free reaches below 2017-01-17.**

### 9.3 The only genuine 2014 series is BTC-quoted

Poloniex `XLM_BTC` serves a real, non-zero-volume daily bar from
2014-08-11 and is continuous thereafter. Using it for USD means a cross,
and the cross has three problems:

- Poloniex's own `XLM_USDT` is unusable — at 2015-04-01 every OHLC value is
  identical with zero volume and `tradeCount=0`, at a price ~10× off XLM's
  real level; at 2015-09-30 its daily quote volume is **$0.0075**.
- Poloniex `BTC_USDT` is **empty before 2014-09-01**, so the cross leg has
  its own hole. The working recipe is `XLM_BTC` × Bitstamp `BTC_USD`
  (verified back to 2014-08-30).
- **Terms: NOT FOUND** — `poloniex.com/terms/` redirects to a page that
  renders no text. Counterparty context, as fact: under Justin Sun's
  control since 2019-10, no US users since 2019-12, on the UK FCA warning
  list.

Since pubnet genesis is 2015-09-30, anything before that date is not a
price of an asset this product indexes, and the honest floor for any XLM
series here is genesis, not 2014.

### 9.4 Stellar-native cannot supply it

Three independent reasons, all measured:

- The reference public Horizon instance, asked for its oldest ledger,
  returns ledger **57,969,361, closed 2025-07-12** — roughly 14 months of
  retention. It cannot serve 2015 at all (ADR-0001 is why nothing here
  depends on it). **The lake described in §0.1 is the deeper source, not
  the fallback.**
- No USD-pegged asset existed on Stellar before 2016-10-18 (§4.1).
- Network-wide classic trade effects were 29 in 2015 and 135 in 2016
  (§4.2).
- `api.stellar.expert` returns 403; terms NOT FOUND.

### 9.5 Free and open sources — every one fails on terms

| source | depth | operative clause |
|---|---|---|
| Kraken | 2017-01-17 | Global Terms §8, §9 — see §9.1 |
| Bitfinex | 2018-05-01 | Market Data Terms (2021-01-04) §1 "solely for your internal purposes"; §2.1.1 "You will not … distribute, disclose or resell any part of the Bitfinex Market Data"; §3 "licensed and not 'sold' to you" ⚠️ read from Bitfinex's own GitHub org, not the live legal page (SPA renders no text) — near-primary, not byte-matched |
| Yahoo Finance | 2017-11-06 | Terms (2025-05-06) §2.4(j) — bars using content "to create any database, archive, mobile application, data feed, widget or any other aggregated data source that competes with or constitutes a material substitute for the Services" |
| DefiLlama | ≤2014-10-01 (CoinGecko upstream) | Terms (2025-06-24) §7 personal/non-commercial; §8(7) bars republication "in any form without permission"; §14 liquidated damages "up to USD 100,000 per violation". Two licences to clear, since the data is CoinGecko's |
| CryptoDataDownload | no XLM pairs listed at all today | (2023-09-20) "Users of this website should use the information contained herein for non-commercial uses only" |
| CoinPaprika | 1 year on the free tier | site terms (2026-09-05) §4.1(c)(d)(e) — bars aggregating for third parties, licensing content, scraping |
| Investing.com | — | "It is prohibited to use, store, reproduce, display, modify, transmit or distribute the data contained in this website without [prior written permission]" |
| CoinCap | gone — `api.coincap.io` is NXDOMAIN | — |
| CoinLore | 365 days | terms NOT FOUND |
| Kaggle dataset | claims 2014-09-17 → 2021-11-29 | licence label is JS-rendered and unread. Independently of that: the uploader scraped an aggregator and cannot grant rights they never held — a dataset licence tag is not a chain of title |
| Nasdaq Data Link / Quandl | no free XLM table located | NOT FOUND |
| CoinAPI | no free plan | legal page returns 403; terms NOT FOUND |

**No free or open source was found that has both genuine pre-2017
XLM/USD daily history and terms permitting redistribution in a commercial
product — zero.** Every source that reaches below 2017 is either
BTC-quoted with unread terms (Poloniex), or carries an explicit
internal-use, non-commercial, or no-republication clause.

### 9.6 The paid aggregators

All three were read on 2026-09-05. Pages quoted below are primary vendor
pages; where a figure could not be established it says so.

**The two things this product would do with the data are different acts,
and the answer differs between them:**

- *display* — vendor bars drawn on charts on this site;
- *serve onward* — the same bars returned by `/v1/ohlc`,
  `/v1/history/since-inception` and the rest of the public API.

| | CoinGecko | CoinMarketCap | CCData (ex-CryptoCompare, now CoinDesk Data) |
|---|---|---|---|
| earliest XLM daily | **≥2015-03-04 proven** — their published XLM all-time low is $0.0004761 on 2015-03-04. "starting from 2014" is a vendor claim, unmeasured | **2014-08-05, measured** — first bar open 0.00297568994574, and a re-probe from 2014-01-01 still starts there | **NOT VERIFIED** — key-gated |
| bar shape | daily **close only** before 2018-02-09; OHLC range endpoint is "Available from February 9, 2018 onwards" | true daily **OHLC from day one** | claimed OHLCV, unverified |
| full-history endpoint | `/coins/{id}/market_chart/range`, no documented max span — the whole pull is one call | `/v2/cryptocurrency/ohlcv/historical`, 365 points/request | daily endpoint, 2,000 points/request |
| granularity trap | **yes** — ">90 days = daily (00:00 UTC)"; `/coins/{id}/ohlc` `interval=daily` caps at 180 days | none found on the daily endpoints; the limit is page size, not an interval switch | none documented |
| cheapest tier reaching full history | **Analyst $129/mo** (Basic $35 caps at 2 years) | **$79 or $699 — unresolved**, see below | **no public pricing at all** |
| display in a commercial product | **yes**, paid tiers, with attribution | **yes**, one product, ≤100k users | **no**, at any price |
| serve onward through our own API | **no** — Enterprise only | **no** — Enterprise only | **no** — negotiated licence only |
| bulk export | not offered | not found | **yes** — S3 / Azure Blob / GCS backfill pipelines |
| rate limits for this job | irrelevant (1 call) | irrelevant (~45 credits) | irrelevant (3 calls) |

**The line that matters is the same for all three: no self-serve tier of
any vendor permits serving their data through this product's own API.**
Every one routes that to a negotiated agreement. That is precisely what
`/v1/ohlc` and `/v1/history/since-inception` would be doing.

Verbatim, the operative clauses:

- **CoinGecko** API Terms (`coingecko.com/en/api_terms`, "Latest Version:
  5 Sept 2025"), cl. 4.1.6 — "You are not permitted to sell, rent, lease,
  sub-license, re-distribute or syndicate access to the CoinGecko API or
  part thereof (unless pursuant to the terms of an Executed Agreement that
  you enter into with CoinGecko)", and in the same clause "You are entitled
  to charge for your services and products that incorporate or integrates
  our CoinGecko API." Attribution: "displaying prominently the message
  'Powered by CoinGecko' in a legible font … no smaller than font size 10".
- **CoinMarketCap** pricing-page FAQ (`coinmarketcap.com/api/pricing/`) —
  "Under the commercial license you can use the data inside your own
  product, but you may not redistribute or resell it as a standalone
  service, whether through your own API or as part of a data distribution
  product." The licence "covers one product with up to 100k users".
- **CCData** API Licence Agreement (`data.coindesk.com/api-licence-agreement`,
  effective 2026-04-22), cl. 2.1 — a licence "for your own internal use
  only"; cl. 2.4.1 not "for Display purposes"; cl. 2.4.2 not "for
  developing, reproducing, or distributing to Customers or any other
  person, any Products". There is **no free/paid split in this document** —
  the grant is internal-use-only however much is paid, which is
  categorically stricter than the other two.

Three findings that would be easy to miss:

1. ⚠️ **CoinGecko's storage clauses sit badly against this plan
   specifically.** Cl. 6.1.1 — "You should refresh the cache at least every
   24 hours"; cl. 6.1.4, on termination, "promptly and permanently delete
   all Data … without keeping any copy thereof"; cl. 6.2 — "you are not
   allowed to duplicate, reproduce, copy, store, derive from or translate
   any Data". This plan would persist their bars indefinitely in `trades`
   and every aggregate built on it, and would keep them after any lapse in
   subscription. That is the opposite of what those clauses describe. The
   wording is "should" not "shall" and 6.2 is qualified by "Except as
   expressly permitted hereunder", so it is ambiguous rather than clearly
   prohibited — which is exactly why it needs a decision rather than an
   assumption.
2. ⚠️ **A $620/month question is unresolved on CoinMarketCap.** The live
   pricing matrix shows full-history daily OHLCV from the Startup tier
   ($79); CMC's own resources article says "The Professional plan provides
   daily OHLCV data back to the beginning" ($699). Two CMC pages disagree
   and neither was reconciled.
3. ⚠️ **CoinMarketCap's 2014-08-05 floor was measured through their site's
   internal endpoint, not the licensed API.** That probe is evidence the
   data exists; it is **not** a usable source — their site terms
   (`coinmarketcap.com/terms/`, 2025-11-24) §5.1(d) prohibit "any data
   mining, crawling, 'scraping', robot or similar automated … method".

**CoinGecko's two agreements must not be conflated.** The website terms
(`coingecko.com/en/terms`, 2025-08-12) cl. 4.5 grant a licence "solely for
your Personal Use … and not for any commercial purpose" — that governs the
site, not the API. The API terms above are a separate document and do
permit commercial products. Reading the first against an API integration
would produce the wrong answer. (The website-terms date and clause text
were relayed second-hand and should be re-read before being relied on; the
API terms were verified directly.)

The pending CoinGecko Pro purchase (§8.4) is the dependency that would
settle the CoinGecko row. It would not, on the clauses above, cover serving
the data through this product's public API.
