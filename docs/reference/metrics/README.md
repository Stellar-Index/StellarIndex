# Metrics Reference

Every metric the Stellar Index binaries emit, with labels, type and purpose. `scripts/ci/lint-docs.sh` enforces that this page and `internal/obs/metrics.go` list the same metrics.

## HTTP layer (emitted by the API binary only)

The indexer's mux (`/metrics`, `/healthz`) is deliberately not wrapped by `obs.HTTPMetrics`, so scrapes do not inflate `http_requests_total`.

### `http_requests_total`

Counter, labels `method`, `route`, `status`.

Counts every request through the `obs.HTTPMetrics` middleware. `route` is the route pattern, or `"unmatched"` for 404s. `status` `"499"` means the caller cancelled before the handler wrote.

### `http_request_duration_seconds`

Histogram, labels `method`, `route`.

Handler latency including middleware. Buckets 1ms to 10s.

### `stellarindex_dependency_up`

Gauge, label `dependency`. Emitted by the API binary only.

Result of each `/v1/readyz` check: `1` answered, `0` did not. Covers `postgres`, `schema`, `redis` and `clickhouse`. Overwritten each round, not a high-water mark.

Usually looked at because `stellarindex_dependency_down` paged. `clickhouse` is the only signal for the lake (no exporter). For `postgres` and `redis` (`pg_up`, redis_exporter fire too), this gauge alone points at the path to the dependency. Alert on `== 0`, never `absent()`.

Runbook: [dependency-down.md](../../operations/runbooks/dependency-down.md).

### `stellarindex_ingest_gap_ledgers`

Gauge, labels `source`, `table`.

Total missing ledgers in contiguous gaps >= the detector's `min-gap-size` (1000 by default). Data-derived, unlike the cursor-derived density in `/v1/diagnostics/ingestion`. `table` disambiguates sources sharing a hypertable (`sdex` / `soroswap` / `phoenix` / `comet` / `aquarius`; `band` / `redstone` / `reflector-*`).

### `stellarindex_ingest_gap_count`

Gauge, labels `source`, `table`.

Number of contiguous gaps at the latest cycle. One 100K-ledger gap and 100 ten-ledger gaps give the same `stellarindex_ingest_gap_ledgers`; compare to tell one big halt from many small drops.

### `stellarindex_ingest_gap_max_size_ledgers`

Gauge, labels `source`, `table`.

Largest contiguous gap at the latest cycle. Drives the `stellarindex_ingest_gap_detected` P1 alert (> 1000 for 15 min).

### `stellarindex_ingest_source_distinct_ledgers`

Gauge, labels `source`, `table`.

Count of distinct ledgers in the per-source hypertable. Numerator of the data-derived density: `distinct_ledgers / (tip - genesis + 1)`, with `tip` from `stellarindex_ingest_gap_detector_tip_ledger`. Sparse sources (Blend auctions, CCTP) read low naturally.

For `source="soroban-events"` the count comes from the `ledger_ingest_log` census, not `COUNT(DISTINCT ledger)`, because the generic scan of `soroban_events` is far too slow. A sink-writer halt makes this density read high. A range repaired without `census-backfill` reads 0.

### `stellarindex_ingest_gap_detector_tip_ledger`

Gauge (no labels).

The live ledgerstream cursor's `last_ledger` at the start of the latest cycle: the upper bound for every scan and the density denominator.

### `stellarindex_ingest_gap_detector_runs_total`

Counter, labels `source`, `table`, `outcome`.

Cycle outcomes: `ok` or `error` (Postgres or timeout failure). A climbing `{outcome="error"}` rate is the diagnostic when the silent-detector alert fires. Do not alert on `rate({outcome="ok"}) == 0`: heavy targets scan every 6h, so the counter stays at 1 and `rate()` cannot see a restart. Liveness uses `stellarindex_ingest_gap_detector_last_success_unix`.

### `stellarindex_ingest_gap_detector_last_success_unix`

Gauge, labels `source`, `table`.

Unix time of the last successful scan per (source, table). The `stellarindex_ingest_gap_detector_silent` alert fires on `(time() - gauge) > 8h`. Errored scans leave it untouched.

### `stellarindex_ingest_gap_detector_duration_seconds`

Histogram, labels `source`, `table`, `outcome`.

Latency of one detector cycle. Buckets extend to 600 s.

### `stellarindex_projector_lag_ledgers`

Gauge, labels `source`.

Ledgers between the projector's per-source cursor and the live ledgerstream tip; 0 = caught up. A source held at a lake hole shows rising lag. Drives `stellarindex_projector_lag_high` (P3 ticket: > 256 for 10 min).

### `stellarindex_projector_runs_total`

Counter, labels `source`, `outcome`.

Per-cycle outcome: `ok` (cursor advanced, nothing dropped), `idle` (caught up), `watermark_held` (lake watermark below ledgers ledgerstream holds: a lake hole), `error` (cursor not advanced, retried), `sink_retry` (a sink write held the cursor), `decode_degraded` (cursor advanced but a decode-failed row was dropped; not `ok`), `gate_widened` (a new contract entered a live gate, so the cursor held for one re-read). `error` drives `stellarindex_projector_error_rate_high`.

### `stellarindex_late_trade_cagg_refresh_total`

Counter, label `outcome` ∈ {`ok`, `error`}, pre-seeded at 0.

Live trade writers refresh the `trades` continuous aggregates when a trade lands older than a view's refresh policy reaches. One count is a cycle, not a batch. `error` means buckets are unmaterialised; retried with backoff.

### `stellarindex_late_trade_cagg_refresh_overdue_seconds`

Gauge, computed at scrape time.

Seconds since the oldest unrefreshed late-trade window fell due. 0 when none is due. Alert: `stellarindex_late_trade_cagg_refresh_failing` (> 600 for 30m).

### `stellarindex_projector_events_decoded_total`

Counter, labels `source`, `outcome`.

Events the projector emitted to its sink. Outcomes: `ok`, `decode_error` (row skipped, cursor advances), `sink_retry`, `sink_permanent`, `sink_i128_overflow`, `sink_quarantined`. Each row counts under exactly one, so they can be summed.

`sink_permanent` increments every cycle that re-reads a row the shed cap still holds. This lets `stellarindex_projector_row_dropped_permanent` fire while the projector only stalls. `sink_i128_overflow` means an int64 reached an amount path (ADR-0003 SEV-1) and drives `stellarindex_projector_i128_overflow` (page). A sustained `decode_error` rate drives `stellarindex_projector_decode_error_rate_high`: a decoder regression, not a poison row.

### `stellarindex_projector_cycle_duration_seconds`

Histogram, labels `source`.

Latency of one projector cycle (scan, decode, sink), bounded by `PerSourceTimeout=60s`. Sustained p99 > 30s for a source suggests the sink is the bottleneck.

### `stellarindex_projector_wedged`

Gauge, labels `source`.

`1` = the adaptive window is at the `MinBatchLimit` floor (25 ledgers) and the source made no progress for `WedgeCycles` (5) cycles, so the cursor will not self-recover. `0` = healthy. Seeded at `0` at startup. Drives the `stellarindex_projector_wedged` ticket alert; remediate by raising the cycle budget or decompressing the range.

### `stellarindex_projector_replay_window_active`

Gauge, labels `source`.

`1` while the cursor is inside a `projector-replay` rewind window recorded in `projection_dirty_windows`; `0` otherwise. Refreshed every `ReplayWindowRefreshInterval` (30s).

It separates intended lag from real lag: `stellarindex_projector_lag_high` carries `unless stellarindex_projector_replay_window_active == 1`, while `stellarindex_projector_replay_stalled` fires if the replay stops advancing. Three bounds keep the excuse narrow:

1. **Provenance.** Only windows written by `projector-replay` count (the `reason` column). `projected-rebuild -write` records into the same table without rewinding the live cursor.
2. **Cursor, exclusive at the top.** The flag clears when the cursor regains its pre-rewind position, not when `compute-completeness` clears the row. The lower bound is `from_ledger - 1`.
3. **Fail open.** A dirty-window read error publishes `0` for every source.

### `http_request_success_duration_seconds`

Histogram, labels `method`, `route`.

The non-5xx twin of `http_request_duration_seconds`, recorded only for status < 500 and not 499. Pair with `http_request_duration_seconds_count` for the latency-SLO ratio.

### `stellarindex_api_cache_ops_total`

Counter, labels `cache`, `op`, `result`.

Every read through the API cache wrappers. `cache` is the wrapper name (e.g. `markets`; Redis layers `assets_redis`, `markets_redis`, `oracle_redis`, `prices_redis` are exempt from the miss-rate alert). `op` is the cached method (`distinct_pairs` / `source_markets` / `asset_markets` / `all_pools`). `result`: `hit`, `stale` (expired value served during refresh), `miss`, `error` (Redis only), `refresh_error`, or `evicted` (bounded cache dropped its oldest entry; `observations` and `oracle` wrappers).

A high `result="miss"` rate despite a running prewarm means prewarm-key drift. Suggested alert: `rate(stellarindex_api_cache_ops_total{result="miss"}[5m]) / rate(stellarindex_api_cache_ops_total{result=~"hit|miss|stale"}[5m]) > 0.5` for 10 min. Keep the denominator to read outcomes, since `evicted` would skew it. A sustained `evicted` rate means churn: working set over the cap, or a key-space walk.

### `stellarindex_api_sparkline7d_rows_total`

Counter, label `result`.

One increment per priced listing row of `/v1/assets?include=sparkline7d`: `served` (series has at least one price) or `empty`. Rows with no published price are never counted. The batch reader returns a null-price skeleton for every id, so an entry in the map is not evidence of data. Suggested alert: `rate(stellarindex_api_sparkline7d_rows_total{result="empty"}[15m]) / rate(stellarindex_api_sparkline7d_rows_total[15m]) > 0.5` for 30 min, which means a lookup-key or pricing regression.
### `stellarindex_api_coverage_floor_probes_total`

Counter, label `result`.

One increment per consultation of the coverage-floor memo behind the empty-window signal on `/v1/ohlc`, `/v1/history`, `/v1/chart` and `/v1/price/at`. `hit` came from the in-process cache; `found` / `absent` / `error` each cost one bounded read against `prices_1d`; `evicted` means admitting a key pushed another out. Counted per constituent, so one fiat-quoted answer can increment it several times on a cold memo.

The probe is anonymous. `rate(stellarindex_api_coverage_floor_probes_total{result!="hit"}[5m])` is the added database load in reads/s. A sustained `evicted` rate means key enumeration, as with `stellarindex_api_cache_ops_total`.

### `stellarindex_api_lcm_home_domain_fallback_total`

Counter, no labels.

One increment per failed read of the issuer home-domain observations (`account_observations`): a storage error or the 100 ms bound expiring. The read is served as "unobserved", so the static `[metadata.issuer_home_domains]` map answers instead. Zero when healthy.

## Ingestion (indexer binary)

### `stellarindex_source_events_total`

Counter, label `source`.

Events the sink attempts to persist, per source. Zero rate with `source_enabled=1` backs the `source-stopped` alert.

### `stellarindex_source_enabled`

Gauge, label `source`.

`1` for sources switched on at startup; `0` during shutdown or when not configured. Qualifies source alerts; shown as `enabled` by `/v1/sources/{name}/health`.

Published by whichever binary runs the source: the `massive` FX worker sets its own, and a source with no series reads as "off" while ingesting. `/v1/status` uses it: `freshness.total_sources` is `count(stellarindex_source_enabled == 1)`.

### `stellarindex_source_last_event_unix`

Gauge, label `source`. Unix-seconds timestamp of the latest event dispatched to the sink.

### `stellarindex_source_last_insert_unix`

Gauge, label `source`. Unix-seconds timestamp of the latest successfully inserted trade row per source (`rowsInserted == 1`, not `ON CONFLICT DO NOTHING`).

With `stellarindex_source_last_event_unix` it exposes a stuck cursor or duplicate flood: the gap grows when every insert conflicts. For example `time() - stellarindex_source_last_insert_unix{source="sdex"} > 3600`. Complements `stellarindex_trade_insert_outcome_total`.

### `stellarindex_source_matched_events_total`

Counter, label `source`.

Inputs the decoder's `Matches()` claimed: the denominator of error rate, `rate(stellarindex_source_decode_errors_total[5m]) / rate(stellarindex_source_matched_events_total[5m])`. Bumped before decode. Unlike `source_events_total` (decoder output).

### `stellarindex_source_decode_errors_total`

Counter, label `source`.

Per-event parse failures (shape mismatch, malformed XDR, invariant violations). Distinct from `orphan_events` (well-formed but partnerless) and `insert_errors` (persistence broke). Includes recovered panics; `stellarindex_decoder_panics_total` is the strict subset that crashed.

### `stellarindex_source_non_directional_swaps_total`

Counter, label `source`.

Completed swap+sync pairs where reserves moved on one token side only, so not a trade. Expected-zero; non-zero means inspect the pair, not a decode failure.

### `stellarindex_source_amount_degraded_total`

Counter, labels `source`, `field`.

Decoded events whose money field could not be parsed, so the row was kept with that amount NULL. Non-zero means served volume is silently under-reported. Emitted by sorocredit's settlement decoder (`field="settled_amount"`).

### `stellarindex_spectra_unlisted_infrastructure_total`

Counter, label `kind`.

Spectra registry `*_change` events naming infrastructure outside the audited set; each leaves the Spectra gate incomplete. `kind` is the change class (`factory_change`, `router_change`, `limit_order_engine_change`, `pt_wasm_hash_change`, `yt_wasm_hash_change`), pre-seeded at 0.

### `stellarindex_decoder_panics_total`

Counter, label `source`.

Decoder panics the dispatcher recovered and turned into a decode error, in `Matches` or `Decode`.

Look the moment it is non-zero: a decoder is silently dropping every event of that shape until a fixed binary ships. One increment is one poison event; a value climbing every ledger means that source is dark.

