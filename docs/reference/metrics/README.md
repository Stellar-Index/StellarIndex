# Metrics Reference

Every metric the Stellar Index binaries emit, with its labels, type,
and purpose. Lint `scripts/ci/lint-docs.sh` section 3 enforces
round-trip: any metric declared in `internal/obs/metrics.go` MUST
appear here, and vice versa.

Declaration source of truth: `internal/obs/metrics.go`.
Emission sites: `grep -rn <metric_name> internal/ cmd/`.

## HTTP layer (emitted by the API binary only)

The indexer also exposes an HTTP mux (for `/metrics` + `/healthz`)
but deliberately does NOT wrap it with `obs.HTTPMetrics`
middleware — every Prometheus scrape would otherwise inflate
`http_requests_total`. These counters reflect only the public API
request path.

### `http_requests_total`

Counter, labels `method`, `route`, `status`.

Counts every request served by `obs.HTTPMetrics` middleware. `method`
is canonicalised via `normalizeMethod` (uppercase-only for standard
verbs to bound cardinality). `route` is the Go 1.22 pattern path with
the method prefix stripped, or `"unmatched"` for 404s. `status` is
numeric; `"499"` is NGINX's "client closed request" — emitted when
the caller's ctx cancelled before the handler wrote.

### `http_request_duration_seconds`

Histogram, labels `method`, `route`.

Handler latency including time-in-middleware. Buckets 1ms – 10s with
extra resolution at the 200ms / 500ms SLO boundaries.

### `stellarindex_dependency_up`

Gauge, label `dependency`. Emitted by the API binary only.

The result of each `/v1/readyz` readiness check — `1` when the
dependency answered, `0` when it did not. Covers `postgres`, `schema`,
`redis` and `clickhouse`. Overwritten on every readiness round, so it
reflects the most recent check rather than a high-water mark.

**When to look at this.** Almost always because
`stellarindex_dependency_down` paged. The dependency label is what
decides how much weight to give it, because the dependencies are not
equally covered elsewhere:

- **`clickhouse`** — this is the *only* signal. ClickHouse is the raw
  lake (ADR-0034) and is the one dependency on r1 with no Prometheus
  exporter of its own, unlike postgres, redis and minio. Before this
  metric existed, the lake disappearing had no single symptom: the
  served API keeps working from the Postgres served tier while ingest
  silently stops advancing.
- **`postgres`, `redis`** — corroboration. Their exporters
  (`pg_up`, redis_exporter) fire alongside and carry more detail. This
  gauge firing *without* them points at the path between the API and
  the dependency — credentials, pool exhaustion, network — rather than
  the dependency itself.
- **`schema`** — the migration state the binary expects versus what the
  database has. Fires after a deploy whose migration did not run.

Alert on `== 0`, never on `absent()`: the gauge is published for
failing checks too, so a zero is a real answer, whereas absence would
also mean "check renamed" or "API restarting". A dependency missing
from the output entirely is a publisher bug, not a healthy dependency —
`internal/api/v1/dependency_up_test.go` pins that, and the assertion
gathers the metric family rather than reading through
`WithLabelValues`, which would create the child on read and make an
unpublished series indistinguishable from a genuine `0`.

Runbook: [dependency-down.md](../../operations/runbooks/dependency-down.md).

### `stellarindex_ingest_gap_ledgers`

Gauge, labels `source`, `table`.

Total missing ledgers in contiguous data-coverage gaps >= the
detector's `min-gap-size` threshold (1000 by default) per
(`source`, `table`). **Data-derived** complement to the
cursor-derived density projection in `/v1/diagnostics/ingestion`:
cursor coverage measures process state ("did we walk this ledger")
and can read 100% while data is missing; this gauge measures
reality by querying each per-source hypertable's distinct-ledger
coverage directly. Refreshed periodically by the gap detector
goroutine in the aggregator binary
(`internal/storage/timescale.RunGapDetector`). The `table` label
disambiguates the sources that share one hypertable (e.g. the
trades-table sources `sdex` / `soroswap` / `phoenix` / `comet` /
`aquarius`, or the `oracle_updates` sources `band` / `redstone` /
`reflector-*`). 26 targets are registered today
(`internal/storage/timescale/per_source_gaps.go`), spanning the
Soroban projections, the classic SDEX path, and the off-chain
oracle tables — NOT `soroban-events` alone.

### `stellarindex_ingest_gap_count`

Gauge, labels `source`, `table`.

Number of contiguous gaps per (`source`, `table`) at the
detector's most recent cycle. A single 100K-ledger gap and 100
ten-ledger gaps both report ≈100K in
`stellarindex_ingest_gap_ledgers` but very different shapes; chart
this alongside the size gauge to distinguish "one big halt"
(cascade signature) from "many small drops" (flaky-write pattern).

### `stellarindex_ingest_gap_max_size_ledgers`

Gauge, labels `source`, `table`.

Size of the largest contiguous gap per (`source`, `table`) at the
detector's most recent cycle. Drives the
`stellarindex_ingest_gap_detected` P1 alert (fires when > 1000
sustained 15 min).

### `stellarindex_ingest_source_distinct_ledgers`

Gauge, labels `source`, `table`.

Count-distinct of ledgers in the per-source hypertable at the
most recent gap-detector cycle. The **numerator** of the ADR-0031
data-derived density signal:

```
density(source) = stellarindex_ingest_source_distinct_ledgers / (tip - genesis + 1)
```

