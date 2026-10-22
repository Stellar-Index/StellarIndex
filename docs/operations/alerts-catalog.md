---
title: Alerts Catalogue
last_verified: 2026-09-05
status: ratified — incremental growth
---

# Alerts Catalogue

Every row is a Prometheus / AlertManager rule. The Runbook column links to
`docs/operations/runbooks/<name>.md` (`scripts/ci/lint-docs.sh` section 9);
its first link must be the rule's `runbook_url` page
(`scripts/ci/lint-alerts-catalog.py`).

**Columns:**

- **Name**: `stellarindex_<area>_<specific>`. Stable; AlertManager routing and the runbook filename use it.
- **Metric**: the triggering expression (metric names drop the `stellarindex_` prefix where unambiguous).
- **Condition**: threshold and duration (`for:`).
- **Severity**: the rule's `labels.severity`, which AlertManager routes on. Three values:

  | Severity | Rules | AlertManager route | Delivery |
  | --- | --- | --- | --- |
  | `page` | 71 | `receiver: chat-page` | Discord **#stellarindex-pages**, `repeat_interval` 12 h. There is **no** PagerDuty leg: nothing wakes anyone up. |
  | `ticket` | 276 | `receiver: chat-default` | Discord **#stellarindex-alerts**, `repeat_interval` 24 h. |
  | `informational` | 11 | `receiver: chat-informational` | Discord **#stellarindex-informational**, a quiet channel kept apart so a notice cannot bury a ticket. `send_resolved: false`. With `DISCORD_WEBHOOK_URL_INFORMATIONAL` unset it degrades to the `silent` stub: delivered to nobody. |

  **`informational` is not a low-priority ticket.** Nothing files a
  ticket; it reaches a human only if they open the quiet channel or the
  AlertManager UI. Each one needs a row in the
  [delivery register](#informational-alerts--delivery-register) (CI-enforced).
  `stellarindex_deadmansswitch` is the exception: routed by alertname to
  Healthchecks.io ahead of the severity matchers.

  A `page` inhibits only `ticket`/`informational` alerts sharing both its
  `component` and `alert_family` labels; a page without a family inhibits
  nothing. Family map:
  [`inhibit-rules-test.sh`](../../configs/alertmanager/inhibit-rules-test.sh).
  Routing:
  [`alertmanager.r1.yml`](../../configs/alertmanager/alertmanager.r1.yml).
- **Runbook**: what the responder does (link).

> Severity is a routing key, not P1/P2/P3. Incident SEV is a separate
> human judgement: [sev-playbook.md](sev-playbook.md).

---

## Ingestion alerts

| Name | Metric | Condition | Severity | Runbook |
| ---- | ------ | --------- | -------- | ------- |
| `stellarindex_ingestion_source_stopped` | `rate(source_events_total[30m])` | == 0 for 15 min, enabled source | ticket | [source-stopped](runbooks/ingestion.md#stellarindex_ingestion_source_stopped) |
| `stellarindex_ingestion_source_stopped_low_volume_dex` | `rate(source_events_total[6h])`, phoenix/soroswap/blend | == 0 for 30 min | ticket | [source-stopped](runbooks/ingestion.md#stellarindex_ingestion_source_stopped) |
| `stellarindex_ingestion_source_stopped_daily_publisher` | `rate(source_events_total[30h])`, ecb | == 0 for 1 h | ticket | [source-stopped](runbooks/ingestion.md#stellarindex_ingestion_source_stopped) |
| `stellarindex_ingestion_source_stopped_hourly_publisher` | `rate(source_events_total[6h])`, band | == 0 for 1 h | ticket | [source-stopped](runbooks/ingestion.md#stellarindex_ingestion_source_stopped) |
| `stellarindex_ingestion_all_sources_stopped` | `sum(rate(stellarindex_source_events_total[5m]))` | == 0 for > 3 min | page | [all-ingestion-down](runbooks/ingestion.md#stellarindex_ingestion_all_sources_stopped) |
| `stellarindex_ingestion_trade_buffer_drop` | `increase(source_insert_errors_total{kind="dropped"}[15m])` | > 0, `for: 0m` | ticket | [trade-insert-backpressure](runbooks/ingestion-sink.md#stellarindex_ingestion_trade_insert_backpressure) |
| `stellarindex_ingestion_ledger_stalled` | ledgerstream `cursor_last_ledger` flat 5 min, or absent | ~10 min without a committed ledger. `all_sources_stopped` and `cursor_stuck` stay quiet in a lake outage | page | [ledger-ingest-stalled](runbooks/ledger-ingest.md#stellarindex_ingestion_ledger_stalled) |
| `stellarindex_ingestion_cursor_stuck` | `increase(cursor_last_ledger[5m])` per source | == 0 while source live | ticket | [cursor-stuck](runbooks/ledger-ingest.md#stellarindex_ingestion_cursor_stuck) |
| `stellarindex_ingestion_orphan_events` | `rate(stellarindex_source_orphan_events_total[10m])` | > 10/min per source | ticket | [orphan-events](runbooks/ingestion-events.md#stellarindex_ingestion_orphan_events) |
| `stellarindex_ingestion_decode_error` | `rate(stellarindex_source_decode_errors_total[5m])` | > 1/s sustained 5 min | ticket | [decode-errors](runbooks/ingestion.md#stellarindex_ingestion_decode_error) |
| `stellarindex_decoder_panicked` | `decoder_panics_total` (raw value) | > 0; that event shape drops until a fixed binary ships | page | [decoder-panicked](runbooks/ingestion.md#stellarindex_decoder_panicked) |
| `stellarindex_ingestion_oracle_unknown_symbols` | `increase(source_unknown_symbols_total[25h])` | > 0 for 30 min (kept as `raw:`) | informational | [oracle-unknown-symbols](runbooks/ingestion-events.md#stellarindex_ingestion_oracle_unknown_symbols) |
| `stellarindex_ingestion_oracle_unrepresentable_symbols` | `increase(source_unrepresentable_symbols_total[25h])` | > 0 for 30 min (slot dropped, no row) | ticket | [oracle-unknown-symbols](runbooks/ingestion-events.md#stellarindex_ingestion_oracle_unrepresentable_symbols) |
| `stellarindex_ingestion_discovery_drops` | `increase(stellarindex_discovery_dropped_hits_total[10m])` | > 0 sustained 10 min | informational | [discovery-drops](runbooks/ingestion.md#stellarindex_ingestion_discovery_drops) |
| `stellarindex_served_value_drift` | `served_value_ok == 0` | 26 h (two daily runs) | ticket | [served-value-drift](runbooks/data-freshness.md#stellarindex_served_value_drift) |
| `stellarindex_served_value_check_stale` | `time() - served_value_last_run_unix` | > 48 h for 1 h, or never existed | ticket | [served-value-drift](runbooks/data-freshness.md#stellarindex_served_value_check_stale) |
| `stellarindex_served_value_unit_failed` | `verify-served-values.service` failed now and 25 h ago | two daily runs failed (~26 h) | ticket | [served-value-drift](runbooks/data-freshness.md#stellarindex_served_value_unit_failed) |
| `stellarindex_served_value_persistently_skipped` | `served_value_skipped == 1` | 26 h (two daily runs) | ticket | [served-value-drift](runbooks/data-freshness.md#stellarindex_served_value_persistently_skipped) |
| `stellarindex_sdf_reserve_list_drift` | `sdf_reserve_list_drift > 0` per `kind` (`missing`/`extra` vs SDF's list) | 26 h. Absent (not 0) = SDF source unreadable: `_persistently_skipped{check="sdf_reserve_list"}` | ticket | [served-value-drift](runbooks/data-freshness.md#stellarindex_sdf_reserve_list_drift) |
| `stellarindex_cex_usd_volume_coverage_low` | share of `trade_inserts_total{usd_volume_populated="yes"}[1h]`, external venues | < 99.9% for 30 min | ticket | [usd-volume-coverage-plan](usd-volume-coverage-plan.md) |
| `stellarindex_onchain_usd_volume_coverage_low` | same share `[6h]`, on-chain venues excl. `unroutable\|thin` | < 99.5% for 1 h | ticket | [usd-volume-coverage-plan](usd-volume-coverage-plan.md) |
| `stellarindex_usd_volume_estimated_tier_high` | `verify_usd_volume_tier_usd{tier="estimated"}` share of last checked day | > 5% for 15 min | ticket | [usd-volume-coverage-plan](usd-volume-coverage-plan.md) |
| `stellarindex_usd_volume_xlm_bound_skipped` | `verify_usd_volume_xlm_bound_judged` | == 0 for 15 min (XLM bounds skipped) | ticket | [usd-volume-coverage-plan](usd-volume-coverage-plan.md) |
| `stellarindex_verify_usd_volume_stale` | `time() - verify_usd_volume_last_run_unix`, or absent | > 30 h for 1 h | ticket | [usd-volume-coverage-plan](usd-volume-coverage-plan.md) |
| `stellarindex_ingestion_ch_live_sink_drops` | `increase(stellarindex_ch_live_sink_ledgers_total{outcome="dropped"}[10m])` | > 0 sustained 10 min | ticket | [ch-live-sink-drops](runbooks/clickhouse.md#stellarindex_ingestion_ch_live_sink_drops) |
| `stellarindex_ingestion_ch_live_sink_drops_sustained` | `increase(stellarindex_ch_live_sink_ledgers_total{outcome="dropped"}[1h])` | > 0 sustained 1 h | page | [ch-live-sink-drops](runbooks/clickhouse.md#stellarindex_ingestion_ch_live_sink_drops) |
| `stellarindex_ingestion_ch_live_sink_errors` | `increase(stellarindex_ch_live_sink_ledgers_total{outcome="errored"}[30m])` | > 0 sustained 15 min | ticket | [ch-live-sink-errors](runbooks/clickhouse.md#stellarindex_ingestion_ch_live_sink_errors) |
| `stellarindex_ingestion_ch_live_sink_read_undercount` | `increase(ch_live_sink_read_undercount_total[30m])` per kind | > 0 for 15 min (ledger written short of txs/events/changes) | ticket | [ch-live-sink-read-undercount](runbooks/clickhouse.md#stellarindex_ingestion_ch_live_sink_read_undercount) |
| `stellarindex_ingestion_sink_undrained_rows` | `increase(sink_undrained_rows_total[1h])` | > 0, `for: 0m` (shutdown drain abandoned rows past the cursor) | ticket | [sink-undrained-rows](runbooks/ingestion-sink.md#stellarindex_ingestion_sink_undrained_rows) |
| `stellarindex_ingestion_trade_insert_backpressure` | `sum(rate(stellarindex_trade_insert_retries_total{outcome="retry"}[5m]))` | > 0 sustained 10 min | ticket | [trade-insert-backpressure](runbooks/ingestion-sink.md#stellarindex_ingestion_trade_insert_backpressure) |
| `stellarindex_ingestion_insert_errors` | `rate(source_insert_errors_total[5m])` | > 0.1/s for 5 min | ticket | [insert-errors](runbooks/ingestion-sink.md#stellarindex_ingestion_insert_errors) |
| `stellarindex_ingestion_persist_drop` | `increase(source_insert_errors_total{kind=~"trade\|soroswap_router_swap\|defindex_flow_strategy\|defindex_flow_vault"}[15m])` | > 0, `for: 0m` (`trade_abandoned` excluded: re-derivable) | ticket | [insert-errors](runbooks/ingestion-sink.md#stellarindex_ingestion_insert_errors) |
| `stellarindex_ingestion_trustline_observation_drop` | `increase(source_insert_errors_total{kind="trustline_observation"}[15m])` | > 0, `for: 0m` (under `insert_errors`' 0.1/s gate) | ticket | [insert-errors](runbooks/ingestion-sink.md#stellarindex_ingestion_insert_errors) |
| `stellarindex_ingestion_discovery_record_failures` | `increase(stellarindex_discovery_record_failures_total[10m])` | > 0 sustained 10 min | ticket | [discovery-drops](runbooks/ingestion.md#stellarindex_ingestion_discovery_drops) |
| `stellarindex_ingestion_duplicate_flood` | `rate(trade_insert_outcome_total{outcome="duplicate"}[10m])` with no `new` | > 0.5/s for 10 min | ticket | [ingestion-duplicate-flood](runbooks/ingestion-sink.md#stellarindex_ingestion_duplicate_flood) |
| `stellarindex_ingestion_source_insert_stale` | `time() - source_last_insert_unix`, enabled source | > 3600 s for 10 min (> 86400 s projected; see `projector_lag_high`) | ticket | [ingestion-duplicate-flood](runbooks/ingestion-sink.md#stellarindex_ingestion_duplicate_flood) |
| `stellarindex_ingestion_dispatcher_tx_skips` | `increase()[15m]` of `stellarindex_dispatcher_{tx_read_errors,tx_event_read_errors,entry_meta_unsupported,evicted_keys_unreadable}_total` | > 0, `for: 0m` | ticket | [dispatcher-tx-skips](runbooks/ingestion.md#stellarindex_ingestion_dispatcher_tx_skips) |
| `stellarindex_ingestion_uncorroborated_calls` | `increase(source_uncorroborated_calls_total[15m])` | > 0, `for: 0m` | ticket | [uncorroborated-calls](runbooks/ingestion-events.md#stellarindex_ingestion_uncorroborated_calls) |
| `stellarindex_dex_trade_unit_ratio_detected` | `increase(dex_trade_unit_ratio_total[30m])` per source | > 25 for 5 min | ticket | [dex-trade-unit-ratio](runbooks/dex.md#stellarindex_dex_trade_unit_ratio_detected) |
| `stellarindex_ingest_gap_detected` | `ingest_gap_max_size_ledgers` per (source, table) | > 1000 for 15 min | page | [ingest-gap-detected](runbooks/ingest-gap.md#stellarindex_ingest_gap_detected) + per-source [sdex-gap-detected](runbooks/ingest-gap.md#stellarindex_ingest_gap_detected) / [projector-replay](runbooks/projector.md#stellarindex_projector_replay_stalled) |
| `stellarindex_ingest_gap_detector_silent` | `time() - ingest_gap_detector_last_success_unix`, absent, or erroring runs | > 8 h, for 10 min | ticket | [ingest-gap-detector-silent](runbooks/ingest-gap.md#stellarindex_ingest_gap_detector_silent) |
| `stellarindex_ops_job_heartbeat_stale` | `ops_job_heartbeat_unix` while `_running` | > 15 min, for 10 min (process died) | ticket | [ops-job-stalled](runbooks/ops-job-stalled.md) |
| `stellarindex_ops_job_no_progress` | `changes()[30m]` of ops_job progress units and bytes while `_running` | both 0 for 30 min, for 15 min (job hung) | ticket | [ops-job-stalled](runbooks/ops-job-stalled.md) |
| `stellarindex_ops_job_run_failed` | `ops_job_last_exit_ok == 0` | last run exited non-zero, `for: 0m` | ticket | [ops-job-stalled](runbooks/ops-job-stalled.md) |
| `stellarindex_entry_index_lagging` | ledgerstream cursor minus `ch-entry-index` cursor | > 20000 ledgers for 30 min (timer silent, builder refusing, or initial build) | ticket | [ops-job-stalled](runbooks/ops-job-stalled.md#entry-index-lagging) |
| `stellarindex_projector_lag_high` | `projector_lag_ledgers` unless a replay window is active | > 256 for 10 min (replay rewinds covered by `projector_replay_stalled`) | ticket | [projector-lag](runbooks/projector.md#stellarindex_projector_lag_high) |
| `stellarindex_projector_error_rate_high` | `rate(projector_runs_total{outcome="error"}[15m])` | > 0.05/s for 15 min | ticket | [projector-lag](runbooks/projector.md#stellarindex_projector_error_rate_high) |
| `stellarindex_projector_row_quarantined` | `increase(projector_events_decoded_total{outcome="sink_quarantined"}[15m])` | > 0, `for: 0m` (poison row skipped) | ticket | [projector-row-quarantined](runbooks/projector.md#stellarindex_projector_row_quarantined) |
| `stellarindex_projector_row_dropped_permanent` | `increase(projector_events_decoded_total{outcome="sink_permanent"}[15m])` | > 0, `for: 0m`. Sink permanently rejected a row (SQLSTATE 22/23); re-counts every re-read. A burst across one source means a migration constraint the live rows violate | ticket | [projector-row-quarantined](runbooks/projector.md#stellarindex_projector_row_dropped_permanent) |
| `stellarindex_projector_i128_overflow` | `increase(projector_events_decoded_total{outcome="sink_i128_overflow"}[15m])` | > 0, `for: 0m`. Never chain data: an `int64` crept into one of our amount paths, so every amount on it is suspect | page | [projector-row-quarantined](runbooks/projector.md#stellarindex_projector_i128_overflow) |
| `stellarindex_projector_decode_error_rate_high` | `rate()[10m]` of `decode_error`/`reconstruct_error` outcomes | > 0.1/s for 15 min (decoder regression; cursor passed the events) | ticket | [projector-decode-error-rate](runbooks/projector.md#stellarindex_projector_decode_error_rate_high) |
| `stellarindex_projector_decode_error_ratio_high` | decode+reconstruct errors over all outcomes, `rate[1h]` | > 10% and >= 10 failures/h, for 15 min (quiet sources) | ticket | [projector-decode-error-rate](runbooks/projector.md#stellarindex_projector_decode_error_ratio_high) |
| `stellarindex_projector_wedged` | `projector_wedged` per source | > 0 for 5 min (cursor retrying one range; manual fix) | ticket | [projector-wedged](runbooks/projector.md#stellarindex_projector_wedged) |
| `stellarindex_spectra_unlisted_infrastructure` | `increase(spectra_unlisted_infrastructure_total[1h])` | > 0, `for: 0m` (unaudited factory/router/engine/WASM; needs code change) | page | [projector-spectra](runbooks/projector.md#stellarindex_spectra_unlisted_infrastructure) |
| `stellarindex_projector_replay_stalled` | replay window open and lag > 256 and not falling for 15 min | for 5 min (rewind stopped; deficit still open) | ticket | [projector-replay](runbooks/projector.md#stellarindex_projector_replay_stalled) |
| `stellarindex_external_poller_stale` | `time() - external_poller_last_success_unix`, all but ecb/tiingo/exchangeratesapi | > 1800 s for 5 min | ticket | [external-pollers](runbooks/external-pollers.md#stellarindex_external_poller_stale) |
| `stellarindex_external_poller_stale_ecb` | same, `source="ecb"` | > 43200 s (12h) for 10 min | ticket | [external-pollers](runbooks/external-pollers.md#stellarindex_external_poller_stale_ecb) |
| `stellarindex_external_poller_stale_tiingo` | same, `source="tiingo"` | > 7200 s (2h) for 10 min | ticket | [external-pollers](runbooks/external-pollers.md#stellarindex_external_poller_stale_tiingo) |
| `stellarindex_external_poller_stale_exchangeratesapi` | same, `source="exchangeratesapi"` | > 7200 s (2h) for 10 min | ticket | [external-pollers](runbooks/external-pollers.md#stellarindex_external_poller_stale_exchangeratesapi) |
| `stellarindex_cex_stream_subscription_rejected` | `cex_stream_subscription_rejected` per (source, symbol) | == 1 for 5 min (one symbol refused; socket healthy) | ticket | [external-pollers](runbooks/external-pollers.md#stellarindex_cex_stream_subscription_rejected) |
| `stellarindex_cex_stream_entry_skips` | `rate(cex_stream_entry_skips_total[15m])` | > 0 for 30 min (entries dropped: symbol rename or re-encoding) | ticket | [external-pollers](runbooks/external-pollers.md#stellarindex_cex_stream_entry_skips) |
| `stellarindex_cex_stream_stalled` | `increase(cex_stream_disconnect_total{reason="stall"}[15m])` | > 0 for 5 min (half-open socket) | ticket | [external-pollers](runbooks/external-pollers.md#stellarindex_cex_stream_stalled) |
| `stellarindex_external_dust_dropped_high` | `external_dust_dropped_total / (+ trade_inserts_total)`, 15m rate | share > 0.5 and > 0.05/s for 30 min | ticket | [external-pollers](runbooks/external-pollers.md#stellarindex_external_dust_dropped_high) |
| `stellarindex_external_poller_error_rate_high` | poll error ratio `[15m]` | > 0.5 for 15 min | informational | [external-pollers](runbooks/external-pollers.md#stellarindex_external_poller_error_rate_high) |
| `stellarindex_external_fx_feed_stale` | `time() - max(external_fx_last_quote_unix)` | > 21600 s (6h) for 15 min | ticket | [external-pollers](runbooks/external-pollers.md#stellarindex_external_fx_feed_stale) |
| `stellarindex_external_fx_feed_absent` | `absent(stellarindex_external_fx_last_quote_unix)` | series missing for 30 min | ticket | [external-pollers](runbooks/external-pollers.md#stellarindex_external_fx_feed_absent) |
| `stellarindex_external_fx_rate_rejections` | `increase(external_fx_rate_rejected_total[3h])`, excl. `*_stuck` reasons | > 2, for 30 min (ticker wedged on last accepted rate) | ticket | [external-pollers](runbooks/external-pollers.md#stellarindex_external_fx_rate_rejections) |
| `stellarindex_fx_fixings_refresh_stale` | `time() - max(fx_fixings_last_refresh_unix)` | > 10800 s (3h) for 15 min | ticket | [external-pollers](runbooks/external-pollers.md#stellarindex_fx_fixings_refresh_stale) |
| `stellarindex_fx_fixings_series_stale` | `time() - max(fx_fixings_newest_bar_end_unix)` | > 14400 s (4h) while FX trades (Mon 02:00-Fri 22:00 UTC), for 15 min | ticket | [external-pollers](runbooks/external-pollers.md#stellarindex_fx_fixings_series_stale) |
| `stellarindex_fx_fixings_quote_disagreement` | `fx_fixings_quote_disagrees` per ticker | == 1 for 2 h (hourly bars outside [0.5, 1.5] of daily rate) | ticket | [external-pollers](runbooks/external-pollers.md#stellarindex_fx_fixings_quote_disagreement) |

## Storage alerts

| Name | Metric | Condition | Severity | Runbook |
| ---- | ------ | --------- | -------- | ------- |
| `stellarindex_source_recognition_failing` | `stellarindex_recognition_ok` | == 0 for > 1 h | ticket | [source-recognition-failing](runbooks/data-freshness.md#stellarindex_source_recognition_failing) |
| `stellarindex_dependency_down` | `stellarindex_dependency_up` | == 0 for > 2 m | page | [dependency-down](runbooks/dependency-down.md) |
| `stellarindex_timescale_primary_down` | `up{job="postgres",role="primary"}` | == 0 for > 30 s | page | [timescale-primary-down](runbooks/postgres.md#stellarindex_timescale_primary_down) |
| `stellarindex_pg_wal_large` | `node_filesystem` used on `/var/lib/postgresql/wal` | > 64 GiB for 15 min | page | [pg-wal-large](runbooks/postgres.md#stellarindex_pg_wal_large) |
| `stellarindex_timescale_replica_lag` | `pg_replication_lag_seconds`, sync replica | > 5 s for 2 min | ticket | [replica-lag](runbooks/postgres.md#stellarindex_timescale_replica_lag) |
| `stellarindex_timescale_disk_full` | avail % on DB volume | < 10 % | page | [db-disk-full](runbooks/postgres.md#stellarindex_timescale_disk_full) |
| `stellarindex_timescale_disk_warning` | same | < 20 % | ticket | [db-disk-full](runbooks/postgres.md#stellarindex_timescale_disk_full) |
| `stellarindex_config_assertion_failed` | hourly `config-assertions.sh`: guard config (rsyslog, journald cap, CH logs, nft 443, redis cap, supply reserves) reverted | == 0 for 65 min | ticket | [config-assertion-failed](runbooks/config-assertion-failed.md) |
| `stellarindex_config_assertions_stale` | config-assertions producer silent | no textfile > 2 h, for 30 min | ticket | [config-assertion-failed](runbooks/config-assertion-failed.md) |
| `stellarindex_patroni_textfile_stale` | `time() - node_textfile_mtime_seconds{file="patroni.prom"}` | > 10 min, for 5 min | ticket | [patroni-textfile-stale](runbooks/textfile-stale.md#stellarindex_patroni_textfile_stale) |
| `stellarindex_textfile_producer_stale` | `time() - node_textfile_mtime_seconds`, any file without its own alert (excl. orphan/one-off `ops_job_*`, `restore_drill*`, `alertmanager_receivers`) | > 24 h, for 30 min | ticket | [textfile-producer-stale](runbooks/textfile-stale.md#stellarindex_textfile_producer_stale) |
| `stellarindex_node_root_disk_filling_fast` | `predict_linear` 10 min trend: root avail hits 0 in 30 min, and avail < 50% | for 2 min (log floods fill root in ~5 min) | page | [node-root-disk-filling-fast](runbooks/infra.md#stellarindex_node_root_disk_filling_fast) |
| `stellarindex_node_root_disk_full` | root `/` avail % (holds /var/log, /tmp, /var/cache) | < 10 % | page | [node-root-disk-full](runbooks/infra.md#stellarindex_node_root_disk_full) |
| `stellarindex_node_root_disk_warning` | same | < 20 % | ticket | [node-root-disk-warning](runbooks/infra.md#stellarindex_node_root_disk_warning) |
| (no active alert — surfaced via API log) | `forex: fx_quotes persist failed` log line — runtime symptom of an unapplied schema migration | repeating every ~5 min | P3 | [fx-history-missing](runbooks/data-freshness.md#fx-history-missing) |
| `stellarindex_worker_panicked` | `increase(worker_panics_total[10m])` | > 0 (background worker stopped until unit restarts) | page | [infra](runbooks/infra.md#stellarindex_worker_panicked) |
| `stellarindex_node_reboot_required` | `node_reboot_required` | == 1 for 7 d (never auto-applied) | ticket | [ops-job-stalled](runbooks/ops-job-stalled.md#applying-pending-service-restarts-maintenance-step) |
| `stellarindex_postgres_ping_failing` | `rate(postgres_ping_total{outcome="error"}[5m])` | > 0.5/s for 2 min (indexer pool wedged) | page | [postgres-ping-failing](runbooks/postgres.md#stellarindex_postgres_ping_failing) |
| `stellarindex_timescale_connections_saturated` | `pg_stat_activity_count / max_connections` | > 80 % for 5 min | ticket | [pg-conns-saturated](runbooks/postgres.md#stellarindex_timescale_connections_saturated) |
| `stellarindex_timescale_lock_table_pressure` | `pg_locks_count / (max_locks_per_transaction * max_connections)` | > 70 % for 5 min | ticket | [pg-conns-saturated](runbooks/postgres.md#stellarindex_timescale_lock_table_pressure) |
| `stellarindex_systemd_unit_failed` | `node_systemd_unit_state{state="failed"}` (catch-all excl. units with own alerts) | failed 15 min (activating dips < 10 min bridged) | ticket | [infra](runbooks/infra.md#stellarindex_systemd_unit_failed) |
| `stellarindex_timescale_cagg_stale` | `time() - cagg_last_refresh_unix` per CAGG | > 5x its refresh interval | ticket | [cagg-stale](runbooks/timescale.md#stellarindex_timescale_cagg_stale) |
| `stellarindex_timescale_cagg_refresh_missing` | CAGG series seen within 1 d now absent (dropped policy; `cagg_stale` cannot see it) | for 15 min | ticket | [cagg-stale](runbooks/timescale.md#stellarindex_timescale_cagg_stale) |
| `stellarindex_late_trade_cagg_refresh_failing` | `late_trade_cagg_refresh_overdue_seconds` | > 600 s for 30 min | ticket | [late-trade-refresh-failing](runbooks/timescale.md#stellarindex_late_trade_cagg_refresh_failing) |
| `stellarindex_timescale_job_failures_climbing` | `increase(timescale_job_failures_total)` per job, excl. concurrent-refresh | > 10 in 6h, >= 3 in 3d, or 36 h of failed intervals | ticket | [timescale-job-failures-climbing](runbooks/timescale.md#stellarindex_timescale_job_failures_climbing) |
| `stellarindex_timescale_compression_lag` | `stellarindex_timescale_chunks_overdue_compression` | > 0 for > 24 h | informational | [compression-lag](runbooks/timescale.md#stellarindex_timescale_compression_lag) |
| `stellarindex_timescale_probe_degraded` | `timescale_probe_query_ok`, `_rows`, `_last_run_unix` | query error, no rows, or stale/never written; 15 min | ticket | [timescale-probe-degraded](runbooks/timescale.md#stellarindex_timescale_probe_degraded) |
| `stellarindex_pg_lock_convoy` | `pg_lock_convoy_wait_seconds_max` (via node_exporter; postgres_exporter dies in a convoy) | > 120 s for 2 min | page | [pg-lock-convoy](runbooks/postgres.md#stellarindex_pg_lock_convoy) |
| `stellarindex_timescale_backup_failed` | `min by (stanza)(pgbackrest_backup_since_last_completion_seconds)` | > 25 h for 5 min | ticket | [infra](runbooks/infra.md#stellarindex_timescale_backup_failed) |
| `stellarindex_timescale_backup_none_24h` | same | > 24 h for 5 min | page | [infra](runbooks/infra.md#stellarindex_timescale_backup_none_24h) |
| `stellarindex_pgbackrest_backup_metrics_absent` | exporter `up == 1` but no real-stanza backup series | 15 min (the two alerts above are blind to this) | page | [infra](runbooks/infra.md#stellarindex_pgbackrest_backup_metrics_absent) |
| `stellarindex_pgbackrest_backup_unit_failed` | `node_systemd_unit_state{name="pgbackrest-backup.service",state="failed"}` | == 1 for 5 min | ticket | [infra](runbooks/infra.md#stellarindex_pgbackrest_backup_unit_failed) |
| `stellarindex_ch_schema_snapshot_stale` | `time() - ch_schema_snapshot_last_success_unix`, or absent | > 36 h, 30 min | ticket | [ch-schema-restore](runbooks/ch-schema-restore.md) |
| `stellarindex_ch_schema_snapshot_offsite_stale` | `time() - ch_schema_snapshot_offsite_last_success_unix`, or absent | > 72 h, 30 min (never-configured is as loud as failing) | ticket | [ch-schema-restore](runbooks/ch-schema-restore.md) |
| `stellarindex_ch_lake_backup_stale` | `time() - ch_lake_backup_last_success_unix`, `last_full_unix` | > 96 h, no full in 72 h, or full older than interval + 4 d; 1 h | ticket | [ch-lake-backup](runbooks/ch-lake-backup.md) |
| `stellarindex_config_backup_stale` | `time() - config_backup_last_success_unix`, or `offsite_configured` absent | > 36 h, 30 min | ticket | [off-site-backup-plan](off-site-backup-plan.md) |
| `stellarindex_config_backup_offsite_stale` | `time() - config_backup_offsite_last_success_unix` | > 72 h, 30 min | ticket | [off-site-backup-plan](off-site-backup-plan.md) |
| `stellarindex_lake_verify_stale` | `time() - lake_verify_last_run_unix` | > 48 h, 1 h | ticket | [lake-verify](runbooks/lake-verify.md) |
| `stellarindex_lake_verify_failed` | `max by (instance, check)(lake_verify_failures)` | > 0 (contiguity, entry_changes, hash_chain, raw_census) | ticket | [lake-verify](runbooks/lake-verify.md) |
| `stellarindex_network_state_verify_stale` | `time() - network_state_verify_last_run_unix` | > 48 h, 1 h | ticket | [network-state-verify](runbooks/network-state-verify.md) |
| `stellarindex_network_state_verify_failed` | `max by (instance, check)(network_state_verify_failures)` | > 0 (hot_archive diff or lumens residual) | ticket | [network-state-verify](runbooks/network-state-verify.md) |
| `stellarindex_wasm_drift` | `max by (source, contract)(wasm_drift)` | > 0 for 1 h (gated contract runs a WASM hash not in `audited_wasm.json`) | ticket | [wasm-drift](runbooks/wasm-drift.md) |
| `stellarindex_wasm_drift_stale` | `time() - wasm_drift_last_run_unix` | > 48 h, 1 h | ticket | [wasm-drift](runbooks/wasm-drift.md) |
| `stellarindex_galexie_archive_mirror_stale` | `time() - galexie_archive_mirror_last_success_timestamp` | > 48 h, 1 h. Metric absent (cannot fire) when the mirror is disabled | ticket | [galexie-archive-mirror](runbooks/archive.md#stellarindex_galexie_archive_mirror_stale) |
| `stellarindex_ch_schema_snapshot_unit_failed` | `node_systemd_unit_state{name="ch-schema-snapshot.service",state="failed"}` | == 1 for 5 min | ticket | [ch-schema-restore](runbooks/ch-schema-restore.md) |
| `stellarindex_ch_schema_drift_detected` | `stellarindex_ch_schema_drift_divergent` | > 0 for ≥ 30 min | ticket | [ch-schema-restore](runbooks/ch-schema-restore.md) |
| `stellarindex_ch_schema_drift_not_converged` | `ch_schema_drift_intent_converged` | == 0 for 30 min | ticket | [ch-schema-restore](runbooks/ch-schema-restore.md) |
| `stellarindex_ch_schema_drift_stale` | `time() - ch_schema_drift_last_run_unix`, or absent | > 36 h, 30 min | ticket | [ch-schema-restore](runbooks/ch-schema-restore.md) |
| `stellarindex_clickhouse_server_down` | `up{job="clickhouse"}` | == 0 for 2 min, or absent 10 min | page | [clickhouse-server-health](runbooks/clickhouse.md#stellarindex_clickhouse_server_down) |
| `stellarindex_clickhouse_query_failures_high` | `rate(ClickHouseProfileEvents_FailedQuery[10m])` | > 0.1/s sustained 15 min | ticket | [clickhouse-server-health](runbooks/clickhouse.md#stellarindex_clickhouse_query_failures_high) |
| `stellarindex_clickhouse_inserts_rejected` | `increase(ClickHouseProfileEvents_RejectedInserts[1h])` | > 0 ("too many parts"); holds until the hour clears. A rejected block is a lake gap | ticket | [clickhouse-server-health](runbooks/clickhouse.md#stellarindex_clickhouse_inserts_rejected) |
| `stellarindex_counts_rollup_plan_unusable` | `min by (rollup) (stellarindex_counts_rollup_plan_ok)` | == 0 for 30m: account panels scan the source instead of the counts rollup | ticket | [clickhouse](runbooks/clickhouse.md#stellarindex_counts_rollup_plan_unusable) |
| `stellarindex_counts_rollup_stale_partitions` | `max by (rollup) (stellarindex_counts_rollup_stale_partitions)` | > 0 for 2h: optype-rollup.timer is not keeping up | ticket | [clickhouse](runbooks/clickhouse.md#stellarindex_counts_rollup_stale_partitions) |
| `stellarindex_restore_drill_stale` | `time() - restore_drill_last_success_unix{repo=~"1\|"}`, or absent | > 40 d, 30 min (on-box repo1 drill) | ticket | [restore-drill-stale](runbooks/restore-drill.md#stellarindex_restore_drill_stale) |
| `stellarindex_zfs_pool_free_low` | `zfs_pool_free_bytes` | < 1.6 TiB for 15 min | ticket | [zfs-snapshots](runbooks/zfs-snapshots.md) |
| `stellarindex_zfs_pool_free_critical` | same | < 1.3 TiB for 5 min (below snapshot job's 1.4 TiB floor) | page | [zfs-snapshots](runbooks/zfs-snapshots.md) |
| `stellarindex_zfs_snapshot_stale` | `time() - zfs_snapshot_latest_unix` per dataset | > 36 h, 30 min | ticket | [zfs-snapshots](runbooks/zfs-snapshots.md) |
| `stellarindex_zfs_snapshot_pool_free_unreadable` | `zfs_snapshot_pool_free_unreadable` | == 1 for 10 min (job refused to prune/snapshot) | ticket | [zfs-snapshots](runbooks/zfs-snapshots.md) |
| `stellarindex_restore_drill_failed` | `restore_drill_failures` per repo | > 0 for 30 min | ticket | [restore-drill-failed](runbooks/restore-drill.md#stellarindex_restore_drill_failed) |
| `stellarindex_restore_drill_offsite_stale` | `time() - restore_drill_last_success_unix{repo="2"}`, or absent | > 35 d, 30 min. A green repo1 drill says nothing about it | ticket | [restore-drill-offsite-stale](runbooks/restore-drill.md#stellarindex_restore_drill_offsite_stale) |
| `stellarindex_restore_drill_textfile_stale` | `time() - node_textfile_mtime_seconds{file=~"restore_drill.*\.prom"}` | > 35 d, 30 min (backstop) | ticket | [restore-drill-stale](runbooks/restore-drill.md#stellarindex_restore_drill_textfile_stale) |
| `stellarindex_backup_offsite_stale` | pgbackrest `backup_info{repo_key="2"}` age | no repo2 backup < 8 d, 1 h. repo1-fresh/repo2-stale is invisible to the alerts above | ticket | [backup-offsite-stale](runbooks/backup-offsite.md#stellarindex_backup_offsite_stale) |

## Cache / serving alerts

| Name | Metric | Condition | Severity | Runbook |
| ---- | ------ | --------- | -------- | ------- |
| `stellarindex_redis_master_down` | `redis_up` per master | == 0 for 30 s | page | [redis-master-down](runbooks/cache.md#stellarindex_redis_master_down) |
| `stellarindex_redis_memory_saturated` | `redis_memory_used_bytes / redis_memory_max_bytes` | > 90 % for 5 min | ticket | [redis-memory](runbooks/cache.md#stellarindex_redis_memory_saturated) |
| `stellarindex_redis_evictions_high` | `rate(redis_evicted_keys_total[5m])` | > 100/s | ticket | [redis-memory](runbooks/cache.md#stellarindex_redis_evictions_high) |
| `stellarindex_redis_replication_broken` | `redis_connected_slaves` per master | < expected for 2 min | ticket | [redis-replication](runbooks/cache.md#stellarindex_redis_replication_broken) |
| `stellarindex_redis_sentinel_textfile_stale` | `node_textfile_mtime_seconds{file="redis_sentinel.prom"}` age | > 10 min, 5 min | ticket | [redis-sentinel-textfile-stale](runbooks/cache.md#stellarindex_redis_sentinel_textfile_stale) |
| `stellarindex_redis_writes_blocked` | `redis_rdb_last_bgsave_status` per master (`MISCONF` in client logs) | == 0 for 60 s | page | [redis-write-blocked-disk-full](runbooks/cache.md#stellarindex_redis_writes_blocked) |
| `stellarindex_redis_write_rejected_oom` | `rate(redis_errors_total{err=~"OOM\|READONLY\|NOREPLICAS"}[5m])` | > 0 for > 2 min | page | [redis-memory](runbooks/cache.md#stellarindex_redis_write_rejected_oom) |

## API plane alerts

| Name | Metric | Condition | Severity | Runbook |
| ---- | ------ | --------- | -------- | ------- |
| `stellarindex_api_down` | `sum(up{job=~"stellarindex[_-]api"})`, or absent | == 0 for 60 s, or absent 5 min | page | [api-down](runbooks/api.md#stellarindex_api_down) |
| `stellarindex_api_latency_p95_high` | p95 of `http_request_duration_seconds` | > 500 ms for 10 min | ticket | [api-latency](runbooks/api.md#stellarindex_api_latency_p95_high) |
| `stellarindex_api_latency_p99_high` | p99, same | > 2 s for 10 min | ticket | [api-latency](runbooks/api.md#stellarindex_api_latency_p99_high) |
| `stellarindex_api_price_stream_not_delivering` | aggregator stream publishes `ok` vs API stream subscribes `ok` | publishing, none fanned out, 15 min | ticket | [price-stream-not-delivering](runbooks/api.md#stellarindex_api_price_stream_not_delivering) |
| `stellarindex_api_error_rate_high` | 5xx share of `http_requests_total[5m]` | > 1 % for 2 min | ticket | [api-5xx](runbooks/api.md#stellarindex_api_error_rate_high) |
| `stellarindex_api_error_rate_critical` | same | > 5 % for 2 min | page | [api-5xx](runbooks/api.md#stellarindex_api_error_rate_critical) |
| `stellarindex_api_price_stale` | `price_staleness_seconds` per (asset, quote) | > 120 s for 5 min | ticket | [price-stale](runbooks/api.md#stellarindex_api_price_stale) |
| `stellarindex_api_cache_miss_rate_high` | cache `miss` share of ops per (cache, op) | > 50% for 10 min on ops >= 0.1 req/s | ticket | [cache-miss-rate-high](runbooks/api.md#stellarindex_api_cache_miss_rate_high) |
| `stellarindex_api_cache_refresh_failing` | `increase(api_cache_ops_total{result="refresh_error"}[15m])` per (cache, op) | > 0 for 30 min (callers get `flags.stale`) | ticket | [cache-miss-rate-high](runbooks/api.md#stellarindex_api_cache_refresh_failing) |

## Notify (transactional-email) alerts

`internal/notify` (Resend) sends the magic-link login and API-signup emails. The login handler swallows send errors (enumeration-safe), so `stellarindex_notify_sends_total` is the only mail-outage signal.

| Name | Metric | Condition | Severity | Runbook |
| ---- | ------ | --------- | -------- | ------- |
| `stellarindex_notify_send_failure_ratio_high` | failed share of `notify_sends_total[15m]` per template | > 0.5 for 15 min (mail outage: logins/signups stop; sessions and keys unaffected) | ticket | [notify-send-failure](runbooks/meta.md#stellarindex_notify_send_failure_ratio_high) |
| `stellarindex_notify_no_send_activity` | 2xx login/signup requests but zero `notify_sends_total` in 6 h | dead-man's switch; idle stays quiet | ticket | [notify-send-failure](runbooks/meta.md#stellarindex_notify_send_failure_ratio_high) |
| `stellarindex_notify_send_failed` | `increase(stellarindex_notify_sends_total{result="failed"}[1h])` per template | > 0, `for: 0m` | ticket | [notify-send-failure](runbooks/meta.md#stellarindex_notify_send_failure_ratio_high) |
| `stellarindex_notify_send_failures_sustained` | `rate(notify_sends_total{result="failed"}[15m])` per template | > 0 for 1 h (trickle under the ratio rule) | ticket | [notify-send-failure](runbooks/meta.md#stellarindex_notify_send_failure_ratio_high) |
| `stellarindex_notify_send_rate_high` | `rate(notify_sends_total{result="sent"}[15m]) * 3600` | > 300 mails/h for 15 min | ticket | [notify-send-failure](runbooks/meta.md#stellarindex_notify_send_failure_ratio_high) |

## SLA-probe alerts

`cmd/stellarindex-sla-probe` runs every 15 min (`configs/healthchecks/stellarindex-sla-probe.timer`) and writes a textfile. Thresholds equal the targets `/sla` publishes (pinned by `internal/ops/chops/sla_figure_consistency_test.go`); the API-plane latency alerts above are looser real-traffic backstops.

| Name | Metric | Condition | Severity | Runbook |
| ---- | ------ | --------- | -------- | ------- |
| `stellarindex_sla_probe_p95_breach` | `sla_probe_latency_ms{quantile="0.95"}` | > 200 ms, 30 min | page | [sla-probe-p95-breach](runbooks/sla-probe.md#stellarindex_sla_probe_p95_breach) |
| `stellarindex_sla_probe_p99_breach` | `sla_probe_latency_ms{quantile="0.99"}` | > 500 ms, 30 min | page | [sla-probe-p99-breach](runbooks/sla-probe.md#stellarindex_sla_probe_p99_breach) |
| `stellarindex_sla_probe_availability_breach` | `stellarindex_sla_probe_availability_pct` | < 99.9 for ≥ 30 min | page | [sla-probe-availability-breach](runbooks/sla-probe.md#stellarindex_sla_probe_availability_breach) |
| `stellarindex_sla_probe_freshness_breach` | `sla_probe_freshness_sec` | `price` > 150 s, others > 30 s; 30 min | page | [sla-probe-freshness-breach](runbooks/sla-probe.md#stellarindex_sla_probe_freshness_breach) |
| `stellarindex_sla_probe_unit_failed_alert` | `stellarindex_sla_probe_unit_failed` | > 0 for ≥ 30 min | ticket | [sla-probe-unit-failed](runbooks/sla-probe.md#stellarindex_sla_probe_unit_failed_alert) |
| `stellarindex_sla_probe_stale` | `time() - sla_probe_last_pass_timestamp` | > 90 min, 5 min | page | [sla-probe-stale](runbooks/sla-probe.md#stellarindex_sla_probe_stale) |

## API-smoke alerts

`scripts/dev/r1-smoke.sh` runs every 5 min (`configs/healthchecks/` timer), writing `api_smoke.prom`. It is the only check that reads response **bodies** (34 GETs with jq shape assertions): it catches a 200 carrying the wrong JSON or a documented 4xx that regressed to 200, which heartbeats and the SLA probe cannot see. Both rows are `ticket`, not `page`: a smoke failure while the API is healthy is not SEV-1. They carry no `alert_family`, so they neither inhibit nor are inhibited by api-plane alerts.

| Name | Metric | Condition | Severity | Runbook |
| ---- | ------ | --------- | -------- | ------- |
| `stellarindex_api_smoke_failing` | `api_smoke_failures` | > 0 for 30 min | ticket | [api-smoke-failing](runbooks/sla-probe.md#stellarindex_api_smoke_failing) |
| `stellarindex_api_smoke_stale` | `time() - api_smoke_last_run_unix`, or absent | > 30 min, 5 min | ticket | [api-smoke-stale](runbooks/sla-probe.md#stellarindex_api_smoke_stale) |
| `stellarindex_curated_rwa_sync_stale` | `time() - curated_rwa_sync_last_run_unix`, or absent | > 30 h, 10 min | ticket | [curated-rwa-sync](runbooks/data-freshness.md#stellarindex_curated_rwa_sync_stale) |
| `stellarindex_curated_rwa_sync_refused` | `curated_rwa_sync_refused == 1` (`DUNE_API_KEY` unset) | >= 2 h | ticket | [curated-rwa-sync](runbooks/data-freshness.md#stellarindex_curated_rwa_sync_refused) |
| `stellarindex_curated_rwa_published_stale` | `time() - curated_rwa_sync_executed_at_unix` while non-zero | > 72 h, 1 h (public query stopped re-executing) | ticket | [curated-rwa-sync](runbooks/data-freshness.md#stellarindex_curated_rwa_published_stale) |

## Healthchecks.io ping delivery

Rules in `deploy/monitoring/rules/healthcheck-ping.yml`. Healthchecks.io judges by silence, so an undelivered ping and a stopped service look identical; this alert says which one it is.

| Name | Metric | Condition | Severity | Runbook |
| ---- | ------ | --------- | -------- | ------- |
| `stellarindex_healthcheck_ping_undelivered` | `healthcheck_ping_failures_total`, `_ping_last_success_unix` | failed and none accepted in 15 min, 5 min | ticket | [healthcheck-ping](runbooks/meta.md#stellarindex_healthcheck_ping_undelivered) |

## SLO burn-rate alerts (multi-window)

Per [ADR-0009](../adr/0009-latency-budget.md); the availability budget is the published ≥ 99.9 % ([stellarindex.io/sla](https://stellarindex.io/sla)). Short and long windows must BOTH agree before firing (Google SRE workbook). The direct-threshold alerts above stay live as backstops.

| Name | SLO | Burn rate (× monthly budget) | Severity | Runbook |
| ---- | --- | ---------------------------- | -------- | ------- |
| `stellarindex_slo_latency_burn_fast` | 99.9% under 200ms | > 14.4× over 5m AND 1h | page | [api-latency](runbooks/api.md#stellarindex_api_latency_p95_high) + tier detail [slo-latency-burn-fast](runbooks/slo.md#stellarindex_slo_latency_burn_fast) |
| `stellarindex_slo_latency_burn_medium` | same | > 6× over 30m AND 6h | page | [api-latency](runbooks/api.md#stellarindex_api_latency_p95_high) + tier detail [slo-latency-burn-medium](runbooks/slo.md#stellarindex_slo_latency_burn_medium) |
| `stellarindex_slo_latency_burn_slow` | same | > 1× over 6h AND 24h | ticket | [api-latency](runbooks/api.md#stellarindex_api_latency_p95_high) + tier detail [slo-latency-burn-slow](runbooks/slo.md#stellarindex_slo_latency_burn_slow) |
| `stellarindex_slo_availability_burn_fast` | 99.9% non-5xx | > 14.4× over 5m AND 1h | page | [api-5xx](runbooks/api.md#stellarindex_api_error_rate_critical) + tier detail [slo-availability-burn-fast](runbooks/slo.md#stellarindex_slo_availability_burn_fast) |
| `stellarindex_slo_availability_burn_medium` | same | > 6× over 30m AND 6h | page | [api-5xx](runbooks/api.md#stellarindex_api_error_rate_critical) + tier detail [slo-availability-burn-medium](runbooks/slo.md#stellarindex_slo_availability_burn_medium) |
| `stellarindex_slo_availability_burn_slow` | same | > 1× over 6h AND 24h | ticket | [api-5xx](runbooks/api.md#stellarindex_api_error_rate_critical) + tier detail [slo-availability-burn-slow](runbooks/slo.md#stellarindex_slo_availability_burn_slow) |

## Stellar / node alerts

> **Inert on r1.** The first four alerts here need stellar-core / stellar-rpc / the core prometheus exporter, all removed from r1 ([r1-deployment-state.md](r1-deployment-state.md)), so they cannot fire there. They stay for Phase-3 (ADR-0004); each runbook's *Deployment posture* callout gives the revival path.
>
> `archive-divergence` is not affected: it reads `stellarindex_verify_archive_mismatches_total`, published by the verify-archive tier-a/tier-b timers.

| Name | Metric | Condition | Severity | Runbook |
| ---- | ------ | --------- | -------- | ------- |
| `stellarindex_stellar_core_ledger_age` | `time() - stellar_core_last_ledger_time_unix` | > 60 s for 2 min | page | [core-lag](runbooks/stellar-node.md#stellarindex_stellar_core_ledger_age) |
| `stellarindex_stellar_core_peers_low` | `stellarindex_stellar_core_peer_count` | < 5 for > 5 min | ticket | [core-peers](runbooks/stellar-node.md#stellarindex_stellar_core_peers_low) |
| `stellarindex_stellar_rpc_lag` | `stellar_rpc_latest_ledger_age_seconds` | > 300 s for 5 min | ticket | [rpc-lag](runbooks/stellar-node.md#stellarindex_stellar_rpc_lag) |
| `stellarindex_stellar_archive_publish_fail` | `increase(stellarindex_stellar_archive_publish_errors_total[1h])` | > 0 | informational | [archive-publish](runbooks/archive.md#stellarindex_stellar_archive_publish_fail) |
| `stellarindex_stellar_archive_divergence` | `increase(verify_archive_mismatches_total[26h])` | > 0 | page | [archive-divergence](runbooks/archive.md#stellarindex_stellar_archive_divergence) |

## Stellar-stack version-lag alerts

`stellar-stack-version-probe.timer` (daily) compares installed `stellar-core`, `stellar-galexie` and `stellar-archivist` to upstream and writes `stellarindex_stellar_stack_version_lag{component}` (0 current, 1 newer available, 2 newer PROTOCOL-MAJOR available), `..._installed_info{component,version}` and `..._probe_success` to the textfile collector.

| Name | Metric | Condition | Severity | Runbook |
| ---- | ------ | --------- | -------- | ------- |
| `stellarindex_stellar_stack_lagging` | `stellar_stack_version_lag` per component | >= 1 for 2 d | ticket | [stellar-stack-version-lag](runbooks/stellar-node.md#stellar-stack-version-lag) |
| `stellarindex_stellar_stack_protocol_lag` | same | >= 2 for 6 h | page | [stellar-stack-version-lag](runbooks/stellar-node.md#stellar-stack-version-lag) |
| `stellarindex_stellar_stack_probe_degraded` | `stellar_stack_probe_success`, probe file mtime | unclassified component, file stale 48 h, or never written; 1 h | ticket | [stellar-stack-version-lag](runbooks/stellar-node.md#stellar-stack-version-lag) |
| `stellarindex_stellar_core_autoupgrade_failed` | `stellar_core_autoupgrade_result{result=~"rolled_back\|failed"}` | == 1 for 5 min (core did not advance the galexie tip, or rollback stalled) | page | [stellar-stack-version-lag](runbooks/stellar-node.md#stellar-stack-version-lag) |
| `stellarindex_stellar_core_autoupgrade_stale` | `stellar_core_autoupgrade_last_run_timestamp_seconds` | no run in 18 h, or never in 1 d; 1 h | ticket | [stellar-stack-version-lag](runbooks/stellar-node.md#stellar-stack-version-lag) |
| `stellarindex_ledger_meta_decode_failing` | `ledger_meta_decode_failures_total` per unit | > 0 for 10 min | page | [ledger-meta-decode-failing](runbooks/ledger-ingest.md#stellarindex_ledger_meta_decode_failing) |
| `stellarindex_ledger_meta_decode_probe_stale` | `ledger_meta_decode_probe_updated_seconds` | age > 1 h for 30 min | ticket | [ledger-meta-decode-failing](runbooks/ledger-ingest.md#stellarindex_ledger_meta_decode_failing) |

The two decode alerts are the REACTIVE backstop to the version-lag pair above.

## StellarIndex own-binary version-skew alerts

Watches OUR binaries (the section above watches third-party ones). A deploy that never touches a binary still exits 0, so a drifted `stellarindex-sla-probe` or `stellarindex-ops` goes unreported.

`stellarindex-binary-version-probe.timer` (every 30 min) writes `stellarindex_binary_version_skew` (distinct installed versions minus one), `..._info{binary,version}`, `..._binaries_total` and `..._probe_success` to the textfile collector.

The alert compares binaries against each other, not an expected version, so it also covers binaries added later.

| Name | Metric | Condition | Severity | Runbook |
| ---- | ------ | --------- | -------- | ------- |
| `stellarindex_binary_version_skew` | `stellarindex_binary_version_skew` | > 0 for 45 m | ticket | [binary-version-skew](runbooks/binary-version-skew.md#stellarindex_binary_version_skew) |
| `stellarindex_binary_version_probe_degraded` | `stellarindex_binary_version_probe_success` | == 0 for 2 h | ticket | [binary-version-skew](runbooks/binary-version-skew.md#stellarindex_binary_version_probe_degraded) |
| `stellarindex_binary_version_probe_stale` | `node_textfile_mtime_seconds`, `binary_version_probe_success` | > 90 min old or absent, 10 min | ticket | [binary-version-skew](runbooks/binary-version-skew.md#stellarindex_binary_version_probe_stale) |

Impact is one-way: `stellarindex-ops` backs the integrity gates (`verify-archive`, `archive-completeness`, `ch-schema-drift`, `restore-drill`), and a stale gate PASSES when it should fail.
The version probe compares released versions, so it cannot know the network has begun emitting new XDR: testnet ingested 240 Protocol-28 ledgers on a galexie that could not decode P28 with no error. `..._decode_failing` watches the failure itself; `..._decode_probe_stale` stops a dead probe reading as "no failures". It is fail-closed (component stops, no corrupt data written): an ingestion outage, not a correctness incident.

## Archive completeness alerts

Per [ADR-0017](../adr/0017-archive-completeness-invariants.md). The primary archive (`galexie-archive/` MinIO) and cross-anchor archive (`/srv/history-archive/`) have hard completeness contracts, enforced daily on R1 by `archive-completeness.timer`; R2 + R3 delegate cross-anchor checks to R1 and verify chain-link locally. See [archive-completeness.md](archive-completeness.md).

| Name | Metric | Condition | Severity | Runbook |
| ---- | ------ | --------- | -------- | ------- |
| `stellarindex_archive_files_missing` | `archive_files_missing` per archive | > 0 for 4 h | ticket | [archive-files-missing](runbooks/archive.md#stellarindex_archive_files_missing) |
| `stellarindex_archive_completeness_stale` | `time() - archive_completeness_last_success_timestamp` | > 26 h | ticket | [archive-completeness-stale](runbooks/archive.md#stellarindex_archive_completeness_stale) |
| `stellarindex_archive_completeness_critical_stale` | same | > 48 h on R1 (integrity leader) | page | [archive-completeness-stale](runbooks/archive.md#stellarindex_archive_completeness_critical_stale) |
| `stellarindex_archive_repair_source_degraded` | repair failures / attempts per source (25 h) | > 0.10 over one verify cycle | informational | [archive-repair-source-degraded](runbooks/archive.md#stellarindex_archive_repair_source_degraded) |
| `stellarindex_galexie_catchup_refused` | `galexie_catchup_refusals_5m` | > 0 for 10 min | page | [galexie-archive](runbooks/archive.md#stellarindex_galexie_catchup_refused) |
| `stellarindex_galexie_catchup_probe_degraded` | `galexie_catchup_probe_read_ok`, `_journal_lines`, `_last_run_unix` | journal read error or empty, file stale 10 min, or never written; 15 min | ticket | [galexie-archive](runbooks/archive.md#stellarindex_galexie_catchup_probe_degraded) |
| `stellarindex_host_swap_activity` | `rate(node_vmstat_pswpout[10m])` | > 100 for 15 m | ticket | [galexie-archive](runbooks/archive.md#stellarindex_host_swap_activity) |
| `stellarindex_galexie_archive_tip_lag_high` | `galexie_archive_tip_lag_ledgers` | > 64,000 for 90 min | ticket | [galexie-archive](runbooks/archive.md#stellarindex_galexie_archive_tip_lag_high) |
| `stellarindex_galexie_archive_tip_lag_severe` | same | > 128,000 for 30 m | page | [galexie-archive](runbooks/archive.md#stellarindex_galexie_archive_tip_lag_severe) |
| `stellarindex_galexie_archive_tip_lag_metric_stale` | `galexie_archive_tip_lag_probe_success == 0`, or `_updated_seconds` > 30 min | 15 min | ticket | [galexie-archive](runbooks/archive.md#stellarindex_galexie_archive_tip_lag_metric_stale) |
| `stellarindex_galexie_archive_gap` | `galexie_archive_unexpected_gaps` (holes/overlaps in the DR mirror; tip-lag only proves the newest edge) | > 0 for 1 h | page | [galexie-archive](runbooks/archive.md#stellarindex_galexie_archive_gap) |
| `stellarindex_galexie_archive_contiguity_silent` | `absent_over_time(galexie_archive_unexpected_gaps[3h])` | for 15 m (hourly scan dark) | ticket | [galexie-archive](runbooks/archive.md#stellarindex_galexie_archive_contiguity_silent) |
| `stellarindex_galexie_archive_scan_degraded` | `galexie_archive_scan_ok`, `_scan_last_run_unix` | listing error, file stale 3 h, or never written; 15 min (only alert when the read fails) | ticket | [galexie-archive](runbooks/archive.md#stellarindex_galexie_archive_scan_degraded) |
| `stellarindex_galexie_archive_upstream_divergence` | `galexie_archive_upstream_objects{result=~"upstream-rewritten\|local-differs\|local-only"}` | > 0 for 15 min | ticket | [galexie-archive](runbooks/archive.md#stellarindex_galexie_archive_upstream_divergence) |
| `stellarindex_galexie_archive_upstream_check_stale` | `time() - galexie_archive_upstream_last_success_unix`, or absent | > 15 d, 1 h | ticket | [galexie-archive](runbooks/archive.md#stellarindex_galexie_archive_upstream_check_stale) |

Defense-in-depth for a 23-day silent stall of `galexie-archive`: these page within hours if the hourly `galexie-archive-fill.timer` (or its `mc` aliases, aws-public IAM, MinIO mtime-poison) breaks. Source: `galexie_archive_tip_lag.prom`, refreshed every 5 min.

## HashDB drift-detector alerts

Per [ADR-0016](../adr/0016-per-region-storage-strategy.md). The indexer appends sha256(LCM) per ledger to an on-disk hashdb and a sweep re-reads a trailing window to catch upstream rewrites a chain-link check cannot see. Off by default (`[hashdb].enabled = false`); both alerts are inert on a region that has not opted in.

| Name | Metric | Condition | Severity | Runbook |
| ---- | ------ | --------- | -------- | ------- |
| `stellarindex_hashdb_drift_detected` | `stellarindex_hashdb_drift_total` | > 0 | ticket | [hashdb](runbooks/hashdb.md#stellarindex_hashdb_drift_detected) |
| `stellarindex_hashdb_verify_failing` | `rate(stellarindex_hashdb_verify_runs_total{window="recent",outcome="error"}[6h]) > rate(...{window="recent",outcome=~"ok\|drift"}[6h])` | sustained 30 min | ticket | [hashdb](runbooks/hashdb.md#stellarindex_hashdb_verify_failing) |
| `stellarindex_hashdb_verify_stale` | `increase(hashdb_verify_runs_total[6h]) == 0` while appends continue | 30 min | ticket | [hashdb](runbooks/hashdb.md#stellarindex_hashdb_verify_stale) |
| `stellarindex_hashdb_append_failing` | `rate(stellarindex_hashdb_append_total{outcome="error"}[15m]) > rate(...{outcome="ok"}[15m])` | sustained 10 min | ticket | [hashdb](runbooks/hashdb.md#stellarindex_hashdb_append_failing) |

## Data-freshness / completeness alerts

`data-freshness.sh` (every 15 min) writes per-domain freshness gauges and the per-source ADR-0033 completeness verdict to `data_freshness.prom`. Covers what the gap detector does not: reference oracles, FX, supply, issuer metadata, and the verdict itself.

| Name | Metric | Condition | Severity | Runbook |
| ---- | ------ | --------- | -------- | ------- |
| `stellarindex_data_source_stale` | `stellarindex_data_freshness_stale{domain,source}` | == 1 for > 1h | ticket | [data-source-stale](runbooks/data-freshness.md#stellarindex_data_source_stale) |
| `stellarindex_supply_assets_stale` | `supply_assets_stale` | > 0 for 2 h (per-asset frozen supply; the domain `supply` check cannot see it) | ticket | [supply-assets-stale](runbooks/data-freshness.md#stellarindex_supply_assets_stale) |
| `stellarindex_completeness_incomplete` | `stellarindex_completeness_incomplete{source}` | == 1 for > 1h | ticket | [completeness-incomplete](runbooks/data-freshness.md#stellarindex_completeness_incomplete) |
| `stellarindex_twap_history_missing` | `twap_history_missing{view}` or `cagg_history_missing{view}` | == 1 for 2 h. A CAGG recreated WITH NO DATA never got its manual refresh; newest-bar freshness reads green while history is empty | ticket | [twap-history-missing](runbooks/data-freshness.md#stellarindex_twap_history_missing) |
| `stellarindex_data_freshness_watchdog_silent` | `absent_over_time(stellarindex_data_freshness_stale[45m])` | for > 15m | ticket | [data-freshness-watchdog-silent](runbooks/data-freshness.md#stellarindex_data_freshness_watchdog_silent) |
| `stellarindex_data_freshness_probe_frozen` | `time() - node_textfile_mtime_seconds{file="data_freshness.prom"}` | > 45 min for 15 min (`watchdog_silent` cannot see it) | ticket | [data-freshness-watchdog-silent](runbooks/data-freshness.md#stellarindex_data_freshness_probe_frozen) |
| `stellarindex_sep41_supply_freshness_absent` | `data_freshness_stale` present but `domain="sep41_supply"` missing | 1 h (`watchdog_silent` cannot see it) | ticket | [data-freshness-watchdog-silent](runbooks/data-freshness.md#stellarindex_sep41_supply_freshness_absent) |
| `stellarindex_serving_insert_frozen` | `time() - max(source_last_insert_unix)` | > 1800 s (no insert from any source) for 10 min | ticket | [data-source-stale](runbooks/data-freshness.md#stellarindex_serving_insert_frozen) |
| `stellarindex_serving_insert_absent` | `absent(stellarindex_source_last_insert_unix)` | series missing for 15 min | ticket | [data-source-stale](runbooks/data-freshness.md#stellarindex_serving_insert_absent) |
| `stellarindex_sep1_refresh_timeouts_high` | `sep1_refresh_failures{reason="timeout",reached="true"} / sep1_refresh_reached` | > 0.3 with >= 20 reached, 2h30m | ticket | [sep1-refresh](runbooks/sep1-refresh.md#stellarindex_sep1_refresh_timeouts_high) |
| `stellarindex_sep1_refresh_failing` | `sep1_refresh_regressed / sep1_refresh_reached` | > 0.3 with >= 20 reached, 2h30m, unless run flagged systemic | ticket | [sep1-refresh](runbooks/sep1-refresh.md#stellarindex_sep1_refresh_failing) |
| `stellarindex_sep1_refresh_stale` | `time() - sep1_refresh_last_run_unix` | > 3 h, or absent 6 h; 10 min | ticket | [sep1-refresh](runbooks/sep1-refresh.md#stellarindex_sep1_refresh_stale) |

## Ledgerstream tier alerts

Per [ADR-0027](../adr/0027-lcm-cache-tiering.md). R1 reads each LCM from the local `galexie-archive` bucket (hot), falling back on `NoSuchKey` to the AWS public bucket (cold). While `storage.s3_cold_bucket_archive` is empty the cold path never runs and these stay silent.

| Name | Metric | Condition | Severity | Runbook |
| ---- | ------ | --------- | -------- | ------- |
| `stellarindex_ledgerstream_tier_both_missing` | `increase(ledgerstream_tier_read_total{outcome="both_missing"}[5m])` | > 0, `for: 0m` | page | [ledgerstream-tier-both-missing](runbooks/ingestion.md#stellarindex_ledgerstream_tier_both_missing) |

`both_missing` is the cold-tier failure ADR-0027 plans for; the runbook walks rehydrate-from-peer and disable-trim-timer.

## verify-archive timer alerts

Per ADR-0016. Tier A (chain-link integrity) runs nightly 03:23 UTC and Tier B (checkpoint anchor against `/srv/history-archive`, catches corruption that is still chain-link-consistent) nightly 04:37 UTC on R1; R2 + R3 trust R1 on a slower cadence. Alerts read systemd unit state via node_exporter `--collector.systemd`: [tier-a](https://github.com/Stellar-Index/StellarIndex/blob/main/deploy/systemd/verify-archive-tier-a.timer), [tier-b](https://github.com/Stellar-Index/StellarIndex/blob/main/deploy/systemd/verify-archive-tier-b.timer).

| Name | Metric | Condition | Severity | Runbook |
| ---- | ------ | --------- | -------- | ------- |
| `stellarindex_verify_archive_unit_failed` | `node_systemd_unit_state{name="verify-archive-tier-a.service",state="failed"}` | == 1 for > 5 min | ticket | [verify-archive-unit-failed](runbooks/verify-archive.md#stellarindex_verify_archive_unit_failed) |
| `stellarindex_verify_archive_run_stale` | `node_systemd_timer_last_trigger_seconds{name="verify-archive-tier-a.timer"}` age | > 36 h, 10 min | page | [verify-archive-run-stale](runbooks/verify-archive.md#stellarindex_verify_archive_run_stale) |
| `stellarindex_verify_archive_tier_b_unit_failed` | `node_systemd_unit_state{name="verify-archive-tier-b.service",state="failed"}` | == 1 for > 5 min | ticket | [verify-archive-tier-b](runbooks/verify-archive.md#stellarindex_verify_archive_tier_b_unit_failed) |
| `stellarindex_verify_archive_tier_b_run_stale` | same, `verify-archive-tier-b.timer` | > 36 h, 10 min | ticket | [verify-archive-tier-b](runbooks/verify-archive.md#stellarindex_verify_archive_tier_b_run_stale) |
| `stellarindex_verify_archive_tier_d_run_stale` | `time() - verify_archive_last_success_unix{tier="peers"}`, or absent | > 8 d, 30 min | ticket | [verify-archive-tier-d](runbooks/verify-archive.md#stellarindex_verify_archive_tier_d_run_stale) |

Tier E (`stellar-archivist scan --verify`) has no alert: it is operator-run only ([galexie-backfill.md](galexie-backfill.md) §Tier E).

## Anomaly + freeze alerts

Per [ADR-0019](../adr/0019-anomaly-response-and-confidence-scoring.md). A freeze fires only when `confidence < 0.45 AND z_score > 5σ AND source_count <= 1`, the corner where multi-source consensus cannot help. The runbook walks review and override.

| Name | Metric | Condition | Severity | Runbook |
| ---- | ------ | --------- | -------- | ------- |
| `stellarindex_anomaly_warn_rate` | `anomaly_warn_total` per class | rate > 0 for 15 min | ticket | [anomaly-freeze-engaged](runbooks/anomaly.md#stellarindex_anomaly_warn_rate) |
| `stellarindex_anomaly_freeze_engaged` | `anomaly_freeze_engaged_total` per class | rate > 0 over 5 min | ticket | [anomaly-freeze-engaged](runbooks/anomaly.md#stellarindex_anomaly_freeze_engaged) |
| `stellarindex_anomaly_freeze_recovery_stalled` | `anomaly_freeze_active` + non-ok `recovery_sweeps_total` | freeze active 2 h+ and sweep errors in 15 min | ticket | [freeze-recovery-stalled](runbooks/anomaly.md#stellarindex_anomaly_freeze_recovery_stalled) |
| `stellarindex_amm_self_pair_swap_burst` | `amm_self_pair_swap_total` per source | increase > 10 in 15 min, 2 min (token_in==token_out: Blend/Comet exploit primitive; normally zero) | ticket | [amm-self-pair-swap-burst](runbooks/anomaly.md#stellarindex_amm_self_pair_swap_burst) |

Alerts on how freezes END. A freeze holds a minimum duration, extends 30 min at each expiry it has not earned auto-unfreeze, and after 4 extensions escalates to operator review and never auto-unfreezes. Rules: [`deploy/monitoring/rules/freeze-lifecycle.yml`](../../deploy/monitoring/rules/freeze-lifecycle.yml).

| Name | Metric | Condition | Severity | Runbook |
| ---- | ------ | --------- | -------- | ------- |
| `stellarindex_aggregator_composite_freeze_suppression_dominant` | `composite_freeze_suppressed_total` vs `anomaly_freeze_engaged_total` | suppression > 2x engaged over 1 h, 30 min | ticket | [anomaly-freeze-engaged](runbooks/anomaly.md#stellarindex_anomaly_freeze_engaged) |
| `stellarindex_aggregator_composite_corroboration_unavailable` | `composite_corroboration{verdict="unavailable"}` | == 1 for 6 h per (pair, window); nothing can be suppressed | ticket | [anomaly-freeze-engaged](runbooks/anomaly.md#stellarindex_anomaly_freeze_engaged) |
| `stellarindex_anomaly_freeze_escalated` | `anomaly_freeze_escalated_total` | increase > 0 in 15 min (4-extension ladder exhausted; will NOT auto-unfreeze) | page | [freeze-lifecycle](runbooks/freeze-lifecycle.md#stellarindex_anomaly_freeze_escalated) |
| `stellarindex_anomaly_freeze_extension_rate` | `anomaly_freeze_extensions_total` | increase >= 3 in 1 h, 10 min | ticket | [freeze-lifecycle](runbooks/freeze-lifecycle.md#stellarindex_anomaly_freeze_extension_rate) |
| `stellarindex_anomaly_freeze_ladder_write_failures` | `anomaly_freeze_ladder_write_failures_total` per op | increase > 0 in 15 min | ticket | [freeze-lifecycle](runbooks/freeze-lifecycle.md#stellarindex_anomaly_freeze_ladder_write_failures) |
| `stellarindex_api_freeze_lookup_failing` | `api_freeze_lookup_failures_total` | increase > 0 in 10 min, 5 min (prices served `frozen_checked=false`) | ticket | [freeze-lifecycle](runbooks/freeze-lifecycle.md#stellarindex_api_freeze_lookup_failing) |
| `stellarindex_anomaly_freeze_operator_unfreeze_rate` | `anomaly_freeze_released_total{mode="operator"}` | increase >= 3 in 1 h, 10 min | ticket | [freeze-lifecycle](runbooks/freeze-lifecycle.md#stellarindex_anomaly_freeze_operator_unfreeze_rate) |
| `stellarindex_anomaly_freeze_active` | `anomaly_freeze_active` | > 0 for 5 min (informational) | informational | [anomaly-freeze-engaged](runbooks/anomaly.md#stellarindex_anomaly_freeze_engaged) |

## Divergence / quality alerts

| Name | Metric | Condition | Severity | Runbook |
| ---- | ------ | --------- | -------- | ------- |
| `stellarindex_price_divergence_warning` | `divergence_max_abs_fraction` | > 5 % for 10 min | informational | [price-divergence](runbooks/divergence.md#stellarindex_price_divergence_warning) |
| `stellarindex_price_divergence_critical` | same | > 10 % for 10 min | ticket | [price-divergence](runbooks/divergence.md#stellarindex_price_divergence_critical) |
| `stellarindex_oracle_stream_rows_unparsed` | `increase(oracle_stream_rows_unparsed_total[6h])`, or counter born in window | > 0 for 10 min | ticket | [oracle-stream-rows-unparsed](runbooks/divergence.md#stellarindex_oracle_stream_rows_unparsed) |
| `stellarindex_oracle_stale` | `time() - oracle_last_update_unix` per (source, asset) | > `oracle_staleness_budget_seconds` (10x resolution; `[[oracle.staleness_overrides]]`) | ticket | [oracle-stale](runbooks/divergence.md#stellarindex_oracle_stale) |
| `stellarindex_divergence_refresh_error_dominant` | `rate(divergence_refresh_total{outcome="refresh_error"}[5m]) > rate(...{outcome="ok"}[5m])` | sustained 30 min | ticket | [divergence-refresh-error-dominant](runbooks/divergence.md#stellarindex_divergence_refresh_error_dominant) |
| `stellarindex_divergence_no_reference` | `rate(divergence_refresh_total{outcome="no_reference"}[5m]) > rate(...{outcome="ok"}[5m])` | sustained 30 min | ticket | [divergence-no-reference](runbooks/divergence.md#stellarindex_divergence_no_reference) |
| `stellarindex_divergence_reference_failing` | failing-outcome share of `divergence_reference_total` per reference | > 0.5 for 30 min | ticket | [divergence-no-reference](runbooks/divergence.md#stellarindex_divergence_reference_failing) |
| `stellarindex_divergence_pair_below_quorum` | `divergence_pair_quorum_met` max over 30 min == 0 | 30 min (warning carried forward, not evaluated) | ticket | [divergence-no-reference](runbooks/divergence.md#stellarindex_divergence_pair_below_quorum) |
| `stellarindex_divergence_no_ok_outcomes` | `increase(divergence_refresh_total{outcome="ok"}[30m]) == 0` with refresher wired | 15 min | ticket | [divergence-no-ok-outcomes](runbooks/divergence.md#stellarindex_divergence_no_ok_outcomes) |

## Aggregator alerts

| Name | Metric | Condition | Severity | Runbook |
| ---- | ------ | --------- | -------- | ------- |
| `stellarindex_aggregator_silent` | `rate(stellarindex_aggregator_vwap_writes_total[5m])` | == 0 for > 5 min | page | [aggregator-silent](runbooks/aggregator.md#stellarindex_aggregator_silent) |
| `stellarindex_aggregator_outlier_storm` | max/min `aggregator_venue_vwap{window="5m"}` - 1, >= 2 venues | > 1% for 15 min | ticket | [aggregator-outlier-storm](runbooks/aggregator.md#stellarindex_aggregator_outlier_storm) |
| `stellarindex_aggregator_outlier_trim_fraction` | share of 24h trades trimmed at outlier stage, >= 20 trades | > 0.2 for 30 min | ticket | [aggregator-outlier-storm](runbooks/aggregator.md#stellarindex_aggregator_outlier_trim_fraction) |
| `stellarindex_aggregator_outlier_volume_trim_fraction` | share of base volume trimmed at outlier stage, every window | > 0.2 for 15 min | ticket | [aggregator-outlier-storm](runbooks/aggregator.md#stellarindex_aggregator_outlier_volume_trim_fraction) |
| `stellarindex_aggregator_class_drop_spike` | `rate(aggregator_dropped_trades_total{reason="class"}[10m])` | > 10x baseline (offset 1h) for 15 min | ticket | [aggregator-class-drop-spike](runbooks/aggregator.md#stellarindex_aggregator_class_drop_spike) |
| `stellarindex_aggregator_fx_snap_fallback_dominant` | `fx_snap_fallback_total` / ok `triangulations_total` | > 0.5 for 30 min | ticket | [aggregator-fx-snap-fallback-dominant](runbooks/aggregator.md#stellarindex_aggregator_fx_snap_fallback_dominant) |
| `stellarindex_aggregator_triangulation_chains_dry` | `missing_leg` triangulations > 0 and `ok` == 0 | 30 min | ticket | [aggregator-triangulation-chains-dry](runbooks/aggregator.md#stellarindex_aggregator_triangulation_chains_dry) |
| `stellarindex_aggregator_bootstrap_cap_reengaged` | pairs with `bootstrap_capped` flipping 0 to 1 | >= 3 pairs in 1 h, 10 min (one pair alone stays silent) | ticket | [aggregator-bootstrap-cap-reengaged](runbooks/aggregator.md#stellarindex_aggregator_bootstrap_cap_reengaged) |
| `stellarindex_aggregator_cache_write_errors` | `rate(stellarindex_aggregator_vwap_cache_write_errors_total[5m])` | > 0 for ≥ 2 min | page | [redis-write-blocked-disk-full](runbooks/aggregator.md#stellarindex_aggregator_cache_write_errors) |
| `stellarindex_customer_webhook_delivery_failing` | `rate(stellarindex_customer_webhook_delivery_attempts_total{outcome=~"server_error\|network_error"}[5m])` | > 0.05/s for ≥ 15 min | ticket | [customer-webhook-delivery-failing](runbooks/api.md#stellarindex_customer_webhook_delivery_failing) |
| `stellarindex_customer_webhook_delivery_exhausted` | `customer_webhook_delivery_attempts_total{outcome="exhausted"}` | > 0, `for: 0m` | ticket | [customer-webhook-delivery-failing](runbooks/api.md#stellarindex_customer_webhook_delivery_exhausted) |
| `stellarindex_customer_webhook_mark_errors` | `increase(...{outcome="mark_error"}[30m])` | > 0 for 15 min (POST done, result not saved; re-POSTs each lease) | ticket | [customer-webhook-delivery-failing](runbooks/api.md#stellarindex_customer_webhook_mark_errors) |
| `stellarindex_customer_webhook_fanout_failing` | `increase(customer_webhook_fanout_failures_total[1h])` per (event_type, reason) | > 0 for 5 min (no delivery row; no retry) | ticket | [customer-webhook-fanout-failing](runbooks/aggregator.md#stellarindex_customer_webhook_fanout_failing) |
| `stellarindex_usage_rollup_failing` | `rate(stellarindex_usage_rollup_sweeps_total{outcome=~"scan_error\|sink_error"}[15m])` | > 0 for ≥ 30 min | ticket | [usage-rollup-failing](runbooks/api.md#stellarindex_usage_rollup_failing) |
| `stellarindex_dex_tvl_refresh_failing` | `dex_tvl_refresh_total` errors > 0 and ok == 0 | 30 min (served TVL silently aging) | ticket | [dex-tvl-refresh-failing](runbooks/api.md#stellarindex_dex_tvl_refresh_failing) |
| `stellarindex_dex_tvl_total_divergent` | `dex_tvl_reconcile_total{outcome="divergent"}` > 0 and ok == 0 | 1 h (headline `tvl_total` wrong or absent) | ticket | [dex-tvl-total-divergent](runbooks/api.md#stellarindex_dex_tvl_total_divergent) |
| `stellarindex_sdex_orderbook_maintain_failing` | `rate(sdex_orderbook_maintain_total{outcome=~"load_error\|advance_error\|verify_error"}[15m])` | > 0 for 30 min (load: warming stuck; advance: drifting from tip; verify: offers withheld) | ticket | [sdex-orderbook-maintain-failing](runbooks/api.md#stellarindex_sdex_orderbook_maintain_failing) |
| `stellarindex_sdex_orderbook_advance_held` | `maintain_total{outcome="advance_held"}` rate | > 0 for 30 min (cursor held below a lake hole or ingest halt; `maintain_failing` stays silent) | ticket | [sdex-orderbook-maintain-failing](runbooks/api.md#stellarindex_sdex_orderbook_advance_held) |
| `stellarindex_sdex_orderbook_reload_failing` | `increase(maintain_total{outcome="load_error"}[3h])` with no `load_ok` | > 0 for 2 h (daily re-load failing; old book serves) | ticket | [sdex-orderbook-maintain-failing](runbooks/api.md#stellarindex_sdex_orderbook_reload_failing) |
| `stellarindex_sdex_orderbook_crossed_book` | `max(sdex_orderbook_crossed_pairs)` | > 0 for 30 min (best bid > best ask: phantom offers) | ticket | [sdex-orderbook-maintain-failing](runbooks/api.md#stellarindex_sdex_orderbook_crossed_book) |
| `stellarindex_ch_schema_probe_absent` | `ch_schema_probe_present` per (instance, probe) | == 0 for 30 min (an "absent" answer latches until restart; surface stays on fallback) | ticket | [ch-schema-probe-absent](runbooks/api.md#stellarindex_ch_schema_probe_absent) |
| `stellarindex_explorer_schema_absent_empty` | `increase(explorer_schema_absent_empty_total[1h])` per table | > 0, any occurrence: a read served empty because its table is missing | ticket | [explorer-schema-absent-empty](runbooks/api.md#stellarindex_explorer_schema_absent_empty) |
| `stellarindex_explorer_exact_fallback_sustained` | `rate(explorer_exact_fallback_total[30m])` per read | > 0.05/s for 1h: page reads re-run as the exact O(history) query | ticket | [explorer-exact-fallback](runbooks/api.md#stellarindex_explorer_exact_fallback_sustained) |
| `stellarindex_derived_index_watermark_missing` | `min(derived_index_watermark_ledger)` per index | == 0 for 30 min: no completeness watermark, gated reads serve their fallback | ticket | [derived-index-watermark-missing](runbooks/api.md#stellarindex_derived_index_watermark_missing) |
| `stellarindex_protocol_events_rollup_failing` | `rate(stellarindex_protocol_events_rollup_sweeps_total{outcome="refresh_error"}[15m])` | > 0 for ≥ 30 min | ticket | [protocol-events-rollup-failing](runbooks/aggregator.md#stellarindex_protocol_events_rollup_failing) |
| `stellarindex_asset_volume_rollup_failing` | `rate(stellarindex_asset_volume_rollup_sweeps_total{outcome="refresh_error"}[15m])` | > 0 for ≥ 30 min | ticket | [asset-volume-rollup-failing](runbooks/aggregator.md#stellarindex_asset_volume_rollup_failing) |
| `stellarindex_asset_character_rollup_failing` | `asset_character_rollup_sweeps_total{outcome="refresh_error"}` over 13 h with no `ok` sweep | 5 min | ticket | [asset-character-rollup-failing](runbooks/aggregator.md#stellarindex_asset_character_rollup_failing) |
| `stellarindex_nonstandard_decimals_correction_failing` | `increase()[15m]` of `nonstandard_decimals_cache_refresh_failures_total`, `price_serve_declined_nonstandard_decimals_total`, `nonstandard_decimals_lockstep_mismatch_total` | > 0 for 5 min. Lockstep per `{site, asset}`: `guard_reconcile` = row repaired against the lake; `asset_detail` = market cap refused, resolvers disagree | ticket | [dex-nonstandard-decimals](runbooks/aggregator.md#stellarindex_nonstandard_decimals_correction_failing) |
| `stellarindex_decimals_guard_sweep_stale` | `time() - decimals_guard_sweep_last_success_unix{job="stellarindex_aggregator"}` > 2700, gauge > 0 | 15 min (sweep wedged). Lake-less aggregator holds 0 and stays silent. Offender counters above are untrustworthy while this fires | ticket | [dex-nonstandard-decimals](runbooks/aggregator.md#stellarindex_decimals_guard_sweep_stale) |
| `stellarindex_change_summary_stale` | `time() - change_summary_last_success_unix{job="stellarindex_aggregator"}` > 1800, gauge > 0 | 15 min (`change_summary_5m` frozen; explorer deltas stale) | ticket | [change-summary-stale](runbooks/aggregator.md#stellarindex_change_summary_stale) |
| `stellarindex_price_alert_eval_failing` | `rate(stellarindex_price_alert_eval_total{outcome="list_error"}[5m]) > rate(...{outcome="ok"}[5m])` | sustained 30 min | ticket | [price-alert-eval-failing](runbooks/price-alert-eval-failing.md) |
| `stellarindex_price_alert_eval_no_successful_sweeps` | `ok` sweep rate == 0 and `partial_error` rate > 0 | 30 min (`list_error` cannot see this) | ticket | [price-alert-eval-failing](runbooks/price-alert-eval-failing.md) |
| `stellarindex_price_alert_evaluations_failing` | error+timeout share of `price_alert_evaluated_total[15m]` | > 0.5 for 30 min (`partial_error` cannot tell one bad alert from all) | ticket | [price-alert-eval-failing](runbooks/price-alert-eval-failing.md) |
| `stellarindex_price_alert_sweep_stale` | `time() - price_alert_last_sweep_unix` > 900, gauge > 0 | 5 min (a wedged sweep emits no outcome) | ticket | [price-alert-eval-failing](runbooks/price-alert-eval-failing.md) |
| `stellarindex_price_alert_eval_never_initialized` | `absent_over_time(price_alert_eval_total{outcome="ok"}[36h])` | 5 min (disabled evaluator is invisible to the `rate()` rules) | ticket | [price-alert-eval-failing](runbooks/price-alert-eval-failing.md) |
| `stellarindex_assets_popular_priceless` | `assets_popular_priceless` (popular, priceless, non-withheld; single-account wash excluded) | > 0 for 1 h | ticket | [assets-popular-priceless](runbooks/data-freshness.md#stellarindex_assets_popular_priceless) |
| `stellarindex_priceless_coverage_check_stale` | `time() - priceless_coverage_check_last_success_unix{job="stellarindex_aggregator"}` | > 1800 s, 30 min (tripwire wedged) | ticket | [assets-popular-priceless](runbooks/data-freshness.md#stellarindex_assets_popular_priceless) |
| `stellarindex_priceless_coverage_check_errors` | `increase(priceless_coverage_check_runs_total{outcome="error"}[15m])` | > 0 for 15 min | ticket | [assets-popular-priceless](runbooks/data-freshness.md#stellarindex_assets_popular_priceless) |
| `stellarindex_api_cors_wildcard_in_prod` | `increase(api_cors_decisions_total{outcome="allowed_wildcard"}[15m])` | > 0 for 5 min (wildcard CORS matching real traffic on r1) | ticket | [api-cors-wildcard](runbooks/api-security.md#stellarindex_api_cors_wildcard_in_prod) |
| `stellarindex_cross_region_divergence` | `rate(cross_region_divergences_total[15m])` per (pair, metric) | > 0 for 5 min (regions disagree on a closed bucket) | ticket | [cross-region-divergence](runbooks/cross-region.md#stellarindex_cross_region_divergence) |
| `stellarindex_cross_region_fetch_errors` | `rate(cross_region_fetch_errors_total[15m])` per (region, pair, metric) | > 0 for 10 min (that region not compared) | ticket | [cross-region-divergence](runbooks/cross-region.md#stellarindex_cross_region_fetch_errors) |
| `stellarindex_cross_region_check_stale` | `time() - cross_region_last_run_timestamp_seconds` | > 300 s, 5 min | ticket | [cross-region-divergence](runbooks/cross-region.md#stellarindex_cross_region_check_stale) |
| `stellarindex_signup_reaper_failing` | `rate(stellarindex_signup_reaper_runs_total{outcome="error"}[6h]) > rate(...{outcome="ok"}[6h])` | sustained 30 min | ticket | [signup-reaper-failing](runbooks/signup-reaper-failing.md) |
| `stellarindex_ratelimit_fail_open` | `increase(ratelimit_fail_open_total[15m])` | > 100 in 15 min (Redis error; window capped at the 30 s dwell, so a rate form never fires) | ticket | [ratelimit-fail-open](runbooks/api.md#stellarindex_ratelimit_fail_open) |
| `stellarindex_ratelimit_fail_closed` | `rate(ratelimit_fail_closed_total[5m])` | > 0 for 2 min (past the dwell every request in the bucket gets 503) | page | [ratelimit-fail-open](runbooks/api.md#stellarindex_ratelimit_fail_closed) |
| `stellarindex_monthly_quota_fail_open` | `rate(monthly_quota_fail_open_total[5m])` | > 0 for 10 min (spend ceiling bypassed on counter read error) | ticket | [monthly-quota-fail-open](runbooks/api.md#stellarindex_monthly_quota_fail_open) |
| `stellarindex_monthly_quota_fail_closed` | `rate(monthly_quota_fail_closed_total[5m])` | > 0 for 2 min (every metered request gets 429 `monthly-quota-unavailable`) | page | [monthly-quota-fail-open](runbooks/api.md#stellarindex_monthly_quota_fail_closed) |
| `stellarindex_usage_write_failing` | `rate(usage_units_dropped_total{counter="billable"}[5m])` | > 0 for 10 min (units dropped silently; quota gate sees no error) | ticket | [usage-write-failing](runbooks/api.md#stellarindex_usage_write_failing) |
| `stellarindex_after_response_tasks_dropping` | `rate(after_response_tasks_dropped_total[5m])` | > 0 for 10 min (post-response worker queue full) | ticket | [after-response-tasks-dropping](runbooks/api.md#stellarindex_after_response_tasks_dropping) |
| `stellarindex_scam_gate_fail_open` | `rate(scam_gate_lookup_failures_total[5m])` per surface | > 0 for 5 min (flagged issuers priced after a directory lookup error) | ticket | [scam-gate-fail-open](runbooks/api.md#stellarindex_scam_gate_fail_open) |
| `stellarindex_directory_sync_unflagged` | `directory_sync_rows_changed{kind="unflagged"}` per (instance, source) | > 0 (last sync cleared a scam tag; issuer priced again); clears at next daily sync with none | ticket | [directory-sync-unflagged](runbooks/api.md#stellarindex_directory_sync_unflagged) |
| `stellarindex_admin_audit_write_failing` | `increase(admin_audit_write_failures_total[1h])` per surface | > 0 for 5 min (mutation committed with no audit row) | ticket | [admin-audit-write-failing](runbooks/api.md#stellarindex_admin_audit_write_failing) |
| `stellarindex_passkey_clone_warning` | `increase(passkey_login_refusals_total{reason="clone_warning"}[1h])` | > 0 for 5 min (sign-counter regression: credential key copied) | ticket | [passkey-clone-warning](runbooks/api.md#stellarindex_passkey_clone_warning) |
| `stellarindex_failed_auth_rate_high` | `rate(failed_auth_total{outcome="rejected"}[5m])` | > 1/s for 15 min (distributed guessing under per-IP caps) | ticket | [failed-auth-rate-high](runbooks/api.md#stellarindex_failed_auth_rate_high) |
| `stellarindex_magic_link_token_table_growing` | `stellarindex_magic_link_token_rows` | > 10000 for 30 min | ticket | [auth-reaper-stalled](runbooks/api.md#stellarindex_magic_link_token_table_growing) |
| `stellarindex_login_code_lockout_table_growing` | `login_code_lockout_rows`, `login_code_lockout_errors_total{op="status_check"}` | rows > 10000 or any fail-open, 30 min | ticket | [login-code-lockout-table-growing](runbooks/api.md#stellarindex_login_code_lockout_table_growing) |
| `stellarindex_auth_reaper_stalled` | `time() - auth_reaper_last_sweep_unix` per reaper | > 3x `auth_reaper_interval_seconds`, 15 min (rows gauge frozen) | ticket | [auth-reaper-stalled](runbooks/api.md#stellarindex_auth_reaper_stalled) |
| `stellarindex_tls_cert_expiring_soon` | `stellarindex_tls_cert_not_after_unix - time()` per host | < 14 days for ≥ 1 h | ticket | [tls-cert-expiring-soon](runbooks/api.md#stellarindex_tls_cert_expiring_soon) |

## Supply alerts

| Name | Metric | Condition | Severity | Runbook |
| ---- | ------ | --------- | -------- | ------- |
| `stellarindex_supply_cross_check_divergence` | `stellarindex_supply_cross_check_divergence_stroops` per `classic_key` | > 1 stroop for > 5 min | ticket | [supply-cross-check-divergence](runbooks/supply.md#stellarindex_supply_cross_check_divergence) |
| `stellarindex_supply_cross_check_unevaluable` | `increase(supply_cross_check_total{outcome=~"missing_snapshot\|read_error\|misaligned"}[30m])` | > 0 for 1 h (divergence alert cannot fire for it) | ticket | [supply-cross-check-unevaluable](runbooks/supply.md#stellarindex_supply_cross_check_unevaluable) |
| `stellarindex_supply_divergence_high` | `stellarindex_supply_divergence_ratio` per `asset` × `reference` | > 1% for ≥ 1 h | ticket | [supply-divergence](runbooks/supply.md#stellarindex_supply_divergence_high) |
| `stellarindex_supply_snapshot_unit_failed_alert` | `stellarindex_supply_snapshot_unit_failed` | > 0 for ≥ 30 min | ticket | [supply-snapshot-unit-failed](runbooks/supply.md#stellarindex_supply_snapshot_unit_failed_alert) |
| `stellarindex_supply_snapshot_stale` | `time() - stellarindex_supply_snapshot_last_success_timestamp` | > 36 h for ≥ 5 min | ticket | [supply-snapshot-stale](runbooks/supply.md#stellarindex_supply_snapshot_stale) |
| `stellarindex_supply_snapshot_critical_stale` | same | > 72 h for ≥ 5 min | page | [supply-snapshot-stale](runbooks/supply.md#stellarindex_supply_snapshot_critical_stale) |
| `stellarindex_supply_snapshot_never_initialized` | `absent_over_time(stellarindex_supply_snapshot_last_success_timestamp[36h])` | == 1 for ≥ 5 min | ticket | [supply-snapshot-never-initialized](runbooks/supply.md#stellarindex_supply_snapshot_never_initialized) |
| `stellarindex_supply_snapshot_circulating_zero` | `stellarindex_supply_snapshot_circulating_xlm{asset_key="XLM"}` | ≤ 0 for ≥ 5 min | page | [supply-snapshot-circulating-zero](runbooks/supply.md#stellarindex_supply_snapshot_circulating_zero) |
| `stellarindex_supply_verify_rollup_unit_failed_alert` | `supply_verify_rollup_unit_failed` | > 0 for 30 min (drift, watched contract with no checkpoint, 0 checked, or error) | ticket | [supply-verify-rollup-unit-failed](runbooks/supply.md#stellarindex_supply_verify_rollup_unit_failed_alert) |
| `stellarindex_supply_verify_rollup_stale` | `time() - stellarindex_supply_verify_rollup_last_success_timestamp` | > 36 h for ≥ 5 min | ticket | [supply-verify-rollup-stale](runbooks/supply.md#stellarindex_supply_verify_rollup_stale) |
| `stellarindex_supply_verify_rollup_never_initialized` | `absent_over_time(stellarindex_supply_verify_rollup_last_success_timestamp[36h])` | == 1 for ≥ 5 min | ticket | [supply-verify-rollup-stale](runbooks/supply.md#stellarindex_supply_verify_rollup_stale) |
| `stellarindex_aggregator_supply_refresh_stalled` | `time() - max(timestamp(aggregator_supply_refresh_total{outcome="ok"}))` | > 30 min, 5 min | page | [supply-refresh-stalled](runbooks/supply.md#stellarindex_aggregator_supply_refresh_stalled) |
| `stellarindex_aggregator_supply_refresh_error_dominant` | error share of `aggregator_supply_refresh_total` | > 50% for 30 min | ticket | [supply-refresh-error-dominant](runbooks/supply.md#stellarindex_aggregator_supply_refresh_error_dominant) |
| `stellarindex_aggregator_supply_refresh_dormant_fleet` | assets with non-zero `outcome="dormant"` rate | > 1 for 30 min | ticket | [supply-refresh-error-dominant](runbooks/supply.md#stellarindex_aggregator_supply_refresh_dormant_fleet) |
| `stellarindex_aggregator_supply_refresh_never_initialized` | `absent_over_time(stellarindex_aggregator_supply_refresh_total{outcome="ok"}[36h])` | == 1 for ≥ 5 min | ticket | [supply-refresh](runbooks/supply.md#stellarindex_aggregator_supply_refresh_never_initialized) |
| `stellarindex_sep41_supply_rollup_no_cursor` | `increase(sep41_supply_rollup_advances_total{outcome="no_cursor"}[15m])` per contract | > 0 for 30 min | ticket | [sep41-supply-rollup-no-cursor](runbooks/supply.md#stellarindex_sep41_supply_rollup_no_cursor) |
| `stellarindex_ch_supply_gapfill_failed` | `node_systemd_unit_state{name="ch-supply.service",state="failed"}` | == 1 for ≥ 10 min | ticket | [ch-supply-gapfill-failed](runbooks/supply.md#stellarindex_ch_supply_gapfill_failed) |

## Infra / host alerts

| Name | Metric | Condition | Severity | Runbook |
| ---- | ------ | --------- | -------- | ------- |
| `stellarindex_host_down` | `up` for any host | == 0 for > 2 min | ticket | [infra](runbooks/infra.md#stellarindex_host_down) |
| `stellarindex_keepalived_textfile_stale` | `node_textfile_mtime_seconds{file="keepalived.prom"}` age | > 10 min, 5 min | ticket | [infra](runbooks/infra.md#stellarindex_keepalived_textfile_stale) |
| `stellarindex_host_cpu_high` | 100 - avg idle CPU `[5m]` | > 90 % for 10 min | informational | [infra](runbooks/infra.md#stellarindex_host_cpu_high) |
| `stellarindex_host_memory_high` | used / total memory | > 90 % for 10 min | informational | [infra](runbooks/infra.md#stellarindex_host_memory_high) |
| `stellarindex_zfs_pool_degraded` | `node_zfs_pool_state{state=~"DEGRADED|FAULTED|UNAVAIL"}` | any, for > 60 s | page | [infra](runbooks/infra.md#stellarindex_zfs_pool_degraded) |
| `stellarindex_zfs_pool_low_space` | `min(node_filesystem_avail_bytes{fstype="zfs"})` | < 1.3 TB for 15 min | ticket | [infra](runbooks/infra.md#stellarindex_zfs_pool_low_space) |
| `stellarindex_zfs_pool_critical_space` | same | < 650 GB for 5 min | page | [infra](runbooks/infra.md#stellarindex_zfs_pool_critical_space) |
| `stellarindex_zfs_pool_fill_85pct_within_30d` | `predict_linear` of 14 d daily avail troughs | < 15% free in 30 d, 1 h | ticket | [infra](runbooks/infra.md#stellarindex_zfs_pool_fill_85pct_within_30d) |
| `stellarindex_zfs_pool_fill_90pct_within_7d` | same | < 10 % free in 7 d, 30 min | page | [infra](runbooks/infra.md#stellarindex_zfs_pool_fill_90pct_within_7d) |
| `stellarindex_nvme_smart_warn` | `increase(nvme_num_err_log_entries_total[1h])` | > 0 for > 5 min | ticket | [infra](runbooks/infra.md#stellarindex_nvme_smart_warn) |
| `stellarindex_nvme_thermal_throttle` | `nvme_temperature_celsius` | > 70 °C for > 5 min | page | [infra](runbooks/infra.md#stellarindex_nvme_thermal_throttle) |
| `stellarindex_nvme_wear_high` | `nvme_percentage_used_ratio` | > 0.80 for > 1 h | ticket | [infra](runbooks/infra.md#stellarindex_nvme_wear_high) |
| `stellarindex_nvme_spare_low` | `nvme_available_spare_ratio` | < 0.20 for > 30 min | page | [infra](runbooks/infra.md#stellarindex_nvme_spare_low) |
| `stellarindex_nvme_media_errors` | `increase(nvme_media_errors_total[24h])` | > 0 for > 5 min | ticket | [infra](runbooks/infra.md#stellarindex_nvme_media_errors) |
| `stellarindex_md_array_degraded` | `node_md_disks{state="failed"}` (r1 OS/boot RAID1: md0 swap, md1 /) | > 0 for 5 min | page | [infra](runbooks/infra.md#stellarindex_md_array_degraded) |
| `stellarindex_filesystem_readonly` | `node_filesystem_readonly` (excl. tmpfs/squashfs/overlay/nsfs/ramfs) | == 1 for 5 min | page | [infra](runbooks/infra.md#stellarindex_filesystem_readonly) |
| `stellarindex_nvme_critical_warning` | `nvme_critical_warning` (SMART bitmask) | > 0 for 5 min | page | [infra](runbooks/infra.md#stellarindex_nvme_critical_warning) |
| `stellarindex_wal_archive_stale` | `pg_stat_archiver_last_archive_age` | > 600 s for 10 min | ticket | [infra](runbooks/infra.md#stellarindex_wal_archive_stale) |
| `stellarindex_process_mappings_high` | `process_memory_mappings_ratio` | > 0.25 of `vm.max_map_count`, 15 min | ticket | [memory-mappings](runbooks/infra.md#process-mappings) |
| `stellarindex_process_mappings_critical` | same | > 0.50, 2 min | page | [memory-mappings](runbooks/infra.md#process-mappings) |
| `stellarindex_process_mappings_exhaustion_projected` | `predict_linear(process_memory_mappings[30m], 3600)` vs limit, when ratio > 0.10 | hits limit within 1 h, 5 min | page | [memory-mappings](runbooks/infra.md#process-mappings) |
| `stellarindex_process_mappings_probe_degraded` | `process_memory_mappings_updated_unix`, `_unreadable`, `_procs` | stamp > 30 min, absent 45 min, unreadable == 1, or procs == 0; 10 min | ticket | [memory-mappings](runbooks/infra.md#process-mappings) |
| `stellarindex_pg_compressed_chunks_unreadable` | `pg_compressed_chunks_unreadable` (nightly decode of every compressed column) | > 0, `for: 0m` | page | [timescale](runbooks/timescale.md#stellarindex_pg_compressed_chunks_unreadable) |
| `stellarindex_pg_compressed_chunk_index_bloat` | `pg_compressed_chunks_index_bloated` (fully compressed chunks with > 1 GiB of indexes, nightly) | > 0, `for: 0m` | ticket | [timescale](runbooks/timescale.md#stellarindex_pg_compressed_chunk_index_bloat) |
| `stellarindex_pg_chunk_scan_stale` | `pg_chunk_scan_last_success_timestamp_seconds` | > 36 h or absent, 1 h | ticket | [timescale](runbooks/timescale.md#stellarindex_pg_compressed_chunks_unreadable) |
| `stellarindex_catchup_lane_failed` | `catchup_lane_failed{lane}` (`<lane>.failed` present) | == 1 for 10 min | page | [ops-job-stalled](runbooks/ops-job-stalled.md#stellarindex_catchup_lane_failed) |
| `stellarindex_catchup_lane_stalled` | `catchup_lane_queued`, `_last_event_timestamp_seconds` | work queued, no log line 24 h, lanes not stopped; 30 min | ticket | [ops-job-stalled](runbooks/ops-job-stalled.md#stellarindex_catchup_lane_failed) |
| `stellarindex_textfile_scrape_error` | `node_textfile_scrape_error` | == 1 for 15 min (unparsable `.prom`; its series and alerts go silent) | ticket | [infra](runbooks/infra.md#stellarindex_textfile_scrape_error) |

## Observability / meta alerts

| Name | Metric | Condition | Severity | Runbook |
| ---- | ------ | --------- | -------- | ------- |
| `stellarindex_prometheus_scrape_failing` | `up{job=~"api\|indexer\|aggregator"}` | == 0 for any target > 2 min | ticket | [scrape-failing](runbooks/meta.md#stellarindex_prometheus_scrape_failing) |
| `stellarindex_alertmanager_config_bad` | `alertmanager_config_last_reload_successful` | == 0 | ticket | [alertmanager-bad-config](runbooks/meta.md#stellarindex_alertmanager_config_bad) |
| `stellarindex_deadmansswitch` | `vector(1)` constant | MUST fire every minute | informational | [deadmansswitch](runbooks/meta.md#stellarindex_deadmansswitch) |
| `stellarindex_alertmanager_down` | `up{job="alertmanager"}` | == 0 for > 5 min OR series absent for 10 min | page | [alertmanager-down](runbooks/meta.md#stellarindex_alertmanager_down) |
| `stellarindex_alertmanager_not_notifying` | `alertmanager_notifications_total{integration="webhook"}` | no increase 15 min (deadman alone gives ~30/h) | page | [alertmanager-not-notifying](runbooks/meta.md#stellarindex_alertmanager_not_notifying) |
| `stellarindex_alertmanager_notifications_failing` | `alertmanager_notifications_failed_total` | increase > 0 over 15 min | ticket | [alertmanager-notifications-failing](runbooks/meta.md#stellarindex_alertmanager_notifications_failing) |
| `stellarindex_alertmanager_optional_receiver_dark` | `alertmanager_optional_receiver_dark{receiver}` | == 1 for 1 h (receiver installed with no URL) | ticket | [alertmanager-notifications-failing](runbooks/meta.md#stellarindex_alertmanager_optional_receiver_dark) |
| `prometheus_down` (TSDB corruption) | systemd `prometheus.service` failed | exit-code != 0; runs ad-hoc, not a rule | **P1** | [prometheus-tsdb-corruption](runbooks/prometheus-tsdb-corruption.md) |
| `stellarindex_redis_exporter_down` | `up{job="redis_exporter"}` | == 0 for > 2 min OR series absent for 5 min | page | [exporter-down](runbooks/meta.md#stellarindex_redis_exporter_down) |
| `stellarindex_postgres_exporter_down` | `up{job="postgres_exporter"}` | == 0 for > 2 min OR series absent for 5 min | page | [exporter-down](runbooks/meta.md#stellarindex_postgres_exporter_down) |
| `stellarindex_pgbackrest_exporter_down` | `up{job="pgbackrest_exporter"}` | == 0 for > 2 min OR series absent for 5 min | page | [exporter-down](runbooks/meta.md#stellarindex_pgbackrest_exporter_down) |
| `stellarindex_minio_exporter_down` | `up{job="minio"}` | == 0 for > 2 min OR series absent for 5 min | page | [exporter-down](runbooks/meta.md#stellarindex_minio_exporter_down) — or [minio-metrics-403](runbooks/infra.md#minio-metrics-403) if the cause is a 403 (bearer-token gap) |
| `stellarindex_metrics_registry_absent` | `metrics_registry_present{component}` | == 0 for 15 min (Registry-less: its metrics and alerts dead) | ticket | [metrics-registry-absent](runbooks/meta.md#stellarindex_metrics_registry_absent) |

The `*_exporter_down` rules exist because each exporter feeds an alert family that fails silently if the exporter dies first. If `stellarindex_minio_exporter_down` shows `HTTP status 403` in Prometheus `lastError`, start at [minio-metrics-403](runbooks/infra.md#minio-metrics-403).

`deadmansswitch` is inverse-logic: it fires every minute to a receiver that alarms when it STOPS (catches AlertManager-down and Prometheus-down).

`stellarindex_alertmanager_notifications_failing` is routed twice on purpose. By severity it goes to `chat-default`, the integration whose failure it reports, so an alertname route also sends it to `alert-delivery-failure` (a Healthchecks.io check separate from the deadman, `continue: true`). Provisioning: [alertmanager-notifications-failing](runbooks/meta.md#stellarindex_alertmanager_notifications_failing).

`prometheus_down` is not a Prometheus rule (Prometheus cannot alert on its own absence; `deadmansswitch` covers that). It is the disk-full / TSDB-corruption family, same root cause as [`cache.md#stellarindex_redis_writes_blocked`](runbooks/cache.md#stellarindex_redis_writes_blocked); the systemd unit's `Restart=on-abnormal` does not auto-recover from it.

---

## Informational alerts — delivery register

Every `severity: informational` rule needs a row here
(`scripts/ci/lint-alerts-catalog.py` fails on a missing or stale row, or a
Triage cell not opening with a token below). The question a row answers:
does this belong in the quiet channel and nowhere louder?

- `silent-correct`: quiet channel only. Unactionable, self-healing,
  unable to fire, routine, or its consequence has a `ticket`/`page` alert.
- `needs-delivery`: should reach a human with no delivered alert covering
  it. Promote to `ticket` and remove from this register.

Counts as of 2026-09-08: 11 rules — 11 `silent-correct`, 0 `needs-delivery`.

<!-- informational-register:begin -->

| Alert | Component | What a firing tells an operator | Triage |
| ----- | --------- | ------------------------------- | ------ |
| `stellarindex_anomaly_freeze_active` | aggregator | An ADR-0019 price freeze held 5 min; pairs serve last-known-good with `flags.frozen=true`. | `silent-correct` — a freeze inside its expected duration is a state; `stellarindex_anomaly_freeze_escalated` (page) and `stellarindex_anomaly_freeze_extension_rate` (ticket) cover the abnormal cases. |
| `stellarindex_archive_repair_source_degraded` | archive | One archive source failed over 10% of repair attempts in 25 h; fallback chain still covers it. | `silent-correct` — the consequence has a delivered escalation, `stellarindex_archive_files_missing`. |
| `stellarindex_deadmansswitch` | meta | Fires constantly; the alarm is when it STOPS. | `silent-correct` — not silent: the alertname route reaches Healthchecks.io ahead of the severity matchers and never touches `receiver: silent`. |
| `stellarindex_external_poller_error_rate_high` | external-poller | Over half of one external poller's polls errored in 15 min; successes still flow. | `silent-correct` — early-warning tier: full staleness fires `stellarindex_external_poller_stale` (ticket) within 30 min. |
| `stellarindex_host_cpu_high` | infra | One host above 90% CPU for 10 min: runaway process or undersized box. | `silent-correct` — expected during backfills; the consequences that matter are delivered (`stellarindex_host_down`, `stellarindex_systemd_unit_failed`, `stellarindex_worker_panicked`). |
| `stellarindex_host_memory_high` | infra | One host above 90% memory for 10 min; OOMKill risk (Postgres `shared_buffers` usual culprit). | `silent-correct` — an OOMKill surfaces as `stellarindex_systemd_unit_failed` or `stellarindex_worker_panicked`, both delivered. |
| `stellarindex_ingestion_discovery_drops` | ingestion | The SEP-41 discovery sink dropped hits for 10 min (recorder pressure or full buffer). | `silent-correct` — self-healing: a dropped contract re-appears on its next event; the hard-failure twin is `stellarindex_ingestion_discovery_record_failures`. |
| `stellarindex_ingestion_oracle_unknown_symbols` | ingestion | An indexed oracle published a symbol or feed_id with no canonical mapping; kept under `raw:<symbol>`, needs a mapping and a replay. | `silent-correct` — a source listing a new token is routine and loses no data. Contrast `oracle_unrepresentable_symbols` (ticket): that slot is dropped with no row written. |
| `stellarindex_price_divergence_warning` | divergence | Aggregated price over 5% from the reference for 10 min. | `silent-correct` — two-tier by design: `stellarindex_price_divergence_critical` (ticket, 10%) is delivered. |
| `stellarindex_stellar_archive_publish_fail` | stellar | stellar-core failed to publish a checkpoint to our history archive. | `silent-correct` — cannot fire: nothing emits `stellarindex_stellar_archive_publish_errors_total` and r1 publishes no history archive. Re-triage when the Phase-3 emitter lands (ADR-0004). |
| `stellarindex_timescale_compression_lag` | storage | Chunks uncompressed 24 h past `compress_after` plus a `schedule_interval`: policy or scheduler misfiring. | `silent-correct` — the consequence is delivered (`stellarindex_zfs_pool_low_space` ticket, `stellarindex_zfs_pool_critical_space` page) and the root cause is `stellarindex_timescale_job_failures_climbing`. If that one goes silent, this one must not. |

<!-- informational-register:end -->

---

## Rules of thumb

- **Every alert has a runbook.** CI enforces it.
- **A page must be actionable.** Otherwise make it a `ticket`; never
  `informational` unless nobody needs to see it.
- **`for:` is mandatory**, so a self-resolving blip does not fire.
- **No duplicate alerts** on one root cause; oncall must not be paged twice.
- **Every alert should have a promtool test** in
  `deploy/monitoring/rule-tests/<area>_test.yml` (`make monitoring-check`).
  No gate requires one yet.

---

## Adding an alert

All in one PR:

1. Define the metric (`internal/obs/*.go`); add it to
   `docs/reference/metrics/README.md` (generated).
2. Write the rule in `deploy/monitoring/rules/<area>.yml`.
3. Write the runbook at `docs/operations/runbooks/<name>.md` (copy `_template.md`).
4. Add a row to this catalogue.
5. Write a firing test in `deploy/monitoring/rule-tests/<area>_test.yml`.

`scripts/ci/lint-docs.sh` §9 checks every `runbook_url` resolves.

---

## References

- [sev-playbook.md](sev-playbook.md): response timelines each severity binds to.
- [runbooks/](runbooks/): per-alert response steps.
- [runbooks/entry-walk-renumbering.md](runbooks/entry-walk-renumbering.md):
  NOT an alert. Deploy-time procedure when `dispatcher.EntryWalkVersion`
  is bumped; a skipped repair shows as a widening `stellarindex_supply_divergence`.
- External:
  - Prometheus best practices: <https://prometheus.io/docs/practices/alerting/>
  - The "USE method": utilisation / saturation / errors.
