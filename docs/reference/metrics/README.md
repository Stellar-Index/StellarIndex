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

Per-cycle outcome counter. Outcomes: `ok` (cursor advanced),
`idle` (caught up, no rows in scan range), `error` (scan / cursor
read / cursor write failed; cursor not advanced — retried next
cycle). Drives the `stellarindex_projector_error_rate_high` alert.

### `stellarindex_projector_events_decoded_total`

Counter, labels `source`, `outcome`.

Number of consumer.Events the projector emitted to its sink.
Outcomes: `ok` (decode succeeded) and `decode_error` (Reconstruct
or Decoder.Decode returned non-nil; row skipped, cursor still
advances).

### `stellarindex_projector_cycle_duration_seconds`

Histogram, labels `source`.

Wall-clock latency of one projector cycle (scan + decode + sink).
Each cycle is bounded by `PerSourceTimeout=60s`. Sustained p99 >
30s for one source is the first sign that the sink is the
bottleneck.

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
`source_markets` / `asset_markets` / `all_pools`); `result` is
`hit` (returned cached value, including single-flight-wait
callers that piggy-backed on an in-progress upstream call) or
`miss` (called upstream).

Use to detect prewarm-key drift: when a prewarm goroutine warms
key A but the handler looks up key B, `result="miss"` rate
stays high even though the prewarm cycle is running. Suggested
alert: `rate(stellarindex_api_cache_ops_total{result="miss"}[5m])
/ rate(stellarindex_api_cache_ops_total[5m]) > 0.5` sustained
for 10 min on any (cache, op) pair — for hot ops the prewarm
should keep miss rate under 10%.

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

### `stellarindex_source_unknown_symbols_total`

Counter, label `source`.