Alert: `stellarindex_decoder_panicked` (`> 0`, page), runbook [decoder-panicked](../../operations/runbooks/ingestion.md#stellarindex_decoder_panicked). It reads the raw value because `increase()` over a series born inside its window is 0. An absent series means no panic since boot, so it is not pre-seeded in `seedBoundedLabelSeries`.

### `stellarindex_dispatcher_tx_read_errors_total`, `stellarindex_dispatcher_tx_event_read_errors_total`, `stellarindex_dispatcher_entry_meta_unsupported_total`, `stellarindex_dispatcher_evicted_keys_unreadable_total`, `stellarindex_dispatcher_ledger_upgrade_entries_total`

Counters, no labels (process-wide — the underlying dispatcher counters
aren't attributable to a source).

- `tx_read_errors`: malformed transactions skipped.
- `tx_event_read_errors`: `GetTransactionEvents()` failed, dropping that tx's Soroban events.
- `entry_meta_unsupported`: entry-change walk skipped for an unhandled `TransactionMeta` version; classic changes in that tx become invisible.
- `evicted_keys_unreadable`: evicted-key list failed to read, so evicted balances stay served as live.
- `ledger_upgrade_entries`: network-parameter upgrade entries. Informational.

Alert on any sustained non-zero rate of the first four. They are cumulative; chart `increase(...[5m])` against the 5m flush interval.

### `stellarindex_source_unknown_symbols_total`

Counter, label `source`.

Oracle slots whose symbol or feed-id is not in the canonical allow-list. The slot is kept as a `raw:<symbol>` row (`canonical.AssetOracleRaw`), so it means "recorded as raw", not "lost". A sustained rate means the allow-list or feed registry needs amending; a re-derive then promotes the raw rows.

Alert: `stellarindex_ingestion_oracle_unknown_symbols`, runbook [oracle-unknown-symbols](../../operations/runbooks/ingestion-events.md#stellarindex_ingestion_oracle_unknown_symbols).

There is no log and no `symbol` label; find the symbol via `asset LIKE 'raw:%'` or `/v1/oracle/streams?include_unmapped=true`.

### `stellarindex_source_unrepresentable_symbols_total`

Counter, label `source`.

Oracle slots **dropped** because the symbol / feed-id cannot be held even as `raw:`: empty, over 64 bytes, or a byte outside printable ASCII `0x21-0x7E` (`canonical.NewOracleRawAsset`).

Separate from `stellarindex_source_unknown_symbols_total`: this is a hole. Nothing was written, so closing it needs a registry entry and a replay. Only RedStone can reach it. The dropped slot is named in the WARN log (`redstone: dropping slot with unrepresentable feed_id`).

Alert: `stellarindex_ingestion_oracle_unrepresentable_symbols`, runbook [oracle-unknown-symbols](../../operations/runbooks/ingestion-events.md#stellarindex_ingestion_oracle_unknown_symbols).

### `stellarindex_source_orphan_events_total`

Counter, label `source`.

Events that never correlated into a complete observation: Soroswap swap without sync (or reverse); Phoenix incomplete N-of-8 field set aged past 5 minutes.

### `stellarindex_source_uncorroborated_calls_total`

Counter, label `source`.

Oracle-class `ContractCall` invocations refused before `Decode` because they were only declared in the auth tree, never executed. Non-zero means a rejected forgery attempt or a routing change refusing legitimate calls. Alert `stellarindex_ingestion_uncorroborated_calls`, runbook [uncorroborated-calls](../../operations/runbooks/ingestion-events.md#stellarindex_ingestion_uncorroborated_calls).

### `stellarindex_external_poller_polls_total`

Counter, labels `source`, `outcome` ∈ {success, error, skipped}.

`PollOnce` invocations per external source and outcome. The `massive` FX worker is not one of them; it stamps `stellarindex_external_fx_last_quote_unix` instead. `skipped` is the cooldown path, kept apart so a backoff cannot mask absence-of-success alerts.

### `stellarindex_cex_stream_disconnect_total`

Counter, labels `source`, `reason` ∈ {stall, reset, broken_pipe, timeout, dial, server_requested, subscription_rejected, other}.

CEX WebSocket disconnects (binance, bitstamp, coinbase, kraken). `stall` is the half-open-socket detector and the most important. `reset` is the most common (Binance recycles every 6–12 min). Sustained `dial` or `timeout` means the venue is unreachable. `subscription_rejected` is coinbase refusing the subscribe frame, usually config. With `stellarindex_cex_stream_last_trade_unix` it separates "churning but flowing" from "stuck"; `stellarindex_external_poller_last_success_unix` is the poller-side gauge.

### `stellarindex_cex_stream_last_trade_unix`

Gauge, label `source`.

Unix-seconds timestamp of the latest trade forwarded from a CEX streamer to the sink. Zero/unset if none since start. Use `time() - <gauge>` for a connected but silent stream; analogue of `stellarindex_external_poller_last_success_unix`.

### `stellarindex_cex_stream_entry_skips_total`

Counter, labels `source`, `reason` ∈ {unknown_symbol, bad_qty, bad_price, bad_timestamp, bad_trade_id, other}.

Trade entries a kraken parser could not convert inside a well-formed frame. `stellarindex_source_decode_errors_total` does not move, so this is the only signal of a renamed symbol or changed encoding. Alerted by `stellarindex_cex_stream_entry_skips`.

### `stellarindex_cex_stream_subscription_rejected`

Gauge, labels `source`, `symbol`.

1 while the venue has refused the trade subscription for `symbol`, 0 once acknowledged. A refused symbol leaves the socket healthy and the pair silent. A symbol outside the configured pair map is recorded as `symbol="unknown"`. Alerted by `stellarindex_cex_stream_subscription_rejected`.

### `stellarindex_external_poller_last_success_unix`

Gauge, label `source`.

Unix-seconds timestamp of the latest successful `PollOnce` per external source. Zero/unset if never succeeded since start. Staleness is `time() - <gauge>` (see `stellarindex_external_poller_stale`, `stellarindex_external_poller_polls_total`).
### `stellarindex_external_fx_last_quote_unix`

Gauge, label `source`.

UNIX-seconds time of the last successful `fx_quotes` write from the fiat-FX feed (`massive`). It advances only when a batch carries a fresh upstream rate; a failed write or an anchor-only batch leaves it unchanged.

Separate from `stellarindex_external_poller_last_success_unix`: `massive` writes outside the poller framework. The forex snap looks back 7 days, so a dead feed hides for a week. Healthy age is under 1 h. `stellarindex_external_fx_feed_stale` fires at 6 h and `stellarindex_external_fx_feed_absent` when the series is missing.

### `stellarindex_external_fx_rate_rejected_total`

Counter, labels `source`, `reason`.

Upstream FX rates refused before `fx_quotes` for failing the sanity band.

`reason` has seven values: `deviation` (moved > 50% from last accepted rate, no confirming second fetch), `non_positive` (rate ≤ 0), `non_finite` (NaN / ±Inf), `history_deviation` (trailing-7d bar > 50% off the current rate), `history_deviation_stuck` (same bar within 1%, refused ≥ 12 times; excluded from the rejection alert), `deviation_history_conflict` (two-fetch confirmation vetoed by the trailing-7d majority), and `deviation_history_conflict_stuck` (same vetoed candidate, refused ≥ 12 times; also excluded from the alert).

Not labelled by ticker (~150 currencies of cardinality); the ticker is in the WARN log (`forex: rejected upstream rate`). Zero-seeded.

### `stellarindex_external_fx_baseline_healed_total`

Counter, label `source`.

Baselines re-pointed at the median of ≥ 4 agreeing trailing-7d bars. Rare by design; confirmed baselines are never healed. A single rejection is expected (two-strike guard); sustained non-zero is alerted by `stellarindex_external_fx_rate_rejections`.

### `stellarindex_fx_fixings_last_refresh_unix`

Gauge, no labels.

UNIX seconds of the last completed `fx_fixings` append cycle, stamped even when nothing was written. `stellarindex_fx_fixings_refresh_stale` fires at 3 h.

### `stellarindex_fx_fixings_newest_bar_end_unix`

Gauge, no labels.

UNIX seconds of the newest `bar_end` committed to `fx_fixings`. A gap while the market trades becomes `fx_leg_unavailable` withholds; `stellarindex_fx_fixings_series_stale` fires at 4 h outside the weekend close.

### `stellarindex_fx_fixings_quote_disagrees`

Gauge, label `ticker`.

1 when an accepted hourly bar is outside [0.5, 1.5] of the guarded daily rate for that ticker and day. `stellarindex_fx_fixings_quote_disagreement` fires after 2 h.

### `stellarindex_fx_fixings_bars_refused_total`

Counter, label `ticker`.

Vendor hourly bars refused because the close was outside [0.5, 1.5] of the median of the preceding 96 h.

### `stellarindex_fx_fixings_fetch_errors_total`

Counter, label `ticker`.

Failed per-ticker hourly-bar fetches. Each cycle refetches the trailing 48 h, so an isolated increment loses nothing.

### `stellarindex_fx_fixings_write_errors_total`

Counter, no labels.

Failed `fx_fixings` batch inserts; the next cycle re-offers the same bars.

### `stellarindex_fx_fixings_write_tx_seconds`

Histogram, no labels.

Duration of one `fx_fixings` batch insert transaction.

### `stellarindex_amm_self_pair_swap_total`

Counter, label `source`.

AMM swaps decoded as a self-pair (`token_in == token_out`) and dropped to zero rows; the raw event stays in `soroban_events`. Detection only. Any sustained count is exploit-shaped and drives `stellarindex_amm_self_pair_swap_burst`.

### `stellarindex_amm_swap_received_divergence_total`

Counter, label `source`.

Decoded swaps where the pool-received sell amount differs from the offer (a fee-on-transfer token), emitted by phoenix with a WARN log. Counts decodes, so replays re-count. Detection only; seeded at zero for `phoenix`.

### `stellarindex_amm_non_positive_swap_total`

Counter, label `source`.

AMM swaps with a non-positive amount (`in <= 0` or `out <= 0`), dropped to zero rows. Sibling of `stellarindex_amm_self_pair_swap_total`. Detection only.

### `stellarindex_external_dust_dropped_total`

Counter, label `source`.

Streamed CEX trades dropped as dust: quote leg below ~$0.001 (the 10^8-scale floor). Dust distorts unweighted OHLC high/low.

A non-trivial rate is healthy; a drop to zero on a dusty venue (e.g. `coinbase`) can mean a wedged streamer (see `stellarindex_cex_stream_last_trade_unix`).

### `stellarindex_external_bad_timestamp_dropped_total`

Counter, label `source`.

CEX trades (streamed and polled) dropped for an implausible vendor timestamp: over 5 minutes ahead of the local clock, or before 2015-09-30. Non-zero means a venue changed format.

### `stellarindex_external_poller_refused_entries_total`

Counter, labels `source`, `reason` ∈ {ambiguous_symbol, id_mismatch}.

Poller response entries refused, so the ticker gets no reference price that poll. Emitted by CoinMarketCap. `ambiguous_symbol` is a `symbol=` lookup returning several coins (needs a `coinmarketcap_id`); `id_mismatch` is a payload fault.

### `stellarindex_discovery_dropped_hits_total`

Counter, no labels.

Discovery hits dropped because the async sink buffer was full. Not labelled per sniffer. Any increase means degrading coverage, not ingest backpressure; usually a Postgres outage or cold-start burst.

### `stellarindex_discovery_skipped_hits_total`

Counter, no labels.

Hits skipped as already enqueued (dedup key contract_id + kind + symbol). A high ratio is healthy; a restart resets the set.

### `stellarindex_discovery_record_failures_total`

Counter, no labels.

Hits whose `Recorder.Record` write failed, distinct from `stellarindex_discovery_dropped_hits_total` (buffer full) and `stellarindex_discovery_skipped_hits_total` (dedup). Counts attempts, not permanent loss; sustained rate alerts via `stellarindex_ingestion_discovery_record_failures`.

### `stellarindex_metrics_registry_present`

Gauge, label `component`. Value 0/1.

`1` when the component has a Prometheus Registry, `0` when Registry-less (SDK metrics never register). Absent means unused on that binary. The case is `component="ledgerstream"`. Alerted by `stellarindex_metrics_registry_absent` (`runbooks/meta.md#stellarindex_metrics_registry_absent`).

This gauge does not imply the `both_missing` page is dead: `stellarindex_ledgerstream_tier_read_total` and `stellarindex_ledgerstream_cold_read_duration_seconds` are registered unconditionally in `internal/obs`.

### `stellarindex_ledgerstream_tier_read_total`

Counter, label `outcome` (`hot` / `cold` / `both_missing`).

Tiered reads by serving tier: `hot` = local `galexie-archive` MinIO, `cold` = `aws-public-blockchain` after a hot miss, `both_missing` = neither had it (reader stalled). Sustained `both_missing` is the P1 page `stellarindex_ledgerstream_tier_both_missing` (`runbooks/ingestion.md#stellarindex_ledgerstream_tier_both_missing`). 

### `stellarindex_ledgerstream_stream_path_total`

Counter, label `path` (`tiered` / `cold_degraded` / `hot_single_ledger` / `sdk`).

One increment per `ledgerstream.Stream` walk, by read path. `cold_degraded` = cold tier configured but failed to open, so the walk ran hot-only; a nonzero rate means archive reads will stall on the first trimmed ledger. Seeded at boot.

### `stellarindex_ledgerstream_cold_read_duration_seconds`

Histogram, label `outcome` (`ok` = cold hit / `miss` = cold not-found / `error` = cold transient failure).

Latency of cold-tier reads only. Buckets run 5 ms to 30 s. Sibling of `stellarindex_ledgerstream_tier_read_total`.

### `stellarindex_ledgerstream_trailing_missing_tolerated_total`

Counter, no labels.

Bounded walks where a missing-object error became a clean walk-complete short of `To`. Expected at the lake tip; a climb well below the tip means a real hole was tolerated.

### `stellarindex_ledgerstream_live_start_retries_total`

Counter, no labels.

Re-attempts of a live-tail stream that failed before delivering any ledger, because the lake could not be opened (MinIO down, credentials rejected, bucket gone).

Look at it first in `runbooks/ledger-ingest.md#stellarindex_ingestion_ledger_stalled`. Cursor flat and this climbing means lake reachability (check `minio.service`, the bucket, credentials in `/etc/default/stellarindex-ops`). Cursor flat and this flat means look at Postgres, the dispatcher or the sink.

No `exhausted` counterpart exists; exhaustion is covered by `stellarindex_ingestion_ledger_stalled` and `stellarindex_minio_exporter_down`.

### `stellarindex_source_insert_errors_total`

Counter, labels `source`, `kind` (`trade` / `trade_abandoned` /
`oracle` / `panic` / `unhandled` / `dropped` / `soroswap_router_swap` /
`defindex_flow_strategy` / `defindex_flow_vault`).

Events that failed to persist. A sustained rate signals storage distress.

`trade` is a trade permanently dropped on a data fault; alerted at any rate by `stellarindex_ingestion_persist_drop`, as are the `soroswap_router_swap` and `defindex_flow_*` kinds. `trade_abandoned` is a retry abandoned on shutdown or cycle timeout; the row re-derives from the CH lake, so it is not a drop. `unhandled` is an event type the sink's type-switch does not recognise, usually a half-wired new source. `panic` is a recovered panic in the event-sink handler. `dropped` is external CEX/FX trades lost when the bounded retry buffer overflowed or a data fault could not be isolated; infrastructure faults retry instead (see [`stellarindex_trade_insert_retries_total`](#stellarindex_trade_insert_retries_total)), so a firing alert means genuine loss.
### `stellarindex_cursor_last_ledger`

Gauge, label `source`.

Mirror of the committed `ingestion_cursors.last_ledger`. It advances only when a ledger was fully processed and its cursor row committed, so it is the liveness signal for the whole ingest path.

Only `source="ledgerstream"` is emitted. `ledger-ingest-stalled` (page) fires on a flat or absent series. `cursor-stuck` joins on `stellarindex_source_enabled == 1`, so it does not cover `ledgerstream` (a cursor namespace, not a source).

### `stellarindex_trade_inserts_total`

Counter, labels `source`, `usd_volume_populated` (`yes` | `no` |
`refused` | `unroutable` | `thin` | `error`). `error` is a live trade left
NULL because a USD resolver read failed; it counts against coverage, since a
re-derive can still price it. `unroutable` is an unpriced trade whose two classic
legs share one issuer, or neither of whose legs had a peg or XLM market at all
(XLM and the pegs never count as having no market, so an outage stays `no`); `thin` is an unpriced trade whose only candidate rate
was refused by the substance gate (market below the valuation floor). The
on-chain coverage alert excludes both from the ratio.

Counts insert attempts; `ON CONFLICT DO NOTHING` dedupe is invisible here.

### `stellarindex_trade_insert_outcome_total`

Counter, labels `source`, `outcome` (`new` | `duplicate`).

`new` means a row was persisted; `duplicate` means the conflict fired. A replay loop or stuck tip shows `duplicate` growing with zero `new`.

Alert with `unless`, never `and on (source) ... == 0`: a source with no new row since start has no `outcome="new"` series, so an `and` join matches nothing. `source` is not pre-seeded.

### `stellarindex_dex_trade_unit_ratio_total`

Counter, label `source`.

Bumps once per landed on-chain trade (`ledger != 0`; CEX/FX use a different scale) whose `base_amount` equals its `quote_amount`, both nonzero. It catches decoders that write every row but get the numbers wrong, which completeness checks miss. A handful is normal; a sustained stream from one source (`increase(...[30m]) > 25`, alert `dex_trade_unit_ratio_detected`) means a broken decoder.

### `stellarindex_trade_insert_retries_total`

Counter, label `outcome` (`retry` | `recovered` | `abandoned`).

The trade sink's blocking-retry path (ADR-0041) for infrastructure faults.

- `retry`: one backoff attempt. A sustained rate means the write path is blocked and the on-chain cursor is not advancing; data is held in memory. `trade_insert_backpressure` fires on it.
- `recovered`: a blocked insert landed after a retry.
- `abandoned`: gave up on shutdown; re-derivable from the CH lake.

Genuine drops land on [`stellarindex_source_insert_errors_total`](#stellarindex_source_insert_errors_total) (`kind=trade` / `kind=dropped`).

### `stellarindex_trades_zero_leg_admitted_total`

Counter, label `source`. Seeded at zero for `sdex`.

Trades stored with exactly one zero leg (an SDEX fill rounded to zero stroops). The row has no price, so price paths filter it out. Incremented before the INSERT, so a batch retried row by row counts twice; it is an upper bound. No alert.

### `stellarindex_trade_insert_buffer_depth`

Gauge (no labels).

External (CEX/FX) trades in the bounded retry buffer; on overflow the oldest drop (`stellarindex_source_insert_errors_total{kind="dropped"}`). On-chain trades block-and-retry, so this is external-only. A depth that stays high means Postgres is unreachable and external price freshness is degrading.

### `stellarindex_stream_publish_total`

Counter, label `stream` (currently only `price_stream`).

Envelopes fanned out to the hub, only on a new closed bucket. Flat with active subscribers means the `PriceReader` sees no new buckets; steady with zero subscribers means clients are not connecting.

### `stellarindex_stream_publish_stall_total`

Counter, label `stream` (currently only `price_stream`).

Poll ticks where the reader missed its per-poll timeout. A climbing value means the reader is slower than the poll interval. Unlike `stellarindex_stream_publish_total`, it does not move on a quiet market.

### `stellarindex_ch_live_sink_ledgers_total`

Counter, label `outcome` (`written` | `buffered` | `dropped` |
`errored`).

Ledgers processed by the ClickHouse real-time dual-sink (ADR-0034); emitted only when `storage.clickhouse_live_sink` is enabled.

- `written`: durably flushed.
- `buffered`: accepted into memory. `buffered − written` is the backlog.
- `dropped`: shed by design on a full channel or buffer (default cap 4096). `ch-live-catchup` re-fills them. A steady climb means the live edge is degrading.
- `errored`: never written, from a sink `Add` / `Flush` failure or a `clickhouse.ExtractLedger` failure. A systematic extract break is not healed by `ch-live-catchup`. Alert `stellarindex_ingestion_ch_live_sink_errors`.

### `stellarindex_ch_live_sink_read_undercount_total`

Counter, label `kind` (`tx_read_errors` | `tx_event_read_errors` |
`entry_meta_unsupported` | `soroban_fee_meta_unsupported` |
`evicted_keys_unreadable` | `tx_read_errors_census` | `tx_event_read_errors_census`). Every kind is
seeded at zero.

Transactions the per-ledger read paths could not fully decode. Each increment is the affected transactions in one ledger (ledgers for `evicted_keys_unreadable`).

- `tx_read_errors`, `tx_event_read_errors`, `entry_meta_unsupported`: the ledger is still written but short, and `ch-live-catchup` never revisits it.
- `soroban_fee_meta_unsupported`: unhandled `TransactionMeta` version, so fee columns are written as 0.
- `evicted_keys_unreadable`: eviction `removed` rows are missing.
- `tx_read_errors_census`, `tx_event_read_errors_census`: from `dispatcher.CensusLedger`; a substrate gap.

Any increase matters; it is zero when healthy. A climb tracking `stellarindex_ch_live_sink_ledgers_total{outcome="written"}` is a `TransactionMeta` version break. Alert `stellarindex_ingestion_ch_live_sink_read_undercount`.

### `stellarindex_sink_undrained_rows_total`

Counter, labels `sink` (`persist_events`) and `kind` (`trade` |
`event`).

Rows a sink's shutdown drain could not land in budget; the Postgres twin of `stellarindex_ch_live_sink_ledgers_total{outcome="dropped"}`. The cursor is upserted first, so these are gaps it never revisits.

- `sink="persist_events"`: the only emitter today.
- `kind="trade"`: per row; recover with `stellarindex-ops ch-rebuild -sdex-gaps`.
- `kind="event"`: non-trade writes; no ledger is carried, so use the source's gap detector.

Pre-seeded at zero; it should be 0 across clean deploys. A scrape can miss it; the journal ERROR line is authoritative. Alert `stellarindex_ingestion_sink_undrained_rows`.

### `stellarindex_hashdb_append_total`

Counter, label `outcome` (`ok` / `error`).

Per-ledger outcome of the ADR-0016 hashdb append side.

`error` means a write failed. Ingest is unaffected, but a sustained rate means the detector is blind, and the verify sweep will not show it (it keeps returning `ok` on a stale window). This is the only signal for append death; no alert.

### `stellarindex_hashdb_append_duration_seconds`

Histogram, label `outcome` (matches `stellarindex_hashdb_append_total`).
Buckets 10 µs – 10 ms.

Latency of `hashdb.Append`, a synchronous O(1) write on the ingest hot path.

### `stellarindex_hashdb_verify_runs_total`

Counter, labels `outcome` (`ok` / `drift` / `error`) and `window`
(`recent` = trailing window).

Hourly sweep re-reading the trailing 20000 ledgers and comparing hashes. `drift` is the serious case (see `stellarindex_hashdb_drift_total`). `error` means the sweep could not complete, so the detector is blind.

### `stellarindex_hashdb_verify_run_duration_seconds`

Histogram, label `outcome` (matches `stellarindex_hashdb_verify_runs_total`).
Buckets 1 s – 30 min.

Wall-clock of one sweep. If `ok` p95/p99 approach the 60-minute interval, shrink the window or widen the interval.

### `stellarindex_hashdb_drift_total`

Counter, unlabelled (deliberately — the natural label, ledger
sequence, is unbounded cardinality).

Ledgers found drifted since the indexer started. Nonzero means upstream rewrote a ledger or the local copy is corrupt. `stellarindex_hashdb_drift_detected` alerts on it. Sequences appear only in the ERROR log.

## Oracle layer (indexer binary, reflector + future sources)

### `stellarindex_oracle_stream_rows_unparsed_total`

Counter, labels `source`, `field` (`asset` | `quote`).

`oracle_updates` rows dropped from the served stream because their canonical text would not parse.

It should stay at zero. Any increase means rows are silently missing from `/v1/oracle/streams` and `/oracles`. Check it after any hand-written relabel of `oracle_updates.asset` / `.quote`; a typo drops the row. `field` names the column.

### `stellarindex_oracle_last_update_unix`

Gauge, labels `source`, `asset`.

Unix-seconds timestamp of the latest oracle observation for the pair. The `oracle-stale` alert compares its age with `oracle_staleness_budget_seconds`.
### `stellarindex_oracle_resolution_seconds`

Gauge, label `source`.

Declared publication cadence of the oracle (Reflector 300 s, Band 3600 s, RedStone 86400 s), set once at construction.

### `stellarindex_oracle_staleness_budget_seconds`

Gauge, labels `source`, `asset`.

How long a pair may go without a publication before `oracle-stale` tickets. Defaults to 10 × the source's resolution; overridden per pair by `[[oracle.staleness_overrides]]`.

## API layer (api binary)

`price_alert`, `divergence_webhook` and `freeze_webhook` of the price-serve gate counters are emitted by the **aggregator** binary, not this one. A dashboard summing the counter without a `job` selector mixes both processes.

### `stellarindex_price_staleness_seconds`

Gauge, labels `asset`, `quote`.

Age of the latest aggregated VWAP write per pair. XLM is emitted under both `native` and `crypto:XLM`. `price-stale` fires above 120 s or when absent.

### `stellarindex_ratelimit_fail_open_total`

Counter, no labels.

Requests that bypassed rate-limiting because Redis errored (deliberate fail-open). Alert: `stellarindex_ratelimit_fail_open`.

### `stellarindex_ratelimit_fail_closed_total`

Counter, label `limiter` (`api` / `failed_auth` / `signup_ip`). Pre-seeded at zero.

Requests rejected after a Redis-backed throttle erred longer than its 30s dwell-time; the limiter then fails **closed** and `stellarindex_ratelimit_fail_open_total` stops moving.

- `api` — 503 to every request on the bucket; sustained means a whole-API outage.
- `failed_auth` — 429 to bad credentials.
- `signup_ip` — 503 on signup and register.

`stellarindex_redis_command_errors_total{class}` names it.

### `stellarindex_redis_command_errors_total`

Counter, label `class`. Pre-seeded at zero.

Failed Redis commands from every client, counted after retries. `class` is the Redis reply prefix (`MISCONF`, `OOM`, `READONLY`, `NOREPLICAS`, `EXECABORT`, `LOADING`, `WRONGTYPE`, `BUSY`, `MASTERDOWN`, `NOAUTH`, `WRONGPASS`, `NOPERM`, `CLUSTERDOWN`, `TRYAGAIN`, `MOVED`, `ASK`, `ERR`), `other`, or a transport class: `timeout`, `canceled` (usually a client abort) or `io`. `redis.Nil` and the `NOSCRIPT` fallback are not counted. Best-effort writes drop their errors, so this is the only signal they are refused.

### `stellarindex_monthly_quota_fail_open_total`

Counter, no labels.

Requests that bypassed the per-key monthly quota because the counter read errored. A metered key then bills past its cap, unrecoverably. Pre-seeded at zero. Alert: `stellarindex_monthly_quota_fail_open`.

### `stellarindex_monthly_quota_fail_closed_total`

Counter, no labels.

Requests rejected with `429` once the counter has been unreadable longer than the dwell window (30s). Non-zero means metered customers are being 429'd. Alert: `stellarindex_monthly_quota_fail_closed`.

### `stellarindex_admin_audit_write_failures_total`

Counter, label `surface` (`account_override` / `key_mint` / `key_revoke` / `status_notice` / `staff_customer_lookup` / `admin_account_read` / `passkey_register` / `passkey_delete` / `passkey_clone_warning` / `passkey_login_replay`). The `passkey_*` surfaces cover add/remove and the refusals counted by `stellarindex_passkey_login_refusals_total`.

Privileged mutations that completed but whose audit row failed to append (logged and swallowed). `staff_customer_lookup` and `admin_account_read` are reads: the audit row is the only evidence of PII access. Non-zero means audit holes to rebuild from logs. Pre-seeded at zero. Alert: `stellarindex_admin_audit_write_failing`.

### `stellarindex_passkey_login_refusals_total`

Counter, label `reason` (`clone_warning` / `ceremony_replay`).

Sign-ins refused after the signature verified. `clone_warning` is a sign-counter regression (at least two copies of the key); `ceremony_replay` is a finish-login request presented twice. Pre-seeded at zero. Alert: `stellarindex_passkey_clone_warning` (on `clone_warning` only).

### `stellarindex_passkey_credential_changes_total`

Counter, label `change` (`added` / `removed`).

Passkeys added or removed; each emails the user (`stellarindex_notify_sends_total`). A burst of `added` across accounts looks like hijacked sessions becoming durable credentials. Pre-seeded at zero. No alert.

### `stellarindex_failed_auth_total`

Counter, label `outcome` (`rejected` / `throttled`).

Credentials the Auth middleware rejected: `rejected` is 401/403, `throttled` is 429 from the failed-auth throttle. 503s are not counted. Pre-seeded at zero. Alert: `stellarindex_failed_auth_rate_high` (on `rejected` only).

### `stellarindex_admin_key_budget_clamps_total`

Counter, label `outcome` (`lowered` / `failed`).

API credentials (not clamp calls) lowered to the tier ceiling on an admin override; four keys add 4. `failed` matters: the credential kept its higher budget. Not alerted. Pre-seeded at zero.

### `stellarindex_mint_scope_clamp_refused_total`

Counter, label `route` (`/v1/admin/keys` / `/v1/account/keys`).

Key-mint requests refused for asking more scope or rate limit than the minting credential holds. Labelled by route, not actor, to bound cardinality. Pre-seeded at zero.

### `stellarindex_aggregator_ticks_total`

Counter, label `outcome` (`ok` / `error`).

One increment per aggregator tick. `error` means at least one refresh failed.

### `stellarindex_aggregator_catalogue_tickers_skipped`

Gauge, no labels.

Catalogue tickers with a `coingecko_id` excluded from the cross-check because they are not on the ADR-0014 allow-list; non-zero means silently unchecked.

### `stellarindex_aggregator_vwap_writes_total`

Counter, no labels.

Cumulative VWAP cache writes. Unlabelled to bound cardinality. A sustained zero rate means the aggregator is silent.

### `stellarindex_aggregator_vwap_cache_write_errors_total`

Counter, no labels.

Failed Redis `SET` attempts when writing the VWAP cache. Sustained failures make `/v1/price` return 404 on cached pairs while `flags.stale` stays unset. Alert on a non-zero rate for 2 min.

### `stellarindex_aggregator_contribution_write_errors_total`

Counter, no labels.

Source-contribution batches that failed to persist to `price_source_contributions`; a failure loses that bucket's whole breakdown.

### `stellarindex_aggregator_empty_windows_total`

Counter, no labels.

(pair, window) refreshes with zero VWAP-eligible trades after filtering. Sustained all-empty means the pair set has out-grown the live data.

### `stellarindex_aggregator_window_truncated_total`

Counter, no labels.

(pair, window) trade fetches that hit `MaxTradesPerWindow` (default 10,000), so the VWAP covers only the newest slice. Chart against `stellarindex_aggregator_vwap_writes_total`.

### `stellarindex_aggregator_stream_publish_total`

Counter, label `outcome` (`ok` / `error`).

Closed-bucket events handed to the `StreamPublisher` (Redis pub/sub). `error` is best-effort; the VWAP cache write is unaffected.

### `stellarindex_api_stream_subscribe_total`

Counter, label `outcome` (`ok` / `decode_error` / `malformed`).

Pub/sub messages the API `Subscriber` processed. `ok` was republished on the local Hub. `decode_error` is wire-format drift. `malformed` had an empty Asset or Quote and was dropped.

### `stellarindex_api_tip_producers`

Gauge. Shared `/v1/price/tip/stream` producers registered, running or lingering; bounded by `api.streaming.max_tip_producers`. A count climbing while SSE connections do not is an abort-loop flood.

### `stellarindex_api_tip_producers_refused_total`

Counter, label `reason` (`caller_quota` / `global_ceiling`).

Producer mints refused. `caller_quota` means one client (IPv4 or IPv6 /64) is over `api.streaming.max_tip_producers_per_caller`; `global_ceiling` means the registry is full at `api.streaming.max_tip_producers`.

### `stellarindex_api_stream_subscriber_drops_total`

Counter. SSE subscribers disconnected because their queue was full, counted once per subscriber. A low rate is routine; clients reconnect.

### `stellarindex_api_sse_streams_active`

Gauge. SSE connections open across all stream endpoints, bounded by `api.streaming.max_concurrent_streams`. Compare with `stellarindex_api_stream_subscribe_total{outcome="ok"}`.

### `stellarindex_api_sse_streams_rejected_total`

Counter, label `reason` (`global_cap` / `per_ip_cap` / `topic_cap`).

SSE connections refused with 503 by the concurrency caps. `topic_cap` means the Hub's topic map is full of topics with live subscribers.

### `stellarindex_api_stream_hub_topics`

Gauge. Topics the Hub holds, updated on topic creation. The topic key is client-supplied on `/v1/price/stream`.

### `stellarindex_api_stream_hub_topics_reaped_total`

Counter. Topics the reaper evicted. Flat at zero beside a high `stellarindex_api_stream_hub_topics` means the retention policy is mistuned.
### `stellarindex_api_cors_decisions_total`

Counter, label `outcome` (`no_origin` / `allowed_origin` /
`allowed_wildcard` / `denied`).

Per-request CORS decisions. `no_origin` = no Origin header; `denied` = Origin not in the allow-list (browser blocks it). Use it to spot a wildcard policy serving real cross-origin traffic in production.

### `stellarindex_customer_webhook_delivery_attempts_total`

Counter, label `outcome` (`delivered` / `server_error` /
`client_error` / `exhausted` / `network_error` / `webhook_missing` /
`disabled` / `no_secret` / `build_error` / `list_error` /
`lookup_error` / `mark_error`). All twelve are pre-seeded at zero.

Per-attempt outcome of the delivery worker. `server_error` and `network_error` are retried; `client_error`, `webhook_missing`, `disabled`, `no_secret` and `build_error` are terminal; `list_error` / `mark_error` are transient db failures.

`mark_error` means the POST succeeded but recording it failed, so the same payload is re-POSTed every lease interval with no retry budget to end it. One wedged row is about 0.003/s, far under the `server_error` alert threshold, so it needs its own alert.

### `stellarindex_customer_webhook_fanout_failures_total`

Counter, labels `event_type` (`incident.sev1` / `incident.resolved` /
`anomaly.freeze` / `divergence.firing` / `price.alert`) × `reason`
(`invalid_payload` / `list_subscribers` / `enqueue`).

Producer-side counterpart to the delivery-attempts counter: events that never became a delivery row. No retry, no dead-letter; the customer has lost the event.

`enqueue` counts once per lost delivery (per subscriber); `list_subscribers` and `invalid_payload` once per lost fan-out (all subscribers). All pairs are pre-seeded at zero.

Alert: `stellarindex_customer_webhook_fanout_failing` →
[customer-webhook-fanout-failing](../../operations/runbooks/aggregator.md#stellarindex_customer_webhook_fanout_failing).

### `stellarindex_usage_units_dropped_total`

Counter, label `counter` (`billable` / `detail`). Pre-seeded at zero.

Request units served but not written to the Redis usage counters. The request still succeeds. A `billable` drop is unmetered quota and cannot be reconstructed. The Redis error class is on `stellarindex_redis_command_errors_total`.

### `stellarindex_after_response_tasks_dropped_total`

Counter. Pre-seeded at zero.

Post-response bookkeeping tasks dropped because the shared worker pool's queue was full. The pool drops rather than block the request; this is the only signal.

Alert: `stellarindex_after_response_tasks_dropping` →
[after-response-tasks-dropping](../../operations/runbooks/api.md#stellarindex_after_response_tasks_dropping).

### `stellarindex_usage_rollup_sweeps_total`

Counter, label `outcome` (`ok` / `scan_error` / `sink_error`).

Per-sweep outcome of the usage-rollup worker (Redis counters into `usage_daily`, every 5 minutes). `scan_error` = Redis trouble; `sink_error` = Postgres upsert failing. Redis keeps counters 35 days, so short outages lose nothing. Informational. Alert: `stellarindex_usage_rollup_failing`.

### `stellarindex_usage_rollup_sweep_duration_seconds`

Histogram, label `outcome` (matches
`stellarindex_usage_rollup_sweeps_total`). Buckets 5 ms – 30 s.

Wall-clock of one sweep. Chart `ok` p95/p99 apart from errors: "slow" is an earlier signal than "failing". Nearing the 5-minute cadence makes sweeps overlap.

### `stellarindex_protocol_events_rollup_sweeps_total`

Counter, label `outcome` (`ok` / `refresh_error`).

Per-sweep outcome of the protocol-events rollup, which feeds `events_24h` on `/v1/protocols`. Sustained `refresh_error` leaves the column stale, not blank. Informational. Alert: `stellarindex_protocol_events_rollup_failing`.

### `stellarindex_protocol_events_rollup_sweep_duration_seconds`

Histogram, label `outcome` (matches
`stellarindex_protocol_events_rollup_sweeps_total`). Buckets 10 ms – 30 s.

Wall-clock of one sweep. Rising `ok` p95/p99 means the census is growing heavier.

### `stellarindex_asset_volume_rollup_sweeps_total`

Counter, label `outcome` (`ok` / `refresh_error`).

Per-sweep outcome of the asset-volume rollup, which feeds `volume_24h_usd` on `/v1/assets`. Sustained `refresh_error` leaves the column stale, not blank. Informational. Alert: `stellarindex_asset_volume_rollup_failing`.

### `stellarindex_asset_volume_rollup_sweep_duration_seconds`

Histogram, label `outcome` (matches
`stellarindex_asset_volume_rollup_sweeps_total`). Buckets 50 ms – 60 s.

Wall-clock of one sweep. Nearing the 2-minute cadence makes sweeps overlap.

### `stellarindex_asset_character_rollup_sweeps_total`

Counter, label `outcome` (`ok` / `refresh_error`).

Per-sweep outcome of the asset-volume-character rollup, which feeds the `volume_character` label on `/v1/assets` and `/v1/assets/{id}`. Sustained `refresh_error` leaves it stale, not blank. Analytics-only. Alert: `stellarindex_asset_character_rollup_failing` (6h cadence, so it keys on a 13h window: errors with no `ok` sweep).

### `stellarindex_asset_character_rollup_sweep_duration_seconds`

Histogram, label `outcome` (matches
`stellarindex_asset_character_rollup_sweeps_total`). Buckets 50 ms – 120 s.

Wall-clock of one sweep; the heaviest asset rollup. Nearing the 15-minute cadence makes sweeps overlap.

### `stellarindex_price_alert_eval_total`

Counter, label `outcome` (`ok` / `list_error` / `partial_error`).

Per-sweep outcome of the price-alert evaluator. Emits only when `[price_alerts] enabled = true`. `list_error` = the alerts read failed, so the whole sweep was skipped. `partial_error` = at least one alert errored or timed out; `stellarindex_price_alert_evaluated_total` says how many. Alert: `stellarindex_price_alert_eval_failing`.

### `stellarindex_price_alert_evaluated_total`

Counter, label `outcome` (`fired` / `not_crossed` / `already_fired` / `no_price` / `stale` /
`cooling_down` / `no_subscriber` / `claim_lost` / `error` / `timeout`),
every child seeded when the evaluator is built.

One increment per alert per sweep. `timeout` = the alert's own deadline (a third of the sweep interval). `stale` = VWAP bucket older than 15 min, rejected. `already_fired` = condition still holds; it re-arms once cleared. Over half `error` / `timeout` for 30 min fires `stellarindex_price_alert_evaluations_failing`.

### `stellarindex_price_alert_last_sweep_unix`

Gauge, no labels. Unix time of the last completed sweep, seeded with start time; 0 if never started. Older than 15 min for 5 min fires `stellarindex_price_alert_sweep_stale`, the only signal for a wedged sweep.

### `stellarindex_price_alert_eval_duration_seconds`

Histogram, label `outcome` (matches
`stellarindex_price_alert_eval_total`). Buckets 5 ms – 30 s.

Wall-clock of one sweep. Nearing the 30 s tick makes sweeps overlap.

### `stellarindex_assets_popular_priceless`

Gauge, no labels.

Assets that are market-popular yet priceless with no recorded reason. "Popular" = 7-day priced USD volume > $10k or > 5k trades, excluding volume concentrated in one `(maker, taker)` pair so wash farms cannot qualify. `> 0` is a coverage gap that should page. Alert: `stellarindex_assets_popular_priceless`.

### `stellarindex_priceless_coverage_check_runs_total`

Counter, label `outcome` (`ok` / `error`).

Per-sweep outcome of the coverage tripwire. `error` = the read failed, so the gauge is stale; sustained `error` means the tripwire is blind.

### `stellarindex_priceless_coverage_check_last_success_unix`

Gauge, no labels.

Unix seconds of the last successful sweep. `time() - this` is the staleness signal. Alert: `stellarindex_priceless_coverage_check_stale` (> 30 min).

### `stellarindex_change_summary_passes_total`

Counter, label `outcome` (`ok` / `partial` / `failed`).

Per-pass outcome of the explorer delta-strip worker. `partial` = some entities failed (a pair with no recent trades fails every pass); `failed` = none upserted, so `change_summary_5m` goes stale.

### `stellarindex_change_summary_last_success_unix`

Gauge, no labels.

Unix seconds of the last pass that upserted an entity. Seeded with start time; 0 on binaries without the worker. Alert: `stellarindex_change_summary_stale` (> 30 min), aggregator only.

### `stellarindex_signup_reaper_runs_total`

Counter, label `outcome` (`ok` / `error`).

Per-sweep outcome of the reaper that deletes orphan `accounts` rows from racing `/v1/auth/callback` provisions. Hourly; emits only when `[signup_reaper] enabled = true`. `error` = the DELETE failed, so orphans pile up slowly. Zero rows deleted is still `ok`. Alert: `stellarindex_signup_reaper_failing`.

### `stellarindex_signup_reaper_run_duration_seconds`

Histogram, label `outcome` (matches
`stellarindex_signup_reaper_runs_total`). Buckets 5 ms – 30 s.

Wall-clock of one sweep. Healthy is a few ms.

### `stellarindex_signup_reaper_rows_deleted_total`

Counter, unlabelled.

Orphan accounts deleted. A steady non-zero `rate()` means a signup race fires regularly; look at `/v1/auth/callback` provisioning.
### `stellarindex_accounts_rows`

Gauge, unlabelled. Refreshed by every signup-reaper sweep.

Rows in `accounts`. `POST /v1/register` is unauthenticated and mints a permanent row per call. Sustained unexpected growth is registration abuse.

### `stellarindex_api_keys_rows`

Gauge, unlabelled. Refreshed alongside `stellarindex_accounts_rows`.

Rows in `api_keys`. One key per account, so it tracks the accounts gauge.

### `stellarindex_login_code_lockout_rows`

Gauge, unlabelled. Refreshed by every retention sweep (hourly).

Rows in `login_code_lockouts`, the per-email failed-verify counter. This is a security signal: the key is attacker-chosen (`POST /v1/auth/verify-code` is unauthenticated), and a wrong guess against a synthetic address inserts a row that sign-in can never clear. Healthy is low tens.

Alert: `stellarindex_login_code_lockout_table_growing` →
[login-code-lockout-table-growing](../../operations/runbooks/api.md#stellarindex_login_code_lockout_table_growing).

### `stellarindex_auth_reaper_last_sweep_unix`

Gauge, labelled `reaper` ∈ {`login_code`, `magic_link`, `signup`,
`session`, `webhook_delivery`}. Set at the end of every completed sweep, including failed ones.

A flat rows gauge with a stale sweep timestamp is a dead reaper, not a quiet table.

Alert: `stellarindex_auth_reaper_stalled` →
[auth-reaper-stalled](../../operations/runbooks/api.md#stellarindex_auth_reaper_stalled).

### `stellarindex_auth_reaper_interval_seconds`

Gauge, labelled `reaper` (same values). Configured sweep cadence; the stalled alert threshold is 3 × this.

### `stellarindex_retention_reaper_rows_deleted_total`

Counter, labelled `reaper` ∈ {`session`, `webhook_delivery`}. Sessions ended over 90 days ago and finished webhook deliveries over 30 days old. `audit_log` is not swept.

### `stellarindex_retention_reaper_errors_total`

Counter, labelled `reaper` (same values), pre-seeded at 0. Failed sweeps or row counts. Rising means the table is growing.

### `stellarindex_retention_reaper_rows`

Gauge, labelled `reaper` ∈ {`webhook_delivery`}. Current row count, refreshed every sweep. The `session` reaper publishes no count.

### `stellarindex_login_code_lockout_rows_deleted_total`

Counter, unlabelled.

Settled lockout rows removed by the sweep. Read next to `stellarindex_login_code_lockout_rows`.

### `stellarindex_login_code_lockout_errors_total`

Counter, label `op` (`status_check` / `register` / `sweep`).

Lockout machinery failures, non-fatal and invisible at the HTTP layer. Pre-seeded.

- `status_check`: the lockout read failed and the handler failed open; the lockout was not enforced.
- `register`: a wrong-code attempt was not recorded; the grinder got a free guess.
- `sweep`: the retention pass failed; attacker-created rows accumulate.

Alert: folded into
`stellarindex_login_code_lockout_table_growing` (the `status_check` arm)
→ [login-code-lockout-table-growing](../../operations/runbooks/api.md#stellarindex_login_code_lockout_table_growing).

### `stellarindex_magic_link_token_rows`

Gauge, unlabelled. Refreshed by every retention sweep (hourly).

Rows in `magic_link_tokens`. Like the lockout gauge this is a security and privacy signal: the table holds plaintext email and `requested_ip`, and `POST /v1/auth/login` is unauthenticated and inserts a permanent row for any address. Healthy is low.

### `stellarindex_magic_link_token_rows_deleted_total`

Counter, unlabelled.

Expired magic-link rows removed by the sweep. Read next to `stellarindex_magic_link_token_rows`.

### `stellarindex_magic_link_token_errors_total`

Counter, label `op` (`sweep`).

Failed magic-link sweeps, invisible at the HTTP layer. Pre-seeded on `sweep`.

### `stellarindex_notify_sends_total`

Counter, labels `template` (`magic-link` / `signup-verify` /
`passkey-changed` / `account-erased`), `result` (`sent` / `failed` / `suppressed`; the last is a send withheld
for a configured bounced address, never counted as a failure).

Transactional-email sends. The magic-link handler swallows send errors, so this is the only mail-outage signal. Zero-seeded across templates × {sent, failed}. A sustained `failed` ratio drives `stellarindex_notify_send_failure_ratio_high`.

### `stellarindex_aggregator_dropped_trades_total`

Counter, labels `reason` (`class` / `outlier`) and `pair`.

Trades removed from the VWAP input set. `class` = non-exchange source removed by the ClassExchange-only filter (usually a mis-registered venue in `external.Registry`). `outlier` = removed by the σ-threshold filter (usually market distress).

`pair` is the configured aggregate target pair (about 12 in production), so cardinality is bounded. Not pre-seeded.

Trap: the filter re-runs over the whole trailing window every tick, so a print that stays outside the band is counted on every tick. The rate is not "new outliers/s". `stellarindex_aggregator_outlier_storm` reads `stellarindex_aggregator_venue_vwap`, `stellarindex_aggregator_outlier_trim_fraction` reads `stellarindex_aggregator_window_trades`; only `stellarindex_aggregator_outlier_trim_rate_legacy` still gates on this counter.

### `stellarindex_aggregator_venue_vwap`

Gauge, labels `pair`, `window` (`5m` / `1h` / `24h`), `source`.

Per-source VWAP of the pre-outlier-filter (post-class-filter) trades for one refresh. A source that leaves the window has its series deleted. Feeds `stellarindex_aggregator_outlier_storm`: `max by (pair) / min by (pair) − 1` over `window="5m"` is venue disagreement.

### `stellarindex_aggregator_window_trades`

Gauge, labels `pair`, `window` (`5m` / `1h` / `24h`), `stage`
(`fetched` / `class` / `outlier`).

Trade count of the current refresh after each filter stage. `1 − outlier/class` is the current trim share. Feeds `stellarindex_aggregator_outlier_trim_fraction` (`> 0.2` on 24h with ≥ 20 class-filtered trades, 30m).

### `stellarindex_aggregator_window_base_volume`

Gauge, labels `pair`, `window` (`5m` / `1h` / `24h`), `stage`
(`class` / `outlier`).

Base-asset volume in whole units after the class and outlier filters. `1 − outlier/class` is the share of traded money removed, which count share cannot tell. Feeds `stellarindex_aggregator_outlier_volume_trim_fraction` (`> 0.2` on every window, 15m).

### `stellarindex_aggregator_dropped_windows_total`

Counter, label `reason` (`min_usd_volume`).

Windows suppressed at the window-level filter, distinct from `dropped_trades_total` (per-trade) and `empty_windows_total` (zero trades). `min_usd_volume` fires when a USD-valuable quote leg has a post-filter window below `aggregate.min_usd_volume`.

### `stellarindex_aggregator_min_usd_volume_unvaluable_total`

Counter, label `pair`.

Once per refresh where `aggregate.min_usd_volume` is set but the pair's on-chain quote asset has no recognised USD peg. The window is dropped fail-closed, so the pair publishes nothing until the peg is added to `trades.usd_pegged_classic_assets` or the matching `supply.sac_wrappers` row. Non-USD fiat quotes do not increment it.

### `stellarindex_price_serve_substance_withheld_total`

Counter, labels `surface` (the table below) and `floor` (`buckets` |
`span` | `volume` | `volume_unvalued`).

Once per aggregated-price decision withheld by the thin-market substance gate (`internal/pricingguard.SubstanceGate`): the pair has an on-chain leg and its trailing activity (USD volume / distinct 1-minute buckets / span) is below the floor, so the surface returned `price-withheld`. Raw surfaces (`/v1/observations`, `/v1/ohlc`, `/v1/history`) still serve.

`floor` is the first floor failed, checked in order buckets, span, volume. `volume_unvalued` means under half the active buckets carried a USD valuation, so `prices_1m` reads volume as $0. A jump in `volume_unvalued` without a rise in `volume` points at the valuation pipeline.

The `surface` values are shared by all four price-serve gate counters (this one, `stellarindex_price_serve_substance_unmeasured_total`, `stellarindex_price_serve_scam_withheld_total` and `stellarindex_scam_gate_lookup_failures_total`); Gates says which gate each surface consults. All four are emitted by both api and aggregator binaries, so sum across `job`. Pinned by `TestSubstanceWithheldLabelsAreDocumented`.

| Surface | Gates | Serving path |
|---|---|---|
| `anomalies` | both | `/v1/anomalies` rows |
| `asset_headline` | both | the asset headline price (GlobalAssetView) |
| `change_summary` | both | `/v1/changes` summary |
| `chart` | scam | `/v1/chart` series |
| `dex_tvl` | both | DEX TVL snapshot refresh valuing pool reserve legs (background) |
| `divergence` | both | `/v1/divergence` rows |
| `divergence_webhook` | both | aggregator divergence webhooks |
| `freeze_webhook` | both | aggregator freeze webhooks |
| `global_asset` | both | `/v1/assets/{slug}` aggregated crypto price (GlobalAssetView) |
| `history_series` | scam | `/v1/history` series |
| `listing` | substance | `/v1/assets` listing rows, catalogue listing price, asset-detail overlay |
| `markets` | scam | `/v1/markets` last price |
| `oracle` | both | SEP-40 oracle passthrough |
| `pairs` | scam | `/v1/pairs` last price |
| `pools` | scam | `/v1/pools` last price |
| `price_alert` | both | customer price-alert evaluator (aggregator) |
| `price_at` | both | point-in-time reads (`/v1/price/at`, `/v1/price/changes`) |
| `price_read` | both | the shared `/v1/price` + batch + assets-enrichment reader |
| `price_stream` | both | the closed-bucket price stream |
| `priceless_coverage` | substance | priceless-popular coverage tripwire (background) |
| `tip` | both | `/v1/price/tip` |
| `transitive` | both | one-hop transitive USD price, both legs |
| `twap` | scam | `/v1/twap` |
| `vwap` | scam | `/v1/vwap` |

A steady non-zero rate is expected (long tail of dust pairs). Investigate step-changes: a jump across all surfaces usually means an upstream ingest/aggregate outage (check `stellarindex_aggregator_ticks_total` and the indexer cursor); a jump after a deploy means the `[pricing_guard]` floors were raised. It counts requests, not pairs (verdicts cached ~60s).

`surface="dex_tvl"` is the one background producer: its rate follows the pool token set, and a withheld leg moves a pool into `unpriced_pools` on `/v1/protocols`.

### `stellarindex_price_serve_substance_unmeasured_total`

Counter, label `surface` (the shared table under
`stellarindex_price_serve_substance_withheld_total`; surfaces that
consult the substance gate).

Once per substance-gate verdict that could not be reached (read errored or timed out). Not cached. Single price lookups served unguarded; the `/v1/assets` listing (`surface="listing"`) withheld `price_usd` and `market_cap_usd` and stamped `flags.stale`. Expected zero; sustained non-zero means the substance store is slow or down (check the timescale readyz probe). Dashboard-only.
### `stellarindex_price_serve_thin_admitted_total`

Counter, labels `surface` and `floor` (both as on
`stellarindex_price_serve_substance_withheld_total`).

Fires once per thin-market verdict served flagged because the request opted in with `?include_thin=true`. Never counts a default response.

### `stellarindex_pricingguard_trailing_fetch_failed_total`

Counter, label `path` (`latest` | `at`).

Fires once per serving-sanity guard trailing-baseline fetch that errored. The guard fails OPEN, serving the candidate unguarded. Correlate with the timescale readyz probe, as `stellarindex_ratelimit_fail_open_total` does with redis.

### `stellarindex_pricingguard_degraded_total`

Counter, labels `path` (`latest` | `at` | `series`) and `reason`
(`outlier` | `unvalidated`).

Fires once per guard decision that did not serve the current prices_1m bucket as validated. `outlier`: failed the robust band. `unvalidated`: no trailing baseline within 24 h. Tells "the guard is holding a pair" apart from "the market is quiet".

### `stellarindex_price_serve_scam_withheld_total`

Counter, label `surface` (the shared table under
`stellarindex_price_serve_substance_withheld_total`; surfaces that
consult the scam gate).

Fires once per aggregated-price decision withheld because the asset's ISSUER carries a scam-class tag (`malicious`/`unsafe`/`fraud`/`scam`/`hack`/`phishing`). Raw data still serves.

The gate FAILS OPEN, so a drop to zero while flagged issuers trade can mean the directory is unreachable; `stellarindex_scam_gate_lookup_failures_total` counts that directly. Verdicts are cached ~60s per issuer.

### `stellarindex_scam_gate_lookup_failures_total`

Counter, label `surface` (same set as
`stellarindex_price_serve_scam_withheld_total`).

Fires once per gate consultation whose `account_directory` lookup errored, so the price was served unguarded. Failures are not cached, so this rises with traffic during an outage.

Alert: `stellarindex_scam_gate_fail_open` →
[scam-gate-fail-open](../../operations/runbooks/api.md#stellarindex_scam_gate_fail_open).

### `stellarindex_explorer_exact_fallback_total`

Counter, label `read` (`recent_operations`, `sourced_tx_keys`, `sourced_op_keys`, `account_movements`, `asset_movements`, `contract_events`).

Explorer page reads whose bounded window could not prove a full page of distinct keys, so the read re-ran as the exact LIMIT 1 BY / dedup query (O(account or contract history)). Alert: `stellarindex_explorer_exact_fallback_sustained`.

### `stellarindex_counts_rollup_plan_ok`

Gauge, label `rollup` (`op-types`, `movements`).

1 while the explorer reader holds a usable read plan for the counts rollup; 0 while the table is missing or unreadable and account panels scan the source. Set on each plan refresh (at most 1/min, on page traffic). Alert: `stellarindex_counts_rollup_plan_unusable`.

### `stellarindex_counts_rollup_stale_partitions`

Gauge, label `rollup`.

Closed partitions the current plan reads from the source because their rollup no longer matches. Alert: `stellarindex_counts_rollup_stale_partitions`.

## Supply derivation (aggregator binary)

### `stellarindex_supply_cross_check_divergence_stroops`

Gauge, labels `classic_key` (`CODE:ISSUER`) and `wrap_class`
(`partial_wrap` | `full_wrap`).

Stroop divergence between a classic asset's Algorithm 2 and SAC-wrapped Algorithm 3 total_supply. The meaning depends on `wrap_class`.

- `wrap_class="partial_wrap"` (default for every configured `sac_wrappers` pair): value = `max(0, classic.SACWrappedStroops − sac_total)`. Zero is the benign state. Positive means the SEP-41 capture missed a mint or double-counted a burn. Over-mint (`sac_total > classic_total`) does not feed this gauge; it appears only as `over_mint_stroops` on the WARN line. Also zero when unchecked. See [ADR-0011](../../adr/0011-supply-algorithm.md) and the [runbook](../../operations/runbooks/supply.md#stellarindex_supply_cross_check_divergence).
- `wrap_class="full_wrap"` (operator-attested via `[supply].fully_wrapped_sacs`): value = `|classic_total − sac_total|`.

Drives the
[`stellarindex_supply_cross_check_divergence`](../../operations/runbooks/supply.md#stellarindex_supply_cross_check_divergence)
alert when > 1.

### `stellarindex_supply_write_band_breach_total`

Counter, labels `asset_key` + `direction` (`up` / `down`).

One increment per snapshot whose `total_supply` is over 10x above (`up`) or below (`down`) the previous one. The row is still written.

### `stellarindex_supply_cross_check_total`

Counter, labels `outcome` (`within` / `over` / `missing_snapshot` /
`read_error` / `misaligned`) and `wrap_class` (`partial_wrap` |
`full_wrap`).

Cross-check evaluations per outcome. `missing_snapshot`, `read_error` and `misaligned` evaluated nothing; the refresher deletes the pair's `stellarindex_supply_cross_check_divergence_stroops` series on them. A sustained rate of any drives
[`stellarindex_supply_cross_check_unevaluable`](../../operations/runbooks/supply.md#stellarindex_supply_cross_check_unevaluable).
A flat gauge with no counter increments means the cross-check stopped.

### `stellarindex_supply_divergence_ratio`

Gauge, labels `asset` (canonical wire form, e.g. `native`) and
`reference` (`stellar-dashboard` / `coingecko`).

Absolute relative divergence `|our − reference| / reference` between our served `circulating_supply` and an external reference. Unlike the `supply_cross_check_*` pair, this compares our figure with the market's. XLM steady state is ~0.0003 ([`docs/methodology/xlm-circulating-supply.md`](../../methodology/xlm-circulating-supply.md)). Drives the
[`stellarindex_supply_divergence_high`](../../operations/runbooks/supply.md#stellarindex_supply_divergence_high)
alert when > 0.01. Not updated on `no_reference` / `refresh_error`.

### `stellarindex_supply_divergence_total`

Counter, label `outcome` (`ok` / `divergent` / `no_reference` /
`refresh_error`).

One increment per (asset, tick). `no_reference`: every reference was unreachable, so the checker is running blind; deliberately not paged. `refresh_error`: our snapshot couldn't be read. Pre-seeded to zero.

### `stellarindex_supply_divergence_duration_seconds`

Histogram, label `outcome` (`ok` / `divergent` / `no_reference` /
`refresh_error`).

Per-(asset, tick) check latency including the reference fan-out. Buckets 10 ms – 30 s.

### `stellarindex_aggregator_triangulations_total`

Counter, label `outcome` (`ok` / `missing_leg` / `parse_error` /
`redis_error` / `frozen_leg` / `frozen_leg_direct_served` /
`low_confidence` / `proxy_pivot`).

Triangulation outcomes per tick × chain × window.

`frozen_leg`: a leg froze earlier in the tick, so the chain did not publish and the freeze is inherited by the target. Not an error. `frozen_leg_direct_served`: no route around the frozen leg, but the target published its own direct print, served unmarked. `low_confidence` and `proxy_pivot` leave the direct price serving; `proxy_pivot` means a de-peg, and `composite_meta.pivot_surface_refusal` names the leg.

All eight outcomes are pre-seeded at zero. A sustained `missing_leg` rate with `ok` at zero usually means a dry fiat-FX feed; that is `stellarindex_aggregator_triangulation_chains_dry` in `deploy/monitoring/rules/aggregator.yml`.

### `stellarindex_aggregator_fx_snap_fallback_total`

Counter, label `leg` (canonical pair string of the FX leg, e.g.
`fiat:USD/fiat:EUR`).

Triangulations that fell back to cached VWAP for an FX leg because `FXQuoteAtOrBefore` returned no row at or before bucket end. Sustained > 50% indicates an FX-source health issue.

### `stellarindex_aggregator_composite_corroboration`

Gauge, labels `pair`, `window`, `verdict` (`corroborated` /
`refuted` / `unavailable`).

Composite-reference verdict for a single-venue allow-listed target; exactly one verdict series is 1 after each evaluation. `corroborated`: the rebuilt chain agrees with the direct print within `tolerance_bps`, so a phase-2 fire is suppressed. `refuted` and `unavailable` leave the freeze engaged.

### `stellarindex_aggregator_route_corroboration_count`

Gauge, labels `pair`, `window`.

The router's independent, tightly-agreeing, non-diverged route count behind the last composite for (pair, window). It is not `path_count` (raw survivor-set size). Also in `composite_meta` as `corroboration_count`.

### `stellarindex_aggregator_composite_reference_leg_sources`

Gauge, labels `pair`, `window`, `leg` (canonical leg pair, e.g.
`crypto:XLM/fiat:USD`).

Distinct venues or providers behind each composite-reference leg on the last bucket. The crypto/USD leg must reach `min_leg_sources` (default 2) to count; the FX leg reads 1.

### `stellarindex_aggregator_composite_reference_leg_dispersion_bps`

Gauge, labels `pair`, `window`, `leg`.

Max |venue VWAP − leg VWAP| / leg VWAP in basis points across venues printing a priced leg. Above `leg_dispersion_bps` (default `tolerance_bps`, 75) the leg cannot corroborate. An uncomputable venue VWAP refuses the leg and the gauge is not updated.

### `stellarindex_aggregator_composite_freeze_suppressed_total`

Counter, labels `pair`, `window`.

Phase-2 freeze fires that were not engaged because the composite reference corroborated the move. Read next to `stellarindex_anomaly_freeze_engaged_total` to judge whether `tolerance_bps` is too loose. Alerted by `stellarindex_aggregator_composite_freeze_suppression_dominant` (`deploy/monitoring/rules/freeze-lifecycle.yml`).

### `stellarindex_divergence_refresh_total`

Counter, label `outcome` (`ok` / `no_vwap` / `parse_error` /
`refresh_error` / `no_reference`).

Per-tick outcomes of the divergence-cache refresh loop. Only `ok` writes a fresh `div:<asset>` entry (feeds `flags.divergence_warning`). Sustained `refresh_error` or `no_reference` dominating `ok` fires `stellarindex_divergence_refresh_error_dominant` / `stellarindex_divergence_no_reference`; zero `ok` for 30 min fires `stellarindex_divergence_no_ok_outcomes`.

### `stellarindex_divergence_refresher_wired`

Gauge, no labels. 1 while the divergence pass has a refresher and windows to run with; 0 when every reference is disabled. It arms `stellarindex_divergence_no_ok_outcomes`.

### `stellarindex_divergence_reference_total`

Counter, labels `reference` (the reference's `Name()`: `coingecko`,
`chainlink`, the on-chain oracle sources, …) and `outcome` (`ok` /
`asset_unsupported` / `price_unavailable` / `too_stale_to_compare` /
`invalid_price` / `timeout` / `overall_deadline_exceeded` / `panicked` /
`error`).

One increment per (reference, pair) lookup. `stellarindex_divergence_refresh_total` leaves `ok` only when EVERY reference fails, so one reference going dark is visible only here. Over half of a reference's lookups failing for 30 min fires `stellarindex_divergence_reference_failing`.

### `stellarindex_divergence_pair_quorum_met`

Gauge, label `pair`. 1 when the latest refresh had at least `min_sources_for_warning` responding references, else 0. At 0 the warning verdict is carried forward, so divergence detection is disarmed for the pair even though the pass counts `ok`. An hour at 0 fires `stellarindex_divergence_pair_below_quorum`.
### `stellarindex_divergence_max_abs_fraction`

Gauge, label `reference`. Largest `|ours − reference| / reference` over the
pairs that reference currently prices (0.05 = 5 %); 0 when it prices none.
Pinned (frozen) refreshes are excluded. Drives
`stellarindex_price_divergence_warning` (> 0.05) and `_critical` (> 0.10).

### `stellarindex_divergence_pairs_over`

Gauge, label `threshold` (`5pct` / `10pct`). Pairs whose worst reference gap
exceeds that fraction. No per-pair label; the pairs are in
`divergence_observations`.

### `stellarindex_aggregator_baseline_refresh_total`

Counter, label `outcome` (`ok` / `ok_unvalued` / `not_enough_samples` /
`ok_per_minute_fallback` / `read_error` / `write_error`).

Outcomes per pair per hourly cycle of the Median + MAD recompute into `volatility_baseline_1m`. Sustained `not_enough_samples` means pairs in bootstrap.

The baseline trains on USD-volume bars, so dust adds only its USD share of a point. `ok_per_minute_fallback`: too little USD flow for three bars (under about $20.8 in 30 days); one point per minute is used and the 30d sample count is clamped to 60. `ok_unvalued`: no USD-valued minute in the window; every minute is used. Sustained `read_error` or `write_error` means a storage problem.

### `stellarindex_aggregator_supply_lake_clamp_ledgers`

Gauge, no labels. How far the `ledgerstream` cursor leads the newest landed `stellar.ledgers` row at snapshot time. One or two ledgers is normal. The resolver refuses past 512 ledgers (about 45 minutes), so a rising series warns supply is about to stop; beyond that the gauge stops updating and `no_ledger` in `stellarindex_aggregator_supply_refresh_total` is the signal.

### `stellarindex_aggregator_supply_refresh_total`

Counter, labels `asset_key` + `outcome`. `outcome` ∈ (`ok` /
`no_ledger` / `no_observation` / `compute_error` / `write_error` /
`stale_component` / `missing_freshness` / `dormant` /
`missing_baseline`). `asset_key` is the `supply.AssetKey` form:
`XLM`, `CODE:ISSUER` for classic credits, the bare contract
C-strkey for SEP-41.

Refresh outcomes, one per asset per tick. Only fires when `[supply] aggregator_refresh_enabled = true`; stays at zero if the systemd timer drives the writer instead.

Steady state is mostly `ok`. Sustained `no_observation` means the observer has not backfilled and the static fallback is empty. `no_ledger` means no chain position to stamp at: no cursor yet, or no `stellar.ledgers` row in the 512 ledgers below it. `missing_baseline` is a SEP-41 SAC-wrapper with no seeded opening balance; benign (excluded from `error_dominant`), cleared by `stellarindex-ops supply seed-sep41-genesis`. A negative total after seeding is `compute_error` (pages).

### `stellarindex_aggregator_confidence_compute_total`

Counter, label `outcome` (`ok` / `skipped` / `baseline_missing` /
`baseline_stale` / `marshal_error` / `write_error`).

Outcomes per (pair, window) per tick. `skipped` is the first tick. Sustained `baseline_missing` means the refresher is behind. `baseline_stale` is a row over one day old; both read as bootstrap. `marshal_error` and `write_error` should stay at zero.

### `stellarindex_aggregator_baseline_age_seconds`

Gauge, label `pair`. Age of the `volatility_baseline_1m` row last read. Normally under ~3600 s; past that the pair's refresh is failing, which `stellarindex_aggregator_baseline_refresh_total` does not show. Past 86400 s it is `baseline_stale`.

### `stellarindex_aggregator_bootstrap_capped`

Gauge, label `pair`. 1 when the pair's last confidence was bounded by the bootstrap ceiling (0.5), 0 when uncapped. Mirrors `confidence_factors.bootstrap_capped`. Pairs on `baseline_missing` or `baseline_stale` keep their last value.

### `stellarindex_aggregator_baseline_density_days`

Gauge, label `pair`. The 30-day baseline density the bootstrap cap gates on: 1-minute buckets divided by 1440, at most 30, negative with no baseline. The cap releases at 28.5 and re-engages below 27. It is sample density, not calendar age. Mirrors `confidence_factors.baseline_age_days`.

### `stellarindex_anomaly_freeze_engaged_total`

Counter, label `class` (`stablecoin` / `treasury` / `crypto` /
`governance` / `default`).

ActionFreeze decisions: no fresh VWAP published, last-known-good kept; `/v1/price` shows `flags.frozen=true`. Class only, to bound cardinality; pair detail is in the `freeze:<asset>:<quote>` Redis marker JSON (with the `state` object). Incremented on every frozen tick, not once per freeze, which the `rate()`-based `stellarindex_anomaly_freeze_engaged` rule relies on. Use `stellarindex_anomaly_freeze_active` to count freezes.

### `stellarindex_anomaly_freeze_active`

Gauge, no labels.

`(pair, window)` freezes held right now. Unlabelled by pair on purpose; `redis-cli GET freeze:<asset>:<quote> | jq .state` shows `fired_at`, `hold_until`, `extensions_used`, `escalated`, `unfreeze_streak` and `corroborated`.

### `stellarindex_anomaly_freeze_extensions_total`

Counter, no labels.

Hold extensions granted: +30 minutes, up to 4 times, when a hold expires without the auto-unfreeze condition (confidence > 0.30 AND z < 3.0 for two consecutive buckets). Leading indicator for escalation. A sustained rate with `stellarindex_anomaly_freeze_active` flat at 1 means one stuck pair.

### `stellarindex_anomaly_freeze_held_unscored_total`

Counter, no labels.

Holds that expired on an unscored bucket; no extension is used. A sustained rate means scoring is down.

### `stellarindex_anomaly_freeze_escalated_total`

Counter, no labels. **Drives a severity:page rule.**

Freezes that exhausted the extension ladder. An escalated freeze does NOT auto-unfreeze; `/v1/price` stays pinned until a human acts. One increment per escalation.

Force-unfreeze with `stellarindex-ops freeze-unfreeze -asset A -quote Q -reason "..."`; it counts under `stellarindex_anomaly_freeze_released_total{mode="operator"}`.

**Do not `redis-cli DEL` the marker.** The durable ladder is a second authority; the aggregator rehydrates the marker on its next tick.

### `stellarindex_anomaly_freeze_released_total`

Counter, label `mode` (`auto` / `operator` / `lapsed`).

How freezes ended. `auto`: auto-unfreeze condition held. `operator`: `stellarindex-ops freeze-unfreeze` cleared it, leaving a `freeze:override:<asset>:<quote>` tombstone. `lapsed`: marker and ladder expired with no tombstone, so the aggregator stalled on the pair. Does not balance against `stellarindex_anomaly_freeze_engaged_total` (ticks). A rising `operator` rate means a threshold or baseline problem.

### `stellarindex_anomaly_freeze_refired_after_override_total`

Counter, no labels.

Freezes that fired again within two hours of an override and resumed its ladder, so `stellarindex_anomaly_freeze_escalated_total` pages again. Non-zero means a force-unfrozen pair was still anomalous.

### `stellarindex_anomaly_warn_total`

Counter, label `class` (`stablecoin` / `treasury` / `crypto` /
`governance` / `default`).

A bucket deviated past `anomaly.warn_pct` but not enough to freeze, so it was published. The only place an anomaly warn surfaces; it does NOT set `flags.divergence_warning`, which comes from the cross-reference service and needs `flags.divergence_checked`. Fires before a freeze.

### `stellarindex_anomaly_freeze_recovered_total`

Counter, no labels.

Freeze rows the recovery worker closed (`MarkRecovered`). Trails `stellarindex_anomaly_freeze_engaged_total` by the marker TTL plus the 60s poll. A persistent gap means the worker is broken; see the [freeze-recovery-stalled runbook](../../operations/runbooks/anomaly.md#stellarindex_anomaly_freeze_recovery_stalled).

### `stellarindex_anomaly_freeze_ladder_rehydrated_total`

Counter, no labels.

Freezes restored from the durable ladder (`freeze_events.hold_until` / `extensions_used` / `escalated` / `corroborated`) because the Redis marker was missing. Not alerted: non-zero means the safety net worked. A burst after a Redis restart is expected; a slow trickle means markers are evicted (`maxmemory-policy`) or expiring early.

### `stellarindex_api_freeze_lookup_failures_total`

Counter, no labels.

API-side freeze-marker reads that errored (Redis outage or timeout); client aborts excluded. Each serves the price with `frozen_checked=false`. Alerted by `stellarindex_api_freeze_lookup_failing`.

### `stellarindex_anomaly_freeze_ladder_write_failures_total`

Counter, label `op` (`record_freeze` / `mark_hold` / `clear`).

Durable freeze writes that did not land: the `freeze_events` row (`record_freeze`) or its ladder (`mark_hold` / `clear`). `record_freeze` is worse: Redis still freezes but SQL readers treat the minute as unfrozen. An unapplied migration makes every ladder write match zero rows (`ErrNotFound`, counted here). Sustained non-zero means Redis-flush protection is inert; `mark_hold` is the dangerous direction. Labels are pre-seeded.

### `stellarindex_anomaly_freeze_recovery_sweeps_total`

Counter, label `outcome` (`ok` / `partial` / `error`).

Recovery-worker poll cycles. `error`: lister or Redis failed for the whole sweep. `partial`: `MarkRecovered` failed for some rows.

## verify-archive (stellarindex-ops one-shot)

Emitted by `stellarindex-ops verify-archive` with `-metrics-listen ADDR`. All vectors are labelled by `chunk_idx` (decimal string), bounded by the `-workers` cap (`[1, 16]`).

`-metrics-listen` is live-run only; nothing scrapes it on a systemd-timer deployment. The durable export is `-textfile-output PATH`, which writes only the mismatch counter, see
[`stellarindex_verify_archive_mismatches_total`](#stellarindex_verify_archive_mismatches_total).

### `stellarindex_verify_archive_ledgers_verified_total`

Counter, label `chunk_idx`.

Ledgers verified per chunk. The rate spots a stalled chunk versus a slow one.
### `stellarindex_verify_archive_current_ledger`

Gauge, label `chunk_idx`.

Most-recent ledger sequence verified by each chunk. Compare with the chunk's `[from, to]` range for percent-complete.

### `stellarindex_verify_archive_checkpoints_total`

Counter, labels `chunk_idx` + `outcome` (`matched` / `missed`).

Tier B checkpoint outcomes. `missed` = archive file absent (warning, or hard fail under `-fail-on-missed`); `matched` = hash-equal proof.

### `stellarindex_verify_archive_mismatches_total`

Counter. Two export paths with deliberately different label sets:

| Export | Labels | Lifetime |
| ------ | ------ | -------- |
| `-metrics-listen` (in-process `/metrics`) | `chunk_idx` + `reason` | the run |
| `-textfile-output` (node_exporter textfile collector) | `tier` + `reason` | cumulative on the host |

`reason` ∈ `chain` / `sequence` / `checkpoint` on both.

Chain breaks, sequence gaps and checkpoint hash mismatches. **Any non-zero reading is a hard failure.**

The textfile export makes the P1 `stellarindex_stellar_archive_divergence` page fireable. It is cumulative across runs and zero-seeded (a series first appearing at 1 would give `increase() == 0`). `tier` is `chain` or `checkpoint`; `chunk_idx` is summed away.

### `stellarindex_anomaly_freeze_recovery_sweep_duration_seconds`

Histogram, label `outcome` (matches the `_sweeps_total` counter:
`ok` / `partial` / `error`).

Latency of the freeze recovery sweep (60 s cadence); scales with open rows. Slowness usually means Postgres or Redis pressure. Buckets 10 ms → 30 s.

### `stellarindex_aggregator_supply_refresh_duration_seconds`

Histogram, label `outcome` (matches the per-asset_key counter's
outcome enum: `ok` / `no_ledger` / `no_observation` /
`compute_error` / `write_error` / `stale_component` /
`missing_freshness`).

Latency of each supply-refresh tick; pairs with `_aggregator_supply_refresh_total{asset_key, outcome}`. No `asset_key` label (cardinality × buckets); use the per-tick log. A p99 above 1 s suggests writer contention. The `stellarindex_supply_snapshot_*` alerts cover freshness.

### `stellarindex_sep41_supply_rollup_advances_total`

Counter, labels `contract_id` + `outcome` (`ok` / `noop` /
`no_cursor` / `error`).

Passes of the SEP-41 supply rollup worker, which keeps a per-contract checkpoint so reads avoid the full-history sum. `noop` is the dormant steady state. `no_cursor` means the projector cursor is absent, so reads take the slow full sum; `stellarindex_sep41_supply_rollup_no_cursor` alerts on it. Sustained `error` freezes that contract's checkpoint; correlate with `_aggregator_supply_refresh_duration_seconds` p99.

### `stellarindex_sep41_supply_rollup_advance_duration_seconds`

Histogram, label `outcome` (matches the counter's enum). Pairs
with `_sep41_supply_rollup_advances_total{contract_id,outcome}`. No `contract_id` label (cardinality).

Steady state is sub-second; a cold contract's first fold can take minutes, so buckets extend to 300 s.

### `stellarindex_divergence_refresh_duration_seconds`

Histogram, label `outcome` (`ok` / `no_vwap` / `parse_error` /
`refresh_error`).

Per-pair divergence-refresh latency; pairs with the `_total` counter. One slow external vendor stretches the tick without a `refresh_error`; chart `ok` p95/p99 to catch it.

### `stellarindex_customer_webhook_delivery_duration_seconds`

Histogram, label `outcome` (`delivered` / `server_error` /
`client_error` / `network_error` / `build_error`).

Latency of the outbound POST in the customer-webhook worker; pairs with `_attempts_total`. `http_request_duration_seconds` covers only inbound handlers. `build_error` is recorded as 0 (no round trip). Buckets to 60 s. `stellarindex_customer_webhook_delivery_failing` covers the failing rate.

### `stellarindex_postgres_ping_total`

Counter, label `outcome` (`ok` / `error`).

Emitted by the indexer's `watchPostgresPing` goroutine every 60 s (5 s timeout). `error` is any failure.

Alert `stellarindex_postgres_ping_failing`: `rate(stellarindex_postgres_ping_total{outcome="error"}[5m]) > 0.5` for 2 m. Brief failures during a restart are expected.

### `stellarindex_postgres_ping_failure_streak`

Gauge, no labels.

Consecutive failed pings; resets to 0 on success. The indexer logs `pool may be wedged` at `streak == 3`.

### `stellarindex_tls_cert_not_after_unix`

Gauge, label `host`.

Unix-seconds NotAfter of the leaf TLS cert, probed every 6 h. Failures keep the last value; see the probe counter.

Alert `stellarindex_tls_cert_expiring_soon` fires when
`(not_after_unix - time()) < 14 * 24 * 3600` sustained 1 h.

### `stellarindex_tls_cert_probe_total`

Counter, labels `host`, `outcome` (`ok` / `dial_error` /
`timeout` / `no_cert`).

TLS self-probe outcomes. A sustained non-`ok` rate with a stale `stellarindex_tls_cert_not_after_unix` means the probe itself is failing.

### `stellarindex_markets_skipped_rows_total`

Counter, no labels.

Trades rows `/v1/markets` skipped because `base_asset` / `quote_asset` did not parse as canonical. **Any non-zero reading is alertable**: something bypassed the write path; delete the row.

## MEV detection (aggregator binary)

The MEV worker scans recent trades every 5 minutes for arbitrage cycles and writes `mev_events` (backing `/v1/mev`).

### `stellarindex_mev_detect_runs_total`

Counter, label `outcome` ∈ `ok | scan_error | write_error`.

Per-run outcome. `scan_error` = the trades scan failed and the run was skipped; `write_error` = an `mev_events` insert failed mid-run. Sustained non-`ok` makes `/v1/mev` stale.

### `stellarindex_mev_events_inserted_total`

Counter, no labels.

New (non-duplicate) events persisted. Overlapping windows are deduped on write, so this counts first detections only. A flat line is normal.

### `stellarindex_mev_scan_truncated_total`

Counter, label `input` ∈ `trades | oracle_updates | auction_fills`.

A detector scan hit `ScanLimit`, so that tick covered only the newest rows. A sustained rate means some patterns go undetected; change `mev.WorkerConfig` (`ScanLimit`, `Window`).

### `stellarindex_mev_lake_order_lookup_skipped_total`

Counter, no labels.

Tx hashes left unordered because a `stellar.tx_hash_index` lookup chunk failed (usually `MEMORY_LIMIT_EXCEEDED`); sandwich detectors skip the tick. Lower `txIndexChunk`.

### `stellarindex_tx_index_tag_lookup_skipped_total`

Counter, no labels.

Twin of the counter above for the `trades.tx_index` tagger: hashes left untagged (NULL) after a failed chunk. Same remedy.

### `stellarindex_mev_detect_duration_seconds`

Histogram, label `outcome` (same set as the runs counter).

Per-run latency; healthy is sub-second. Chart `ok` apart from `scan_error`.

## Decimals-assumption guard (aggregator binary)

### `stellarindex_dex_trade_nonstandard_decimals_total`

Counter, labels `source`, `asset`.

**Should be permanently absent or zero.** Served price assumes both legs share a decimals scale (7 today). The `internal/decimalsguard` sweep increments this once per (`source`, `asset`), latched, when a token's `decimals()` is confirmed `!= 7`; prices for that asset are then skewed by `10^(7−decimals)`. `asset` is the C-strkey contract id. Not pre-seeded; the alert is a bare `> 0`. Any non-zero value is silent mispricing (P2). Runbook: `docs/operations/runbooks/dex.md#dex-nonstandard-decimals`.

### `stellarindex_decimals_guard_sweep_last_success_unix`

Gauge, no labels.

Unix seconds of the last successful sweep; 0 where the guard is disabled. `time() - this` is the staleness signal: a zero on `stellarindex_dex_trade_nonstandard_decimals_total` or `stellarindex_nonstandard_decimals_lockstep_mismatch_total` otherwise cannot be told from a guard that never armed.

Alert `stellarindex_decimals_guard_sweep_stale` (> 45 min). Scoped to `job="stellarindex-aggregator"` because other binaries register it at zero, the same trap `stellarindex_priceless_coverage_check_stale` guards against.

## Nonstandard-decimals serving guard (API binary)

### `stellarindex_price_serve_declined_nonstandard_decimals_total`

Counter, label `asset`. **HISTORICAL — permanently zero.**

The decline path is gone (replaced by read-time normalization, `aggregate.AdjustPrice`), so nothing increments this. Kept registered so dashboards don't break; remove at the next metrics cleanup. Runbook: `docs/operations/runbooks/dex.md#dex-nonstandard-decimals`.
### `stellarindex_nonstandard_decimals_cache_refresh_failures_total`

Counter, no labels.

Failed 60s background refreshes of the API's mirror of `nonstandard_decimals_assets`. The cache fails open: it keeps serving the previous snapshot. This is an infra-health signal, not a pricing-correctness one.

### `stellarindex_nonstandard_decimals_partial_alias_family_total`

Counter, no labels.

Flagged non-7-decimal assets seen at refresh whose alias family is only PARTLY flagged.

Unlike the counter above, this **is** a pricing-correctness signal. `NonstandardDecimalsCache.Lookup` does not alias-fold, so the price and supply legs can diverge by a **power of ten** while both look plausible. Expected value is 0. A nonzero value means one spelling of an aliasing asset was flagged (XLM: `native`, `crypto:XLM`, its SAC). Flag the missing spellings.

### `stellarindex_nonstandard_decimals_lockstep_mismatch_total`

Counter. Labels: `site` (`guard_reconcile` | `asset_detail`), `asset` (C-strkey).

The lake's on-chain `decimals()` and the `nonstandard_decimals_assets` projection disagree for one asset. Expected value is 0.

- `site="guard_reconcile"`: the aggregator's guard found a persisted row differing from the lake and repairs it.
- `site="asset_detail"`: `GET /v1/assets/{id}` saw the mismatch and refused `market_cap_usd` / `fdv_usd` for that request. One increment per refused request.

Third arm of `stellarindex_nonstandard_decimals_correction_failing`.

## Background cache workers (API binary, v0.21.4)

### `stellarindex_dex_tvl_refresh_total`

Counter. Labels: `outcome` (`ok` | `error`).

One increment per DEX TVL refresh (10-min cadence). `error` means a protocol's read failed and it carries its previous entry forward, so the figure ages invisibly. Sustained `error` with no `ok` fires `stellarindex_dex_tvl_refresh_failing`.

### `stellarindex_dex_tvl_refresh_duration_seconds`

Histogram. Labels: `outcome` (matches the counter). Buckets 50 ms → 180 s.

Wall time of one full refresh.

### `stellarindex_dex_tvl_reconcile_total`

Counter. Labels: `outcome` (`ok` | `divergent`).

One increment per refresh. `divergent` means at least one protocol was refused (carried-forward figure or unbalanced pool counts), dropped from the `tvl_total` sum and named in `excluded`.

`divergent` tracks `stellarindex_dex_tvl_refresh_total{outcome="error"}`. It is the only signal for one protocol failing permanently, because `stellarindex_dex_tvl_refresh_failing` needs zero successful refreshes; `stellarindex_dex_tvl_total_divergent` fires on it.

### `stellarindex_sdex_orderbook_maintain_total`

Counter. Labels: `outcome` (`load_ok` | `load_error` | `advance_ok` |
`advance_error` | `advance_held` | `verify_ok` | `verify_error`).

Maintenance attempts for the in-process order book behind `/v1/sdex/orderbook`. `load_*` is the one-off initial load (retried every 60s); `advance_*` is the 60s incremental apply; `verify_*` drains the suspect-offer quarantine; unobserved when empty.

`advance_held` is a clean tick with ZERO cursor progress: an unhealed lake hole or an ingest halt. `stellarindex_sdex_orderbook_advance_held` (30 min) covers it.

### `stellarindex_sdex_orderbook_maintain_duration_seconds`

Histogram. Labels: `outcome` (matches the counter). Buckets 50 ms → 30 min.

`load_ok` is the initial-load time; watch it against the 30-min cap.

### `stellarindex_sdex_orderbook_crossed_pairs`

Gauge.

Pairs whose SERVED book is crossed (best bid > best ask). On-chain books cannot cross, so it should read 0. A touching book is not counted. Sustained non-zero means phantom offers: a removal the lake never ingested. Alert: `stellarindex_sdex_orderbook_crossed_book` (30 min).

### `stellarindex_sdex_orderbook_pending_offers`

Gauge.

Offers quarantined from the served book until the change-stream probe proves no removal exists (the `intra_ledger_seq == 0` suspect class). Drains at 2,500/tick, so a populated quarantine after a deploy is normal. Stuck high with `verify_error` means a thinner book.

### `stellarindex_sdex_orderbook_undecodable_offers_total`

Counter.

Offer change rows skipped because `entry_xdr` failed to decode. A skip FREEZES that offer's earlier state. Should read 0; sustained increments mean an upstream lake problem.

### `stellarindex_worker_panics_total`

Counter, label `worker`. Incremented by `worker.Recover` when a background goroutine panics and is swallowed.

Any increment means that worker is DEAD until its unit restarts. Alert: `stellarindex_worker_panicked` (page).

### `stellarindex_explorer_swr_refresh_total`

Counter. Labels: `cache` (`accounts_wealth` | `asset_holders` |
`contract_detail` | `contracts_dir` | `native_lp_listing` |
`network_throughput` | `op_type_stats` | `ops_directory` |
`protocol_bespoke` | `ttl_liveness`), `outcome` (`ok` | `error`).

Detached stale-while-revalidate refresh outcomes for the explorer's snapshot caches. Failures are invisible at the API (stale data keeps serving with `flags.stale`), so this is the only place a dying refresher shows before its data is hours old.

### `stellarindex_explorer_refresh_gate_saturated_total`

Counter. Labels: `class` (the class passed to `RefreshGate.TryAcquireClass`: `account_state` | `asset_holders` |
`contracts_dir` | `network_throughput` | `ops_directory` |
`protocol_bespoke` | `contract_detail` / `contract_detail_<key prefix>`;
`unclassed` for a bare `TryAcquire`), `bound` (`class` = the per-class
cap refused; `global` = the pool-wide limit refused).

Refreshes the shared `clickhouse.RefreshGate` SKIPPED because it was saturated. Skipped refreshes never reach `stellarindex_explorer_swr_refresh_total`, and a cold request waiting on one gets a retryable 503. A sustained rate on one `class` with `bound="class"` is key churn; `bound="global"` across many classes means the limit is too small.

### `stellarindex_explorer_swr_refresh_duration_seconds`

Histogram. Labels: `cache`, `outcome` (matches the counter). Buckets 50 ms → 300 s.

Per-refresh wall time. p95 nearing the refresh timeout predicts growing stale age.

### `stellarindex_ch_schema_probe_present`

Gauge. Label `probe` (`tx_hash_index` | `contract_active_ledgers` |
`contract_instance_changes` | `contract_instance_changes_tx_key` |
`contracts_census_daily` | `accounts_stats` | `account_creators_rollup` |
`account_sponsors_rollup` | `account_creator_edges` |
`account_sponsor_edges` | `asset_holders_rollup` | `ops_by_source` |
`account_activity` | `ledger_entries_current_version`).

The lake's last answer to each explorer schema probe: `1` = exists, `0` = absent or empty, so the explorer uses its slow fallback or 503s. An "absent" verdict sticks until restart (alert `stellarindex_ch_schema_probe_absent`).

### `stellarindex_ch_schema_probe_unanswered_total`

Counter. Label `probe` (as above).

Probes that got no answer (transport error, deadline, or a non-schema ClickHouse exception). The gauge `stellarindex_ch_schema_probe_present` stays unchanged, so a rising rate means "the lake is not answering", not "the object is gone".

### `stellarindex_explorer_schema_absent_empty_total`

Counter. Label `table` (`account_entry_ledgers`, `asset_entry_ledgers`, `sponsorship_events`).

Explorer reads answered as empty because their ClickHouse table or column is missing. Alert: `stellarindex_explorer_schema_absent_empty`.

### `stellarindex_protocol_detail_refresh_total`

Counter. Labels: `outcome` (`ok` | `stale` | `degraded` | `timeout`).

One increment per detached `/v1/protocols/{name}` rebuild (prewarm sweep and request-kicked revalidation). `ok`: fully built. `stale`: complete but the bespoke block is past the 45-minute horizon. `degraded`: some analytics component failed or was skipped. `timeout`: outran the 90 s budget; the previous entry is kept. Sustained `degraded`/`timeout` with no `ok` means pages run on old snapshots.

### `stellarindex_protocol_detail_refresh_duration_seconds`

Histogram. Labels: `outcome` (matches the counter). Buckets 50 ms → 90 s.

Wall time of one rebuild. 90 s is the hard budget, not headroom.

## Rolling ZFS snapshots (textfile collector, ansible-managed)

Emitted by `scripts/ops/zfs-snapshot.sh` into `zfs_snapshot.prom` on every run. NOT Go-declared, so not covered by §3 of `scripts/ci/lint-docs.sh`.

### `stellarindex_zfs_pool_free_bytes`

Gauge, label `pool`. Pool-level free bytes, the number the min-free guard acts on.

### `stellarindex_zfs_snapshot_min_free_bytes`

Gauge, label `pool`. The guard floor (default 2 TiB), exported so dashboards can draw it.

### `stellarindex_zfs_snapshot_latest_unix`

Gauge, label `dataset`. Creation time of the newest `auto-*` snapshot; `0` when none. Input for `stellarindex_zfs_snapshot_stale`.

### `stellarindex_zfs_snapshot_count`

Gauge, label `dataset`. Number of `auto-*` snapshots held; manual snapshots are excluded.

### `stellarindex_zfs_snapshot_used_bytes`

Gauge, label `dataset`. Bytes held by *all* snapshots of the dataset, manual included.

### `stellarindex_zfs_snapshot_guard_skipped`

Gauge, label `dataset`. `1` when the last run skipped the dataset because the pool was below the floor even after pruning.

### `stellarindex_zfs_snapshot_last_run_unix`

Gauge. Unix time the job last completed in any mode (`rotate`, `now`, `metrics`).

### `stellarindex_zfs_snapshot_pool_free_unreadable`

Gauge, label `pool`. Written to `zfs_snapshot_error.prom` (value `1`) when a run could not read free space; the job then refuses to prune or snapshot (fail-closed). Alert: `stellarindex_zfs_snapshot_pool_free_unreadable`.

## Per-process memory-mapping headroom (textfile collector, ansible-managed)

Emitted by `configs/ansible/roles/archival-node/files/memory-mappings.sh` into `memory_mappings.prom` every 5 minutes. NOT Go-declared, so not covered by §3 of `scripts/ci/lint-docs.sh`.

### `stellarindex_process_memory_mappings`

Gauge, label `process`. Virtual-memory mappings (one per `/proc/<pid>/maps` line) of the watched process. Reports the HIGHEST-mapping process matching the executable basename, since `vm.max_map_count` is enforced per process. Withheld, never published as 0, when nothing matched.
### `stellarindex_process_memory_mappings_limit`

Gauge, no label. `/proc/sys/vm/max_map_count`, the per-process ceiling. Kernel-wide, hence unlabelled.

### `stellarindex_process_memory_mappings_ratio`

Gauge, label `process`. Mappings as a fraction of the limit (0–1). Alerts use this ratio so a change to `vm.max_map_count` moves every threshold with it.

### `stellarindex_process_memory_mappings_procs`

Gauge, label `process`. How many processes matched the watched executable. `0` means the probe watches nothing (stopped, moved or renamed binary); the mapping gauges are withheld then, which looks like health.

### `stellarindex_process_memory_mappings_unreadable`

Gauge, label `process`. `1` when a process matched but its headroom could not be computed (`/proc/<pid>/maps` unreadable, or `vm.max_map_count` unreadable). Fail-closed: the affected gauges are withheld, not published as 0.

### `stellarindex_process_memory_mappings_updated_unix`

Gauge, no label. Unix time of the last successful run. node_exporter re-serves a stale textfile verbatim, so a stopped timer freezes the gauges above at their last value; only this series sees that.

## Galexie mirror vs upstream dataset (textfile collector, Go-emitted)

Emitted by `stellarindex-ops galexie-mirror-verify` via the weekly `galexie-mirror-verify.timer`. The file is rewritten only when a comparison completes, so a failing run leaves the previous verdict in place.

### `galexie_archive_upstream_objects`

Gauge, label `result`. Objects in partitions present on both sides, by outcome of comparing ETag and size with upstream: `matched`, `equivalent` (bytes differ, decoded ledgers equal; not divergence), `upstream-rewritten`, `local-differs`, `local-only`, `unverifiable` (multipart ETag, sizes equal), `missing-local` (not yet mirrored). `upstream-rewritten`, `local-differs` and `local-only` are divergence.

### `galexie_archive_upstream_partitions_compared`

Gauge. Partitions present on both sides that the last run compared.

### `galexie_archive_upstream_last_success_unix`

Gauge. Unix time the last completed comparison wrote the file.

## Changelog

- 2026-10-03 — added `stellarindex_change_summary_passes_total`, `stellarindex_change_summary_last_success_unix`, `stellarindex_change_summary_stale` alert.
- 2026-09-27 — added `stellarindex_ch_live_sink_read_undercount_total` and `stellarindex_ingestion_ch_live_sink_read_undercount` alert.
- 2026-09-24 — `stellarindex_decimals_guard_sweep_last_success_unix` seeded at start; `stellarindex_decimals_guard_sweep_stale` gains a `> 0` arm.
- 2026-09-18 — added `stellarindex_decimals_guard_sweep_last_success_unix` and `stellarindex_decimals_guard_sweep_stale` alert.
- 2026-09-10 — added the `stellarindex_process_memory_mappings*` family (textfile-only).
- 2026-09-05 — added `stellarindex_galexie_catchup_probe_read_ok`, `stellarindex_galexie_catchup_probe_journal_lines`, `stellarindex_galexie_catchup_probe_last_run_unix`, `galexie_archive_scan_ok`, `galexie_archive_scan_listing_lines`, `galexie_archive_scan_last_run_unix`; read by `stellarindex_galexie_catchup_probe_degraded`, `stellarindex_galexie_archive_scan_degraded`, `stellarindex_stellar_stack_probe_success`, `stellarindex_stellar_stack_probe_degraded`.
- 2026-09-03 — `stellarindex_source_enabled` gained a second emitter (the `massive` FX worker in the API binary).
- 2026-09-03 — `stellarindex_ch_live_sink_ledgers_total{outcome="errored"}` now alerted by `stellarindex_ingestion_ch_live_sink_errors`; `stellarindex_customer_webhook_delivery_attempts_total` outcomes pre-seeded, with `stellarindex_customer_webhook_mark_errors` rule.
- 2026-08-29 — added `stellarindex_zfs_pool_free_bytes` and `stellarindex_zfs_snapshot_{latest_unix,count,used_bytes,guard_skipped,min_free_bytes,last_run_unix,pool_free_unreadable}`.
- 2026-08-29 — added `stellarindex_pgbackrest_backup_last_success_unix{repo}`, `stellarindex_pgbackrest_backup_last_rc{repo}`, `stellarindex_pgbackrest_backup_duration_seconds{repo}` (textfile-only).
- 2026-08-11 — removed `stellarindex_stripe_platform_sync_errors_total`, `stellarindex_stripe_dead_letters_open`.
- 2026-07-10 — `stellarindex_price_serve_declined_nonstandard_decimals_total` is now permanently zero.
- 2026-07-09 — added `stellarindex_stellar_stack_probe_success`, `stellarindex_stellar_stack_version_lag{component}`, `stellarindex_stellar_stack_installed_info{component,version}` (textfile-only).
- 2026-07-09 — added `stellarindex_dex_trade_unit_ratio_total` (`source`).
- 2026-07-09 — added `stellarindex_price_serve_declined_nonstandard_decimals_total` (`asset`) and `stellarindex_nonstandard_decimals_cache_refresh_failures_total`.
- 2026-07-09 — added `stellarindex_hashdb_append_total`, `stellarindex_hashdb_append_duration_seconds`, `stellarindex_hashdb_verify_runs_total`, `stellarindex_hashdb_verify_run_duration_seconds`, `stellarindex_hashdb_drift_total`.
- 2026-07-07 — added `stellarindex_dex_trade_nonstandard_decimals_total` (`source`, `asset`).
- 2026-06-18 — added `stellarindex_mev_detect_runs_total`, `stellarindex_mev_events_inserted_total`, `stellarindex_mev_detect_duration_seconds`.
- 2026-06-12 — added `stellarindex_ch_live_sink_ledgers_total` (`outcome`).
- 2026-06-01 — added `stellarindex_markets_skipped_rows_total`.
- 2026-05-27 — added `stellarindex_postgres_ping_total`, `stellarindex_postgres_ping_failure_streak`, `stellarindex_postgres_ping_failing` alert.
- 2026-05-13 — added `stellarindex_anomaly_freeze_recovery_sweep_duration_seconds`.
- 2026-05-13 — added `stellarindex_aggregator_supply_refresh_duration_seconds`.
- 2026-05-13 — added `stellarindex_divergence_refresh_duration_seconds`.
- 2026-05-13 — added `stellarindex_customer_webhook_delivery_duration_seconds`.
- 2026-05-13 — added `stellarindex_stripe_platform_sync_errors_total`.
- 2026-04-29 — added `stellarindex_verify_archive_*`.
- 2026-04-28 — added supply cross-check metrics.
- 2026-04-25 — added `stellarindex_aggregator_*`.
- 2026-04-23 — initial reference document.
