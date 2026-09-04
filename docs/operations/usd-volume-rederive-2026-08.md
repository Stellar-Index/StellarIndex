---
title: Operator runbook — 2026-08 usd_volume re-derive + CAGG rebuild (tier-3b poisoning, exact tiers, XLM-base anchor)
last_verified: 2026-09-03
status: ready-to-execute
---

# 2026-08 historical remediation: re-derive `trades.usd_volume`, rebuild the CAGGs

**What happened.** Commit `87b8a203` (2026-07-22) introduced the tier-3b
XLM bridge: an on-chain trade with no recognised USD peg is valued as
`token/XLM × XLM/USD`, with the `token/XLM` rate read from `prices_1m` —
**a continuous aggregate over `trades` itself**. The rate for a thin
token is therefore whatever the last trader wrote, up to 24h stale, and
one honest ~$9 seed trade valued a 5-XLM dump at **$8,559,224**
(reproduced digit-for-digit; see `NEXT-SESSION.md` §1 and memory
`project_valuation_identity_fixes_2026_08_04.md`). Fleet-wide, ~$21.4M
of the ~$22M reported 24h on-Stellar XLM `usd_volume` was artifact.

**Code fixes already on `main`** (all `verify.sh`-green):

| commit | what |
|---|---|
| `fd1860bd` | XLM-based trades valued off their measured XLM leg (tier-4 anchor now BEATS the bridge) — the insert-time fix |
| `52b04a63` + follow-up | every verified ticker flaggable as impersonation; reference-only warning restored; caps suppressed on collisions |
| this session | serving-side thin-market substance gate (`[pricing_guard]`); orchestrator `min_usd_volume` fail-closed; explorer chart/provenance |

**Poisoned population.** Rows inserted while `87b8a203` was live and
`fd1860bd` was not: `ts >= '2026-07-22'` up to the deploy of the fixed
indexer — ~2.5M trades, ~1,650 materially (>10×) wrong, errors in BOTH
directions. The CAGGs built over them (`prices_1m/15m/1h/4h/1d/1w/1mo`
`volume_usd`, `dex_volume_by_pair_1d`, `source_volume_1h`,
`pools_per_source_1h`) and the `asset_volume_24h` rollup inherit the
poison.

---

## Step 0 — DEPLOY FIRST (hard prerequisite)

**r1 is still running v0.22.0 binaries and is writing mispriced
`usd_volume` on every new XLM-based trade right now.** Until the fixed
indexer is deployed, the dirty span has no right edge and any re-derive
chases a moving target.