Asset slots skipped from an otherwise-decoded oracle event because
the symbol or feed-id isn't in our canonical asset allow-list
(ADR-0010). Distinct from `decode_errors`: the rest of the event
still decoded cleanly — the parent decoder `continue`d past the
unknown slot rather than failing the whole event. Reflector, Band,
and Redstone are the live emitters; CEX streamers don't fan out
into mixed-asset batches the same way. A sustained non-zero rate
means an upstream oracle expanded its feed set and our allow-list
needs an amendment. F-1234 (codex audit-2026-05-12).

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
named component was wired with a Prometheus Registry (its metrics +
dependent alerts are live), `0` when it is running Registry-less (those
metrics/alerts are DEAD — the source series never registers so the
alert can never fire). Set per component at startup on the binaries
that build the affected config; an absent series means "component not
used on this binary" and is not alerted. The concrete case is
`component="ledgerstream"`: `pipeline.LedgerstreamConfig` deliberately
leaves `Config.Registry` nil (the SDK's metric registration panics on
the repeated `Stream` calls the archive→live→catch-up path makes), so
`stellarindex_ledgerstream_tier_read_total` is dead in production and
the `both_missing` page is inert. Alerted by
`stellarindex_metrics_registry_absent`; remediation in
`runbooks/metrics-registry-absent.md`.

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
upsert. `cursor-stuck` alert fires when `increase(...[5m]) == 0` with
`source_enabled=1`.

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
`rate({outcome="new"}[5m]) == 0 AND rate({outcome="duplicate"}[5m]) > 0`
to catch the live r1-2026-05-28 signature (157 SDEX insert
attempts/min while the hypertable's `max(ts)` was 11 h old).

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
- `errored` — failed `Add` / `Flush` operations (CH down, wedged,
  or disk-full). A climb is a CH write-path fault.

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
gone silently blind: it's recording nothing, so the periodic verify
sweep will find `missing`, not `drifted`, even if real drift is
happening. There's no dedicated alert on this counter alone (a
disabled detector is a silent no-op, not an incident) — treat a
climbing `error` rate as "this region's hashdb coverage is a lie" and
fix the underlying disk/permission issue.

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
`disabled` / `build_error` / `list_error` / `mark_error`).

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
```

F-1270 (audit-2026-05-12).

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

### `stellarindex_aggregator_dropped_trades_total`

Counter, label `reason` (`class` / `outlier`).

Trades removed from the VWAP input set, broken down by which filter
discarded them. `class` = removed by the ClassExchange-only filter
(non-exchange source: aggregator / oracle / authority_sanity / not
registered). `outlier` = removed by the σ-threshold filter
(`OutlierSigmaThreshold > 0`). A spike in `class` is usually a venue
mis-registered in `external.Registry`; a spike in `outlier` is
usually a market-distress event flooding the window with anomalies.

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
window publishes WITHOUT the manipulation floor applied. Not
alert-wired: this is a "know your exposure" dashboard signal, not a
health failure — a directly-configured Soroban- or classic-quoted
pair whose peg isn't declared is a deliberate operator-visible gap
(add the missing entry to `trades.usd_pegged_classic_assets` / the
matching `supply.sac_wrappers` row to close it), not an incident.
Zero on every deployment today (2026-07-10): the built-in default
pair set and every checked-in TOML, including r1's, are entirely
fiat:USD/EUR/GBP-quoted, so no live pair currently hits this branch.
Fiat pairs quoted in a non-USD currency (EUR, GBP, …) do NOT increment
this counter — that's a separate, pre-existing, already-understood
scope boundary (see `Config.MinUSDVolume`'s doc), not the gap this
metric watches for.

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
`redis_error`).

Triangulation outcomes per tick × chain × window. The aggregator
runs one row per (chain, window) per tick after the per-pair
refresh; steady state is mostly `ok` with periodic `missing_leg`
entries when a leg's window was empty this tick. Sustained
`parse_error` or `redis_error` rates above baseline indicate
upstream regression worth investigating (Redis blip, malformed
cached value).

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
Redis marker JSON (deviation_pct, reason, frozen_at) — labelled by
class only here so cardinality stays bound to the small AssetClass
enum.

### `stellarindex_anomaly_freeze_recovered_total`

Counter, no labels.

Freeze rows the recovery worker closed (`MarkRecovered` stamped
`recovered_at` on the durable `freeze_events` row after the Redis
marker TTL elapsed). Steady-state rate trails
`stellarindex_anomaly_freeze_engaged_total` by the freeze TTL plus
the recovery-worker poll interval (default 60s). A persistent gap
between the two indicates the recovery worker is broken — see the
[freeze-recovery-stalled runbook](../../operations/runbooks/freeze-recovery-stalled.md).

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

Counter, labels `chunk_idx` + `reason` (`chain` / `sequence` /
`checkpoint`).

Chain breaks, sequence gaps, and checkpoint hash mismatches.
**Any non-zero reading is a hard failure** — the counter exists so
dashboards can distinguish "mismatch fired and the run aborted at
second X" from "chunk aborted for an unrelated reason (canceled
context)".

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

### `stellarindex_stripe_platform_sync_errors_total`

Counter, label `operation` (`get_account` / `upsert_subscription` /
`account_update` / `list_keys` / `key_update`).

Failures inside the Stripe webhook's platform-store side-effects path
(`internal/api/v1/stripe_webhook.go::applyPlatformSideEffects` and
the subscription / invoice handlers). The webhook deliberately does
NOT 5xx on platform-store failures — Stripe retries would just keep
applying the same Redis rate-limit without making the platform-store
path any healthier — so this counter is the operator-visible signal
that the bridge is degraded.

**Any non-zero reading is alertable**: the customer's dashboard /
Postgres-backed key state is drifting from their Stripe billing
state. Per-`operation` breakdown isolates the failing layer:
`get_account` → no row for that Stripe customer (signup never
completed); `upsert_subscription` → Postgres write failure;
`account_update` → tier sync failure; `list_keys` / `key_update` →
per-key rate-limit lift failure.

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

## Changelog

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