Where `tip` comes from
`stellarindex_ingest_gap_detector_tip_ledger` and `genesis` is the
per-source first-deploy ledger (hard-coded in the diagnostic
handler's source-genesis map). Dense sources (SDEX, Soroswap)
approach the [genesis, tip] span; sparse-by-design sources (Blend
auctions, CCTP) are naturally lower because the contract doesn't
emit per ledger.

**Count source per target.** For every target the numerator is
`COUNT(DISTINCT ledger)` over the target's own hypertable, EXCEPT
`source="soroban-events"`, which since 2026-08-28 reads the
`ledger_ingest_log` census instead (`COUNT(*) WHERE
soroban_event_count > 0` over the scan window — a PK range scan).
`soroban_events` has no index on `ledger`; the generic count was a
556 s full scan of a 257 GB hypertable per cycle and took r1's
serving path down. The census is the indexer's LCM-derived record of
"this ledger carried >= 1 eligible contract event", written
post-enqueue (after `ProcessLedger` returns, before the sink drains —
not a rows-in-Postgres marker), and equals the observed-row count by
the ADR-0033 Claim 3 invariant; a divergence is a persistence
shortfall that `stellarindex-ops verify` reconciliation surfaces, not
this gauge. Two honest edges: a sink-writer halt makes this density
read *high* (census >0, rows absent) while the `_gap_*` gauges stay
observed-row-based; and `stellarindex-ops backfill` does not populate
`ledger_ingest_log`, so a range repaired without `census-backfill`
(ADR-0033 recovery §1) reads density 0 for soroban-events.
The gap gauges for that target (`..._gap_*`) still come from a scan
of `soroban_events` itself. Same gauge, same
`source_coverage_snapshots` row, same `density_pct` in
`/v1/diagnostics/ingestion` — only the source of the number differs
(`GapDetectorTarget.DistinctLedgerCountSQL`).

### `stellarindex_ingest_gap_detector_tip_ledger`

Gauge (no labels).

The live ledgerstream cursor's `last_ledger` at the most recent
gap-detector cycle's start — the upper bound used by every scan.
ADR-0031 consumers subtract per-source genesis from this to
compute the density denominator. One gauge for the whole detector
because every target uses the same tip in the same cycle.

### `stellarindex_ingest_gap_detector_runs_total`

Counter, labels `source`, `table`, `outcome`.

Periodic gap-detector cycle outcomes — `ok` on a clean scan,
`error` on a Postgres / timeout failure (a non-ok outcome is also
logged loudly as `gap-detector: scan failed` with `elapsed_s` so a
timeout is unmistakable). A climbing `{outcome="error"}` rate is the
diagnostic when the silent-detector alert fires. Do NOT alert on
`rate({outcome="ok"}) == 0`: the heavy targets scan once per 6h, so
their ok counter is pinned at 1 within a process life and 1 → 1 across
a restart is invisible to `rate()` (it only detects a decrease) — that
false-fired the silent alert for >7h on 2026-07-06. Liveness is keyed
off `stellarindex_ingest_gap_detector_last_success_unix` instead.

### `stellarindex_ingest_gap_detector_last_success_unix`

Gauge, labels `source`, `table`.

Wall-clock timestamp (Unix seconds) of the most recent SUCCESSFUL
per-(source, table) scan. This is the reset-proof liveness primitive
the `stellarindex_ingest_gap_detector_silent` ticket-tier alert keys
off: it fires on `(time() - gauge) > 8h`. A wall-clock stamp survives
restarts correctly where a rarely-incrementing counter does not — a
healthy startup scan re-stamps it to `now()`, clearing staleness
immediately, while a genuinely wedged target's stamp stops advancing.
Only advances on a clean scan; an errored/timed-out scan leaves the
previous stamp untouched so staleness grows. A target that has never
once succeeded since process start emits no series here (that case is
covered by the `runs_total{outcome="error"}` rate).

### `stellarindex_ingest_gap_detector_duration_seconds`

Histogram, labels `source`, `table`, `outcome`.

Wall-clock latency of one detector cycle. The LAG()-over-DISTINCT
scan against the large hypertables is slow on r1 — the
`soroban_events` scan measures ~300 s against ~50 M distinct
ledgers, which is why the buckets extend to 600 s and the scan
timeout was raised past the original 60 s cap.

### `stellarindex_projector_lag_ledgers`

Gauge, labels `source`.

Distance (in ledgers) between the projector's per-source cursor
and the live ledgerstream tip at the end of the last cycle. 0 =
caught up. Drives the `stellarindex_projector_lag_high` alert (P3
ticket: > 256 ledgers sustained 10 min). See ADR-0032.

### `stellarindex_projector_runs_total`

Counter, labels `source`, `outcome`.

Per-cycle outcome counter. Outcomes: `ok` (cursor advanced, nothing
dropped), `idle` (caught up, no rows in scan range), `error` (scan /
cursor read / cursor write failed; cursor not advanced — retried next
cycle), `sink_retry` (a sink write held the cursor below a ledger for
retry), `decode_degraded` (the cursor advanced but at least one
decode-failed row was dropped — a clean-looking advance that is NOT
`ok`; DATA-6 / NS-2). Drives the `stellarindex_projector_error_rate_high`
alert (on `error`).

### `stellarindex_projector_events_decoded_total`

Counter, labels `source`, `outcome`.

Number of consumer.Events the projector emitted to its sink.
Outcomes: `ok` (decode succeeded), `decode_error` (Reconstruct or
Decoder.Decode returned non-nil, or a decoder panic was recovered; row
skipped, cursor still advances), plus the sink dispositions
`sink_retry` / `sink_permanent` / `sink_quarantined`. A sustained
per-source `decode_error` rate drives the
`stellarindex_projector_decode_error_rate_high` alert (DATA-6 / NS-2) —
that pattern is a decoder regression draining a whole class of events,
not the odd poison row.

### `stellarindex_projector_cycle_duration_seconds`

Histogram, labels `source`.

Wall-clock latency of one projector cycle (scan + decode + sink).
Each cycle is bounded by `PerSourceTimeout=60s`. Sustained p99 >
30s for one source is the first sign that the sink is the
bottleneck.

### `stellarindex_projector_wedged`

Gauge, labels `source`.

Per-source cursor-wedge flag. `1` = the adaptive window has bottomed
out at the `MinBatchLimit` floor (25 ledgers) AND the source has failed
to commit forward progress for `WedgeCycles` (5) consecutive cycles — a
floor-sized range that stays over `PerSourceTimeout` (a dense +
compressed chunk) is retried identically every cycle forever, a stuck
cursor that will not self-recover. `0` = healthy. Cleared on any
advancing (or caught-up) cycle. Seeded at `0` per source at startup so
the alert reads a real zero rather than "no data". Drives the
`stellarindex_projector_wedged` alert (ticket; manual remediation — raise
the per-cycle budget or decompress the range). See ADR-0032.

### `stellarindex_projector_replay_window_active`

Gauge, labels `source`.

`1` while the source's projector cursor is still INSIDE an
operator-recorded `projector-replay` rewind window (the
`projection_dirty_windows` row migration 0125 added, written by the
replay tool before it rewinds); `0` otherwise. Published by ONE projector
goroutine (not per source) every `ReplayWindowRefreshInterval` (30s) from
a single query; its first pass runs at startup, which is also what seeds
`0` for every registered source so the alert reads a real zero rather
than "no data".

The discriminator between an INTENDED lag and a real one (issue #325):
`stellarindex_projector_lag_high` carries `unless
stellarindex_projector_replay_window_active == 1`, so the ~4h ticket a
2.5M-ledger rewind used to raise — which told the operator nothing and
masked any genuine lag on the same source — is excused, while
`stellarindex_projector_replay_stalled` fires if the replay stops
advancing inside that window.

Three bounds keep the excuse narrow:

1. **Provenance.** Only a window written by `projector-replay` counts
   (the `reason` column). `projected-rebuild -write` records into the
   same table but never rewinds the live cursor, and its range is **not**
   kept below that cursor: `-to` defaults to the live cursor, its
   one-writer guard admits `liveLastLedger >= to` (equality), and
   `-allow-live-overlap` bypasses the guard entirely (used on r1
   2026-07-27). Without the provenance gate, a source HELD at such a
   window's ledger — a sink-retry hold, a poison hold, a wedge — would
   have its lag ticket suppressed with no operator rewind on record.
2. **Cursor, exclusive at the top.** The flag clears the moment the
   cursor regains its pre-rewind position, not when the dirty-window row
   is finally cleared by `compute-completeness` (up to a day later); a
   projector wedged exactly AT that ledger stays alertable. The lower
   bound is `from_ledger - 1`, where the replay parks the cursor.
3. **Fail open.** A dirty-window read error publishes `0` for every
   source, so a monitoring-side failure can never silence a real lag
   ticket.

Known residual: the table holds one row per source and the upsert widens
it (`LEAST`/`GREATEST`) while keeping the newest `reason`, so a replay
recorded while a `projected-rebuild` window is still pending inherits the
higher `to_ledger` and the flag expires there rather than at the replay's
own pre-rewind position. Still provenance-gated, still cursor-bounded,
still expiring — and it needs both tools pending on the same source at
once.

### `http_request_success_duration_seconds`

Histogram, labels `method`, `route`.

The non-5xx twin of `http_request_duration_seconds`. The HTTP
middleware records into this histogram only when the response status
is < 500 (and not 499 / client-aborted). Pair with
`http_request_duration_seconds_count` for the latency-SLO ratio so a
fast 5xx burns budget: numerator counts fast successes; denominator
counts all requests including errors. Added in 2026-05-28 to close
F-0105 (audit 2026-05-26) — pre-this-PR the SLO ratio reported a
5ms 500 as a "good fast" response.

### `stellarindex_api_cache_ops_total`

Counter, labels `cache`, `op`, `result`.

Every read through the API's in-memory cache wrappers
(`v1.CachedMarketsReader`, future `v1.CachedCoinsReader`, …)
increments this counter. `cache` is the wrapper name (e.g.
`markets`); `op` is the cached method (`distinct_pairs` /
`source_markets` / `asset_markets` / `all_pools`). `result` is a
READ outcome — `hit` (returned cached value, including
single-flight-wait callers that piggy-backed on an in-progress
upstream call), `stale` (served an expired value while a background
refresh runs) or `miss` (called upstream) — or one of two
side-events: `refresh_error` (a background refresh failed) and
`evicted` (a bounded cache dropped its oldest entry to admit a new
key; emitted by the `observations` and `oracle` wrappers, which cap
their maps because their key is caller-controlled on a public route).

Use to detect prewarm-key drift: when a prewarm goroutine warms
key A but the handler looks up key B, `result="miss"` rate
stays high even though the prewarm cycle is running. Suggested
alert: `rate(stellarindex_api_cache_ops_total{result="miss"}[5m]) /
rate(stellarindex_api_cache_ops_total{result=~"hit|miss|stale"}[5m]) >
0.5` sustained for 10 min on any (cache, op) pair — for hot ops the
prewarm should keep miss rate under 10%. Keep the denominator
filtered to the read outcomes: `evicted` increments once per admitted
key, so an unfiltered ratio can never exceed 0.5 during the
key-enumeration storm the alert is meant to catch.

A sustained `result="evicted"` rate is itself the signal that a
bounded cache is being churned — a working set larger than its cap, or
an anonymous caller walking the key space.

### `stellarindex_api_sparkline7d_rows_total`

Counter, label `result`.

One increment per listing ROW for which `/v1/assets?include=sparkline7d`
asked the batch reader for a 7-day price series: `served` when the
series came back with at least one actual price, `empty` when it came
back with nothing to draw. Only rows that publish a price are counted —
a scam-gated, substanceless, or simply priceless row deliberately has no
chart and is never looked up.

The batch reader returns a bucket skeleton (7 days, null price) for
every id it is asked about, so "the map had an entry" is not evidence of
data — which is how #355 shipped a chart column of dashes for the entire
verified top of `/assets` (the listing asked for the series under the
catalogue slug `xlm` / `aqua` rather than the Stellar `asset_id`) with
no error, no log, and a byte-identical response. Suggested alert:
`rate(stellarindex_api_sparkline7d_rows_total{result="empty"}[15m]) /
rate(stellarindex_api_sparkline7d_rows_total[15m]) > 0.5` sustained for
30 min — half the priced rows on the directory rendering an empty chart
is a lookup-key or pricing-pipeline regression, not a quiet market.

## Ingestion (indexer binary)

### `stellarindex_source_events_total`

Counter, label `source`.

Every event the live indexer sink attempts to persist for that
source. Emitted from `internal/pipeline/sink.go`, not the retired
legacy orchestrator path. Zero rate + `source_enabled=1` backs the
`source-stopped` alert.

### `stellarindex_source_enabled`

Gauge, label `source`.

`1` for sources the current indexer enabled from config at startup;
`0` during shutdown or when the source is not configured. Used to
qualify source-level alerts so intentionally disabled sources do not
page.

### `stellarindex_source_last_event_unix`

Gauge, label `source`. Unix-seconds timestamp of the most recent
event dispatched to the sink. Dashboards use it for a last-seen clock.

### `stellarindex_source_last_insert_unix`

Gauge, label `source`. Wall-clock Unix-seconds timestamp of the
most recent SUCCESSFULLY-inserted trade row per source (i.e.
`Store.InsertTrade` returned with `rowsInserted == 1`, not
`ON CONFLICT DO NOTHING`).

Pairs with `stellarindex_source_last_event_unix` to expose the
stuck-cursor / duplicate-flood pattern: when the dispatcher matches
events (last_event climbs) but every insert hits the ON CONFLICT
short-circuit (last_insert flat-lines), the gap between the two
grows. Direct alert template:

    time() - stellarindex_source_last_insert_unix{source="sdex"} > 3600

catches the live r1 2026-05-28 pattern (157 SDEX insert-attempts/
min, every one a duplicate, max(ts) 11 h old) within an hour of
recurrence. Complements `stellarindex_trade_insert_outcome_total`'s
rate-shape signal with a timestamp-shape signal that doesn't
require sustained traffic to fire.

### `stellarindex_source_matched_events_total`

Counter, label `source`.

Per-source count of inputs (events, contract calls, entry changes,
classic ops) the decoder's `Matches()` claimed. The DENOMINATOR of
decoder error-rate — chart
`rate(stellarindex_source_decode_errors_total[5m]) /
rate(stellarindex_source_matched_events_total[5m])` per source.
Bumped pre-Decode so a decoder that matches then errors still
counts; error-rate stays interpretable (errors / attempted) rather
than tautological (errors / successful).

Distinct from `source_events_total` — that's downstream of decoding
(decoder OUTPUT, what reaches the sink). A decoder that buffers
(soroswap swap+sync correlation) or matches an intermediate event
producing zero outputs would register here but not on
`source_events_total`.

### `stellarindex_source_decode_errors_total`

Counter, label `source`.

Per-event parse failures — SCVal shape mismatch, malformed XDR,
canonical-invariant violations. Distinct from `orphan_events`
(events were well-formed but partnerless) and `insert_errors`
(decoded fine but persistence broke). Emitted from dispatcher stats
deltas after each processed ledger. Denominator is
`stellarindex_source_matched_events_total`.

Includes recovered decoder PANICS — see
`stellarindex_decoder_panics_total` below, which is the strict subset
of this counter that crashed rather than refused.

### `stellarindex_decoder_panics_total`

Counter, label `source`.

Decoder panics the dispatcher recovered and converted into a decode
error (#371 F1). Incremented in `internal/dispatcher/panic_guard.go`
from all four dispatch seams — Soroban events, contract calls, classic
ops, ledger-entry changes — and covering `Matches` as well as `Decode`,
because a decoder that crashes deciding whether it owns an input is as
broken as one that crashes parsing it.

**When to look at this:** the moment it is non-zero. A recovered panic
means a decoder is dropping every event of that shape, silently, and
will keep doing so until a fixed binary ships. Nothing else in the
ingest path complains: the input is skipped, the ledger completes, the
cursor advances, and the only downstream tell is `/v1/coverage` turning
`complete=false` for that window after the next ADR-0033 re-derive.

Before the guard existed this class of bug had a much louder failure
mode and a much worse one: the panic escaped to the LEDGER-level
recover in `internal/pipeline/processor.go`, which discarded every
source's outputs for the ledger and returned an error that exited the
indexer process. systemd restarted it onto the same cursor, the same
input panicked, and `StartLimitBurst` restarts later the unit parked in
`failed` — one decoder's bug stopping all ingest indefinitely. The
counter is what buys the right to skip instead.

Chart it against `stellarindex_source_decode_errors_total` (this is a
strict subset) — the shape matters: one increment is a single poison
event; a value climbing every ledger is a whole event schema the
decoder no longer understands, i.e. that source is dark from that
ledger on.

Alert: `stellarindex_decoder_panicked` (`> 0`, page) → runbook
[decoder-panicked](../../operations/runbooks/decoder-panicked.md).
The expression deliberately reads the raw value rather than
`increase(...)`: the counter is process-lifetime and a poison input
typically panics once, so the series is born at 1 and never moves —
`increase()` over a series that first appears inside its own lookback
window evaluates to 0 and the page would never fire.

An ABSENT series means "this process has recovered no decoder panic
since boot". Because the alert is a raw-value comparison rather than a
rate, absence is unambiguous, which is why this counter is deliberately
NOT pre-seeded in `seedBoundedLabelSeries` the way the `increase()`- and
`rate()`-based counters are.

### `stellarindex_source_unknown_symbols_total`

Counter, label `source`.

Asset slots in an otherwise-decoded oracle event whose symbol or
feed-id isn't in our canonical asset allow-list (ADR-0010 fiat,
ADR-0014 crypto, ADR-0028 RWA / RedStone feed registry). Since the
oracle capture-totality change the slot is **recorded verbatim as a
`raw:<symbol>` row** (`canonical.AssetOracleRaw`) rather than
dropped, so the counter means "recorded as raw", not "lost".
Distinct from `decode_errors`: the rest of the event still decoded
cleanly. Reflector, Band, and Redstone are the live emitters; CEX
streamers don't fan out into mixed-asset batches the same way. A
sustained non-zero rate means an upstream oracle expanded its feed
set and our allow-list / feed registry needs an amendment — once it
lands, a re-derive promotes the raw rows in place on the same PK.
F-1234 (codex audit-2026-05-12).

Alert: `stellarindex_ingestion_oracle_unknown_symbols` (any per-source
increase over a trailing 25 h, sustained 30 min — the window exceeds
Band's daily cadence so it cannot flap) → runbook
[oracle-unknown-symbols](../../operations/runbooks/oracle-unknown-symbols.md).
The 2026-08-04 cold audit found this counter had no consumer at all
while r1 carried 7,794 dropped Reflector slots. The oracle decoders
now record unmapped slots verbatim as `raw:<symbol>` rows
(`canonical.AssetOracleRaw`, oracle capture-totality design PR-2) and
the counter keeps incrementing — a raw row is still a mapping gap to
close.

Alert: `stellarindex_ingestion_oracle_unknown_symbols` (any per-source
increase over a trailing 25 h, sustained 30 min — the window exceeds
Band's daily cadence so it cannot flap) → runbook
[oracle-unknown-symbols](../../operations/runbooks/oracle-unknown-symbols.md).
The 2026-08-04 cold audit found this counter had no consumer at all
while r1 carried 7,794 dropped Reflector slots. Once the oracle
decoders record unmapped slots verbatim as `raw:<symbol>` rows
(`canonical.AssetOracleRaw`, oracle capture-totality design) the counter
keeps incrementing — a raw row is still a mapping gap to close.

### `stellarindex_source_unrepresentable_symbols_total`

Counter, label `source`.

Oracle asset slots **dropped** because the published symbol / feed-id
cannot be held even by the record layer's verbatim `raw:` namespace:
empty, longer than 64 bytes, or carrying a byte outside printable
ASCII `0x21-0x7E` (`canonical.NewOracleRawAsset`).

Deliberately separate from `stellarindex_source_unknown_symbols_total`.
That counter means "recorded as `raw:<symbol>`" — the row exists and a
later allow-list / feed-registry entry promotes it in place. This one
is a **hole**: nothing was written, so closing it needs the registry
entry *and* a replay of the affected ledgers. One shared series would
send operators hunting for raw rows that do not exist.

Only an ScString-keyed oracle can reach it in practice: RedStone
feed_ids are `ScString` (arbitrary bytes, unbounded length) while
Reflector / Band symbols are `ScSymbol`. The refusal is per-SLOT, not
per-event (#291) — `write_prices` batches every updated feed into one
event, so an event-level refusal would take all ~19 RedStone feeds
dark until a code change, the inverse of oracle capture-totality. The
dropped slot's identity is in the accompanying WARN log line
(`redstone: dropping slot with unrepresentable feed_id`).

Alert: `stellarindex_ingestion_oracle_unrepresentable_symbols` (any
per-source increase over a trailing 25 h, sustained 30 min — same
window as its unknown-symbols sibling so a daily-cadence oracle cannot
flap) → runbook
[oracle-unknown-symbols](../../operations/runbooks/oracle-unknown-symbols.md).

### `stellarindex_source_orphan_events_total`

Counter, label `source`.

Events that arrived but never correlated into a complete observation.
Soroswap: swap without matching sync (or vice versa). Phoenix:
incomplete N-of-8 field set aged past the buffer's 5-min ceiling.
Aquarius / Reflector don't emit orphans — they're 1-event-per-
observation. Emitted from decoder-maintained orphan counters via the
live dispatcher path.

### `stellarindex_external_poller_polls_total`

Counter, labels `source`, `outcome` ∈ {success, error, skipped}.

Per-source, per-outcome count of `PollOnce` invocations from the
external-poller runner. Emitted on every poll tick of every
configured external source (CoinGecko, CoinMarketCap, CryptoCompare,
ECB, ExchangeRatesAPI, PolygonForex, Binance, Coinbase, Kraken,
Bitstamp). The `skipped` outcome covers the per-poller cooldown path
(e.g. CoinGecko's post-throttle backoff) — distinct from `success`
so absence-of-success alerting isn't masked by the poller silently
respecting a backoff window.

### `stellarindex_cex_stream_disconnect_total`

Counter, labels `source`, `reason` ∈ {reset, broken_pipe, timeout, dial, server_requested, other}.

Per-source, per-reason count of CEX WebSocket stream disconnects from
the Binance and Bitstamp streaming sources. `reset` is the most common
on r1 (Binance proactively recycles connections every 6–12 min); a
sustained rate of `dial` or `timeout` means the venue is unreachable
or our keepalive isn't recovering the socket. Combined with
`stellarindex_external_poller_last_success_unix` (when the streamer
emits trades the runner forwards to the poller's success channel),
operators can distinguish "stream churning but data flowing" from
"stream stuck and we're losing the venue". F-0029 (audit-2026-05-27)
fix landed alongside this metric — bounded 5–60 s exponential backoff
with a healthy-connection reset path, plus TCP keepalive on the
dialer.

### `stellarindex_external_poller_last_success_unix`

Gauge, label `source`.

UNIX-seconds timestamp of the most recent successful `PollOnce` per
external source. Zero / unset when the poller has never succeeded
since process start. Companion to
`stellarindex_external_poller_polls_total`: a gauge makes "data is
stale by N minutes" expressible as `time() - <gauge>` rather than
multi-window rate math, which simplifies alerting (see
`stellarindex_external_poller_stale`).

### `stellarindex_external_fx_last_quote_unix`

Gauge, label `source`.

UNIX-seconds timestamp of the most recent successful `fx_quotes` WRITE
from the active fiat-FX feed (`massive`, the `internal/sources/external/forex`
worker in the API binary). Advances ONLY when `InsertFXQuoteBatch`
commits a non-empty batch — a failed write or an empty snapshot
(upstream returned no usable rates) leaves the prior stamp untouched, so
a wedged-but-erroring worker cannot keep the feed looking fresh.

Deliberately SEPARATE from `stellarindex_external_poller_last_success_unix`:
`massive` does not run under the `external.Connector` poller framework
(it writes `fx_quotes` directly, out of band from the poller runner), so
it emits no `external_poller` series at all. The triangulation
forex-snap (`Store.FXQuoteAtOrBefore`) reads `fx_quotes` with a **7-day
lookback** to price every fiat-quoted pair, so a dead feed prices fine
off a stale row for up to a week before fiat pairs silently break.

When to look: `time() - <gauge>` is the feed's age. Healthy is < 1 h
(r1's forex worker writes exactly hourly). The
`stellarindex_external_fx_feed_stale` alert fires at 6 h (well below the
7-day cliff); the companion `stellarindex_external_fx_feed_absent` fires
when the series is missing entirely (worker never wrote since startup).

### `stellarindex_external_fx_rate_rejected_total`

Counter, labels `source`, `reason`.

Upstream FX rates the forex worker **refused to persist** because they
failed its sanity band, before they could reach `fx_quotes` (C2-030,
audit-2026-07-23). `fx_quotes` is the denominator of every fiat-quoted
`usd_volume` the X2.5 triangulation derives, so one mis-scaled bar — a
decimal shift, a unit-scale change, a redenomination applied upstream
without a ticker change — would silently re-scale that currency's whole
conversion history. `persistSnapshot` previously wrote whatever the
upstream said.

`reason` is bounded at seven values: `deviation` (moved > 50% from the
last accepted rate for that ticker with no confirming second fetch),
`non_positive` (rate ≤ 0 — `1/rate` feeds `InverseUSD`, so it would
poison the row both ways), `non_finite` (NaN / ±Inf),
`history_deviation` (a trailing-7d history bar > 50% off the current
accepted rate), `history_deviation_stuck` (the same bar, within 1%,
refused ≥ 12 consecutive times — excluded from the rejection alert),
`deviation_history_conflict` (a two-fetch confirmation vetoed because
the ticker's heal-grade trailing-7d majority refutes the candidate —
a persistently-broken current feed repeating its own bad bar, the
Massive UZS case; history refuses the confirm but never sets the
baseline), and `deviation_history_conflict_stuck` (the same vetoed
candidate, within 1%, refused ≥ 12 consecutive times — excluded from
the rejection alert like `history_deviation_stuck`).
Deliberately NOT
labelled by ticker: ~150 currencies would be pure cardinality for a
signal whose actionable question is "is the feed producing junk". The
rejected ticker is on the worker's WARN log line
(`forex: rejected upstream rate`).

Zero-seeded so an untripped band is a real zero, not "no data".

### `stellarindex_external_fx_baseline_healed_total`

Counter, label `source`.

The forex worker re-pointed a ticker's sanity-band baseline at the
median of ≥ 4 mutually-agreeing trailing-7d history bars that refuted a
still-unconfirmed single-sample bootstrap baseline (the 2026-08-24
Massive UZS poisoned-bootstrap incident). Rare by design — each
increment is one wrong baseline corrected without operator action; the
ticker is on the worker's WARN log line. Confirmed baselines are never
healed (an agreeing-but-wrong history endpoint must not overwrite a
corroborated current rate — the MR-1 direction).

When to look: a single rejection is expected and self-healing — the
guard is two-strike, so a genuine devaluation confirmed by the next
hourly fetch is accepted one refresh later. **Sustained** non-zero means
a ticker is wedged on its last accepted rate while the upstream keeps
disagreeing; that is what `stellarindex_external_fx_rate_rejections`
alerts on.

### `stellarindex_amm_self_pair_swap_total`

Counter, label `source`.

AMM swap events decoded as a **self-pair swap** (`token_in == token_out`)
and dropped to zero rows. A self-pair swap moves no value between distinct
assets and has **no honest purpose** — it is the primitive the 2026-08-25
Blend/Comet exploit ran ~390 times to walk a pool's spot price, defeating
the freeze + divergence guards (which never saw it: the self-pair rows
decode to `(nil, nil)` and never reach the served `trades` table; only the
raw event lands in `soroban_events`). Incremented at the decoder drop point
(comet `dispatcher_adapter`). **Detection only** — it changes no serving or
freeze decision.

When to look: historically `comet` emitted **zero** of these before the
exploit window, so any sustained count is an exploit-shaped signal, not
noise — it drives the `stellarindex_amm_self_pair_swap_burst` alert. Find
the offending tx/signer in `soroban_events` (topic POOL/swap on the pool
contract).

### `stellarindex_external_dust_dropped_total`

Counter, label `source`.

Streamed CEX trades dropped at ingest as **dust** — the quote leg is
below ~$0.001 (the 10^8-scale floor `minStreamQuoteUnits`). CEX feeds
emit sub-microcent fills whose tiny integer amounts make `quote/base` a
meaningless round fraction (1/8, 1/10, …); ingested, they set the
**unweighted** OHLC high/low (`max/min(quote/base)`) and produced absurd
wicks on the served `/v1/ohlc` API while carrying ~zero real volume.

When to look: a non-trivial rate here is expected and healthy (it's the
noise we're filtering out). A sudden drop to zero for a normally-dusty
venue (e.g. `coinbase`) can mean the streamer wedged — cross-check
`stellarindex_external_poller_last_success_unix` / the CEX stream
disconnect counter.

### `stellarindex_discovery_dropped_hits_total`

Counter, no labels.

Discovery hits dropped because the async discovery sink buffer was
full. Covers all three sniffers sharing the one sink — SEP-41 token
sightings ([internal/canonical/discovery.Sniff]) plus the broader
oracle-suggestive event and event-less-call sightings added per
docs/architecture/generic-oracle-sep-onboarding.md §3(b)
(`SniffOracleEvent` / `SniffOracleCall`) — the counter is not
labeled per-sniffer, so a spike doesn't distinguish which lane
dropped; check `stellarindex-ops discovery list`'s `KIND` column
after resolving the underlying pressure if that matters. Emitted by
the live indexer from periodic `DroppedCount` sampling, not only at
shutdown, so operators can alert on sustained loss while the process
is still running. Any non-zero increase means discovery coverage is
degrading under recorder pressure; this is best-effort data loss, not
a backpressure signal on the main ingest path. With in-process dedup
(per `stellarindex_discovery_skipped_hits_total`) healthy
steady-state should never drop — a non-zero rate typically means a
Postgres outage or cold-start burst.

### `stellarindex_discovery_skipped_hits_total`

Counter, no labels.

Discovery hits skipped because their dedup key (contract_id + kind +
symbol) had already been enqueued in this process and the recorder
upserts on the same key — re-enqueue is wasted work. Same shared
sink as `stellarindex_discovery_dropped_hits_total` (SEP-41 +
oracle-suggestive event/call sightings). Emitted by the live indexer
from periodic `SkippedCount` sampling. A high ratio of Skipped to
(Skipped + Recorded) is expected and healthy: most contract
events/calls are duplicates from already-discovered contracts.
Tracked for capacity-planning visibility, not for alerting. A process
restart resets the dedup set; the first push for any key after
restart still records (no-op upsert if already in DB).

### `stellarindex_discovery_record_failures_total`

Counter, no labels.

Discovery hits whose `Recorder.Record` write FAILED (Postgres
error / timeout) — distinct from
`stellarindex_discovery_dropped_hits_total` (buffer-full pre-write
drop) and `stellarindex_discovery_skipped_hits_total` (in-process
dedup). Before this counter (audit-2026-07-16 C4-3), a Record failure
in the async sink was only logged, so a persistent recorder outage
silently stopped `discovered_assets` from growing with no metric or
alert. Emitted by the live indexer from periodic `FailedCount`
sampling, mirroring how `stellarindex_discovery_dropped_hits_total` is
fed. Best-effort by design (the contract re-appears on a later event),
so this counts failed write ATTEMPTS, not permanent loss — but a
sustained non-zero rate means discovery coverage is degrading and is
alerted by `stellarindex_ingestion_discovery_record_failures`.

### `stellarindex_metrics_registry_present`

Gauge, label `component`. Value 0/1.

Boot-time detectability gauge (audit-2026-07-16 C4-4): `1` when the
named component was wired with a Prometheus Registry (its SDK-side
metrics are live), `0` when it is running Registry-less (those SDK
metrics never register). Set per component at startup on the binaries
that build the affected config; an absent series means "component not
used on this binary" and is not alerted. The concrete case is
`component="ledgerstream"`: `pipeline.LedgerstreamConfig` deliberately
leaves `Config.Registry` nil (the SDK's metric registration panics on
the repeated `Stream` calls the archive→live→catch-up path makes), so
the SDK `BufferedStorageBackend` buffer metrics
(`buffer_fetch_latency_seconds` etc.) are not exported. Alerted by
`stellarindex_metrics_registry_absent`; remediation in
`runbooks/metrics-registry-absent.md`.

**NOTE (W5-mon-3):** this gauge no longer implies the ledgerstream-tier
`both_missing` page is dead. `stellarindex_ledgerstream_tier_read_total`
and `stellarindex_ledgerstream_cold_read_duration_seconds` moved to
`internal/obs` package-level (registered unconditionally at boot), so
that page is live in production regardless of this gauge's value. The
gauge now tracks only the SDK buffer-metric coverage.

### `stellarindex_ledgerstream_tier_read_total`

Counter, label `outcome` (`hot` / `cold` / `both_missing`).

Tiered-datastore reads (ADR-0027 LCM cache tiering) partitioned by which
tier served the request: `hot` = local `galexie-archive` MinIO, `cold` =
`aws-public-blockchain` fallback after a hot miss, `both_missing` =
neither tier had the object (the reader is stalled). A sustained
`both_missing` increase is the **P1** data-integrity page
`stellarindex_ledgerstream_tier_both_missing`
(`runbooks/ledgerstream-tier-both-missing.md`); chart the `cold` rate as
a proxy for "is the hot trim window sized right, or am I paying
cross-Atlantic latency for ranges that should be hot?".

Emitted by `internal/ledgerstream/tiered.go`'s `TieredDataStore` and
registered unconditionally at boot in `internal/obs` — NOT gated on the
per-`Stream` registry that `pipeline.LedgerstreamConfig` leaves nil
(W5-mon-3; before that fix this metric was nil in production and the
`both_missing` page could never fire).

### `stellarindex_ledgerstream_cold_read_duration_seconds`

Histogram, label `outcome` (`ok` = cold hit / `miss` = cold not-found,
i.e. a `both_missing` read / `error` = cold transient failure).

Latency of cold-tier (AWS public bucket) reads only — includes the hot
miss → cold attempt, excludes hot-tier reads. Wider buckets than the
default (5 ms → 30 s) because cold reads are cross-Atlantic
whole-partition fetches. The paired-histogram sibling of
`stellarindex_ledgerstream_tier_read_total`; same always-registered
rationale.

### `stellarindex_source_insert_errors_total`

Counter, labels `source`, `kind` (`trade` / `oracle` / `panic` /
`unhandled` / `dropped` / `soroswap_router_swap` /
`defindex_flow_strategy` / `defindex_flow_vault`).

The `soroswap_router_swap` / `defindex_flow_*` kinds were added
audit-2026-07-16 (C4-3): those persist paths previously logged a Warn
without bumping any counter, so a dropped DEX-swap / vault-flow row was
invisible; they now count like every sibling case and are alerted by
`stellarindex_ingestion_persist_drop`.

`unhandled` fires when a source emits an event type the sink's
type-switch doesn't recognise — usually a half-wired new source
registered in `buildSources()` without a matching case in
`handleOneEvent`. Silent drops would otherwise look like "metrics
say we're ingesting" with empty tables.

`dropped` (2026-07-06 outage / ADR-0041) counts external CEX/FX
trades genuinely lost when the bounded retry buffer overflowed
(drop-oldest) or a data fault couldn't be isolated — the
vendor-refillable path. Infrastructure faults are NOT counted here:
they retry with backpressure (see
[`stellarindex_trade_insert_retries_total`](#stellarindex_trade_insert_retries_total)),
so a firing `insert-errors` alert now means genuine loss (data fault
or external overflow), not a transient outage.

Events that failed to persist to the store. `panic` kind flags a
recovered panic in the event-sink handler. A sustained rate signals
storage-layer distress; the `insert-errors` alert escalates.

### `stellarindex_cursor_last_ledger`

Gauge, label `source`.

Mirror of the committed `ingestion_cursors.last_ledger` value for the
live ledgerstream pipeline, updated after each successful cursor
upsert — so it advances only when a ledger was fully processed AND its
cursor row committed (`processAndPersistCursor`). That makes it the
liveness signal for the whole Galexie → ledgerstream → dispatcher →
Postgres path, not just for the read.

Today the indexer emits exactly one series, `source="ledgerstream"`.
Two alerts read it, and only one of them can fire for that series:

- `ledger-ingest-stalled` (**page**) — flat for 5 min, or the series
  absent, sustained 5 min (≈ 10 min without a committed ledger). The
  gauge is created on the first successful commit, so a restart that
  never gets one leaves it absent; the rule's `absent_over_time` branch
  carries the page across that handover rather than resolving a SEV-1
  while ingest is still dead.
- `cursor-stuck` (ticket) — `increase(...[5m]) == 0` joined `on (source)
  stellarindex_source_enabled == 1`. That join is why it does **not**
  cover the ledgerstream cursor: `ledgerstream` is a cursor namespace,
  not a configured source, so no `source_enabled{source="ledgerstream"}`
  series exists. It is armed for per-SOURCE cursors, of which the
  dispatcher-based ingest currently writes none.

### `stellarindex_trade_inserts_total`

Counter, labels `source`, `usd_volume_populated` (`yes` | `no`).

Per-source attempt counter for `Store.InsertTrade`, broken out by
whether `usd_volume` was populated at insert time (per L2.2 phase 1
— see `internal/storage/timescale.Store.WouldPopulateUSDVolume`).
Operators flipping on `[trades].usd_pegged_classic_assets` use this
to verify their allow-list actually covers what the indexer is
seeing. Counts attempts; the trades hypertable's `ON CONFLICT DO
NOTHING` dedupe is invisible to this counter — pair with
[`stellarindex_trade_insert_outcome_total`](#stellarindex_trade_insert_outcome_total)
below to see new-vs-duplicate.

### `stellarindex_trade_insert_outcome_total`

Counter, labels `source`, `outcome` (`new` | `duplicate`).

Per-source counter of trade-insert outcomes. `new` = the
`INSERT ... ON CONFLICT DO NOTHING` actually persisted a row;
`duplicate` = the conflict short-circuit fired and no row was
written.

On a healthy live indexer `outcome=new` tracks 1:1 with attempts;
a cursor-replay loop or stuck-tip pattern produces a fast-growing
`outcome=duplicate` rate with zero `outcome=new`. Alert on
`rate({outcome="duplicate"}[10m]) > 0.5 unless on (source) rate({outcome="new"}[10m]) > 0`
to catch the live r1-2026-05-28 signature (157 SDEX insert
attempts/min while the hypertable's `max(ts)` was 11 h old).

Use `unless`, never `and on (source) rate({outcome="new"}[10m]) == 0`:
both children are call-site-seeded, so a source that has landed no new
row since process start has **no** `outcome="new"` series at all and an
`and` join matches nothing — the alert would go silent in precisely the
post-restart replay flood it exists for (#302). `source` is
config-dependent and therefore deliberately not pre-seeded in
`obs.seedBoundedLabelSeries`.

### `stellarindex_dex_trade_unit_ratio_total`

Counter, label `source`.

**When to look at this: when the `dex_trade_unit_ratio_detected` alert
fires, or never otherwise — steady-state should be flat.** Emitted from
`internal/storage/timescale`'s `InsertTrade` + `BatchInsertTrades` —
the seam every landed trade write passes through exactly once,
regardless of whether it arrived via the dispatcher's live batch path,
the projector's per-event sink, or a `stellarindex-ops ch-rebuild` /
backfill re-derive. Bumps once per landed **on-chain** trade
(`ledger != 0` — CEX/FX trades are excluded because they normalise
amounts onto a different fixed scale, CLAUDE.md "External-source
amount scaling is NOT uniform") whose `base_amount` exactly equals its
`quote_amount`, both nonzero.

Founding incident: on 2026-07-07, a Phoenix decoder field-mapping bug
swapped/collapsed `base_amount` and `quote_amount`, landing 237k trades
at an exact 1:1 price for months undetected — ADR-0033 completeness
checks verify row PRESENCE, not economic PLAUSIBILITY, so a decoder
that writes a row for every event but gets the numbers wrong sails
through green. This counter is the cheap plausibility check that class
of bug needed. A handful of hits is normal (genuine equal-value
cross-asset fills exist); a sustained stream from one source
(`increase(...[30m]) > 25`, see `dex_trade_unit_ratio_detected`) is the
signature of a broken decoder. Runbook:
`docs/operations/runbooks/dex-trade-unit-ratio.md`.

### `stellarindex_trade_insert_retries_total`

Counter, label `outcome` (`retry` | `recovered` | `abandoned`).

The trade sink's blocking-retry path (2026-07-06 Postgres-outage
fix, ADR-0041). Before this, an infrastructure fault
(`connection refused` / PG restarting) made the sink DROP the write
while the ledger cursor kept advancing; now it retries with
backpressure instead.

- `retry` — one backoff retry attempt after an infrastructure-
  classified insert failure. **A sustained nonzero rate means the
  served-tier write path is blocked and the on-chain ledger cursor is
  NOT advancing** — data is held safely in memory, not lost. The
  `trade_insert_backpressure` alert fires on this.
- `recovered` — a previously-blocked insert (batch or row) landed
  after ≥ 1 retry. A healthy recovery shows a burst of `retry` then
  one `recovered`.
- `abandoned` — a blocked insert gave up because the context was
  cancelled mid-retry (shutdown). On-chain rows are re-derivable from
  the CH lake (ADR-0034); the exact ledger range is logged at ERROR.

Genuine drops (permanent data faults + external-buffer overflow) are
NOT counted here — they land on
[`stellarindex_source_insert_errors_total`](#stellarindex_source_insert_errors_total)
(`kind=trade` / `kind=dropped`).

### `stellarindex_trade_insert_buffer_depth`

Gauge (no labels).

Number of external (CEX/FX) trades currently held in the bounded
in-memory retry buffer, waiting to land after an infrastructure fault
(ADR-0041). External trades have no ledger cursor and are
vendor-refillable, so they are buffered and retried asynchronously
rather than blocking the pipeline; on overflow the OLDEST are dropped
(counted on `stellarindex_source_insert_errors_total{kind="dropped"}`).
On-chain trades do NOT use this buffer — they block-and-retry (cursor
gating) — so this gauge is external-only. A depth that climbs and
stays high means Postgres has been unreachable long enough that
external price freshness is degrading.

### `stellarindex_stream_publish_total`

Counter, label `stream` (currently only `price_stream`).

Per-stream counter of envelopes the API binary's
`internal/api/streampublish.Publisher` fanned out to a
`streaming.Hub`. Increments only on a NEW closed bucket — the
publisher short-circuits when `ObservedAt` hasn't advanced, so a
flat counter against an active subscription means the upstream
`PriceReader` isn't seeing new buckets (cursor stuck, aggregator
stalled, etc.). Operators read this alongside per-pair subscriber
counts to verify the closed-bucket fanout path: steady publishes
with zero subscribers means clients aren't connecting; zero
publishes with active subscribers means the producer is starved.

### `stellarindex_ch_live_sink_ledgers_total`

Counter, label `outcome` (`written` | `buffered` | `dropped` |
`errored`).

Ledgers processed by the ClickHouse real-time dual-sink (ADR-0034
#18), the inline non-blocking fan-out that keeps the Tier-1 lake
within seconds of the chain. Emitted only when the dual-sink is
enabled (`storage.clickhouse_live_sink`); the indexer's periodic
stats goroutine samples the `LiveSink`'s monotonic counters and adds
the per-tick delta.

- `written` — ledgers DURABLY flushed to ClickHouse (post-`Flush`).
- `buffered` — ledgers accepted into the in-memory buffer
  (pre-flush). `buffered − written` ≈ the unflushed backlog; a
  growing gap is the early-warning signal of a CH write stall
  before any drop happens.
- `dropped` — bounded-dropped ledgers: a full worker channel (live
  ingest out-paced the worker) or a full Sink buffer during a
  sustained CH outage (G12-01 cap, default 4096 ledgers). Dropping
  is deliberately preferred over unbounded heap growth on the
  shared r1 host; the `ch-live-catchup` gap-scan timer re-fills
  dropped ledgers and the projector stalls at the hole rather than
  losing data. A steady non-zero climb means the live edge of the
  lake is degrading and CH or the host needs attention.
- `errored` — a ledger the sink never wrote. Two producers: the
  sink's own `Add` / `Flush` failing (CH down, wedged, disk-full),
  and `clickhouse.ExtractLedger` failing in the indexer's live read
  loop, in which case the sink was never offered the ledger at all.
  Distinct from `dropped`, which is load the sink DID accept and
  then shed: a systematic extract break (a TransactionMeta version
  change fails every ledger in lock-step) is not healed by
  `ch-live-catchup`, because the catch-up re-extracts with the same
  decoder. Alerted since #371 F6 by
  `stellarindex_ingestion_ch_live_sink_errors` (ticket) — until then
  both live-sink rules matched `outcome="dropped"` only, so this
  outcome had no alert of any kind. Runbook:
  [ch-live-sink-errors](../../operations/runbooks/ch-live-sink-errors.md).

### `stellarindex_hashdb_append_total`

Counter, label `outcome` (`ok` / `error`).

Per-ledger outcome of the ADR-0016 hashdb drift detector's append
side: as the indexer's live LCM read loop reads each ledger, it
computes `sha256(LCM)` and appends the record to the on-disk hashdb
file (`internal/hashdb`). Emitted only when `[hashdb].enabled = true`
(off by default — first production exposure 2026-07-08).

When to look at it: `error` means a hashdb write failed (disk full,
permission error) — this is failure-tolerant by design (never stalls
or fails ingest), but a sustained `error` rate means the detector has
gone silently blind: it's recording nothing new. Worse, the verify
sweep will NOT tell you: its window trails the append side's own
last-recorded ledger, so when appends freeze the sweep keeps
re-verifying the same fully-recorded stale window every hour with
`outcome="ok"` — green sweeps while coverage stopped accruing. This
counter's `error` rate is therefore the ONLY signal for append death.
There's no dedicated alert on it (a deliberate call — a disabled
detector is a silent no-op, not an incident) — treat a climbing
`error` rate as "this region's hashdb coverage is a lie" and fix the
underlying disk/permission issue.

### `stellarindex_hashdb_append_duration_seconds`

Histogram, label `outcome` (matches `stellarindex_hashdb_append_total`).
Buckets 10 µs – 10 ms.

Latency of the per-ledger `hashdb.Append` call, which runs
SYNCHRONOUSLY on the ingest hot path (a deliberate choice — see
`recordHashdb`'s doc comment in `cmd/stellarindex-indexer/main.go`:
it's a single O(1) positional file write, cheap enough that async
plumbing would add more risk than it removes). A latency regression
here directly costs ingest throughput; buckets top out at 10 ms
because anything reaching that tier already indicates a disk problem
worth investigating on its own.

### `stellarindex_hashdb_verify_runs_total`

Counter, label `outcome` (`ok` / `drift` / `error`).

Per-sweep outcome of the ADR-0016 hashdb drift detector's periodic
verify side: every `[hashdb].verify_interval_minutes` (default 60),
the indexer re-reads a trailing window of `[hashdb].verify_window_ledgers`
(default 20000) ledgers from the same bucket the append side reads
and compares each one's freshly-computed hash against the recorded
value (`internal/archivecompleteness.HashDBWindowVerifier`).

When to look at it: `drift` means the sweep found at least one
ledger whose current bytes don't match what the indexer originally
recorded — see `stellarindex_hashdb_drift_total` and the
`hashdb-drift-detected` runbook, this is the serious case.  `error`
means the sweep itself couldn't complete (bucket unreachable, hashdb
file I/O error) — the detector went blind, not "found nothing". A
healthy, opted-in region should show a steady `ok` rate at roughly
one observation per verify interval.

### `stellarindex_hashdb_verify_run_duration_seconds`

Histogram, label `outcome` (matches `stellarindex_hashdb_verify_runs_total`).
Buckets 1 s – 30 min.

Wall-clock of one full verify sweep — re-reading thousands of ledgers
from S3/MinIO is not cheap, unlike the append side. Chart `ok` p95/p99
to see whether the window size (`[hashdb].verify_window_ledgers`) is
still comfortably inside the sweep interval; if `ok` durations
approach `verify_interval_minutes`, either shrink the window or widen
the interval so sweeps don't start overlapping.

### `stellarindex_hashdb_drift_total`

Counter, unlabelled (deliberately — the natural label, ledger
sequence, is unbounded cardinality).

Cumulative count of individual ledgers the periodic verify sweep has
found drifted (recorded hash != freshly-observed hash) since the
indexer process started. Any nonzero value means either upstream
retroactively rewrote a previously-fetched ledger's bytes, or the
region's local copy of that ledger is corrupted — the founding case
was ledger 63332650 (2026-07-08). This is the metric
`stellarindex_hashdb_drift_detected` alerts on
(`deploy/monitoring/rules/hashdb.yml` +
`configs/prometheus/rules.r1/hashdb.yml`); the drifted sequences
themselves are only in the indexer's ERROR-level "hashdb DRIFT
DETECTED" log line, not on the metric (cardinality). See the
`hashdb-drift-detected` runbook for the full investigation +
mitigation path.

## Oracle layer (indexer binary, reflector + future sources)

### `stellarindex_oracle_stream_rows_unparsed_total`

Counter, labels `source`, `field` (`asset` | `quote`).

`oracle_updates` rows dropped from the served stream because their
stored canonical text would not parse.

**When to look at this:** it should be flat at zero forever. Any increase
means rows are silently absent from `/v1/oracle/streams` and the
explorer `/oracles` page — they are not being served and no error is
returned to anyone. Check it after ANY hand-written relabel of
`oracle_updates.asset` / `.quote`: that column has no `CHECK`
constraint, so a typo drops the row from the served surface rather than
erroring, which looks from the outside exactly like a successful
relabel.

`field` tells you which column to inspect without a query.

### `stellarindex_oracle_last_update_unix`

Gauge, labels `source`, `asset`.

Unix-seconds timestamp of the most recent oracle observation for the
(source, asset) pair. `oracle-stale` alert compares to
`oracle_resolution_seconds`.

### `stellarindex_oracle_resolution_seconds`

Gauge, label `source`.

Declared publication cadence of the oracle (Reflector: 300 s). Set
once at source construction. Used by `oracle-stale` to make "> 10×
resolution" tractable without hard-coding per-source intervals in
the rule.

## API layer (api binary)

### `stellarindex_price_staleness_seconds`

Gauge, label `asset`.

Age of the most recent price served for `asset` via `/v1/price`, in
seconds. Updated per request so a popular asset keeps a fresh
reading; unqueried assets stop updating and the `price-stale` alert
uses `change()` to distinguish "no-update" from "updated-but-stale".

### `stellarindex_sep1_cache_ops_total`

Counter, label `result` (`hit` / `miss` / `upstream_error`).

SEP-1 resolver cache outcomes. Operators watch `hit / total` for
cache effectiveness and `upstream_error` rate for issuer-side
outages. `upstream_error` deliberately doesn't cache — a 404 from
an issuer is a real signal, typically transient.

### `stellarindex_ratelimit_fail_open_total`

Counter, no labels.

Requests that bypassed rate-limiting because the Redis backing store
errored. The middleware fails open deliberately (Redis outage
shouldn't take down the API); this metric gives ops a quantitative
signal that correlates with `redis` readyz turning red.

Alert: `stellarindex_ratelimit_fail_open` →
[ratelimit-fail-open](../../operations/runbooks/ratelimit-fail-open.md).

### `stellarindex_monthly_quota_fail_open_total`

Counter, no labels.

Requests that bypassed the per-key **monthly quota ceiling** because
the month-to-date counter read errored
(`internal/api/v1/middleware/monthly_quota.go`). The exact twin of
`stellarindex_ratelimit_fail_open_total` and deliberately the same
shape: the middleware fails open on purpose, because the cap is a
billing-fairness mechanism rather than a security boundary and a
Redis blip must not 429 paying customers.

The exposure runs the other way round from the rate limiter's,
though: while this is open a metered key bills past its agreed cap
and the overage cannot be reclaimed, because the requests were
served. Pre-seeded at zero so "quiet" is distinguishable from
"dead" (C3-082, audit-2026-07-23 — before this counter the only
trace was a `logger.Debug` line, below the API's production log
level).

Alert: `stellarindex_monthly_quota_fail_open` →
[monthly-quota-fail-open](../../operations/runbooks/monthly-quota-fail-open.md).

### `stellarindex_monthly_quota_fail_closed_total`

Counter, no labels.

The dwell-guarded companion to
`stellarindex_monthly_quota_fail_open_total`. The middleware fails
**open** on a transient month-to-date read error (a Redis blip must not
429 paying customers), but only inside a dwell window (default 30s,
matching `stellarindex_ratelimit_fail_open`'s dwell). Once the counter
has been unreadable continuously for longer than the dwell window the
middleware flips to fail-**closed** — it rejects with `429` +
`Retry-After` and increments this counter instead
(`internal/api/v1/middleware/monthly_quota.go`, W1-flow-register-4).

The exposure it closes: without the dwell inversion, a key already
at/past its cap bills unmetered for the *entire* duration of a counter
outage, unrecoverable once served. A non-zero rate here means the usage
backend has been down long enough that metered customers are now being
429'd — the mirror-image alerting concern to the fail-open counter, and
worth a distinct signal. Pre-seeded at zero so "quiet" is
distinguishable from "dead".

### `stellarindex_admin_audit_write_failures_total`

Counter, label `surface` (`account_override` / `key_mint` /
`key_revoke` / `status_notice` / `staff_customer_lookup`).

Privileged mutations that **completed** but whose durable audit row
failed to append. Every one of these call sites appends best-effort:
the mutation has already committed when the append runs, so the
error is logged and swallowed rather than propagated (un-doing a
committed tier change because the audit store blipped would be
worse). The consequence is that a tier override, a minted or revoked
credential, or a public status notice can be live with no record of
the actor, the reason, or the previous values.

`staff_customer_lookup` is the one **read** in the set (C3-056):
`GET /v1/account/admin/lookup` returns another customer's billing
email, tier, status and every user's email + last-login. Nothing is
mutated, which is precisely why the audit row is the only evidence
the access happened — a lost row here is an unrecorded PII access,
not an unrecorded change.

Non-zero means the admin audit trail has holes that must be
reconstructed from application logs before their retention window
closes. All five label values are pre-seeded at zero (C3-067,
C3-056, audit-2026-07-23).

Alert: `stellarindex_admin_audit_write_failing` →
[admin-audit-write-failing](../../operations/runbooks/admin-audit-write-failing.md).

### `stellarindex_admin_key_budget_clamps_total`

Counter, label `outcome` (`lowered` / `failed`).

API **credentials** — not clamp calls — whose per-minute budget was
lowered to their account's tier ceiling by
`Server.clampKeyBudgetsToTier` (the admin
`PATCH /v1/admin/accounts/{id}` override path). One account
downgrade that lowers four keys adds 4.

`failed` is the one that matters operationally: the downgrade errored
and the credential KEPT its old, higher budget, i.e. over-tier
throughput is still live past the downgrade.

Deliberately **not alerted** — downgrades are routine. The counter
exists because a clamp is invisible from the customer's side (their
key keeps authenticating and simply starts 429-ing sooner), so
support needs to be able to tie "my key started rate-limiting" to a
billing event, and an accidental mass-clamp from a bad tier map
should not be invisible. Both label values pre-seeded at zero.

### `stellarindex_aggregator_ticks_total`

Counter, label `outcome` (`ok` / `error`).

One increment per aggregator orchestrator tick. `error` fires when
at least one (pair, window) refresh inside the tick failed — a tick
with all-pair-success records as `ok`. Per-pair errors still surface
as soft warnings; this counter is the tick-level rollup operators
watch for sustained instability.

### `stellarindex_aggregator_vwap_writes_total`

Counter, no labels.

Cumulative VWAP cache writes performed by the aggregator. Pair-level
detail intentionally excluded — Prometheus cardinality stays bounded
and the per-pair lens lives in the Redis key namespace
(`vwap:<base>:<quote>:<window>`). Operators alert on a sustained
zero-rate as the "aggregator is silent" signal.

### `stellarindex_aggregator_vwap_cache_write_errors_total`

Counter, no labels.

Cumulative count of failed Redis `SET` attempts during the VWAP
cache write step in `internal/aggregate/orchestrator/orchestrator.go`.
The aggregator returns an error and the next tick retries — but
from the customer surface, sustained failures here mean
`/v1/price` returns 404 on every cached pair (rewritten,
triangulated, stablecoin-proxy paths) while the Timescale-direct
paths continue serving. Surfaces the May-10 SEV-2 incident class
(`internal/incidents/data/2026-05-10-redis-writes-blocked-disk-full.md`)
where Redis BGSAVE failed for ~9 h and the only customer signal
was 404s on rewritten pairs because `flags.stale` was not flipped
(the aggregator process was alive and ticking, just unable to
publish). Operators alert on `rate(...[5m]) > 0` for ≥ 2 min as
the upstream-of-stale signal.

### `stellarindex_aggregator_empty_windows_total`

Counter, no labels.

Count of (pair, window) refreshes that produced zero VWAP-eligible
trades after class filtering, stablecoin expansion, and outlier
filtering. The `vwap_writes / empty_windows` ratio surfaces pair
coverage gaps without per-pair cardinality cost — a sustained
all-empty signal usually means the configured pair set has
out-grown the live data.

### `stellarindex_aggregator_window_truncated_total`

Counter, no labels.

Count of (pair, window) trade fetches that hit `MaxTradesPerWindow`
(default 10,000) — i.e. the window held more trades than the
per-query cap, so the VWAP was computed over only the **newest**
`cap` of them (F-1319; the truncation keeps the most-recent slice, not
the oldest). A non-zero rate means a busy pair/window is being
aggregated over a partial slice. Chart `rate(...)` against
`stellarindex_aggregator_vwap_writes_total`; sustained firing means the
cap (or the window) needs raising, or that window should move to a
SQL-side aggregate. Unlabelled to keep cardinality bounded — the
per-pair lens lives in the WARN log line the orchestrator emits
alongside each increment.

### `stellarindex_aggregator_stream_publish_total`

Counter, label `outcome` (`ok` / `error`).

Closed-bucket events handed to the orchestrator's
[`StreamPublisher`](../../../internal/aggregate/orchestrator/orchestrator.go)
(L3.9 SSE fan-out). Production wiring is the Redis-pub/sub
publisher in `internal/api/streaming/redispub`; the API binary's
matching subscriber republishes each event on the in-process
`streaming.Hub` so `/v1/price/stream` clients receive the
fan-out. `outcome="error"` is best-effort failure (publish
errored; the next tick retries; the VWAP cache write itself
is unaffected).

### `stellarindex_api_stream_subscribe_total`

Counter, label `outcome` (`ok` / `decode_error` / `malformed`).

Closed-bucket Redis pub/sub messages the API binary's
[`Subscriber`](../../../internal/api/streaming/redispub/subscriber.go)
processed (L3.9 SSE fan-out, consumer side). `ok` = decoded and
republished on the local `streaming.Hub` so `/v1/price/stream`
SSE subscribers receive the event. `decode_error` = JSON
unmarshal failed (wire-format drift between aggregator's
Publisher and this Subscriber — investigate if non-zero).
`malformed` = JSON decoded but Asset or Quote was empty (no
valid topic to route to; message dropped). All paths log; only
the `ok` path forwards.

### `stellarindex_api_cors_decisions_total`

Counter, label `outcome` (`no_origin` / `allowed_origin` /
`allowed_wildcard` / `denied`).

Per-request CORS decisions emitted by the API binary's CORS
middleware. `no_origin` = request had no Origin header (server-
to-server, curl); `allowed_origin` = exact-match allow-list hit;
`allowed_wildcard` = wildcard policy (`*`) matched; `denied` =
Origin header present but not in the allow-list (browser will
block the response).

The pre-existing `warnOpenCORS` startup-only check fires once at
boot then drifts out of memory. This counter is the per-request
companion — operators dashboard cross-origin traffic patterns and
alert when a wildcard policy starts handling real cross-origin
traffic in production (the silent failure mode of
`STELLARINDEX_ALLOWED_ORIGINS=*` slipping into prod with
credentialed auth_mode). F-1244.

### `stellarindex_customer_webhook_delivery_attempts_total`

Counter, label `outcome` (`delivered` / `server_error` /
`client_error` / `exhausted` / `network_error` / `webhook_missing` /
`disabled` / `no_secret` / `build_error` / `list_error` /
`lookup_error` / `mark_error`). All twelve are pre-seeded at boot
(`obs.seedBoundedLabelSeries`, #368 M6) so `increase()`/`rate()` read
a real zero rather than "no data" before an outcome's first
occurrence — without that, the window containing the FIRST
`exhausted` or `mark_error` evaluates to 0 and the alert stays
silent for exactly the event it exists to report.

Per-attempt outcome of the customer-webhook delivery worker
(`internal/customerwebhook`). `delivered` = 2xx response;
`server_error` = 5xx (scheduled for retry); `client_error` =
4xx (terminally failed — the customer's URL is broken);
`exhausted` = retry budget hit; `network_error` = TCP/TLS/timeout
(retry); `webhook_missing` = registry row deleted mid-flight
(terminal); `disabled` = `webhook.Enabled=false` (terminal);
`build_error` = malformed URL (terminal); `list_error` /
`mark_error` = db transport failure on the queue surface
(transient).

Operator alerts:

```
rate(...{outcome="server_error"}[5m]) > 0.1
  # one customer's URL is sustained-failing — open a ticket

rate(...{outcome="exhausted"}[1h]) > 0
  # a delivery permanently failed after 15 retries — drag the
  # deliveries log

increase(...{outcome="mark_error"}[30m]) > 0
  # the POST completed and the write recording it did not, so the
  # row keeps its claim lease and the SAME payload is re-POSTed
  # every lease interval — a duplicate-delivery loop with no retry
  # budget to end it. Its own rule rather than an arm of the
  # server_error one: a single wedged row is ~0.003/s, thirty times
  # under that rule's 0.1/s threshold.
```

F-1270 (audit-2026-05-12); `mark_error` alert + seeding #368 M6.

### `stellarindex_customer_webhook_fanout_failures_total`

Counter, labels `event_type` (`incident.sev1` / `incident.resolved` /
`anomaly.freeze` / `divergence.firing` / `price.alert`) × `reason`
(`invalid_payload` / `list_subscribers` / `enqueue`).

The **producer-side** counterpart to the delivery-attempts counter
above. `customerwebhook.Fanout.Publish` writes one
`webhook_deliveries` row per subscriber when a product event fires;
this counts the events that never became a row at all — so unlike a
failed delivery attempt there is **no retry**, no dead-letter, and
nothing downstream that re-derives it. The subscribed customer has
permanently lost the event.

`enqueue` increments once per lost **delivery** (per subscriber);
`list_subscribers` and `invalid_payload` increment once per lost
**fan-out**, where the loss covers every subscriber of that event
type.

Emitted by the aggregator binary (freeze + divergence hot paths) and
by `stellarindex-ops emit-incident`; the ops CLI is short-lived and
unscraped, which is why that call site also exits non-zero. Every
`event_type` × `reason` pair is pre-seeded at zero — a fan-out that is
quiet because it is healthy must not look like a metric nobody emits,
which is the exact silence this counter closes.

Alert: `stellarindex_customer_webhook_fanout_failing` →
[customer-webhook-fanout-failing](../../operations/runbooks/customer-webhook-fanout-failing.md).

C3-023 (audit-2026-07-23).

### `stellarindex_usage_rollup_sweeps_total`

Counter, label `outcome` (`ok` / `scan_error` / `sink_error`).

Per-sweep outcome of the API binary's usage-rollup worker
(`internal/usage.Rollup`), which folds the Redis per-endpoint
request counters (written by `middleware.UsageTracker`) into the
`usage_daily` Timescale hypertable every 5 minutes. That table
backs the per-endpoint rows on `/v1/account/usage` and the
dashboard's usage analytics.

When to look at it: the dashboard's per-endpoint usage table has
stopped advancing (today's row frozen) or `/v1/account/usage` has
degraded to endpoint-less legacy rows. Sustained `scan_error` =
Redis trouble on the SCAN/HGETALL pass; sustained `sink_error` =
Postgres upsert failing (connectivity, or migration 0071 missing on
this deployment). Counters keep accumulating in Redis with a 35-day
TTL, so short outages lose nothing — the next successful sweep
catches up. Informational severity: customer pricing traffic is
unaffected. Alert: `stellarindex_usage_rollup_failing`
(deploy/monitoring/rules/api.yml + configs/prometheus/rules.r1/api.yml).

### `stellarindex_usage_rollup_sweep_duration_seconds`

Histogram, label `outcome` (matches
`stellarindex_usage_rollup_sweeps_total`). Buckets 5 ms – 30 s.

Wall-clock of one full sweep: Redis SCAN + one HGETALL per active
(subject, day) hash + one batched Timescale upsert. Chart `ok`
p95/p99 separately from the error outcomes — "sweep slow" (key
population growing with the customer base, Postgres contention) is
an earlier, different signal from "sweep failing". A healthy sweep
with tens of active subjects sits well under 100 ms; approaching
the 5-minute cadence means sweeps start overlapping their schedule
and the rollup lag becomes user-visible on the dashboard's "today"
row.

### `stellarindex_protocol_events_rollup_sweeps_total`

Counter, label `outcome` (`ok` / `refresh_error`).

Per-sweep outcome of the aggregator's protocol-events rollup worker
(`internal/aggregate/protoeventsrollup`, #43), which folds the
trailing-24h per-source event census (a UNION ALL count over ~17
served protocol hypertables) into the `protocol_events_24h` table
every couple of minutes. That table backs the `events_24h` column on
`/v1/protocols` and `/v1/protocols/{name}`, so the handler reads a
keyed-on-PK lookup instead of running the multi-second census per
request (the 2026-07-06 latency incident).

When to look at it: the explorer's protocol pages show a frozen
`events_24h`. Sustained `refresh_error` = the census/upsert
transaction is failing (Postgres unreachable, or migration 0086
missing on this deployment). The rollup keeps its last-good rows, so
the column goes stale, not blank. Informational severity: customer
pricing traffic is unaffected. Alert:
`stellarindex_protocol_events_rollup_failing`
(deploy/monitoring/rules/aggregator.yml + configs/prometheus/rules.r1/aggregator.yml).

### `stellarindex_protocol_events_rollup_sweep_duration_seconds`

Histogram, label `outcome` (matches
`stellarindex_protocol_events_rollup_sweeps_total`). Buckets 10 ms – 30 s.

Wall-clock of one rollup sweep: the trailing-24h UNION ALL census over
the served protocol hypertables + one upsert + one prune. This is the
multi-second leg the #43 rollup moved off the `/v1/protocols` request
path, so watching `ok` p95/p99 here is how an operator learns the
served-tier census is getting heavier as the protocol tables grow —
long before it would have shown up as a slow endpoint.

### `stellarindex_asset_volume_rollup_sweeps_total`

Counter, label `outcome` (`ok` / `refresh_error`).

Per-sweep outcome of the aggregator's asset-volume rollup worker
(`internal/aggregate/assetvolrollup`, #43), which folds the trailing-24h
per-asset USD-volume SUM over the `prices_1m` continuous aggregate
(single-sided: each asset as base OR quote) into the `asset_volume_24h`
table every couple of minutes. That table backs the `volume_24h_usd`
column on the `/v1/assets` listing, so the listing LEFT JOINs a
keyed-on-PK lookup instead of the ~256k-row per-request scan the
2026-07-06 latency incident measured (~4.8s cold).

When to look at it: the explorer's assets list shows a frozen
24h-volume column or a stale volume-ranked order. Sustained
`refresh_error` = the sum/upsert transaction is failing (Postgres
unreachable, or migration 0087 missing on this deployment). The rollup
keeps its last-good rows, so the column goes stale, not blank.
Informational severity: customer pricing traffic is unaffected. Alert:
`stellarindex_asset_volume_rollup_failing`
(deploy/monitoring/rules/aggregator.yml + configs/prometheus/rules.r1/aggregator.yml).

### `stellarindex_asset_volume_rollup_sweep_duration_seconds`

Histogram, label `outcome` (matches
`stellarindex_asset_volume_rollup_sweeps_total`). Buckets 50 ms – 60 s.

Wall-clock of one rollup sweep: the trailing-24h base-OR-quote SUM over
`prices_1m` (all pairs) + one upsert + one prune. This is the heaviest
of the two #43 rollups and the query the rollup moved off the
`/v1/assets` request path, so watching `ok` p95/p99 here is how an
operator learns the served-tier volume scan is getting heavier as the
prices_1m history grows. If it climbs toward the 2-minute cadence the
sweeps start overlapping and the rollup lag becomes user-visible.

### `stellarindex_asset_character_rollup_sweeps_total`

Counter, label `outcome` (`ok` / `refresh_error`).

Per-sweep outcome of the aggregator's asset-volume-character rollup worker
(`internal/aggregate/assetcharacterrollup`, wash-and-scam-signals design
§2), which folds the trailing-window all-asset account-structure roll over
the `trades` hypertable (each trade counted on BOTH sides, folded onto its
canonical asset) into the `asset_volume_character` table (migration 0149).
That table backs the `volume_character` label + signals on `/v1/assets`
(listing) and `/v1/assets/{id}` (detail), so both read a keyed-on-PK lookup
instead of the ~4s per-request trades roll (measured 4.09s on the USDC
detail, tripping the 4s per-request timeout → null).

When to look at it: asset pages show a stale or missing `volume_character`
label, or the directory's demote-adjusted order stops moving. Sustained
`refresh_error` = the roll/upsert transaction is failing (Postgres
unreachable, or migration 0149 missing on this deployment). The rollup keeps
its last-good rows, so the label goes stale, not blank. Informational
severity: `volume_character` is analytics-only — pricing, verification, and
the raw `volume_24h_usd` chain fact are unaffected.

### `stellarindex_asset_character_rollup_sweep_duration_seconds`

Histogram, label `outcome` (matches
`stellarindex_asset_character_rollup_sweeps_total`). Buckets 50 ms – 120 s.

Wall-clock of one rollup sweep: the all-asset trailing-window `trades` roll
(with unordered account-pair aggregation) + batched upsert + prune. This is
the heaviest asset rollup and the query the rollup moved off the
`/v1/assets{,/{id}}` request path, so watching `ok` p95/p99 here is how an
operator learns the roll is getting heavier as the `trades` history grows —
long before it would surface as a slow endpoint. If it climbs toward the
15-minute cadence the sweeps start overlapping.

### `stellarindex_price_alert_eval_total`

Counter, label `outcome` (`ok` / `list_error` / `partial_error`).

Per-sweep outcome of the aggregator's price-alert evaluator
(`internal/pricealerts`, BACKLOG #60), which checks every enabled
`price_alerts` row against the latest closed 1-minute VWAP each tick
and enqueues account-scoped `price.alert` customer-webhook deliveries
when a threshold is crossed (respecting cooldown + `last_fired_at`).
Only emits when `[price_alerts] enabled = true`.

When to look at it: customers report their price-threshold webhooks
stopped firing. `list_error` = the `ListEnabledPriceAlerts` read
failed, so the WHOLE sweep was skipped — nothing is being evaluated
(Postgres unreachable, or the `price_alerts` table is superuser-owned
per migrations/README rule 7). `partial_error` = the sweep ran but at
least one alert hit a price-read / parse / enqueue error; narrower and
self-heals per-alert. Notifications-only degradation — the public
pricing surface is unaffected. Alert:
`stellarindex_price_alert_eval_failing`
(deploy/monitoring/rules/price-alerts.yml +
configs/prometheus/rules.r1/price-alerts.yml).

### `stellarindex_price_alert_eval_duration_seconds`

Histogram, label `outcome` (matches
`stellarindex_price_alert_eval_total`). Buckets 5 ms – 30 s.

Wall-clock of one full evaluation sweep: one enabled-alerts read +
per-alert VWAP point-reads + per-fire webhook enqueues. Chart `ok`
p95/p99 separately from the `list_error` fast-fail path. A healthy
sweep over a handful of alerts sits well under 100 ms; approaching the
tick cadence (default 30 s) means the alert set or per-account webhook
fan-out has grown enough that sweeps overlap.

### `stellarindex_assets_popular_priceless`

Gauge, no labels.

Count of assets that, at the most recent coverage-check sweep, are
market-popular yet priceless with no recorded reason
(`internal/pricelesscoverage`, task #28 Part B). "Popular" is measured
on MARKET-CHARACTER volume — 7-day priced USD volume > $10k OR > 5k
trades, with volume concentrated in a single `(maker, taker)` account
pair EXCLUDED so a volume-painting wash farm (the scam-AUD pattern)
cannot self-select in. "Priceless" = no servable USD/XLM-proxy price;
"no recorded reason" = its recent 24 h market clears the substance serve
floor, so the gate is not deliberately withholding it.

When to look at it: `> 0` means a genuinely-traded asset renders
priceless on `/v1/assets` with no explanation — a pricing-coverage gap
that should page rather than wait for an operator to notice it while
browsing. Alert: `stellarindex_assets_popular_priceless`
(deploy/monitoring/rules/pricing-coverage.yml +
configs/prometheus/rules.r1/pricing-coverage.yml). The aggregator warns
one log line per firing asset (`priceless-popular coverage gap`) with
its `asset_id` + signals.

### `stellarindex_priceless_coverage_check_runs_total`

Counter, label `outcome` (`ok` / `error`).

Per-sweep outcome of the priceless-popular coverage tripwire. `error` =
the candidate read failed (Postgres unreachable / query error), so the
gauge is stale for that tick. A sustained `error` rate means the
tripwire is blind — pair it with the staleness gauge below.

### `stellarindex_priceless_coverage_check_last_success_unix`

Gauge, no labels.

Unix seconds of the most recent SUCCESSFUL tripwire sweep. `time() -
this` is the staleness signal: a wedged worker stops updating it, so a
new coverage gap would go unseen even while the count gauge sits at a
stale 0. Alert: `stellarindex_priceless_coverage_check_stale` (> 30 min,
three sweep intervals).

### `stellarindex_signup_reaper_runs_total`

Counter, label `outcome` (`ok` / `error`).

Per-sweep outcome of the API binary's speculative-account reaper
(`internal/signupreaper`, F-1255), which deletes orphan `accounts`
rows left behind when two concurrent `/v1/auth/callback` provisions
raced for the same just-verified email: the loser is Suspended with a
`signup-race:` reason and never gets a user attached. Runs hourly by
default; emits only when `[signup_reaper] enabled = true` (the default)
AND the dashboard/Postgres account store is wired.

When to look at it: `error` = the reap DELETE failed (Postgres
unreachable, or the `accounts` table is superuser-owned per
migrations/README rule 7), so signup-race orphans accumulate unbounded
— a slow table leak, not a customer-facing outage. A sweep that deletes
zero rows is still `ok`. Alert: `stellarindex_signup_reaper_failing`
(deploy/monitoring/rules/signup-reaper.yml +
configs/prometheus/rules.r1/signup-reaper.yml).

### `stellarindex_signup_reaper_run_duration_seconds`

Histogram, label `outcome` (matches
`stellarindex_signup_reaper_runs_total`). Buckets 5 ms – 30 s.

Wall-clock of one reaper sweep — a single bounded `DELETE` over the
tiny, indexed set of suspended `signup-race:` orphans. A healthy sweep
is a few ms; the wide tail catches a degraded / lock-contended
Postgres. Chart `ok` p95/p99 separately from the `error` path.

### `stellarindex_signup_reaper_rows_deleted_total`

Counter, unlabelled.

Cumulative count of speculative (signup-race) orphan accounts the
reaper has deleted. Chart as a `rate()` to see the signup-race orphan
production rate: a steady non-zero rate means a race is firing
regularly — investigate the `/v1/auth/callback` provisioning path
(F-1255), separate from the reaper's own health (which the `runs_total`
`error` outcome + `signup_reaper_failing` alert cover).

### `stellarindex_login_code_lockout_rows`

Gauge, unlabelled. Refreshed by every retention sweep
(`internal/logincodereaper`, hourly).

Rows in `login_code_lockouts` — the durable per-email failed-verify
counter behind the dashboard email-code sign-in (migration 0122,
C3-032).

This is a **security** signal, not capacity trivia. The table's primary
key is **attacker-chosen**: `POST /v1/auth/verify-code` is
unauthenticated and accepts any well-formed address, so a wrong guess
against a synthetic address inserts a row that the
clear-on-successful-login path can never remove (nobody can sign in as
an address that does not exist). The retention sweep is the only bound;
this gauge is how an operator sees that bound holding, instead of
learning about a remote table-fill from the volume-level disk-full page
— an alarm that names the wrong subsystem, after the damage.

A healthy deployment sits in the low tens: rows exist only for addresses
with recent failures, are deleted on any successful sign-in, and are
swept once settled (48 h, live locks exempt).

Alert: `stellarindex_login_code_lockout_table_growing` →
[login-code-lockout-table-growing](../../operations/runbooks/login-code-lockout-table-growing.md).

### `stellarindex_auth_reaper_last_sweep_unix`

Gauge, labelled `reaper` ∈ {`login_code`, `magic_link`, `signup`}. Set at
the END of every completed sweep of the three auth-table reapers in the
API binary (`internal/logincodereaper`, `internal/magiclinkreaper`,
`internal/signupreaper`) — including sweeps that FAILED (a failing reaper
is alive; its errors counter reports the failure), excluding the
ctx-cancelled early return (that is the reaper going away).

Why it exists (#368 M5): each reaper already reported WHAT it did, none
reported THAT it ran. A reaper that dies leaves its rows gauge frozen at
the last healthy value and its errors counter at zero — the bound on an
attacker-fillable table is lost with every other signal reading "fine".
Read this next to the rows gauges: a flat rows gauge with a stale sweep
timestamp is a dead reaper, not a quiet table.

A disabled reaper (config-gated, never constructed) publishes no series.

Alert: `stellarindex_auth_reaper_stalled` →
[auth-reaper-stalled](../../operations/runbooks/auth-reaper-stalled.md).

### `stellarindex_auth_reaper_interval_seconds`

Gauge, labelled `reaper` (same values). The configured sweep cadence,
published once at construction so the stalled alert's threshold
(3 × interval) follows each deployment's own setting instead of a
hard-coded hour.

### `stellarindex_login_code_lockout_rows_deleted_total`

Counter, unlabelled.

Cumulative settled lockout rows removed by the retention sweep. Charted
as a `rate()` it is the production rate of failed-verify addresses; read
next to `stellarindex_login_code_lockout_rows` it separates "the table is
small because nothing is happening" from "the table is small because the
sweep is keeping up with a flood".

### `stellarindex_login_code_lockout_errors_total`

Counter, label `op` (`status_check` / `register` / `sweep`).

Failures of the durable login-code lockout's own machinery (C3-032).
Every one is deliberately non-fatal to the request — the lockout is
defence-in-depth over the per-token `maxCodeAttempts` cap, and failing a
customer's sign-in because a counter table blipped would be the worse
trade — which is exactly why the counter has to exist: all three are
**invisible at the HTTP layer**.

- `status_check` — the pre-match lockout read failed and the handler
  **failed open**. The lockout is not being enforced for that request,
  and the response is byte-identical to the healthy path. The scenario
  the fail-open posture is chosen for (migration 0122 lagging a node)
  would otherwise disable the control silently.
- `register` — a wrong-code attempt was not recorded against the durable
  counter. The grinder got a free guess.
- `sweep` — the retention pass (or its row count) failed; rows an
  unauthenticated caller can create accumulate until it recovers.

Pre-seeded across all three ops.

Alert: folded into
`stellarindex_login_code_lockout_table_growing` (the `status_check` arm)
→ [login-code-lockout-table-growing](../../operations/runbooks/login-code-lockout-table-growing.md).

### `stellarindex_magic_link_token_rows`

Gauge, unlabelled. Refreshed by every retention sweep
(`internal/magiclinkreaper`, hourly).

Rows in `magic_link_tokens` — the single-use email-link / email-code
sign-in + verification + invite tokens (migration 0027).

Like `stellarindex_login_code_lockout_rows` this is a **security /
privacy** signal, not capacity trivia (PRV-2). The table is durable
plaintext PII (email + `requested_ip`) with an **attacker-chosen** key:
`POST /v1/auth/login` is unauthenticated and inserts a permanent row for
any well-formed address, and a link nobody clicks is never consumed. The
retention sweep is the only bound; this gauge is how an operator sees it
holding, instead of learning about a remote table-fill from the
volume-level disk-full page — an alarm that names the wrong subsystem,
after the damage.

A healthy deployment sits low: rows exist only for recent mints and are
swept once expired past retention (48 h; live, unexpired tokens exempt).

### `stellarindex_magic_link_token_rows_deleted_total`

Counter, unlabelled.

Cumulative expired magic-link rows removed by the retention sweep.
Charted as a `rate()` it is the production rate of expired mints; read
next to `stellarindex_magic_link_token_rows` it separates "the table is
small because nothing is happening" from "the table is small because the
sweep is keeping up with a flood".

### `stellarindex_magic_link_token_errors_total`

Counter, label `op` (`sweep`).

Failures of the magic-link retention sweep (PRV-2). The sweep is a
background janitor and a failed pass is **invisible at the HTTP layer**,
so — like the login-code lockout reaper it mirrors — the counter has to
exist for an operator to tell a never-failed janitor from an absent one.

- `sweep` — the retention pass (or its row count) failed; rows an
  unauthenticated caller can create accumulate until it recovers.

Pre-seeded on the `sweep` op.

### `stellarindex_notify_sends_total`

Counter, labels `template` (`magic-link` / `signup-verify`), `result`
(`sent` / `failed`).

Transactional-email sends through `internal/notify` (the Resend client).
Before this counter, `internal/notify` had **zero** prometheus visibility,
so a mail outage was silent — the magic-link login handler swallows the
send error (returns 200 either way to avoid an enumeration oracle) and the
signup-verify path only logs it. Incremented at every `notify.Sender.Send`
call site: `sent` when Resend accepts, `failed` on any returned error
(validation, provider-rejected, or transient/network). `magic-link` is the
dashboard sign-in email; `signup-verify` is the API-signup confirmation
email — the two are the only `notify.Sender` paths (price alerts deliver
via webhooks, not mail). A sustained `failed` ratio drives the
`stellarindex_notify_send_failure_ratio_high` alert. Zero-seeded across the
two templates × {sent, failed} so the ratio reads a real 0 before the first
email.

### `stellarindex_aggregator_dropped_trades_total`

Counter, labels `reason` (`class` / `outlier`) and `pair`.

Trades removed from the VWAP input set, broken down by which filter
discarded them. `class` = removed by the ClassExchange-only filter
(non-exchange source: aggregator / oracle / authority_sanity / not
registered). `outlier` = removed by the σ-threshold filter
(`OutlierSigmaThreshold > 0`). A spike in `class` is usually a venue
mis-registered in `external.Registry`; a spike in `outlier` is
usually a market-distress event flooding the window with anomalies.

`pair` (added after the 2026-08-14 outlier_storm, where attributing a
single-issuer SDEX token-farm spam wave took ad-hoc SQL) is the
canonical string of the **configured** aggregate target pair whose
refresh dropped the trade — bounded cardinality by construction (only
`o.cfg.Pairs` entries flow through `refreshPairWindow`, ~12 in
production). Diagnose a storm with
`topk(5, rate(stellarindex_aggregator_dropped_trades_total{reason="outlier"}[10m]))`.
Config-dependent, so NOT pre-seeded (the `AggregatorFXSnapFallbackTotal`
`leg` convention); the class-drop alert `sum()`s across labels and is
unaffected by absent pair series.

Semantics caveat (2026-08-28): the orchestrator re-runs the filter over
the whole trailing window every tick, so a print that stays outside the
band is counted again on every tick it remains in the window, once per
window. The rate is "band-residents × windows / tick", not "new
outliers/s". `stellarindex_aggregator_outlier_storm` reads
`stellarindex_aggregator_venue_vwap` and
`stellarindex_aggregator_outlier_trim_fraction` reads
`stellarindex_aggregator_window_trades`; the only alert still gating on
this counter is the renamed overlap copy of the old gate,
`stellarindex_aggregator_outlier_trim_rate_legacy` (retire 2026-09-04),
after which this counter is diagnostic only.

### `stellarindex_aggregator_venue_vwap`

Gauge, labels `pair`, `window` (`5m` / `1h` / `24h`), `source`.

Per-source VWAP of the **pre-outlier-filter** (post-class-filter) trade
set for one (pair, window) refresh, on the served price scale. Set on
every refresh; a source that has left the window has its series
**deleted** so a venue that stopped trading cannot pin a stale level
into the disagreement ratio. Feeds
`stellarindex_aggregator_outlier_storm` (2026-08-28 redesign):
`max by (pair) / min by (pair) − 1` over `window="5m"` is venue
disagreement measured directly, replacing the counter-based rule that
fired for hours on agreed venues while the whole-window band trimmed a
genuine step. Cardinality: configured pairs × windows × sources that
traded in the window. Config-dependent, not pre-seeded. An operator
signal, never a served value (the float64 conversion happens at the
gauge boundary only).

### `stellarindex_aggregator_window_trades`

Gauge, labels `pair`, `window` (`5m` / `1h` / `24h`), `stage`
(`fetched` / `class` / `outlier`).

Number of trades in the **current** (pair, window) refresh after each
filter stage: what the store returned (post-truncation), what survived
the ClassExchange-only filter, and what survived the outlier filter (the
VWAP input). `1 − outlier/class` is the current window's trim share
without the per-tick re-counting of the drop counter. Feeds
`stellarindex_aggregator_outlier_trim_fraction` (`> 0.2` on the 24h
window with ≥ 20 class-filtered trades, for 30m) — the single-venue spam
shape (2026-08-14 token farm) that venue disagreement cannot see.
Bounded: configured pairs × windows × 3 stages.

### `stellarindex_aggregator_dropped_windows_total`

Counter, label `reason` (`min_usd_volume`).

Windows the orchestrator suppressed at the window-level filter step
— distinct from `dropped_trades_total` (per-trade) and
`empty_windows_total` (zero trades to begin with). `min_usd_volume`
fires when a target pair whose quote leg the orchestrator can value
in USD (fiat:USD directly, a classic asset on
`trades.usd_pegged_classic_assets`, or a Soroban SAC wrapper of one
— see `usdQuoteDecimals` in `internal/aggregate/orchestrator`) has a
post-class + post-outlier window with less total USD volume than
`aggregate.min_usd_volume` (closes launch-readiness L2.1; extended to
classic/Soroban-quoted pairs 2026-07-10, Guard 1). Operators alert on
a sustained fraction-of-ticks dropping for `min_usd_volume` as a sign
that a configured pair has thinned out beyond the threshold or the
threshold is mis-tuned.

### `stellarindex_aggregator_min_usd_volume_unvaluable_total`

Counter, label `pair`.

Fires once per (pair, window) refresh where `aggregate.min_usd_volume`
is configured (> 0) but the target pair's quote is an on-chain asset
(classic or Soroban) with NO recognised USD peg — the orchestrator
can't compute a USD volume to compare against the threshold, so the
window is DROPPED fail-closed (2026-08-04 inversion; before that date
these windows published WITHOUT the manipulation floor, which is the
exposure the 2026-08-04 valuation incident closed). Not
alert-wired: this is a "know your exposure" dashboard signal, not a
health failure — a directly-configured Soroban- or classic-quoted
pair whose peg isn't declared now publishes NOTHING until the
operator adds the missing entry to `trades.usd_pegged_classic_assets`
/ the matching `supply.sac_wrappers` row, which un-blacks the pair
WITH valuation.
Zero on every deployment today (2026-07-10): the built-in default
pair set and every checked-in TOML, including r1's, are entirely
fiat:USD/EUR/GBP-quoted, so no live pair currently hits this branch.
Fiat pairs quoted in a non-USD currency (EUR, GBP, …) do NOT increment
this counter — that's a separate, pre-existing, already-understood
scope boundary (see `Config.MinUSDVolume`'s doc), not the gap this
metric watches for.

### `stellarindex_price_serve_substance_withheld_total`

Counter, label `surface` (`price_read` | `tip` | `oracle` |
`asset_headline` | `price_alert` | `dex_tvl`).

Fires once per aggregated-price serve WITHHELD by the serving-side
thin-market substance gate (`internal/pricingguard.SubstanceGate`,
`[pricing_guard]` config, 2026-08-04 valuation incident): the
requested pair has an on-chain leg and its trailing market activity
(USD volume / distinct 1-minute buckets / wall-clock span, alias
union, both directions) is below the serve floor, so the surface
returned the `price-withheld` verdict instead of a price. Raw
surfaces (`/v1/observations`, `/v1/ohlc`, `/v1/history`) still serve
the pair.

When to look at it: a steady non-zero rate is EXPECTED — Stellar's
long tail of dust pairs is large, and each withheld request is the
gate doing its job. What warrants investigation is a step-change:
a sudden jump across ALL surfaces usually means an ingest/aggregate
outage upstream of `prices_1m` (every pair suddenly "looks thin" —
check `stellarindex_aggregator_ticks_total` and the indexer cursor
first), while a jump after a config deploy means the
`[pricing_guard]` floors were raised past the intended population.
Verdicts are cached ~60s per pair, so the counter tracks withheld
REQUESTS, not distinct pairs. Dashboard-only, no alert rule.

`surface="dex_tvl"` is the one BACKGROUND producer: the DEX TVL
snapshot refresh asks the gates about every pool reserve token on its
10-minute cadence, so its rate is a function of the pool token set
rather than of request traffic, and a withheld leg shows up as a pool
moving into `unpriced_pools` on `/v1/protocols` (the `≥` lower bound),
never as a smaller number claiming to be exact.

### `stellarindex_price_serve_scam_withheld_total`

Counter, label `surface` (`price_read` | `tip` | `oracle` |
`asset_headline` | `dex_tvl`).

Fires once per aggregated-price serve WITHHELD by the serving-side
scam-pricing gate (`internal/pricingguard.ScamGate`): the requested
asset's ISSUER carries a scam-class tag
(`malicious`/`unsafe`/`fraud`/`scam`/`hack`/`phishing`) in the curated
account directory (migration 0136), so the surface returned the
`price-withheld` verdict instead of a price, and the listing/detail
payload suppressed market_cap/fdv/change too. Raw surfaces
(`/v1/observations`, `/v1/ohlc`, `/v1/history`) and the raw
`circulating_supply` still serve — the gate withholds the aggregated
CLAIM, not the underlying data.

When to look at it: unlike the substance gate a non-zero rate here is
tied to the (small, slow-moving) set of scam-flagged issuers that are
also actively traded. The gate FAILS OPEN — a directory-reader error
does NOT withhold — so a drop to zero while flagged issuers still
trade can mean the gate is failing open (the local directory table is
unreachable); the paired `scam pricing gate: directory lookup failed`
warn log is the corroborating signal. Verdicts are cached ~60s per
issuer. Dashboard-only, no alert rule.

## Supply derivation (aggregator binary)

### `stellarindex_supply_cross_check_divergence_stroops`

Gauge, labels `classic_key` (`CODE:ISSUER`) and `wrap_class`
(`partial_wrap` | `full_wrap`).

Stroop divergence between a classic asset's Algorithm 2 total_supply
(ledger-entry sum) and its SAC-wrapped Algorithm 3 total_supply
(SEP-41 event sum). **2026-07-08 fix (BACKLOG #59):** the meaning of
the value depends on `wrap_class`, not a single fixed equality —
applying total-vs-total equality unconditionally produced 8 standing
false positives on partially-wrapped classic assets (e.g. AQUA:
Algorithm 2 ≈ 86.4B, Algorithm 3 ≈ 0 — a monitoring category error,
not indexer corruption).

- `wrap_class="partial_wrap"` (the default for every configured
  `sac_wrappers` pair): value = `max(0, sac_total − classic_total)`.
  Zero whenever `sac_total ≤ classic_total` — the expected, benign
  state for a partially-wrapped asset (SACWrapped is one of
  Algorithm 2's own non-negative addends, so it can never legitimately
  exceed the classic total). Positive only when the SAC reports MORE
  than the classic side could possibly back — impossible under
  correct accounting, a genuine "escrow != minted" violation.
- `wrap_class="full_wrap"` (operator-attested via
  `[supply].fully_wrapped_sacs`; none configured as of 2026-07-08):
  value = `|classic_total − sac_total|` — the ORIGINAL ADR-0011
  equality compare, for a pair confirmed 100% SAC-represented.

Drives the
[`stellarindex_supply_cross_check_divergence`](../../operations/runbooks/supply-cross-check-divergence.md)
alert when > 1. The alert expression is unchanged (`> 1`, no
`wrap_class` filter needed) — the false positives are fixed in what
the value MEANS, not in the alert condition.

### `stellarindex_supply_cross_check_total`

Counter, labels `outcome` (`within` / `over` / `missing_snapshot` /
`read_error`) and `wrap_class` (`partial_wrap` | `full_wrap` —
2026-07-08, BACKLOG #59).

Cross-check evaluations classified by whether the divergence stayed
within tolerance. Drives the alert's rate-of-failure view and
provides a "is the cross-checker even running" check orthogonal to
the gauge — a flat gauge with zero counter increments means the
orchestrator stopped invoking the cross-check, not that everything's
healthy.

### `stellarindex_supply_divergence_ratio`

Gauge, labels `asset` (canonical wire form, e.g. `native`) and
`reference` (`stellar-dashboard` / `coingecko`).

The absolute relative divergence `|our − reference| / reference`
between OUR served `circulating_supply` and an EXTERNAL authoritative
reference's. **Distinct from the `supply_cross_check_*` pair above**:
that pair is an internal consistency check (two of our own numbers);
this is our served figure vs the market's.

Look at this when you need to know whether
`/v1/assets/native` circulating supply still tracks the Stellar Network
Dashboard. Steady state for XLM is ~0.0003 (the ~0.03% Fee-Pool noise
floor documented in
[`docs/methodology/xlm-circulating-supply.md`](../../methodology/xlm-circulating-supply.md)).
Drives the
[`stellarindex_supply_divergence_high`](../../operations/runbooks/supply-divergence.md)
alert when > 0.01 (1%) — a threshold two-plus orders of magnitude above
that noise floor, so it fires only on a real drift (usually a stale
SDF-reserve exclusion account list). NOT updated on the `no_reference`
/ `refresh_error` outcomes: a frozen gauge is the correct behaviour
when a reference goes dark, so a dead reference never manufactures a
divergence reading. Emitted only while `[divergence.supply].enabled`
(off by default; opt in on r1 via ansible).

### `stellarindex_supply_divergence_total`

Counter, label `outcome` (`ok` / `divergent` / `no_reference` /
`refresh_error`).

One increment per (asset, tick) of the supply cross-check worker.
`ok` = agreed with every responding reference within the threshold;
`divergent` = a responding reference disagreed by more than the
threshold (the ratio gauge carries the magnitude); `no_reference` =
served figure loaded but every reference was unreachable / didn't
publish the asset (CoinGecko 429, Dashboard outage) — the
graceful-degrade "checker running blind" signal, deliberately NOT
paged so a dead reference isn't a false supply alarm; `refresh_error` =
our served snapshot couldn't be read (bootstrap, storage error).

Watch a sustained `no_reference` rate to know the check has gone blind;
watch `divergent` to know a real drift is firing. Pre-seeded to zero
for all four outcomes so the alert PromQL reads a real zero before the
first tick.

### `stellarindex_supply_divergence_duration_seconds`

Histogram, label `outcome` (`ok` / `divergent` / `no_reference` /
`refresh_error`).

Per-(asset, tick) supply cross-check latency including the served read
+ the HTTP fan-out to every reference. Labelled by outcome (matches the
counter) so operators chart the healthy `ok` path separately from the
slow-vendor / timeout `no_reference` path. Buckets 10 ms – 30 s: a warm
served read is single-digit ms; a single slow reference is ~1-10 s; the
worst case is the per-reference timeout (default 10 s) compounded across
the reference set.

### `stellarindex_aggregator_triangulations_total`

Counter, label `outcome` (`ok` / `missing_leg` / `parse_error` /
`redis_error` / `frozen_leg`).

Triangulation outcomes per tick × chain × window. The aggregator
runs one row per (chain, window) per tick after the per-pair
refresh; steady state is mostly `ok` with periodic `missing_leg`
entries when a leg's window was empty this tick. Sustained
`parse_error` or `redis_error` rates above baseline indicate
upstream regression worth investigating (Redis blip, malformed
cached value).

`frozen_leg` (MNY-22) means a leg of the chain was frozen earlier in
the same tick, so the chain deliberately did NOT publish: the freeze
path keeps the leg's last-known-good in cache, and reading it here
would launder a value we just declined to publish into a derived pair
that carries no frozen flag of its own. The freeze is inherited onto
the target pair instead. Treat a sustained `frozen_leg` rate as
"the chain's legs are under anomaly protection", not as an error.

All five outcomes are pre-seeded at zero in `internal/obs`, so
`rate()` / `absent()` on any of them is a real zero rather than a gap
before the first event.

A sustained `missing_leg` rate with `ok` pinned at zero means the
chains are configured but their legs never resolve — usually a dry
fiat-FX feed. That is
`stellarindex_aggregator_triangulation_chains_dry` in
`deploy/monitoring/rules/aggregator.yml`.

### `stellarindex_aggregator_fx_snap_fallback_total`

Counter, label `leg` (canonical pair string of the FX leg, e.g.
`fiat:USD/fiat:EUR`).

Triangulations that fell back to cached VWAP for an FX leg because
`FXQuoteAtOrBefore` returned no row at-or-before the bucket-end
timestamp. The X2.5 forex-snap rule (ADR-0018 §"Forex factor handling")
mandates the FX factor be the most recent FX-source quote at-or-before
bucket close; on miss, the orchestrator falls back to the leg's
cached VWAP so the chain still publishes (degraded but functional).

Steady state should be near-zero once FX ingestion is warm. Cardinality
is bounded by the operator-configured triangulation chain set —
typically a single-digit number of FX legs across all chains. Sustained
> 50% of triangulations indicates an FX-source health issue; the alert
in `deploy/monitoring/rules/aggregator.yml` fires at 30m sustained
fallback dominance.

### `stellarindex_aggregator_composite_corroboration`

Gauge, labels `pair`, `window`, `verdict` (`corroborated` /
`refuted` / `unavailable`).

Current-bucket composite-reference verdict for a structurally
single-venue target (`[aggregate.composite_reference] targets`,
2026-08-29 — design doc §10.1). Evaluated only on a single-venue
bucket of an allow-listed target; one series per verdict, exactly one
of them 1 after each evaluation. `corroborated` = the target's
triangulation chain rebuilt on THIS bucket (this tick's crypto/USD
publish × a fresh FX snap) agrees with the direct print within
`tolerance_bps`, so a phase-2 fire on this bucket is suppressed
(`corroboration_basis=composite`). `refuted` = it disagrees →
venue-specific → the freeze engages as before. `unavailable` = a leg
could not back a reference (thin, not refreshed, FX stale or not
FX-class) → freeze as before, the reason names the cause. Cardinality:
allow-list × windows × 3.

### `stellarindex_aggregator_composite_reference_leg_sources`

Gauge, labels `pair`, `window`, `leg` (canonical leg pair, e.g.
`crypto:XLM/fiat:USD`).

Distinct venues / providers behind each leg of the composite reference
on the last evaluated bucket — how STRONG a corroboration was. The
crypto/USD leg must reach `min_leg_sources` (default 2) for the
composite to count at all; the FX leg reads 1 (one provider,
`massive`). Also carried in `composite_meta.composite_leg_sources` and
the freeze reason string.

### `stellarindex_aggregator_composite_reference_leg_dispersion_bps`

Gauge, labels `pair`, `window`, `leg`.

Max |venue VWAP − leg VWAP| / leg VWAP in basis points across the
venues printing a priced composite-reference leg on the last evaluated
bucket (venue VWAPs over the same post-filter survivor slice the leg
VWAP came from). Above `[aggregate.composite_reference]
leg_dispersion_bps` (default = `tolerance_bps`, 75) the leg cannot
corroborate — `composite_unavailable: leg_dispersion=…bps` — because
two venues only count as two when they agree: a dominant venue plus a
dust print 3 % off is one opinion and an artefact (verifier advisory A1,
2026-08-29). A venue VWAP that cannot be computed refuses the leg too
(`leg_dispersion=uncomputable`, fail-closed — A4); the gauge is then
not updated for that leg. Also carried in the freeze reason as
`composite_leg_dispersion_bps={…}`.

### `stellarindex_aggregator_composite_freeze_suppressed_total`

Counter, no labels.

Phase-2 freeze fires (the 3-signal AND held on a single-venue bucket)
that were NOT engaged because the current-bucket composite reference
corroborated the move. Every increment is a bucket that would have
frozen before 2026-08-29; read it next to
`stellarindex_anomaly_freeze_engaged_total` when judging whether
`tolerance_bps` is too loose. Log line: `phase2 freeze suppressed:
composite reference corroborates the move`.

### `stellarindex_divergence_refresh_total`

Counter, label `outcome` (`ok` / `no_vwap` / `parse_error` /
`refresh_error`).

Per-Tick outcomes for the orchestrator's divergence-cache refresh
loop (ADR-0019 / launch-readiness L2.10 + L2.11). The aggregator
calls `divergence.Service.RefreshPair` once per configured pair
per Tick, using the pair's shortest-window VWAP as "our price"
input; the Service queries CoinGecko + Chainlink (when configured),
computes the divergence percent vs the median external reference,
and writes the result to `div:<asset>` in Redis. The API's
`flags.divergence_warning` reads from that cache.

`no_vwap` is benign on cold start and after Phase-1/Phase-2 freezes
(no fresh VWAP to compare against). Sustained `refresh_error` means
external references are unreachable — `flags.divergence_warning`
goes stale across the API surface; alert on a sustained rate via
`stellarindex_divergence_refresh_error_dominant` (deploy/monitoring/rules/aggregator.yml).

### `stellarindex_aggregator_baseline_refresh_total`

Counter, label `outcome` (`ok` / `not_enough_samples` / `read_error` /
`write_error`).

Baseline refresh outcomes per pair × refresh cycle (ADR-0019 Phase 2).
The aggregator's baseline-refresh worker recomputes Median + MAD over
each pair's 30-day VWAP window on an hourly cadence and UPSERTs the
result into `volatility_baseline_1m`. One increment per pair per cycle.

Steady state is mostly `ok`. Sustained `not_enough_samples` indicates
pairs in bootstrap (ADR-0019 §"Bootstrap policy") — the API's
confidence score for those pairs will fall back to the bootstrap
factor instead of using a per-asset baseline. Sustained `read_error`
or `write_error` rates indicate the storage layer needs investigation
(prices_1m read failing or volatility_baseline_1m write conflict).

### `stellarindex_aggregator_supply_refresh_total`

Counter, labels `asset_key` + `outcome`. `outcome` ∈ (`ok` /
`no_ledger` / `no_observation` / `compute_error` / `write_error` /
`stale_component` / `missing_freshness` / `dormant` /
`missing_baseline`). `asset_key` is the `supply.AssetKey` form:
`XLM`, `CODE:ISSUER` for classic credits, the bare contract
C-strkey for SEP-41.

Supply-snapshot refresh outcomes per (asset_key, outcome) per
refresh cycle (ADR-0011, ADR-0021, ADR-0022, ADR-0023). The
aggregator's supply-refresh goroutine recomputes each watched
asset's supply on the operator-configured cadence (`[supply]
aggregator_refresh_cadence`, default 5 min) and inserts the
snapshot into `asset_supply_history` (idempotent on
`(asset_key, ledger_sequence)`). One increment per (asset, tick).

Only fires when `[supply] aggregator_refresh_enabled = true` —
operators that drive the writer via the systemd timer in
`deploy/systemd/supply-snapshot.timer` instead see this counter
stay at zero.

Steady state is mostly `ok` per asset. Sustained `no_observation`
on an asset indicates the AccountEntry observer hasn't backfilled
the relevant accounts yet AND the static fallback config is also
empty or missing entries — expected briefly post-deploy, alarming
sustained. `no_ledger` fires before the indexer produces its
first ingestion cursor; clears as soon as ingest catches up.
`write_error` indicates the storage layer needs investigation.
`missing_baseline` is a SEP-41 SAC-wrapper whose pre-Soroban opening
balance hasn't been seeded — its Soroban-era-only total reads
Σburn > Σmint (incident 2026-07-06); it is benign (excluded from
`error_dominant`) and clears after `stellarindex-ops supply
seed-sep41-genesis`. A negative total AFTER the baseline is seeded
surfaces as `compute_error` (genuine inconsistency, pages).

The `asset_key` label lets operators chart per-asset bootstrap
progress + isolate failure modes per asset rather than chasing
a single aggregate signal across the watched-set.

### `stellarindex_aggregator_confidence_compute_total`

Counter, label `outcome` (`ok` / `skipped` / `baseline_missing` /
`marshal_error` / `write_error`).

Confidence-score compute outcomes per (pair, window) × tick (ADR-0019
§"Multi-factor confidence score"). The aggregator computes a
`confidence.Score` after each successful VWAP publish and writes it to
Redis at `confidence:<base>:<quote>:<window>`.

`skipped` covers the first-tick / no-prev-VWAP case (expected on
startup until the comparator slot warms). `baseline_missing` covers
pairs whose 30d baseline isn't yet computed — sustained values here
indicate the L2.5 baseline-refresh worker isn't keeping up with the
configured Pair set, and the API's confidence on those pairs falls
back to bootstrap. `ok` should be the dominant value in steady state.

`marshal_error` / `write_error` indicate the JSON encoder or Redis
itself misbehaved — both should be flat-zero in healthy operation.

### `stellarindex_anomaly_freeze_engaged_total`

Counter, label `class` (`stablecoin` / `treasury` / `crypto` /
`governance` / `default`).

ActionFreeze decisions emitted by the aggregator anomaly checker
(ADR-0019). Each increment means the orchestrator declined to
publish a fresh VWAP for some pair (kept the prior bucket's
last-known-good value); the API's `/v1/price` for the affected
pair will surface `flags.frozen=true` on the next read.

Pair-specific freeze details live in the `freeze:<asset>:<quote>`
Redis marker JSON (deviation_pct, reason, frozen_at, and the
`state` object described below) — labelled by class only here so
cardinality stays bound to the small AssetClass enum.

Incremented on every FROZEN TICK, not once per freeze: a freeze
held through its ADR-0019 duration keeps this counter climbing for
as long as it is held, which is what makes the `rate()`-based
`stellarindex_anomaly_freeze_sustained` rule work. Use
`stellarindex_anomaly_freeze_active` to count freezes rather than
frozen ticks.

### `stellarindex_anomaly_freeze_active`

Gauge, no labels.

How many `(pair, window)` freezes the aggregator is holding right
now, set at the end of every tick (ADR-0019 §"Freeze duration").

The counter above cannot answer this: one pair frozen for an hour
and sixty pairs frozen for one tick each produce the same
`rate()`. This gauge is the freeze-lifecycle equivalent, and it
falls back to zero on release rather than latching.

Unlabelled by pair on purpose — `len(Pairs) × len(Windows)` is
operator-configured. Per-pair identity lives in the marker JSON:
`redis-cli GET freeze:<asset>:<quote> | jq .state` shows
`fired_at`, `hold_until`, `extensions_used`, `escalated`,
`unfreeze_streak` and `corroborated`.

### `stellarindex_anomaly_freeze_extensions_total`

Counter, no labels.

Freeze-hold extensions granted. A freeze that reaches its hold
expiry without the pair meeting the auto-unfreeze condition
(confidence > 0.30 AND z < 3.0 for two consecutive buckets) gets
+30 minutes, up to 4 times.

This is the leading indicator for the escalation below: four
extensions on one pair and it pages. A sustained rate with
`stellarindex_anomaly_freeze_active` flat at 1 means one pair is
stuck; rising alongside the gauge usually means broad false-firing
on cold or sparse per-asset baselines rather than a market event.

### `stellarindex_anomaly_freeze_escalated_total`

Counter, no labels. **Drives a severity:page rule.**

Freezes that exhausted the extension ladder and escalated to
operator review. ADR-0019 holds an escalated freeze "until manual
unfreeze": it does NOT auto-unfreeze however healthy the pair
subsequently looks, so every increment is a pair whose `/v1/price`
is pinned to a last-known-good value until a human acts.

One increment per escalation transition (not per frozen tick), so
`increase(...[15m]) > 0` reads as "a new pair escalated" and an
un-actioned escalation is a flat line rather than a climbing one.

Force-unfreeze with
`stellarindex-ops freeze-unfreeze -asset A -quote Q -reason "..."`,
which clears the Redis marker AND stamps `recovered_at` on the durable
row, and counts the release under
`stellarindex_anomaly_freeze_released_total{mode="operator"}`.

**Do not `redis-cli DEL` the marker.** Since migration 0119 the
durable ladder is a second authority: deleting the key alone leaves
`freeze_events.recovered_at` NULL, so the aggregator reads the missing
marker as "Redis lost it", rehydrates the ladder and re-writes the
marker on its next tick. For an *escalated* freeze — the only kind
that needs a manual unfreeze at all — a bare DEL is therefore
permanently inert.

### `stellarindex_anomaly_freeze_released_total`

Counter, label `mode` (`auto` / `operator`).

Freezes that ENDED, by how. `auto` = the ADR-0019 auto-unfreeze
condition held (confidence > 0.30 AND z < 3.0 for two consecutive
buckets, once the initial hold was served). `operator` = the marker
was cleared out of band, which is ADR-0019's "operator override
always available".

Not expected to balance against
`stellarindex_anomaly_freeze_engaged_total`, which counts frozen
ticks rather than freezes. The label to watch is `operator`: a
rising manual-unfreeze rate means the calibration is producing
freezes humans keep having to undo, which is a threshold or
baseline problem rather than an incident.

### `stellarindex_anomaly_warn_total`

Counter, label `class` (`stablecoin` / `treasury` / `crypto` /
`governance` / `default`).

ActionWarn decisions emitted by the aggregator anomaly checker
(ADR-0019) — a bucket deviated past `anomaly.warn_pct` but not far
enough (or had multi-source corroboration) to freeze, so it WAS
published. Mirrors `stellarindex_anomaly_freeze_engaged_total`.

**This is the only place an anomaly warn surfaces.** It deliberately
does NOT set `flags.divergence_warning` on the wire, despite several
doc comments having claimed so until audit COR-09/AGT-06. That flag
is produced by the cross-reference divergence service and is
meaningful only alongside `flags.divergence_checked` (CS-087: a
`false` warning must not be read as "prices agree"). An anomaly warn
runs no cross-reference check, so folding it in would publish
`divergence_warning=true` with `divergence_checked=false` — exactly
the state CS-087 declares un-interpretable. Surfacing it on the wire
needs its own flag, which is an API-shape decision.

Before that audit the decision was computed and discarded, so
`warn_pct` was a tunable knob with no observable effect anywhere.
If you are alerting on price-feed instability, this is the signal
that fires BEFORE a freeze.

### `stellarindex_anomaly_freeze_recovered_total`

Counter, no labels.

Freeze rows the recovery worker closed (`MarkRecovered` stamped
`recovered_at` on the durable `freeze_events` row after the Redis
marker TTL elapsed). Steady-state rate trails
`stellarindex_anomaly_freeze_engaged_total` by the freeze TTL plus
the recovery-worker poll interval (default 60s). A persistent gap
between the two indicates the recovery worker is broken — see the
[freeze-recovery-stalled runbook](../../operations/runbooks/freeze-recovery-stalled.md).

### `stellarindex_anomaly_freeze_ladder_rehydrated_total`

Counter, no labels.

Freeze lifecycles restored from the **durable** ADR-0019 ladder
(migration 0119, `freeze_events.hold_until` / `extensions_used` /
`escalated` / `corroborated`) because the Redis marker was missing but
an open, unlapsed row was still there.

Each increment is one freeze — extension count, escalation flag and
all — that would have silently released before 0119: the orchestrator
reads a missing marker under a live freeze as the ADR-0019 operator
force-unfreeze, so a Redis flush used to unfreeze every held pair,
including ones that had climbed the full 2-hour ladder to `escalated`
("stays active until manual unfreeze") and had already paged a human.

Deliberately **not alerted**: the healthy steady state is zero, and a
non-zero reading means the safety net worked. Its value is
correlation — a burst immediately after a Redis restart is the
expected shape, whereas a slow trickle with Redis healthy means
markers are being evicted (`maxmemory-policy`) or expiring early,
which is a real configuration fault worth chasing.

### `stellarindex_anomaly_freeze_ladder_write_failures_total`

Counter, label `op` (`mark_hold` / `clear`).

Durable ADR-0019 ladder writes (migration 0119) that did not land.

The failure was always possible; the **silence** is what this closes.
Neither `freeze.Writer` nor `timescale.FreezeEventSink` holds a
logger, so a persistently failing ladder write produced no signal on
any surface: every freeze looked healthy right up until a Redis flush
needed the ladder that had never been written, at which point the
escalated freeze released exactly as it did before 0119.

The shape that makes this concrete is a partially-failed deploy. The
pipeline applies migrations **before** swapping the binary, so a new
binary running against a schema where 0119 did not apply sees every
ladder write match zero rows — uniformly absent durable state, zero
complaints. The sink reports that as `ErrNotFound` and it is counted
here.

Any sustained non-zero value means the durable ladder is not being
maintained and the Redis-flush protection is **inert**. `mark_hold`
failing is the dangerous direction (ladders never recorded); `clear`
failing only widens the pre-existing recovery-worker window. Both
label values are pre-seeded — a metric that only appears once it
breaks is indistinguishable from a dead one, which is the exact class
of gap this counter exists to close.

### `stellarindex_anomaly_freeze_recovery_sweeps_total`

Counter, label `outcome` (`ok` / `partial` / `error`).

Recovery-worker poll cycles. `error` outcomes mean the lister or
Redis transport failed for the entire sweep; `partial` means
`MarkRecovered` failed for one or more rows (postgres write path
issue) but the rest of the sweep completed. Sustained non-`ok`
indicates an upstream infrastructure problem; the recovery worker
itself retries on the next tick.

## verify-archive (stellarindex-ops one-shot)

Emitted by `stellarindex-ops verify-archive` when the operator
passes `-metrics-listen ADDR`. One-shot diagnostic command, but the
run can take hours on full pubnet sweeps — live metrics let
operators dashboard the bottleneck during the run rather than
guessing from log tails.

All vectors labelled by `chunk_idx` (decimal string) so a parallel
run with `-workers 8` produces per-chunk series. Cardinality bound
by the `-workers` cap (currently `[1, 16]`).

`-metrics-listen` is a LIVE-RUN view only: the HTTP server dies with
the process, so nothing scrapes it on a systemd-timer deployment.
The durable export is `-textfile-output PATH`, which writes the
mismatch counter (only) into node_exporter's textfile-collector
directory — see
[`stellarindex_verify_archive_mismatches_total`](#stellarindex_verify_archive_mismatches_total)
below.

### `stellarindex_verify_archive_ledgers_verified_total`

Counter, label `chunk_idx`.

Ledgers walked + verified per chunk. Rate over time gives ledgers/sec
per chunk — primary signal for spotting a stalled chunk versus a
slow one.

### `stellarindex_verify_archive_current_ledger`

Gauge, label `chunk_idx`.

Most-recent ledger sequence verified by each chunk. Together with
the chunk's `[from, to]` range (operator-known) gives a
percent-complete view; together across chunks gives a
ledger-distance-fan picture of leading vs trailing chunks.

### `stellarindex_verify_archive_checkpoints_total`

Counter, labels `chunk_idx` + `outcome` (`matched` / `missed`).

Tier B checkpoint outcomes per chunk. `missed` = archive file
absent (warning, or hard fail under `-fail-on-missed`); `matched` =
hash-equal proof.

### `stellarindex_verify_archive_mismatches_total`

Counter. Two export paths with deliberately different label sets:

| Export | Labels | Lifetime |
| ------ | ------ | -------- |
| `-metrics-listen` (in-process `/metrics`) | `chunk_idx` + `reason` | the run |
| `-textfile-output` (node_exporter textfile collector) | `tier` + `reason` | cumulative on the host |

`reason` ∈ `chain` / `sequence` / `checkpoint` on both.

Chain breaks, sequence gaps, and checkpoint hash mismatches.
**Any non-zero reading is a hard failure** — the counter exists so
dashboards can distinguish "mismatch fired and the run aborted at
second X" from "chunk aborted for an unrelated reason (canceled
context)".

The textfile export (issue #282) is what makes the P1
`stellarindex_stellar_archive_divergence` page fireable on r1; the
`-metrics-listen` server dies with the one-shot process, so it has
no scrape window on a timer deployment. Three properties of that
file are load-bearing and are pinned by tests
(`internal/ops/archive/verify_archive_textfile_test.go`):

- **cumulative** — each run folds its increments into the previous
  file's totals, so a clean run does not reset the counter and
  `increase()` stays meaningful;
- **zero-seeded** — all three `reason` values are written on every
  run even at 0, because a counter series that first *appears* at 1
  and then stays flat yields `increase() == 0` and would never page
  (the F-0033 / C4-038 "absence reads as health" trap);
- **`tier`-labelled, `chunk_idx`-free** — `tier` (`chain` for
  verify-archive-tier-a, `checkpoint` for tier-b) keeps the two
  units' `.prom` files from exposing an identical series through the
  one node_exporter target; `chunk_idx` is a per-run worker slot
  with no cross-run meaning, so it is summed away.

### `stellarindex_anomaly_freeze_recovery_sweep_duration_seconds`

Histogram, label `outcome` (matches the `_sweeps_total` counter:
`ok` / `partial` / `error`).

Latency of the freeze recovery worker's per-sweep tick. Pairs
with `_sweeps_total` — that one tells you whether sweeps succeed,
this one tells you how long they take. Sweep does ListOpen
(Postgres read) plus, per open row, a Redis GET and possibly
MarkRecovered (Postgres write). Fast path is sub-100 ms when
zero rows are open; latency scales with open-row count.

Latency degradation typically means Postgres pressure or Redis
lag rather than a freeze-policy issue. Sweep cadence is 60 s,
so even a multi-second sweep doesn't lose correctness — the
next tick catches up — but sustained slowness is worth
investigating before the freeze_events table accumulates open
rows the operator UI shows as permanently firing.

Buckets span 10 ms → 30 s. No alert wired today.

### `stellarindex_aggregator_supply_refresh_duration_seconds`

Histogram, label `outcome` (matches the per-asset_key counter's
outcome enum: `ok` / `no_ledger` / `no_observation` /
`compute_error` / `write_error` / `stale_component` /
`missing_freshness`).

Latency of the supply.Refresher.Tick call per supply-refresh
cycle. Pairs with `_aggregator_supply_refresh_total{asset_key,
outcome}` — that one tells you which assets refreshed + how often
they succeeded; this one tells you how long each tick took.

**Why no `asset_key` label here?** Histograms multiply cardinality
by buckets; pairing `asset_key × outcome × 12 buckets` blows up
fast on deployments watching many assets. Operators correlate
per-asset latency from the per-tick log line emitted by
`supply.Refresher.Tick` (timestamps + asset_key) when needed.

Steady-state ~50-200 ms per tick. A p99 climb past 1 s typically
means the snapshot inserter is contending with another writer or
a per-component freshness reader fell off its index. Buckets span
10 ms → 30 s. No alert wired today; the existing
`stellarindex_supply_snapshot_*` alert family covers freshness
+ never-initialised paths.

### `stellarindex_sep41_supply_rollup_advances_total`

Counter, labels `contract_id` + `outcome` (`ok` / `noop` /
`error`).

Counts passes of the aggregator's SEP-41 supply rollup worker —
the incremental maintainer (migration 0085) that keeps the
Algorithm-3 supply reader off the full-history aggregate. Each
pass folds a watched contract's newly-SETTLED mint/burn/clawback
events into a per-contract running checkpoint
(`sep41_supply_rollup`) so `SEP41KindTotalsAtOrBefore` reads
`checkpoint + a bounded live delta` instead of re-summing the whole
per-contract history. Background: on 2026-07-06 the full per-tick
aggregate over `sep41_supply_events` (grown to hundreds of millions
of rows by the 2026-07-05 re-derive) took minutes, ran in parallel
across watched contracts, saturated Postgres IO, and blew up API
p95/p99. `noop` is the dormant-token steady state (nothing new
settled). Sustained `error` for a `contract_id` means that
contract's checkpoint is frozen and the reader silently fell back
to the slow full sum for it — correlate with a p99 climb on
`_aggregator_supply_refresh_duration_seconds`.

### `stellarindex_sep41_supply_rollup_advance_duration_seconds`

Histogram, label `outcome` (matches the counter's enum). Pairs
with `_sep41_supply_rollup_advances_total{contract_id,outcome}` —
that one tells you which contracts advance + how often; this one
tells you how long each pass takes.

**Why no `contract_id` label here?** Same cardinality reasoning as
the supply-refresh histogram — `contract_id × outcome × buckets`
multiplies fast; per-contract latency is recoverable from the
worker log line + timestamp.

Steady-state is sub-second (a bounded tail sum on the
`(contract_id, ledger DESC)` index). The one expected outlier is a
cold contract's FIRST fold, which sums the whole per-contract
history once (seconds→minutes on the large table) before every
later pass goes incremental — buckets extend to 300 s to capture
it. A sustained high p99 *after* warm-up means the tail delta
stopped being bounded (worker starved / checkpoint not advancing).

### `stellarindex_divergence_refresh_duration_seconds`

Histogram, label `outcome` (`ok` / `no_vwap` / `parse_error` /
`refresh_error`).

Per-pair divergence-refresh latency. Pairs with the existing
`_total` counter — that one tells you how often refreshes happen
+ whether they succeed; this one tells you how long they take.

`RefreshPair` fans out to every configured external reference
(CoinGecko, Chainlink, …) for the pair, so the natural failure
mode is "one vendor's API goes slow and the whole refresh tick
stretches" — invisible without this metric. Operators chart
`ok` p95/p99 separately to detect vendor slowdown without a
`refresh_error` outcome (the slow vendor still returns,
eventually).

Buckets span 10 ms → 30 s — covers a healthy local cache-only
refresh (≤ 50 ms when every reference is cached), a single slow
vendor (~1-5 s on CG / Chainlink), and the worst-case
per-reference timeout (`per_reference_timeout_seconds`,
default 5 s) compounded across multiple references. No alert
wired today; the existing failing-rate signal lives in the
`_total` counter.

### `stellarindex_customer_webhook_delivery_duration_seconds`

Histogram, label `outcome` (`delivered` / `server_error` /
`client_error` / `network_error` / `build_error`).

Latency of the outbound HTTP POST inside the customer-webhook
delivery worker (`internal/customerwebhook/worker.go`). Pairs with
the `_attempts_total` counter — that one tells you how often
attempts happen + whether they succeed; this one tells you how
long they take.

The standard `http_request_duration_seconds` histogram covers the
INBOUND HTTP handler surface but not goroutine workers, so this
metric closes the corresponding gap for the OUTBOUND delivery
path. Includes body-drain time (the worker io.Copy(io.Discard,
resp.Body) so the connection can be reused).

Operators chart p95/p99 latency separately per outcome to isolate:

- `delivered` p99 climbing → a customer's endpoint is slow.
  Customer-side problem; we keep delivering, just slower.
- `server_error` p99 high → the customer's endpoint takes long
  AND returns 5xx; usually the same endpoint failing harder
  rather than two distinct problems.
- `network_error` p99 → connect or TLS handshake stalling. Often
  upstream DNS / network blip rather than the customer.
- `build_error` recorded as 0 (no HTTP roundtrip happened) so the
  bucket still populates in dashboards.

Buckets span 10 ms → 60 s (the worker's per-request context
timeout). No alert wired today; the existing
`stellarindex_customer_webhook_delivery_failing` covers the
failing-rate signal, latency degradation surfaces in the
dashboard.

### `stellarindex_postgres_ping_total`

Counter, label `outcome` (`ok` / `error`).

Emitted by the indexer's `watchPostgresPing` resilience goroutine
every 60 s. Probes the *sql.DB pool with `PingContext` (5 s
timeout). `ok` = healthy round-trip; `error` = any failure mode
(timeout, connection refused, dead pool, DSN misconfig).

**Why this exists (F-0151 / 2026-05-26 cascade):** when
postgres@15-main crashed and recovered during the disk-full SEV,
the indexer's pool held stale conns and silently failed writes
for ~14 h until a manual restart. The pool now retires conns every
30 min via `SetConnMaxLifetime` — automatic safety-net — and this
counter is the live observability signal so the next cascade
surfaces in minutes via `stellarindex_postgres_ping_failing`
instead of hours of silent drift.

Alert: `rate(stellarindex_postgres_ping_total{outcome="error"}[5m]) > 0.5`
for 2 m → page. Brief failures during a postgres restart are
expected; sustained means the pool is wedged.

### `stellarindex_postgres_ping_failure_streak`

Gauge, no labels.

Consecutive failed-ping count from the same `watchPostgresPing`
goroutine. Resets to 0 on the next success. Pair with
`stellarindex_postgres_ping_total` on dashboards to chart the live
streak alongside the cumulative outcome counts. The indexer logs a
structured error at `streak == 3` (`pool may be wedged`); search
the journal for that string when triaging the
`stellarindex_postgres_ping_failing` page. F-0151.

### `stellarindex_tls_cert_not_after_unix`

Gauge, label `host`.

Unix-seconds NotAfter timestamp of the leaf TLS cert observed at
the configured host. Emitted by the API binary's self-probe
(`internal/api/v1/tls_probe.go::RunTLSCertProbe`) on a 6 h cadence.
Probe failures keep the last-known value in place — the probe
counter below is the freshness signal. F-0051.

Alert `stellarindex_tls_cert_expiring_soon` fires when
`(not_after_unix - time()) < 14 * 24 * 3600` sustained 1 h.

### `stellarindex_tls_cert_probe_total`

Counter, labels `host`, `outcome` (`ok` / `dial_error` /
`timeout` / `no_cert`).

TLS cert self-probe outcomes per host. A growing `ok` rate while
`stellarindex_tls_cert_not_after_unix` stays flat is the success
signal; a sustained non-`ok` rate alongside a stale gauge means
the probe itself is failing — investigate before the gauge ages
out via the alert rule's 14-day threshold. F-0051.

### `stellarindex_markets_skipped_rows_total`

Counter, no labels.

Count of trades rows the `/v1/markets` scanner skipped because
their `base_asset` / `quote_asset` failed to parse as canonical
asset strings. The ingest pipeline only emits canonical asset
codes, so any non-zero reading means something bypassed the
normal write path (manual SQL insert, integration test residue,
etc.) and the row should be cleaned up.

**Any non-zero reading is alertable** — a single unparseable row
used to 500 the entire `/v1/markets` surface and trip page-tier
`api_error_rate_critical` + `slo_availability_burn_fast` alerts
(2026-06-01 incident, one row with `base_asset='test'`). The
handler now skips + bumps this counter instead of failing the
whole response, but operators should still investigate + delete
any row that increments this counter.

## MEV detection (aggregator binary)

The MEV worker (`internal/aggregate/mev`) scans the recent trade
window every 5 minutes for atomic-arbitrage cycles and writes new
ones to `mev_events` (backing `/v1/mev`). These metrics make the
worker's health + output rate observable.

### `stellarindex_mev_detect_runs_total`

Counter, label `outcome` ∈ `ok | scan_error | write_error`.

Per-run outcome of the MEV detection loop. `ok` = the scan +
detection completed (new inserts are counted separately). `scan_error`
= the bounded trades scan failed (Postgres unreachable / slow) and the
run was skipped (retried next tick). `write_error` = an `mev_events`
insert failed mid-run.

**When to look:** a sustained non-`ok` rate means the `/v1/mev` feed
is going stale. Not alert-worthy on its own — this is analytics, not
an SLO path — but a persistent `scan_error` streak points at the same
Postgres health the ingest/aggregator alerts already cover.

### `stellarindex_mev_events_inserted_total`

Counter, no labels.

New (non-duplicate) MEV events persisted across all runs. The detector
re-scans overlapping windows and dedups on write (`dedup_key`), so this
counts genuine first-detections, not re-observations. A flat line is
normal (arbitrage is intermittent on Stellar); use it to confirm the
detector is wired, not as an alert.

### `stellarindex_mev_detect_duration_seconds`

Histogram, label `outcome` (same set as the runs counter).

Per-run latency. A healthy run (bounded ts-window scan + in-memory
grouping + a few inserts) is sub-second; chart the `ok` p95/p99
separately from `scan_error` to tell "Postgres scan is slow" from
"detector is failing fast".

## Decimals-assumption guard (aggregator binary)

### `stellarindex_dex_trade_nonstandard_decimals_total`

Counter, labels `source`, `asset`.

**When to look at this: never — it should be permanently absent/zero.**
The served price is `Σ(quote_amount)/Σ(base_amount)` on raw smallest-unit
integers (the `prices_*` continuous aggregates and `aggregate.VWAP`); the
per-asset decimals cancel in that ratio only when the base and quote share a
decimals scale. That holds today because every DEX-traded Stellar token is
7-decimal (SACs are always 7; classic credits are 7; observed pure-SEP-41
tokens declare 7). The aggregator's `internal/decimalsguard` sweep resolves
each recently-DEX-traded Soroban token's on-chain `decimals()` from the
certified lake and increments this counter — once per (`source`, `asset`),
latched — the first time one is confirmed `!= 7`, i.e. the moment the
assumption is violated and every served price for a pair involving that
`asset` is silently skewed by `10^(7−decimals)`. `source` is the DEX
connector; `asset` is the token's C-strkey contract id. The label set is
unbounded in principle but near-empty in practice, so it is **not**
pre-seeded — a series exists only once a real offender is detected, and the
alert is a bare `> 0`. The exact decimals + skew magnitude are in the guard's
ERROR log line, not a label. Any non-zero value is a real, silent mispricing
on a live pair — page-adjacent (P2). Runbook:
`docs/operations/runbooks/dex-nonstandard-decimals.md`.

## Nonstandard-decimals serving guard (API binary)

### `stellarindex_price_serve_declined_nonstandard_decimals_total`

Counter, label `asset`. **HISTORICAL — permanently zero since 2026-07-10.**

This was the READ-TIME enforcement half of the dex-nonstandard-decimals
guard: from 2026-07-09 the price surfaces declined (`422 problem+json`)
any pair with a leg confirmed non-7-decimal in
`nonstandard_decimals_assets` (migration 0093), and this counter fired
once per declined request. On 2026-07-10 the decline was replaced
endpoint-by-endpoint with read-time decimals NORMALIZATION
(`aggregate.AdjustPrice` — the served value is corrected, not refused),
and the last two declining paths (`/v1/price` closed-1m bucket,
`/v1/ohlc?interval=` series) were normalized too, so the decline code
path no longer exists and nothing increments this counter. Retained
(registered, always zero) one release so dashboards/queries referencing
it don't break; remove alongside the next metrics cleanup. Runbook:
`docs/operations/runbooks/dex-nonstandard-decimals.md`.

### `stellarindex_nonstandard_decimals_cache_refresh_failures_total`

Counter, no labels.

Failed background refreshes (60s cadence) of the API's in-process mirror of
`nonstandard_decimals_assets`. The cache is fail-open on a refresh error —
it keeps serving the previous snapshot rather than clearing it, since
availability wins over the guard for infra blips — so this is an
infra-health signal, not a pricing-correctness one: a rising value means
the cache is coasting on a stale (but still valid) snapshot. No dedicated
alert; the underlying Postgres-health alerts already cover the infra
failure this reflects.

## Background cache workers (API binary, v0.21.4)

### `stellarindex_dex_tvl_refresh_total`

Counter. Labels: `outcome` (`ok` | `error`).

One increment per DEX TVL snapshot refresh (10-min cadence +
startup). `error` means at least one protocol's read failed — that
protocol carries its PREVIOUS entry forward, so the served figure
ages invisibly. Look at this when a protocol page's TVL seems frozen:
sustained `error` with no `ok` is exactly that, and is what the
`stellarindex_dex_tvl_refresh_failing` alert fires on. Isolated
errors during lake merges are normal.

### `stellarindex_dex_tvl_refresh_duration_seconds`

Histogram. Labels: `outcome` (matches the counter). Buckets 50 ms → 180 s.

Wall time of one full refresh (all four protocols). Chart `ok` p95:
a creeping value is the early warning that a reserve read lost its
thread/memory pin (the 2026-07-29 40× read-amplification class)
before it starts erroring at the 3-min timeout.

### `stellarindex_sdex_orderbook_maintain_total`

Counter. Labels: `outcome` (`load_ok` | `load_error` | `advance_ok` |
`advance_error` | `verify_ok` | `verify_error`).

Maintenance attempts for the in-process SDEX order book behind
`/v1/sdex/orderbook`. `load_*` is the once-per-process full-slice
initial load (retried on the 60s ticker until it lands); `advance_*`
is the 60s incremental change apply; `verify_*` is the per-tick
quarantine drain that lake-verifies version-tie suspect offers
(2026-07-31 crossed-book fix) — unobserved when the quarantine is
empty. Look here when the endpoint 503s past startup (repeated
`load_error`), serves a stale `as_of_ledger` (`advance_error`), or
serves thinner-than-real depth while
`stellarindex_sdex_orderbook_pending_offers` stays high
(`verify_error`). Pre-load advance ticks observe nothing by design —
a healthy advance rate must not mask a stuck load.

### `stellarindex_sdex_orderbook_maintain_duration_seconds`

Histogram. Labels: `outcome` (matches the counter). Buckets 50 ms → 30 min.

`advance_ok` p95 is the drift-risk signal as offer churn grows; the
raw `load_ok` observation is the initial-load wall time the launch
plan tracks as an acceptance item — compare it across deploys before
it approaches the 30-min cap. `verify_ok` should sit in the
single-digit seconds (one bounded batch of partition-pruned point
probes per tick).

### `stellarindex_sdex_orderbook_crossed_pairs`

Gauge.

Asset pairs whose SERVED order book is crossed (best bid >= best
ask). Stellar's DEX executes crossing offers at submission, so a
resting book can never be crossed on-chain — this is the book's
data-quality invariant and it should read 0. Any sustained non-zero
value means phantom offers are being served: the founding case
(2026-07-31) was 4.7-year-dead XLM/USDC bids kept "live" by
ReplacingMergeTree version ties on intra-less backfill rows, serving
a 0.4327 best bid against a 0.1722 best ask. The quarantine +
verification path removes that class; what this gauge catches is the
residual — an offer whose removal the lake never ingested at all,
i.e. an entry-change coverage gap to chase with the completeness
tooling, not a serving bug.

### `stellarindex_sdex_orderbook_pending_offers`

Gauge.

Offers loaded from `ledger_entries_current` but quarantined from the
served book until the change-stream probe proves no removal exists at
their winning ledger (the `intra_ledger_seq == 0` version-tie suspect
class). Drains at 2,500/tick after every process start — a populated
quarantine right after a deploy is normal and converges in hours.
Stuck high alongside `verify_error` means the served book is honest
but thinner than the real chain state.

### `stellarindex_sdex_orderbook_undecodable_offers_total`

Counter.

Offer-entry change rows the order-book reader SKIPPED because their
`entry_xdr` failed to decode (audit 2026-07-31). A skipped non-removed
change silently FREEZES that offer key's previously-applied state in
the served book — the price/amount update it carried is lost until the
key's next decodable change. Offer entries are core-emitted XDR, so
this should read 0; each skip also logs a warn line with the key and
ledger. Sustained increments point at a lake ingestion/schema problem
upstream of the book, and mean served depth is quietly stale for the
affected keys — treat like an entry-change coverage gap, not a serving
bug.

### `stellarindex_worker_panics_total`

Counter, label `worker`. Incremented by `worker.Recover` when a background
goroutine panics and is swallowed so the process stays up. **When to look:**
any increment means that worker is DEAD until the owning unit restarts —
`worker.Recover` does not restart it. Alert: `stellarindex_worker_panicked`
(page). Before this metric a recovered panic was one log line and nothing
else (#368 M4). See runbooks/worker-panicked.md.

### `stellarindex_explorer_swr_refresh_total`

Counter. Labels: `cache` (`accounts_wealth` | `asset_holders` |
`contract_detail` | `contracts_dir` | `native_lp_listing` |
`network_throughput` | `op_type_stats` | `ops_directory` |
`protocol_bespoke` | `ttl_liveness`), `outcome` (`ok` | `error`).

Detached stale-while-revalidate refresh outcomes for the explorer's
snapshot caches (route-sweep 2026-07-29; `contract_detail` — the shared
per-contract events/interactions/code-history cache — joined 2026-07-30;
`network_throughput` — the /v1/network/throughput daily series — and
`protocol_bespoke` — the last-good block under the
/v1/protocols/{name} visual suite, a SERVED-TIER refresh sharing this
pair because its contract is identical — joined 2026-08-13;
`ops_directory` — the /v1/operations first page, which moved from
fill-on-miss to stale-serve when its unbounded lake read was bounded —
and `native_lp_listing` — the /v1/liquidity-pools ranked listing, whose
60s refresh used to run inline under a held mutex — joined 2026-09-02).
The SWR design makes
refresh failures invisible at the API surface by construction —
stale-but-real keeps serving with `flags.stale` — so this counter is
the ONLY place a persistently dying refresher is visible before its
data is hours old. Look here first when an explorer surface's
`as_of` stops advancing.

### `stellarindex_explorer_swr_refresh_duration_seconds`

Histogram. Labels: `cache`, `outcome` (matches the counter). Buckets 50 ms → 300 s.

Per-refresh wall time of the exact reads that used to time out
inline at request deadlines. Chart each cache's `ok` p95 against its
refresh timeout (90 s holders / 60 s op-stats / 3 min wealth / 5 min
ttl-verdicts): p95 approaching the timeout predicts the stale-age
growing user-visible before errors appear.

### `stellarindex_protocol_detail_refresh_total`

Counter. Labels: `outcome` (`ok` | `stale` | `degraded` | `timeout`).

One increment per detached `/v1/protocols/{name}` detail rebuild —
both the prewarm sweep (every protocol × `?days=` window, re-swept 10
minutes after each sweep ends) and request-kicked stale revalidations
share the single-flight and count here. `ok` means the lake analytics
AND the bespoke block both built (`analytics.status="ok"` on the
wire); `stale` means the page built COMPLETE but its bespoke block came
from the last-good cache past the 45-minute staleness horizon (served
with `analytics.status="stale"` — every panel present, the bespoke
numbers older than the sweep cadence, i.e. that battery has been
failing or starved); `degraded` means the build completed but some
analytics component failed/was skipped (served with
`analytics.status="unavailable"`); `timeout` means the build outran
its 90 s detached budget — a previously built entry is kept, so the
page stale-serves rather than blanking. Look here when protocol pages
lose their visual suites (the 2026-07-31 replay-load failure): a
sustained `degraded`/`timeout` rate with no `ok` means every page is
running on old snapshots. Bursts during lake merges/replays self-heal
on the next sweep.

### `stellarindex_protocol_detail_refresh_duration_seconds`

Histogram. Labels: `outcome` (matches the counter). Buckets 50 ms → 90 s.

Wall time of one detail rebuild (roster/verdict joins + three parallel
lake reads + the category's bespoke query battery; measured on r1
under replay load 2026-07-31: soroswap 90d bespoke ~1.9 s, cctp
~0.4 s). Chart `ok` p95 — a creep is the early warning that a bespoke
query lost its rollup (the raw-trades-scan class) before builds start
hitting the 90 s top bucket, which is the hard budget, not headroom.

## Rolling ZFS snapshots (textfile collector, ansible-managed)

Emitted by `scripts/ops/zfs-snapshot.sh` (installed by the
archival-node role, tag `zfs-snapshots`) into
`/var/lib/node_exporter/textfile_collector/zfs_snapshot.prom` on every
run — daily via `zfs-snapshot.timer`, and on each
`scripts/ops/zfs-snapshot-now.sh`. NOT Go-declared, so not covered by
§3 of `scripts/ci/lint-docs.sh` (same textfile-only convention as the
`stellar_stack_*` / `galexie_archive_tip_lag_*` families). Alerted on
by `deploy/monitoring/rules/zfs-snapshots.yml`; runbook
`docs/operations/runbooks/zfs-snapshots.md`.

### `stellarindex_zfs_pool_free_bytes`

Gauge, label `pool`. `zpool list -Hp -o free` — pool-level free bytes,
the number the job's min-free guard acts on (unlike
`node_filesystem_avail_bytes`, which is per-dataset and subject to
quotas/reservations).

### `stellarindex_zfs_snapshot_min_free_bytes`

Gauge, label `pool`. The configured guard floor
(`zfs_snapshot_min_free_bytes`, default 2 TiB), exported so dashboards
can draw the line the job will act on.

### `stellarindex_zfs_snapshot_latest_unix`

Gauge, label `dataset`. Creation time of the newest `auto-*` snapshot
of the dataset; `0` when there is none. Staleness input for
`stellarindex_zfs_snapshot_stale`.

### `stellarindex_zfs_snapshot_count`

Gauge, label `dataset`. Number of `auto-*` snapshots currently held
(manual / operator snapshots are excluded — they are outside the
job's ownership).

### `stellarindex_zfs_snapshot_used_bytes`

Gauge, label `dataset`. `zfs get usedbysnapshots` — bytes held
exclusively by *all* snapshots of the dataset, manual ones included.
A large value with a small `_count` means a forgotten manual snapshot.

### `stellarindex_zfs_snapshot_guard_skipped`

Gauge, label `dataset`. `1` when the last run skipped this dataset's
snapshot because the pool was below the min-free floor even after
pruning; `0` otherwise.

### `stellarindex_zfs_snapshot_last_run_unix`

Gauge. Unix time the job last completed in any mode (`rotate`, `now`,
`metrics`).

### `stellarindex_zfs_snapshot_pool_free_unreadable`

Gauge, label `pool`. Written to a separate `zfs_snapshot_error.prom`
(value `1`) when a run could not read `zpool list -o free` — the job
then refuses to prune or snapshot and exits non-zero (fail-closed; a
run must never treat "unknown free" as "zero free"). Removed by the
next successful run, so the series is absent when healthy. Alerted on
by `stellarindex_zfs_snapshot_pool_free_unreadable`.

## Changelog

- 2026-09-03 — no new metric; two existing counters gained the
  alerting they were already documented as carrying.
  `stellarindex_ch_live_sink_ledgers_total{outcome="errored"}` was
  emitted from day one and matched by no rule in either tree (both
  live-sink rules selected `outcome="dropped"`); it is now covered by
  `stellarindex_ingestion_ch_live_sink_errors` (ticket) with its own
  runbook — the existing drops page is unchanged, because an error and
  a drop have different remedies (#371 F6). And every outcome of
  `stellarindex_customer_webhook_delivery_attempts_total` is now
  pre-seeded, with a `mark_error` rule
  (`stellarindex_customer_webhook_mark_errors`, ticket) for the
  duplicate-delivery loop `markTerminal`'s godoc already claimed was
  "visible to an alert" (#368 M6).

- 2026-08-29 — added the rolling ZFS snapshot textfile gauges
  (`stellarindex_zfs_pool_free_bytes`,
  `stellarindex_zfs_snapshot_{latest_unix,count,used_bytes,guard_skipped,min_free_bytes,last_run_unix,pool_free_unreadable}`),
  emitted by `scripts/ops/zfs-snapshot.sh`.

- 2026-08-29 — added the per-repo nightly pgBackRest wrapper metrics
  (`stellarindex_pgbackrest_backup_last_success_unix{repo}`,
  `stellarindex_pgbackrest_backup_last_rc{repo}`,
  `stellarindex_pgbackrest_backup_duration_seconds{repo}`), emitted by
  `pgbackrest-backup.sh` (ansible-managed,
  `configs/ansible/roles/archival-node/templates/pgbackrest-backup.sh.j2`)
  into the node_exporter textfile collector — NOT Go-declared, same
  textfile-only convention as the `stellar_stack_probe` family below.
  `last_success_unix` is carried forward across a failed run so a
  failing repo2 shows as an ageing timestamp rather than a vanished
  series. Added when repo2 (S3) went live on r1 and the wrapper turned
  out to back up only repo1 (pgBackRest `backup` is single-repo).
- 2026-08-11 — removed the Stripe metrics
  (`stellarindex_stripe_platform_sync_errors_total`,
  `stellarindex_stripe_dead_letters_open`) and the
  `stripe_plan_upgrade` / `stripe_dead_letter` audit-failure
  surfaces — the Stripe/billing integration was deleted when the
  platform went free.
- 2026-07-10 — `stellarindex_price_serve_declined_nonstandard_decimals_total`
  is now HISTORICAL (permanently zero): the 422 decline path it counted
  was removed when read-time decimals normalization
  (`aggregate.AdjustPrice`) reached the last CAGG-reading serving paths
  (`/v1/price` closed-1m bucket, `/v1/ohlc?interval=` series, `/v1/chart`,
  markets/pools/pairs `last_price`, SEP-40 oracle passthroughs). Kept
  registered one release for dashboard continuity.
- 2026-07-09 — added the stellar-stack version-lag textfile-collector
  probe metrics (`stellarindex_stellar_stack_probe_success`,
  `stellarindex_stellar_stack_version_lag{component}`,
  `stellarindex_stellar_stack_installed_info{component,version}`),
  emitted by `stellar-stack-version-probe.sh` (ansible-managed,
  `configs/ansible/roles/archival-node/tasks/10-observability.yml`) —
  NOT Go-declared, so these are not covered by this file's normal
  round-trip lint (§3 of `scripts/ci/lint-docs.sh` only enforces
  `internal/obs/metrics.go` → doc; see the `galexie_archive_tip_lag_*`
  / `stellarindex_galexie_catchup_refusals_5m` family for the same
  textfile-only convention). Listed here because the probe is a
  direct response to two incidents in one week (P27 core-freeze
  2026-07-08, galexie CAP-0071 crash-loop 2026-07-09) both caused by
  nobody watching whether the installed Stellar toolchain lagged
  upstream. Full detail:
  `deploy/monitoring/rules/stellar-stack-version.yml` +
  `docs/operations/runbooks/stellar-stack-version-lag.md`.
- 2026-07-09 — added `stellarindex_dex_trade_unit_ratio_total`
  (`source`), emitted by `internal/storage/timescale`'s `InsertTrade` +
  `BatchInsertTrades`. Sentinel for the 2026-07-07 Phoenix decoder
  field-mapping bug (237k trades landed with `base_amount ==
  quote_amount` for months, undetected by presence-only completeness
  checks): fires when a source produces a sustained stream of landed
  on-chain trades with an exact 1:1 base/quote ratio.
- 2026-07-09 — added `stellarindex_price_serve_declined_nonstandard_decimals_total`
  (`asset`) and `stellarindex_nonstandard_decimals_cache_refresh_failures_total`,
  the READ-TIME enforcement half of the dex-nonstandard-decimals guard
  (`internal/api/v1.NonstandardDecimalsCache` + `declineIfNonstandardDecimals`).
  Turns the 2026-07-07 detector into an actual stop-serving lever after a
  confirmed production bug (token `CC2RB…`, 100x-wrong price, 35 trades).
- 2026-07-09 — added the hashdb drift-detector metrics
  (`stellarindex_hashdb_append_total`,
  `stellarindex_hashdb_append_duration_seconds`,
  `stellarindex_hashdb_verify_runs_total`,
  `stellarindex_hashdb_verify_run_duration_seconds`,
  `stellarindex_hashdb_drift_total`), emitted by the indexer when
  `[hashdb].enabled = true` (ADR-0016, off by default). Wires
  `internal/hashdb` — previously a complete, tested library with zero
  production callers — into the live ingest loop (append) and a new
  periodic verify sweep. Founding case: ledger 63332650.
- 2026-07-07 — added `stellarindex_dex_trade_nonstandard_decimals_total`
  (`source`, `asset`), emitted by the aggregator's decimals-assumption guard
  (`internal/decimalsguard`). Detection-only signal for the served-price
  decimals landmine (decoder-correctness audit Finding 2): fires when a DEX
  trade lands for a Soroban token whose on-chain `decimals()` != 7.
- 2026-06-18 — added the MEV detection metrics
  (`stellarindex_mev_detect_runs_total`,
  `stellarindex_mev_events_inserted_total`,
  `stellarindex_mev_detect_duration_seconds`), emitted by the
  aggregator's MEV worker (atomic-arbitrage detector backing
  `/v1/mev`). Paired counter + duration-histogram + new-events
  counter, matching the divergence_refresh / supply_refresh shape.
- 2026-06-12 — added `stellarindex_ch_live_sink_ledgers_total`
  (`outcome=written|buffered|dropped|errored`), emitted by the
  indexer's periodic stats goroutine when the ClickHouse real-time
  dual-sink is enabled. Closes G12-02: the LiveSink counters were
  previously never exported despite the code comment claiming they
  were, and `written` was bumped on buffer-enqueue rather than
  durable flush (now split into `buffered` vs `written`). Pairs
  with the G12-01 bounded-drop buffer cap (`dropped` outcome).
- 2026-06-01 — added `stellarindex_markets_skipped_rows_total`
  to surface non-canonical rows in the trades table that the
  /v1/markets scanner is skipping. Closes the 2026-06-01
  incident root cause (one stray test-row 500ed every markets
  request).
- 2026-05-27 — added postgres-pool resilience metrics
  (`stellarindex_postgres_ping_total` +
  `stellarindex_postgres_ping_failure_streak`) emitted by the
  indexer's `watchPostgresPing` goroutine. Closes the F-0151
  observability gap surfaced by the 2026-05-26 cascade (dead
  pool, ~14 h silent drift before manual restart). Pairs with
  the new `stellarindex_postgres_ping_failing` page alert in
  `configs/prometheus/rules.r1/storage.yml` +
  `deploy/monitoring/rules/storage.yml`.
- 2026-05-13 — added freeze-recovery-sweep latency histogram
  (`stellarindex_anomaly_freeze_recovery_sweep_duration_seconds`).
  Pairs with the existing `_sweeps_total` counter; surfaces
  Postgres / Redis pressure as a chartable signal before the
  freeze_events table accumulates open rows.
- 2026-05-13 — added supply-refresh latency histogram
  (`stellarindex_aggregator_supply_refresh_duration_seconds`).
  Pairs with the existing per-asset_key `_total` counter;
  histogram labels by outcome only to keep cardinality bounded
  on deployments watching many assets.
- 2026-05-13 — added divergence-refresh latency histogram
  (`stellarindex_divergence_refresh_duration_seconds`). Pairs
  with the existing `_total` counter to give operators per-pair
  per-outcome p95/p99 — surfaces "one vendor's API is slow" as
  a chartable signal even when the refresh still eventually
  succeeds.
- 2026-05-13 — added customer-webhook delivery latency
  histogram (`stellarindex_customer_webhook_delivery_duration_seconds`).
  Pairs with the existing `_attempts_total` counter to give
  operators per-outcome p95/p99 latency on the OUTBOUND
  webhook surface (the standard `http_request_duration_seconds`
  covers inbound only).
- 2026-05-13 — added Stripe platform-bridge error counter
  (`stellarindex_stripe_platform_sync_errors_total`) covering the
  five platform-store side-effect failure sites in the Stripe
  webhook path. Closes the long-standing TODO from F-1219 wave 32.
- 2026-04-29 — added verify-archive metrics (`stellarindex_verify_archive_*`)
  covering per-chunk ledger progress, checkpoint outcomes, and
  mismatches.
- 2026-04-28 — added supply cross-check metrics (L2.12 PR 5)
- 2026-04-25 — added aggregator orchestrator metrics
  (`stellarindex_aggregator_*`) covering tick outcomes, VWAP writes,
  empty windows, and per-stage trade drops.
- 2026-04-23 — initial reference document to close the lint drift.