1. Cut the release per `/cut-release` (CHANGELOG `[Unreleased]` already
   carries this session's entries):
   ```sh
   git checkout main && git pull --ff-only origin main
   bash scripts/dev/cut-release.sh vX.Y.Z --dry-run   # then for real
   ```
2. Deploy per `/deploy-r1` — **indexer + api + aggregator** (the gate
   lives in api+aggregator, the valuation fix in the indexer's insert
   path via the shared store):
   ```sh
   gh workflow run deploy.yml -f region=r1 -f version=vX.Y.Z \
     -f binaries=stellarindex-indexer,stellarindex-aggregator,stellarindex-api
   ```
   No new migrations in this batch. Post-deploy: run the r1-smoke
   battery, then confirm the gate is live —
   `curl -s .../v1/price?asset=<known-dust-pair>&quote=native` must
   return the `price-withheld` problem type, and
   `stellarindex_price_serve_substance_withheld_total` must be moving.
3. **Record the deploy ledger** — the re-derive's right edge:
   ```sql
   SELECT max(ledger) FROM trades;  -- call this L_HI, at deploy time
   ```

## Step 1 — pin the dirty range precisely

```sql
-- left edge: first ledger valued under 87b8a203 (deployed 2026-07-22)
SELECT min(ledger) AS l_lo, min(ts) FROM trades WHERE ts >= '2026-07-22';
-- right edge: L_HI from step 0.
```

Sanity-size it (expected ~2.5M):

```sql
SELECT count(*) FROM trades WHERE ts >= '2026-07-22' AND ledger <= <L_HI>;
```

## Step 2 — re-derive via `ch-rebuild` (the Go path, NOT hand SQL)

Use the ONE valuation implementation (`tradeUSDVolume` — same function
insert-time and re-derive-time; the verifier's header explicitly warns
against SQL reimplementations for heterogeneous classes, and this class
IS heterogeneous: post-fix values depend on tier ordering + per-source
decimals + the XLM/USD series at each trade's ts).

Mechanics (verified in repo, 2026-08-04):

- `stellarindex-ops ch-rebuild -config /etc/stellarindex.toml
  -ch-addr 127.0.0.1:9300 -from <L_LO> -to <L_HI> -sdex -write`
  re-decodes from the ClickHouse lake and upserts through
  `InsertTrade`/`BatchInsertTrades`.
- It calls `SetDeriveGeneration(now.Unix())` + installs the FX/peg
  resolvers itself (`ch_rebuild.go:162-172`) — the
  `reDeriveNullVolumeGuard` fail-closed check is satisfied; if it ever
  fires, STOP: it means the config's peg lists are missing on the host.
- The upsert guard is `derive_generation <= EXCLUDED.derive_generation`,
  so re-running a window is safe/idempotent, and live gen-0 writes can
  never claw a corrected row back.
- Add `-sources` only if you want to scope; default = all event-based
  sources. `-sdex` includes the op-derived SDEX trades (needed — SDEX
  is most of the volume).

Execution learnings (2026-08-04 run — folded in so the next operator
doesn't rediscover them):

- **Source the env file first**: the TOML's `postgres_dsn` carries a
  placeholder password; the real one is injected via
  `/etc/default/stellarindex` (systemd EnvironmentFile). A bare
  one-shot fails 28P01. `set -a; . /etc/default/stellarindex; set +a`.
- **DECOMPRESS FIRST** (the projector-replay lesson generalises):
  upserting into a COMPRESSED trades chunk crawls — the calibration
  window ran ~10× slower than the uncompressed-path precedent
  (per-batch segment decompression). Post-consolidation trades chunks
  are 7-DAY, so only `_hyper_1_31953_chunk` (07-16→07-23, 1.8 GB
  compressed / 31 GB raw) overlapped this range compressed.
  `decompress_chunk(...)` it under the heavy wrapper (5.2 TB free on
  the pool — headroom is not a concern), and **pause the trades
  compression policy first** (`SELECT alter_job(1000, scheduled =>
  false)`) so it can't recompress the chunk mid-run — then re-enable
  (`scheduled => true`) after the final window and let it drain.

Operational discipline (ALL are prior-incident lessons):

```sh
# per window, on r1 — UNIQUE job name per attempt (stale-lock trap):
/usr/local/sbin/run-heavy-job.sh rederive-2026-08-w<N>-try<K> \
  /usr/local/bin/stellarindex-ops ch-rebuild \
    -config /etc/stellarindex.toml -ch-addr 127.0.0.1:9300 \
    -from <W_LO> -to <W_HI> -sdex -write
```

- **Windows of ≤50,000 ledgers** — the sdex read OOMs the 10 GiB client
  pin above that (measured 2026-07-30). The 13-day span is ~190k
  ledgers → ~4 windows.
- **Oldest → newest**, one window at a time (one heavy job at a time is
  the standing rule).
- Throughput reality: ~620 rows/s into compressed chunks was measured;
  2.5M rows ≈ **~70–90 min of writes**, plus decode time. Budget a few
  hours wall.
- Chunks in the span older than 7 days are compressed. `ch-rebuild`
  writes through the normal upsert (no bulk-UPDATE GUC needed by the Go
  path); if you fall back to any SQL step, per-session
  `SET timescaledb.max_tuples_decompressed_per_dml_transaction = 0;`
  and ship SQL by `scp` + `-f file.sql` — never inline `$$` over ssh.
- After each window, **refresh `prices_1m` over that window's time
  range before starting the next** (step 3 command, scoped) — this
  breaks the tier-3b circularity for any remaining bridge-valued rows
  in later windows: their `prices_1m` inputs are then already
  corrected.

## Step 3 — CAGG rebuild over the span

`ch-rebuild` refreshes **nothing**; `prices_1m/15m` and all four
volume/derived CAGGs are OUTSIDE the Go allow-list — psql only. With
`[T_LO, T_HI]` = the span's time range (pad ≥ 2× bucket per
`PadRefreshWindow` semantics):

```sql
-- interleaved per-window during step 2, then once over the full span:
CALL refresh_continuous_aggregate('prices_1m',  '<T_LO>', '<T_HI>');
CALL refresh_continuous_aggregate('prices_15m', '<T_LO>', '<T_HI>');
CALL refresh_continuous_aggregate('prices_1h',  '<T_LO>', '<T_HI>');
CALL refresh_continuous_aggregate('prices_4h',  '<T_LO>', '<T_HI>');
CALL refresh_continuous_aggregate('prices_1d',  '<T_LO>', '<T_HI>');
CALL refresh_continuous_aggregate('prices_1w',  '<T_LO>', '<T_HI>');
CALL refresh_continuous_aggregate('prices_1mo', '<T_LO>', '<T_HI>');
CALL refresh_continuous_aggregate('twap_1h',    '<T_LO>', '<T_HI>');
CALL refresh_continuous_aggregate('twap_1d',    '<T_LO>', '<T_HI>');
-- volume/derived (names verified against migrations 0036/0064/0068):
CALL refresh_continuous_aggregate('dex_volume_by_pair_1d', '<T_LO>', '<T_HI>');
CALL refresh_continuous_aggregate('source_volume_1h',      '<T_LO>', '<T_HI>');
CALL refresh_continuous_aggregate('pools_per_source_1h',   '<T_LO>', '<T_HI>');
```

Run under a heavy-job scope too; a `55P03` concurrent-refresh conflict
just means the policy job is running — retry. Then force the
`asset_volume_24h` rollup refresh (it re-sums `prices_1m.volume_usd`;
it self-heals on its cadence but verify it did).

Compression backlog after the writes self-drains via the policy; check
`timescaledb_information.jobs` / the `compression-lag` runbook if
chunks stay uncompressed >1 day.

## Step 4 — verification (acceptance gates)

1. **Exact tiers**: `stellarindex-ops verify-usd-volume -config
   /etc/stellarindex.toml -days 30` → **0 violations** (structurally
   blind to tier 3/4 — necessary, not sufficient).
2. **XLM-base identity** (the class that was wrong; the check the
   verifier doesn't have yet). For the span, estimated-tier rows whose
   base is `native` must satisfy
   `usd_volume ≈ base_amount/1e7 × XLM/USD(ts)`:
   ```sql
   -- spot-verify per day; xlm_usd from the crypto:XLM/fiat:USD or
   -- native/USDC-GA5Z… prices_1m series at the trade's minute.
   -- Expect |delta|/usd_volume < 1% except genuine bridge-tier rows.
   ```
   (Queue the durable version: extend `USDVolumeTier.Exact()`'s
   companion with a `TierXLMBase` checkable identity —
   `usd_volume_reconcile.go:85` is the seam. That closes the
   13-days-invisible blindness class for good.)
3. **Fleet-wide magnitude**: re-run the incident measurement — 24h
   `sum(usd_volume)` for `base_asset='native'` vs `sum(base_amount/1e7
   × xlm_usd)`. Pre-fix these differed **$21.96M vs $0.60M**; post-
   re-derive they must agree to within FX noise. Also: zero rows >10×
   off their XLM leg (was 225 over, 11 under).
4. **Served surfaces**: `/v1/assets?code=XRP` → no `market_cap_usd`,
   flag present; a dust pair's `/v1/price` → `price-withheld`;
   explorer `/assets/USDT` after the next Pages deploy shows the
   provenance caption and a USD-quoted chart.
5. **Determinism spot-check** (2026-07-29 precedent): re-run one
   already-corrected window; row md5s must be byte-identical.

## Step 5 — W5.3: re-stamp the pre-07-23 EXACT-tier rows (`usd-volume-restamp`)

A different class from steps 1–4 and a different tool. Steps 1–4 re-derive
ESTIMATED-tier rows (the tier-3b bridge) through the resolver-backed
waterfall; that needs `ch-rebuild`. This step repairs EXACT-tier rows —
quote leg or base leg USD-pegged — that were stamped before the peg
identity was the insert path: the 2026-07-30 sweep measured
**[2026-05-12, 2026-07-22], 66 dirty days, every violation a
`[base_pegged] sdex` USDC-base row** valued by the resolver's VWAP
(~+0.7%) instead of `base_amount / 10^7` (evidence:
`evidence/2026-07-30-verify-usd-volume-30d.md`). That night's fix was a
hand SQL UPDATE; `usd-volume-restamp` is that UPDATE as a tool, with the
discipline built in — use it for any exact-tier violation
`verify-usd-volume` reports from now on.

What it does (per `internal/ops/chops/usd_volume_restamp.go`):

- classifies every (source, base, quote) group of each UTC day with the
  SAME `ClassifyUSDVolumeTier` + peg inputs (`trades.usd_pegged_classic_assets`
  + `supply.sac_wrappers`) as the insert path and the verifier — the tool
  never decides "which leg / which scale" itself;
- rewrites only rows whose stored `usd_volume` differs from
  `pegged_leg / 10^decimals` (`IS DISTINCT FROM`), to exactly the value the
  insert path writes (`round(leg / 10^d, 8)` == `big.Rat.FloatString(8)`);
- stamps every rewritten row with the run's `derive_generation`
  (`now.Unix()`, like `ch-rebuild`) guarded by `derive_generation <= gen`
  — INV-3: a live gen-0 replay can never claw the correction back;
- leaves correct rows untouched (value AND generation), so a re-run
  reports 0 — idempotent;
- leaves NULL rows alone unless `-fill-null` (a coverage change, opt-in);
- walks `-from..-to` (inclusive UTC days, never today) oldest → newest in
  `-slice` windows (default 1h), each window one transaction with
  `SET LOCAL timescaledb.max_tuples_decompressed_per_dml_transaction = 0`
  — the 2026-07-30 lesson, no manual GUC step. LOCAL, so Postgres unwinds
  the lifted cap at COMMIT and it can never ride the pooled connection
  into a later statement (#312);
- dry-run by default; `-write` applies; ch-backfill-style heartbeat
  (`ops_job="usd-volume-restamp"`, the standing stall alerts apply).

Mechanics:

```sh
# 0. size it (read-only; run anywhere with the config):
stellarindex-ops verify-usd-volume -config /etc/stellarindex.toml \
  -day 2026-07-22 -days 72
# 1. dry run — per-day candidate counts, Σ|Δ| before, no writes:
stellarindex-ops usd-volume-restamp -config /etc/stellarindex.toml \
  -from 2026-05-12 -to 2026-07-22
# 2. apply, under the heavy wrapper, UNIQUE job name per attempt
#    (stale-lock trap), env file sourced (28P01 trap), one window at a time:
set -a; . /etc/default/stellarindex; set +a
/usr/local/sbin/run-heavy-job.sh usd-restamp-w1-try1 \
  /usr/local/bin/stellarindex-ops usd-volume-restamp \
    -config /etc/stellarindex.toml -from 2026-05-12 -to 2026-05-31 -write
# 3. acceptance — the tool prints this line for the window it ran:
stellarindex-ops verify-usd-volume -config /etc/stellarindex.toml \
  -day 2026-07-22 -days 72        # → 0 violations
```

- Windows: any size is safe (the tool slices internally), but keep a
  heavy job to ~2–3 weeks so a failed attempt is cheap to re-run — and
  re-running IS cheap: repaired rows are skipped, only the remainder is
  written.
- DECOMPRESS FIRST still applies for throughput (not correctness): the
  tool raises the decompression cap itself, but DML into a compressed
  chunk is ~10× slower than into a decompressed one. Pause the trades
  compression policy, `decompress_chunk` the span, re-enable after.
- CAGG rebuild (step 3 above) over the restamped span afterwards — the
  tool refreshes nothing; `prices_1m`'s `volume_usd` and everything above
  it inherit the corrected column only on refresh.
- Do NOT pass `-fill-null` on the first pass. Unpriced exact-tier rows are
  the coverage alerts' population; fill them as a deliberate second pass
  once the value repair has been accepted.
- Tier-3b (quote-side FX bridge) violations are NOT this tool's job —
  steps 0–4 (`ch-rebuild`). The tier-4 XLM anchor IS, via `-tier
  xlm-base` — Step 6.

## Step 6 — #372: re-derive the pre-`fd1860bd` XLM-base rows (`usd-volume-restamp -tier xlm-base`)

A third class again. Step 5 repairs a SQL identity; this one RE-DERIVES
the tier-4 XLM anchor, which is a function of `prices_1m` at the row's
own timestamp and therefore cannot be spelled in SQL without
re-implementing the waterfall.

**The population.** Every on-chain DEX trade whose BASE leg is XLM
(`native` or its SAC) and whose QUOTE leg is not USD-pegged, written
before `fd1860bd` (2026-08-04, v0.25.0). Until that commit the waterfall
reached the QUOTE side first, so the trade was valued through the token's
own thin `<token>/USDC` `prices_1m` bucket — a rate its counterparties
author — instead of off the XLM leg, which is the measured side of the
trade and whose rate is a direct market. `a7892962` (2026-07-09) added
the anchor only as a FALLBACK after the quote leg, so the whole
2026-03-12 → 2026-07-21 span is dirty; every row is at
`derive_generation = 0` (the 2026-08-05 re-derive covered `ts >=
2026-07-22` only).

**What the tool does** (`internal/storage/timescale/usd_volume_restamp_xlmbase.go`):

- scans `(source ∈ the DEX registry, base_asset ∈ the two XLM wire forms,
  derive_generation <= -max-generation)` per `-slice` window, and decides
  the tier in GO with the insert path's own `usdVolumeDecimals` — a
  USD-pegged quote is EXACT-tier and belongs to Step 5, never to this
  one, so the two tools cannot undo each other;
- rebuilds each row into its `canonical.Trade` and calls the store's own
  `tradeUSDVolumeViaXLMBaseAnchor` with the resolver installed by
  `InstallUSDVolumeResolution` — the same function `InsertTrade` calls.
  The resolver is time-anchored to the ROW's `ts`, not to `now()`, so the
  re-derive is deterministic given `prices_1m`;
- **stops at the anchor.** The live insert path, when the anchor declines,
  falls through to the quote side — the route that wrote the defect. This
  tool reports such a row instead (`anchor declined, stored NULL` /
  `stored VALUE`): a stored NULL stays NULL, and a stored value is never
  blanked. An unpriced row stays recoverable; a confidently-wrong row at a
  winning `derive_generation` does not;
- INV-3, idempotence, the `SET LOCAL` decompression cap and the dry-run
  default are exactly as Step 5, plus a `-batch` bound (default 2000 rows
  per UPDATE transaction) and a live-overlap refusal: the window's top
  on-chain ledger must be at or below the live `ledgerstream` cursor
  (`-allow-live-overlap` overrides).

**Measured on r1, read-only, 2026-09-03** (`-report`, three sample days):

| day | scanned | quote-pegged (Step 5) | already correct | would change | of which NULL→value | anchor declined | Σ stored → Σ want |
|---|---|---|---|---|---|---|---|
| 2026-03-12 | 69,907 | 11,966 | 567 | 57,374 | 57,374 | 0 | $0.00 → $119,793.50 |
| 2026-05-19 | 353,444 | 68,076 | 1,770 | 283,598 | 87,387 | 0 | $101,854.74 → $132,704.79 |
| 2026-07-20 | 250,439 | 50,135 | 7,248 | 193,056 | 0 | 0 | $134,120.82 → $133,697.73 |

Read that as three eras. **March**: the anchor did not exist at insert
time and the quote side priced almost nothing, so 99% of the day's
non-pegged XLM-base rows are NULL and $119,793 of one day's volume is
invisible. **May**: mixed — 87,387 NULL plus a priced population that
moves, with 23,442 rows ≥ 10% and 5 rows ≥ 10× (the largest, a `BUCK`
row, from $0.0065 to $1.299). **July** (post-`a7892962`): no NULLs left,
but 193,056 rows still differ because the anchor was only a fallback;
722 move ≥ 10%, 35 ≥ 100%, and the day's total barely moves (−$423).
`anchor declined = 0` on all three days — the anchor can price the entire
population, so this re-derive leaves no residue.

Mechanics:

```sh
# 1. REPORT first (read-only; refuses -write). This is the decision input.
stellarindex-ops usd-volume-restamp -config /etc/stellarindex.toml \
  -tier xlm-base -from 2026-05-19 -to 2026-05-19 -report -fill-null
# 2. value repair, oldest → newest, one heavy job per ~2-3 weeks,
#    UNIQUE job name per attempt, env file sourced:
set -a; . /etc/default/stellarindex; set +a
/usr/local/sbin/run-heavy-job.sh usd-xlmbase-w1-try1 \
  /usr/local/bin/stellarindex-ops usd-volume-restamp \
    -config /etc/stellarindex.toml -tier xlm-base \
    -from 2026-03-12 -to 2026-03-31 -write
# 3. coverage fill as a DELIBERATE second pass, same windows:
/usr/local/sbin/run-heavy-job.sh usd-xlmbase-null-w1-try1 \
  /usr/local/bin/stellarindex-ops usd-volume-restamp \
    -config /etc/stellarindex.toml -tier xlm-base \
    -from 2026-03-12 -to 2026-03-31 -fill-null -write
# 4. CAGG refresh over the span — Step 3's list, in Step 3's ORDER.
# 5. acceptance:
stellarindex-ops verify-usd-volume -config /etc/stellarindex.toml \
  -day 2026-07-21 -days 132
```

- **Resuming.** The tool is idempotent — a row already holding the
  anchor's value is not a candidate — so an interrupted run is resumed by
  re-running it from the day it stopped on. It prints that command on
  failure. There is no cursor to reset.
- **DECOMPRESS FIRST** applies here even more than in Step 5: this rewrites
  ~28M rows across 132 days.
- **`-min-rel-delta`** narrows a first pass to the large moves (e.g.
  `0.1` for ≥ 10%), at the cost of leaving the rest at their old value and
  the group at mixed generations. Prefer the full pass; the flag exists
  for a load-constrained window, not as the default.
- **CAGG ordering matters and is not alphabetical.** `prices_1m/15m/1h/4h/1d/1w/1mo`,
  `pools_per_source_1h`, `dex_volume_by_pair_1d` and `source_volume_1h`
  read `trades` DIRECTLY; `twap_1h` and `twap_1d` read `prices_1m`
  (migration 0147). Refresh `prices_1m` first and the two `twap_*` LAST,
  or the TWAP views inherit the pre-restamp `volume_usd`. Step 3's list is
  already in that order.
- **The refresh changes more than `volume_usd`.** Since migration 0115/0147
  the price CAGGs compute `high_price`/`low_price`/`first_price`/`last_price`
  with a `FILTER (WHERE usd_volume >= 0.01)` dust floor, so filling ~7M
  NULL rows admits trades into the OHLC extremes that were previously
  excluded. Expect historical highs/lows on thin XLM pairs to MOVE (they
  become more complete, not less), and diff a few before/after.
- **Feedback loop, deliberately one-way.** `prices_1m.volume_usd` is read
  back by the resolver's tier-3b bridge leg (`queryXLMLeg`,
  `volume_usd >= 0.01`), so the refresh does change which buckets can
  price OTHER tokens later. The XLM anchor itself is NOT affected: it
  resolves `native` through `queryDB`, whose floor is `vwap * volume`
  (the bucket's own quote notional), not `volume_usd`. So the restamp
  cannot move its own inputs — but a LATER `ch-rebuild` over the same era
  will now be able to price token/token rows it previously could not.
  That is a coverage gain, and it is why the CAGG refresh belongs AFTER
  the whole span is restamped, not interleaved.
- `asset_volume_character` needs no action: it is a trailing-14-day
  rollup on a 15-minute cadence and the span is months old.

### Step 6, chunk mode — `-chunks` (USE THIS for the 2026-01-01 → 2026-07-21 span)

**Measured 2026-09-03.** The mechanics above, run as written
(`-from 2026-01-01 -to 2026-07-21 -fill-null -write`, 28.6M rows), moved
at **~1,574 rows/min** — one 2,000-row UPDATE took over 14 minutes — and
was stopped with 0 rows committed (the generation guard held). Cause: all
90 `trades` chunks in the window are compressed (policy `compress_after
7 days`; TimescaleDB 2.26.4; `max_tuples_decompressed_per_dml_transaction
= 100000`), and a DML into a compressed chunk decompresses per row. The
dry run only reads, so it never showed it. The "DECOMPRESS FIRST" bullet
above was the by-hand remedy; `-chunks` is the same remedy inside the
tool, one chunk at a time, so no more than one chunk is ever
decompressed — and with the compression policy paused for the run, which
the by-hand path already required (Step 3's execution learnings,
`alter_job(…, scheduled => false)`).

Sizing at the time: 90 chunks, 379 GB uncompressed / 25 GB compressed,
largest chunk 160 GB, pool 4.69 TB free.

What `-chunks` does (`internal/ops/chops/usd_volume_restamp_chunks.go`,
`internal/storage/timescale/trades_chunks.go`):

0. resolve the `trades` compression policy job
   (`timescaledb_information.jobs WHERE proc_name = 'policy_compression'
   AND hypertable_name = 'trades'`) — **refuses to start if there is
   none** — and check the window against its lag (below). A `-write`
   run then, in this order: takes the run lock
   (`pg_try_advisory_lock(hashtext('usd-volume-restamp:trades'))` on a
   dedicated connection, held for the whole run — **refuses if another
   session holds it**); reads the policy again under the lock and
   **refuses if it is already unscheduled** unless
   `-resume-paused-policy`; prints the re-enable statement to stderr;
   PAUSES the job (`alter_job(id, scheduled => false)`); waits — at most
   10 minutes, one progress line per poll — while
   `timescaledb_information.job_stats` still reports the job's proc
   running (the pause stops the NEXT fire; one in flight finishes on
   its own, and a decompress beside it would race it); then lists the
   chunks AGAIN and walks that listing, not the one the plan was
   printed from;
1. per `trades` chunk intersecting the window, oldest first: probe the
   chunk READ-ONLY, slice by slice, stopping at the first slice that
   would change a row — a chunk with nothing to change (rows already at
   the run's generation, or already holding the anchor's value) is
   **skipped without being decompressed** (this is what makes a rerun
   resume);
2. re-measure free space against THIS chunk, then
   `SELECT decompress_chunk(<chunk>, if_compressed => true)` — for a
   chunk that was compressed when the run listed it;
3. the SAME plan + apply as the day walk (same anchor, same
   `derive_generation` guard), restricted to that chunk's slice of the
   window, in `-chunk-batch` UPDATE transactions (default 20,000 — a
   plain heap UPDATE now). **Ahead of every batch the chunk's
   `is_compressed` is read**; `true` means something took the chunk
   back underneath the run (a by-hand `compress_chunk`, a fire that
   slipped the pause), and the walk STOPS there — the batch is not
   written, the error names the chunk, the `RESUME:` line is printed —
   because an UPDATE into a compressed chunk does not fail, it crawls;
4. `SELECT compress_chunk(<chunk>, if_not_compressed => true)` — again
   only for a chunk that was compressed at listing, and as a DEFERRED
   call, so a failed batch, a SIGTERM and a Go panic inside the work all
   reach it. **The bracket restores the state it found**: a chunk that
   was uncompressed at listing is restamped in place and left
   uncompressed; the policy compresses it on its own schedule once
   re-enabled;
5. a heartbeat tick and one progress line: rows changed, seconds, bytes
   before → decompressed → after;
6. on EVERY exit — success, failure, SIGTERM — re-enable the policy
   (`alter_job(id, scheduled => true)`) on a context that survives the
   cancellation, and THEN release the run lock, so a run waiting on the
   lock never inherits a paused policy. After a `-chunks -write` run has
   exited on its own, the policy is scheduled. A run that finds the
   policy already unscheduled at start does NOT take it over silently:
   it refuses, naming the by-hand re-enable and `-resume-paused-policy`.
   After a previous attempt was killed before its re-enable, either
   re-enable by hand first (the SQL checks under SIGKILL below) or rerun
   with `-resume-paused-policy`, which owns the paused policy for the
   run and re-enables it at exit.

Why the pause is not optional: the policy proc selects
`show_chunks(older_than => lag) … WHERE status != fully_compressed` and
compresses each — a chunk this tool has just decompressed IS selected.
Over a multi-day run the 12-hourly fire would re-compress the open chunk
between two batches, and the next batch would then run its `SET LOCAL
max_tuples_decompressed_per_dml_transaction = 0` UPDATE against a
compressed chunk: no error, just the ~1,574 rows/min path this mode
exists to escape. The integration harness runs
`timescaledb.max_background_workers = 0`, so no test can watch the race
fire; the unit tests pin the ORDER (pause before the first decompress;
re-enable after the last chunk, on failure, and on a cancelled
context), and the integration test pins that the real job is paused
while the run is in flight and scheduled again after it.

**Two attempts at once.** `run-heavy-job.sh`'s lock is per job NAME,
and the sequence below mandates a unique name per attempt — so the
wrapper does nothing to stop a second `-chunks -write` from starting
while the first is alive. Without a guard the second would read the
policy as already unscheduled, walk beside the first, and re-enable the
policy at ITS exit while the first was still inside a chunk: the first
run's open chunk goes to the policy's next fire, its next batch crawls at
~1,574 rows/min with no error, and the span ends at two generations.
Two guards, both in the tool: the session advisory lock (one `-write`
run per database; a held lock is a refusal before the policy is
touched, and the server drops the lock with its connection, so a
SIGKILL cannot leave it behind), and the already-unscheduled refusal (a
paused policy is evidence of another actor — a killed attempt or a
by-hand pause — and the run will not guess which without
`-resume-paused-policy`). The integration test pins both: a `-write`
started while a second session holds the lock is refused and touches
nothing; a `-write` against an unscheduled policy is refused and leaves
it unscheduled, and succeeds with the flag and re-enables it. To find
the holder of the lock:

```sql
SELECT a.pid, a.application_name, a.backend_start, a.state, l.classid, l.objid
  FROM pg_locks l JOIN pg_stat_activity a USING (pid)
 WHERE l.locktype = 'advisory';
```

Guards, in addition to everything Step 6 already has (dry-run default,
`-from/-to` closed days only, live-tail refusal, INV-3):

- **The dry run prints the chunk plan** — count, compressed and
  uncompressed totals, the largest chunk, one line per chunk — the
  pre-flight verdict and what it would do to the policy, then walks the
  window read-only on the chunks as they are. Nothing is decompressed,
  nothing is paused.
- **Pre-flight.** `-write` refuses to start unless free space on the
  data volume EXCEEDS 2 × the largest chunk's uncompressed size, and the
  same check runs against each chunk right before its decompress. 2 × is
  a GUARD, not a bound: the decompress peaks at about 1 × plus the
  compressed copy; the UPDATEs add a new tuple version per changed row
  (non-HOT after a decompress, so heap AND index growth, up to about
  1 × more); and `compress_chunk` writes the compressed copy beside the
  bloated heap before truncating it — worst case ≈ 2 × plus the WAL all
  three generate. Free space is `statfs` on the directory the database
  reports for `trades` (`pg_tablespace_location` or `data_directory`) —
  **so run it ON r1**, as a role that can read `data_directory`. Where
  it cannot be measured the tool refuses; `-min-free-bytes N` overrides
  with a loud warning after you have checked `zfs list` / `df` yourself,
  and is then NOT re-measured per chunk.
- **Live-adjacent refusal.** A window whose `-to` + 1 day is past
  `now() - compress_after` is refused: the chunks there are deliberately
  uncompressed (the ledgerstream cursor-regression replay upserts into
  them) and the in-place walk is the right tool for them.
  `-allow-live-adjacent` walks them anyway; they are restamped in place
  and stay uncompressed.
- **`-generation` in the future is refused.** A typo there would stamp
  rows above every default run's `-max-generation` — a permanent
  lock-out on a money column.
- **A failed chunk is re-compressed before the tool exits non-zero**, on
  a context that survives SIGTERM. Only a failed re-compress leaves a
  chunk decompressed, and the error then says `LEFT DECOMPRESSED` with
  the `compress_chunk` to run by hand. A failed re-enable of the policy
  says `LEFT PAUSED` the same way.
- **Resume** = rerun the same command. The tool prints it on failure as
  `RESUME:`, carrying `-generation N` (so the whole span ends at ONE
  generation) and every flag that shaped the population (`-fill-null`,
  `-sources`, `-max-generation`, `-min-rel-delta`, `-slice`). Finished
  chunks are probed at dry-run cost and skipped.
- `-batch` does not apply with `-chunks` (it is refused; use
  `-chunk-batch`); `-chunk-batch`, `-min-free-bytes`, `-generation`,
  `-allow-live-adjacent` and `-resume-paused-policy` are refused without
  `-chunks`.

**What it does to the serving database while it runs.** None of this is
throttled by the heavy-job wrapper: `IOWeight=50` / `CPUWeight=50` apply
to the ops binary's own cgroup, and the decompress, the UPDATEs and the
compress are executed by the Postgres backend, at serving priority.

- `decompress_chunk` holds an `ExclusiveLock` on the chunk while it
  copies and takes an `AccessExclusiveLock` at the end — it waits for
  in-flight readers of that chunk and queues new ones behind it for that
  moment. `compress_chunk` holds an `ExclusiveLock` for its duration.
  Readers of OTHER chunks are unaffected; a query that spans the open
  chunk (a long `/v1/history` range, a CAGG refresh reaching back that
  far) waits.
- While a chunk is decompressed, every scan over it reads a heap an order
  of magnitude larger (15 × on the window's totals: 379 GB / 25 GB;
  160 GB for the largest chunk). Nothing in the serving path reads
  months-old `trades` rows routinely, but a CAGG refresh over that span
  would — do the Step 3 refreshes AFTER the run, never interleaved.
- I/O: each chunk is written out once (the decompress, about its
  uncompressed size), rewritten in part (the UPDATEs), then read and
  written again (the compress) — about 379 GB × 2 of writes over the
  window plus WAL. Budget the run in DAYS, not hours. There is no
  measured per-chunk figure yet: the first chunks in range order are the
  small ones, and **their progress lines print seconds and bytes for
  exactly this — read the first few and extrapolate before the 160 GB
  chunk starts.**

**SIGKILL.** `systemctl stop <unit>.scope` — the watchdog's low-disk
trip, or a by-hand stop — is SIGTERM, then SIGKILL when the scope's
`TimeoutStopSec` expires. SIGTERM cancels the run: the batch in flight
rolls back (the committed ones stay committed), then the tool
re-compresses the open chunk and re-enables the policy. That cleanup is
what the bound has to cover, and it is why `run-heavy-job.sh` now
renders the scope with `-p TimeoutStopSec=` from
`HEAVY_JOB_STOP_TIMEOUT` instead of leaving it on systemd's 90 s
default — 90 s cannot re-compress a 160 GB chunk. **The wrapper's own
default is `5min`**, which is right for the payloads that die on SIGTERM
at once and wrong for this one, so **this job's launch line sets
`HEAVY_JOB_STOP_TIMEOUT=2h` itself** (step 2 below). 2h is a budget, not
a measurement (160 GB at ~22 MB/s of uncompressed input); the per-chunk
progress lines print seconds and bytes, so **record the largest chunk's
re-compress from the first run** and tighten the launch value to it. A
value below 90 s, or one systemd would read as a different unit than you
meant (a bare integer is SECONDS — `2` is two seconds, not two hours),
is refused by the wrapper before the job starts. The bound is host
state: r1 has it only after `--tags heavy-job-wrapper` is applied (see
`docs/operations/runbooks/ops-job-stalled.md` § "Stopping a wrapped job"
for the by-scope stop, the pre-apply `systemd-run --scope` smoke check,
what the long bound disarms in the disk watchdog, and the post-kill
checks), and a scope already running keeps the 90 s it was created with.

If the kill does land — an unapplied host, a run started before the
apply, a re-compress that outlives the bound — SIGKILL drops the
connection, the server aborts `compress_chunk` (the chunk stays
DECOMPRESSED, not corrupt), and the process is gone before it can print
`LEFT DECOMPRESSED` or `RESUME:`, or re-enable the policy. For exactly
this the tool prints the by-hand repair to stderr BEFORE each decompress
and each re-compress:

```
chunk 41/90 _timescaledb_internal._hyper_1_31000_chunk [...]: re-compressing — ...
    SELECT compress_chunk('_timescaledb_internal._hyper_1_31000_chunk');
    SELECT alter_job(1000, scheduled => true);
```

After ANY exit that did not end in the tool's own summary, check both,
from the job log or directly:

```sql
-- chunks the policy would have compressed by now and has not:
SELECT chunk_schema, chunk_name, range_start, range_end
  FROM timescaledb_information.chunks
 WHERE hypertable_name = 'trades' AND NOT is_compressed
   AND range_end < now() - interval '7 days';
-- the policy itself (scheduled must be true):
SELECT job_id, scheduled FROM timescaledb_information.jobs
 WHERE proc_name = 'policy_compression' AND hypertable_name = 'trades';
```

Compress a listed chunk by hand, or let the re-enabled policy do it at
its next fire — a rerun of the tool will NOT: it restores the state it
lists, and that chunk now lists as uncompressed. The run lock is gone
with the killed connection; the policy is NOT — it stays paused, and a
rerun REFUSES while it is. Either set `scheduled` back to true by hand
and run the `RESUME:` command (or the same command again), or run it
with `-resume-paused-policy` and let the tool own the paused policy and
re-enable it at exit. The `RESUME:` line never carries
`-resume-paused-policy`: it is printed only on an exit that has already
re-enabled the policy.
(The follow-up this section carried — give the heavy-job scope a stop
timeout long enough for the largest re-compress — is codified as of
2026-09-04 in
`configs/ansible/roles/archival-node/tasks/14-stellarindex-services.yml`
and covered by `scripts/ci/run-heavy-job-test.sh`: the wrapper takes the
bound from `HEAVY_JOB_STOP_TIMEOUT` and validates it, and this job's
launch line carries `2h`. It is host state until the
`heavy-job-wrapper` tag is applied to r1 — `grep -c TimeoutStopSec
/usr/local/sbin/run-heavy-job.sh` reads `0` there as of 2026-09-04, so
until it reads `1` a stop of this job still SIGKILLs at 90 s.)

Operator sequence (replaces steps 2–5 of the Step 6 mechanics for this
span). **The right edge is `-to 2026-07-21`** — the window issue #372
reconciled against (fills with `-fill-null` = +$27.17M). 2026-07-20 and
2026-07-21 are not free of changes: the `-report` table above measured
193,056 rows differing on 2026-07-20 (net −$423, the long tail of small
wrong-leg corrections), and both days sit in the same 7-day chunk
`[07-16, 07-23)` the run decompresses for 07-19 anyway, so they cost a
scan, not a decompress. The acceptance window is the write set:
`-day 2026-07-21 -days 202` is exactly [2026-01-01, 2026-07-21] (`-day`
is the LAST day, `-days` counts back from it), and it is byte-for-byte
the `acceptance:` line the tool prints in its summary.

What the command touches, and what it deliberately leaves:

- **touched**: every `trades` row with `ts` in [2026-01-01, 2026-07-22)
  whose source is in the DEX registry, whose base leg is an XLM form
  (`native` or its SAC), whose quote leg is not a declared USD peg,
  whose `derive_generation` is at or below the run's, and whose stored
  `usd_volume` differs from the anchor's value — including, with
  `-fill-null`, rows stored NULL that the anchor can price;
- **left, by design**: rows at `ts ≥ 2026-07-22`, already at
  `derive_generation` 1785871528 from the 2026-08 re-derive and outside
  the window; exact-tier rows (a USD-pegged quote) — Step 5's
  population, never this one's; rows outside the DEX / XLM-base /
  non-pegged population (CEX and FX rows, token/token pairs, pairs with
  XLM on the quote side); rows the anchor declines (reported, never
  blanked); and the chunks in [2026-01-01, 2026-03-12), which have no
  candidate — they are probed read-only and skipped without a
  decompress, which is why `-from 2026-01-01` costs a scan and nothing
  else.

```sh
# 0. on r1, as root; env sourced (28P01 trap)
set -a; . /etc/default/stellarindex; set +a
# 1. dry run — chunk plan + pre-flight verdict + policy job + per-chunk
#    row counts; decompresses nothing, pauses nothing, takes no lock:
stellarindex-ops usd-volume-restamp -config /etc/stellarindex.toml \
  -tier xlm-base -chunks -from 2026-01-01 -to 2026-07-21 -fill-null
# 2. the run, under the heavy wrapper, UNIQUE job name per attempt.
#    Value repair AND coverage fill in one pass (-fill-null): each chunk
#    is decompressed once; a second -fill-null pass would decompress all
#    90 again. ONE attempt at a time: the tool refuses a second while
#    the first holds the run lock.
#    HEAVY_JOB_STOP_TIMEOUT=2h: this job's SIGTERM cleanup is a
#    re-compress of the open chunk (up to 160 GB), which the wrapper's
#    5min default would SIGKILL through. It is a budget, not a
#    measurement — tighten it to the largest chunk's measured
#    re-compress after the first run. The wrapper prints the bound it
#    used; the value is refused below 90 s, and a bare integer is
#    SECONDS (write "2h", never "2").
HEAVY_JOB_STOP_TIMEOUT=2h \
/usr/local/sbin/run-heavy-job.sh usd-xlmbase-chunks-try1 \
  /usr/local/bin/stellarindex-ops usd-volume-restamp \
    -config /etc/stellarindex.toml -tier xlm-base -chunks \
    -from 2026-01-01 -to 2026-07-21 -fill-null -write
#    on a non-zero exit: read the error (LEFT DECOMPRESSED? LEFT PAUSED?
#    STOPPED — re-compressed underneath?), then run the RESUME: line it
#    printed, with a NEW job name. After a SIGKILL (no summary, no RESUME
#    line): the two SQL checks above first; the rerun refuses while the
#    policy is unscheduled unless -resume-paused-policy.
# 3. the 12 CAGG refreshes the tool prints at the end, in the ORDER it
#    prints them (prices_1m first, twap_1h/twap_1d last), psql on r1,
#    under a heavy-job scope; then force the asset_volume_24h rollup.
# 4. acceptance — byte-for-byte the command the tool prints in its summary:
stellarindex-ops verify-usd-volume -config /etc/stellarindex.toml -day 2026-07-21 -days 202
```

Not covered by `-chunks`: `-tier exact` (Step 5). Its population is a
66-day class of `[base_pegged] sdex` rows and it is a set-based UPDATE
per slice, not a per-row one; if it ever measures like the above, the
same chunk walk applies and should be extended to it rather than
decompressing by hand.

## Explicitly OUT of scope here (queued, do not silently absorb)

- **`classic_assets.slug` backfill** (194,057 rows, all NULL → CODE is
  the public slug; root enabler of the `/assets/USDT` impersonator
  page). Needs a disambiguation scheme decision — coupled fix with the
  explorer's build-cache winner-picking (`page.tsx` `bySlug`
  last-write-wins vs `bySlugCI` first-wins is case-dependent).
- **Catalogue-listing `price_usd` gating**: the `/v1/assets` listing
  SQL computes `price_usd` from a 7-day `prices_1m` window +
  XLM-triangulation, outside the substance gate. The explorer now
  LABELS it; gating it server-side (same substance predicate, SQL-side
  or post-query) is the remaining ungated aggregated-price surface.
- ~~**`usd-volume-restamp` ops tool** (launch-plan §"queued buildable")
  for the pre-07-23 pegged-row era — unrelated class, still open.~~
  BUILT — see Step 5 below. Still a separate class from the tier-3b
  re-derive above; run it as its own heavy job.
- Pre-deploy dirty tail: any rows written between this doc's authoring
  and the step-0 deploy join the span automatically (the range is
  pinned at execution time by L_HI).
