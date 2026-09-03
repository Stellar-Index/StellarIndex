---
title: Alerts Catalogue
last_verified: 2026-09-02
status: ratified — incremental growth
---

# Alerts Catalogue

**Ratified:** 2026-04-22 (table shape); entries grow with each
feature PR per repo-hygiene-plan.md §16 ("no alert without a
runbook").

Every row is a Prometheus / AlertManager rule. The `Runbook` column
links to `docs/operations/runbooks/<name>.md`; a missing runbook
fails `scripts/ci/lint-docs.sh` section 9 (runbook-url check,
enforced 2026-04-23 onward).

**Shape of each alert:**

- **Name** — `stellarindex_<area>_<specific>`. Stable — referenced
  from AlertManager routing + the runbook filename.
- **Metric** — the Prometheus expression that triggers.
- **Condition** — the threshold + duration (`for:`).
- **Severity** — the rule's own `labels.severity`, which is what
  AlertManager routes on. Exactly three values exist:

  | Severity | Rules | AlertManager route | Delivery |
  | --- | --- | --- | --- |
  | `page` | 50 | `receiver: chat-page` | Discord **#stellarindex-pages**, `repeat_interval` 12 h. There is **no** PagerDuty leg — `pagerduty_configs` is unset, so nothing wakes anyone up. |
  | `ticket` | 134 | `receiver: chat-default` | Discord **#stellarindex-alerts**, `repeat_interval` 24 h. |
  | `informational` | 21 | `receiver: silent` | **Delivered to nobody, deliberately.** `silent` is declared with no `*_configs` block at all, which in Alertmanager means the alert is accepted and then dropped. It accumulates in the AlertManager UI and nothing else happens. |

  **`informational` is not "a low-priority ticket".** There is no
  low-priority queue and nothing files a ticket: an `informational`
  alert reaches a human only if that human independently opens the
  AlertManager UI. Read the Delivery column literally — the empty
  `silent` stanza in
  [`configs/alertmanager/alertmanager.r1.yml`](../../configs/alertmanager/alertmanager.r1.yml)
  is a deliberate black hole, not an unfinished config, and there is
  no `pagerduty_configs` anywhere in the file for any severity. Which
  of the 21 rules *should* be delivered is an open policy question
  (issue #485); the per-alert triage that feeds that decision is the
  [informational-alerts delivery register](#informational-alerts--delivery-register)
  below, and `scripts/ci/lint-alerts-catalog.py` requires a register
  row for every `informational` rule so a new one cannot join the
  bucket unnoticed.

  (`stellarindex_deadmansswitch` is the one exception: it carries
  `severity: informational` but is routed by ALERTNAME to
  Healthchecks.io ahead of the severity matchers, with
  `continue: false`, so it never reaches `silent` — see its runbook.)
  A `page` inhibits the `ticket`/`informational` alerts sharing its
  `component` label. Routing:
  [`configs/alertmanager/alertmanager.r1.yml`](../../configs/alertmanager/alertmanager.r1.yml).
- **Runbook** — what the responder does (link).

> ⚠️ **The Severity column used to read `P1`/`P2`/`P3` and claim those
> mapped to SEV-1/2/3 in [sev-playbook.md](sev-playbook.md).** No rule
> has ever carried a `P*` severity label, so the column was not the
> routing key and could not be checked against anything: 190 of the 203
> rows disagreed with the rule they described. The worst class was
> `P3`, which reads as "low but escalating" and was in fact
> `informational` — **routed to a receiver with no delivery** — for 15
> alerts including `ingestion_orphan_events` and `ingestion_decode_error`.
> The column is now generated from the rule files (2026-09-02) and
> `scripts/ci/lint-alerts-catalog.py` fails CI if it drifts again. SEV
> classification for an INCIDENT (which is a human judgement about
> customer impact) still lives in the playbook and is not the same axis.

---

## Ingestion alerts

| Name | Metric | Condition | Severity | Runbook |
| ---- | ------ | --------- | -------- | ------- |
| `stellarindex_ingestion_source_stopped` | `rate(stellarindex_source_events_total[30m])` per high-volume source | == 0 for > 15 min on an enabled source | ticket | [source-stopped](runbooks/source-stopped.md) |
| `stellarindex_ingestion_source_stopped_low_volume_dex` | `rate(stellarindex_source_events_total[6h])` for comet / phoenix / soroswap / blend | == 0 for > 30 min | ticket | [source-stopped](runbooks/source-stopped.md) |
| `stellarindex_ingestion_source_stopped_daily_publisher` | `rate(stellarindex_source_events_total[30h])` for ecb / band | == 0 for > 1 h | ticket | [source-stopped](runbooks/source-stopped.md) |
| `stellarindex_ingestion_all_sources_stopped` | `sum(rate(stellarindex_source_events_total[5m]))` | == 0 for > 3 min | page | [all-ingestion-down](runbooks/all-ingestion-down.md) |
| `stellarindex_ingestion_trade_buffer_drop` | `increase(stellarindex_source_insert_errors_total{kind="dropped"}[15m])` | > 0 for 15 min (any trade permanently dropped after the retry buffer overflowed) | ticket | [trade-insert-backpressure](runbooks/trade-insert-backpressure.md) |
| `stellarindex_ingestion_ledger_stalled` | `max_over_time(stellarindex_cursor_last_ledger{source="ledgerstream"}[5m]) - min_over_time(...)`, or `absent_over_time(...)` | == 0 (or series gone) for > 5 min ⇒ ~10 min without a committed ledger. The CEX/FX connectors share the indexer binary, so `all_sources_stopped` stays quiet through a lake outage; `cursor_stuck` cannot fire for this cursor (it joins `source_enabled`, and `ledgerstream` is not a configured source). | page | [ledger-ingest-stalled](runbooks/ledger-ingest-stalled.md) |
| `stellarindex_ingestion_cursor_stuck` | `increase(stellarindex_cursor_last_ledger[5m])` per source | == 0 while source is live | ticket | [cursor-stuck](runbooks/cursor-stuck.md) |
| `stellarindex_ingestion_orphan_events` | `rate(stellarindex_source_orphan_events_total[10m])` | > 10/min per source | informational | [orphan-events](runbooks/orphan-events.md) |
| `stellarindex_ingestion_decode_error` | `rate(stellarindex_source_decode_errors_total[5m])` | > 1/s sustained 5 min | informational | [decode-errors](runbooks/decode-errors.md) |
| `stellarindex_decoder_panicked` | `stellarindex_decoder_panics_total` | > 0 — a decoder's Matches/Decode panicked; the dispatcher skipped that one input and ingest continued, but the decoder keeps dropping every event of that shape until a fixed binary ships (#371 F1). Raw value, not `increase()`: a one-off panic creates a series born at 1 that `increase()` can never see. | page | [decoder-panicked](runbooks/decoder-panicked.md) |
| `stellarindex_ingestion_oracle_unknown_symbols` | `sum by (source) (increase(stellarindex_source_unknown_symbols_total[25h]))` | > 0 sustained 30 min (an on-chain oracle publishes a symbol / feed_id the canonical allow-list does not map) | ticket | [oracle-unknown-symbols](runbooks/oracle-unknown-symbols.md) |
| `stellarindex_ingestion_oracle_unrepresentable_symbols` | `sum by (source) (increase(stellarindex_source_unrepresentable_symbols_total[25h]))` | > 0 sustained 30 min (an oracle publishes a feed_id the record layer cannot store even as `raw:` — the slot is dropped, no row written) | ticket | [oracle-unknown-symbols](runbooks/oracle-unknown-symbols.md) |
| `stellarindex_ingestion_discovery_drops` | `increase(stellarindex_discovery_dropped_hits_total[10m])` | > 0 sustained 10 min | informational | [discovery-drops](runbooks/discovery-drops.md) |
| `stellarindex_served_value_drift` | `stellarindex_served_value_ok == 0` | sustained 26 h (two daily runs) | ticket | [served-value-drift](runbooks/served-value-drift.md) |
| `stellarindex_served_value_check_stale` | `time() - stellarindex_served_value_last_run_unix` | > 48 h | ticket | [served-value-drift](runbooks/served-value-drift.md) |
| `stellarindex_served_value_persistently_skipped` | `stellarindex_served_value_skipped == 1` | sustained 26 h (two daily runs) | ticket | [served-value-drift](runbooks/served-value-drift.md) |
| `stellarindex_cex_usd_volume_coverage_low` | per-source `increase(stellarindex_trade_inserts_total{usd_volume_populated="yes"}[1h])` / total, external venues | < 99.9% sustained 30 min | ticket | [usd-volume-coverage-plan](usd-volume-coverage-plan.md) |
| `stellarindex_onchain_usd_volume_coverage_low` | same ratio over `[6h]`, aggregated across on-chain venues | < 99.5% sustained 1 h | ticket | [usd-volume-coverage-plan](usd-volume-coverage-plan.md) |
| `stellarindex_ingestion_ch_live_sink_drops` | `increase(stellarindex_ch_live_sink_ledgers_total{outcome="dropped"}[10m])` | > 0 sustained 10 min | ticket | [ch-live-sink-drops](runbooks/ch-live-sink-drops.md) |
| `stellarindex_ingestion_ch_live_sink_drops_sustained` | `increase(stellarindex_ch_live_sink_ledgers_total{outcome="dropped"}[1h])` | > 0 sustained 1 h | page | [ch-live-sink-drops](runbooks/ch-live-sink-drops.md) |
| `stellarindex_ingestion_trade_insert_backpressure` | `sum(rate(stellarindex_trade_insert_retries_total{outcome="retry"}[5m]))` | > 0 sustained 10 min | ticket | [trade-insert-backpressure](runbooks/trade-insert-backpressure.md) |
| `stellarindex_ingestion_insert_errors` | `rate(stellarindex_source_insert_errors_total[5m])` per (source, kind) | > 0.1/s (≈6/min) sustained 5 min | ticket | [insert-errors](runbooks/insert-errors.md) |
| `stellarindex_ingestion_persist_drop` | `increase(stellarindex_source_insert_errors_total{kind=~"soroswap_router_swap\|defindex_flow_strategy\|defindex_flow_vault"}[15m])` | > 0 sustained 15 min (any rate) | ticket | [insert-errors](runbooks/insert-errors.md) |
| `stellarindex_ingestion_discovery_record_failures` | `increase(stellarindex_discovery_record_failures_total[10m])` | > 0 sustained 10 min | informational | [discovery-drops](runbooks/discovery-drops.md) |
| `stellarindex_ingestion_duplicate_flood` | `rate(stellarindex_trade_insert_outcome_total{outcome="duplicate"}[10m])` UNLESS `rate(...{outcome="new"}[10m]) > 0` per source | duplicates > 0.5/s with zero-or-absent new for 10 min | ticket | [ingestion-duplicate-flood](runbooks/ingestion-duplicate-flood.md) |
| `stellarindex_ingestion_source_insert_stale` | `time() - stellarindex_source_last_insert_unix` per source AND `source_enabled=1` | > 3600 s for ≥ 10 min | ticket | [ingestion-duplicate-flood](runbooks/ingestion-duplicate-flood.md) |
| `stellarindex_dex_trade_unit_ratio_detected` | `sum by (source) (increase(stellarindex_dex_trade_unit_ratio_total[30m]))` | > 25 per source, sustained 5 min | ticket | [dex-trade-unit-ratio](runbooks/dex-trade-unit-ratio.md) |
| `stellarindex_ingest_gap_detected` | `max by (source) (stellarindex_ingest_gap_max_size_ledgers) > 1000` per (source, table) | sustained 15 min | page | [ingest-gap-detected](runbooks/ingest-gap-detected.md) + per-source [sdex-gap-detected](runbooks/sdex-gap-detected.md) / [projector-replay](runbooks/projector-replay.md) |
| `stellarindex_ingest_gap_detector_silent` | `(time() - stellarindex_ingest_gap_detector_last_success_unix) > 8h` OR detector metric absent for 15 min OR `runs_total{outcome="error"}` present now + 8h ago with no last-success stamp in 8h | for ≥ 10 min | ticket | [ingest-gap-detector-silent](runbooks/ingest-gap-detector-silent.md) |
| `stellarindex_ops_job_heartbeat_stale` | `stellarindex_ops_job_heartbeat_unix` + `_running`, joined `on (ops_job, instance, pid)` | running==1 and no liveness write for > 15 min, for 10 min (the ops job PROCESS died) | ticket | [ops-job-stalled](runbooks/ops-job-stalled.md) |
| `stellarindex_ops_job_no_progress` | `changes(stellarindex_ops_job_progress_total[30m])` + `_running`, joined `on (ops_job, instance, pid)` | running==1 and zero units completed in 30 min, for 15 min (the ops job is HUNG) | ticket | [ops-job-stalled](runbooks/ops-job-stalled.md) |
| `stellarindex_projector_lag_high` | `max by (source) (stellarindex_projector_lag_ledgers)` `unless` `stellarindex_projector_replay_window_active == 1` | > 256 ledgers sustained 10 min, EXCEPT while an operator-recorded `projector-replay` rewind is still climbing (#325 — that intended lag is covered by `stellarindex_projector_replay_stalled`) | ticket | [projector-lag](runbooks/projector-lag.md) |
| `stellarindex_projector_error_rate_high` | `rate(stellarindex_projector_runs_total{outcome="error"}[15m])` per source | > 0.05/s sustained 15 min | ticket | [projector-lag](runbooks/projector-lag.md) |
| `stellarindex_projector_row_quarantined` | `increase(stellarindex_projector_events_decoded_total{outcome="sink_quarantined"}[15m])` | > 0 for 15 min (a poison row was skipped so the sole-writer projector could advance) | ticket | [projector-row-quarantined](runbooks/projector-row-quarantined.md) |
| `stellarindex_projector_decode_error_rate_high` | `sum by (source) (rate(stellarindex_projector_events_decoded_total{outcome="decode_error"}[10m]))` per source | > 0.1/s sustained 15 min (a decoder regression drained a whole class of events; cursor advanced past them) | ticket | [projector-decode-error-rate](runbooks/projector-decode-error-rate.md) |
| `stellarindex_projector_wedged` | `max by (source) (stellarindex_projector_wedged)` | > 0 for 5 min (the adaptive window floored at MinBatchLimit and the source has failed to advance for WedgeCycles+ cycles — a stuck cursor retrying the identical range forever; manual remediation) | ticket | [projector-wedged](runbooks/projector-wedged.md) |
| `stellarindex_projector_replay_stalled` | `stellarindex_projector_replay_window_active == 1` AND `stellarindex_projector_lag_ledgers > 256` AND `max_over_time(stellarindex_projector_lag_ledgers[15m]) <= stellarindex_projector_lag_ledgers` | replay window open, lag still over the same 256-ledger bound `lag_high` uses, and lag has not fallen for 15 min, for 5 min (the operator's rewind has stopped advancing — the served-row deficit it was started to repair is still open). The lag floor is what stops a caught-up source with an open window ticketing as a stalled replay | ticket | [projector-replay](runbooks/projector-replay.md) |
| `stellarindex_external_poller_stale` | `time() - stellarindex_external_poller_last_success_unix{source!="ecb"}` | > 1800 s for > 5 min | ticket | [external-poller-stale](runbooks/external-poller-stale.md) |
| `stellarindex_external_poller_stale_ecb` | `time() - stellarindex_external_poller_last_success_unix{source="ecb"}` | > 43200 s (12h) for > 10 min | informational | [external-poller-stale](runbooks/external-poller-stale.md) |
| `stellarindex_external_poller_error_rate_high` | `rate(stellarindex_external_poller_polls_total{outcome="error"}[15m]) / sum(...) ` | > 0.5 sustained 15 min | informational | [external-poller-error-rate-high](runbooks/external-poller-error-rate-high.md) |
| `stellarindex_external_fx_feed_stale` | `time() - max(stellarindex_external_fx_last_quote_unix)` | > 21600 s (6h) for > 15 min | ticket | [fx-feed-stale](runbooks/fx-feed-stale.md) |
| `stellarindex_external_fx_feed_absent` | `absent(stellarindex_external_fx_last_quote_unix)` | series missing for 30 min | ticket | [fx-feed-stale](runbooks/fx-feed-stale.md) |
| `stellarindex_external_fx_rate_rejections` | `sum by (reason) (increase(stellarindex_external_fx_rate_rejected_total{reason!~"history_deviation_stuck\|deviation_history_conflict_stuck"}[3h]))` | > 2 rejections in 3h, for 30 min (a ticker is wedged on its last accepted rate) | ticket | [fx-rate-rejected](runbooks/fx-rate-rejected.md) |

Historical note: the former `stellarindex_ingestion_lag_high` alert was retired
when the repo moved off the legacy orchestrator topology and the live indexer
stopped emitting a trustworthy per-source lag gauge. Its last runbook remains
archived at [ingestion-lag](runbooks/ingestion-lag.md) until a replacement
signal lands.

## Storage alerts

| Name | Metric | Condition | Severity | Runbook |
| ---- | ------ | --------- | -------- | ------- |
| `stellarindex_source_recognition_failing` | `stellarindex_recognition_ok` | == 0 for > 1 h | ticket | [source-recognition-failing](runbooks/source-recognition-failing.md) |
| `stellarindex_dependency_down` | `stellarindex_dependency_up` | == 0 for > 2 m | page | [dependency-down](runbooks/dependency-down.md) |
| `stellarindex_timescale_primary_down` | `up{job="postgres",role="primary"}` | == 0 for > 30 s | page | [timescale-primary-down](runbooks/timescale-primary-down.md) |
| `stellarindex_timescale_replica_lag` | `pg_replication_lag_seconds` on sync replica | > 5 s for > 2 min | ticket | [replica-lag](runbooks/replica-lag.md) |
| `stellarindex_timescale_disk_full` | `(node_filesystem_avail_bytes / node_filesystem_size_bytes) * 100` on DB vol | < 10 % | page | [db-disk-full](runbooks/db-disk-full.md) |
| `stellarindex_timescale_disk_warning` | same | < 20 % | ticket | [db-disk-full](runbooks/db-disk-full.md) |
| `stellarindex_config_assertion_failed` | a load-bearing guard config (rsyslog suppress / journald cap / CH-logs-on-ZFS / nft 443 / redis cap / supply reserves) is missing or reverted — hourly config-assertions.sh producer | ==0 for 65m | ticket | [config-assertion-failed](runbooks/config-assertion-failed.md) |
| `stellarindex_config_assertions_stale` | the config-assertions producer itself went silent (>2h without fresh textfile output) | for 30m | ticket | [config-assertion-failed](runbooks/config-assertion-failed.md) |
| `stellarindex_node_root_disk_filling_fast` | predict_linear 10m trend on root avail reaching 0 within 30 min (AND avail < 50%) — the log-flood early warning (the 2026-06-11 class fills root in ~5 min, faster than the static page can be acted on) | trend < 0 for 2m | page | [node-root-disk-filling-fast](runbooks/node-root-disk-filling-fast.md) |
| `stellarindex_node_root_disk_full` | same expr on `mountpoint="/"` (distinct from DB vol — root FS holds /var/log + /tmp + /var/cache) | < 10 % | page | [node-root-disk-full](runbooks/node-root-disk-full.md) |
| `stellarindex_node_root_disk_warning` | same | < 20 % | ticket | [node-root-disk-warning](runbooks/node-root-disk-warning.md) |
| (no active alert — surfaced via API log) | `forex: fx_quotes persist failed` log line — runtime symptom of an unapplied schema migration | repeating every ~5 min | P3 | [fx-history-missing](runbooks/fx-history-missing.md) |
| `stellarindex_worker_panicked` | `increase(stellarindex_worker_panics_total[10m])` | > 0 — a background worker panicked and is stopped until its unit restarts (#368 M4) | page | [worker-panicked](runbooks/worker-panicked.md) |
| `stellarindex_postgres_ping_failing` | `rate(stellarindex_postgres_ping_total{outcome="error"}[5m])` | > 0.5/s for > 2 min — indexer pool wedged (F-0151) | page | [postgres-ping-failing](runbooks/postgres-ping-failing.md) |
| `stellarindex_timescale_connections_saturated` | `pg_stat_activity_count / pg_settings_max_connections * 100` | > 80 % for > 5 min | ticket | [pg-conns-saturated](runbooks/pg-conns-saturated.md) |
| `stellarindex_timescale_lock_table_pressure` | `sum by (instance)(pg_locks_count) / on (instance)(pg_settings_max_locks_per_transaction * pg_settings_max_connections)` | > 70 % for > 5 min | ticket | [pg-conns-saturated](runbooks/pg-conns-saturated.md) |
| `stellarindex_systemd_unit_failed` | `node_systemd_unit_state{state="failed"}` (catch-all, minus dedicated-alert units) | in `failed` 15m | ticket | [systemd-unit-failed](runbooks/systemd-unit-failed.md) |
| `stellarindex_timescale_cagg_stale` | `time() - stellarindex_cagg_last_refresh_unix` per CAGG | > 5× its refresh interval | ticket | [cagg-stale](runbooks/cagg-stale.md) |
| `stellarindex_timescale_job_failures_climbing` | `increase(stellarindex_timescale_job_failures_total[6h])` per job | > 10 failures in 6h, 30m | informational | [timescale-job-failures-climbing](runbooks/timescale-job-failures-climbing.md) |
| `stellarindex_timescale_compression_lag` | `stellarindex_uncompressed_chunks_older_than_7d` | > 0 for > 24 h | informational | [compression-lag](runbooks/compression-lag.md) |
| `stellarindex_timescale_backup_failed` | `min by (stanza)(pgbackrest_backup_since_last_completion_seconds{stanza!~"all-stanzas.*"})` | > 25 h for 5 min | ticket | [backup-failed](runbooks/backup-failed.md) |
| `stellarindex_timescale_backup_none_24h` | same | > 24 h for 5 min | page | [backup-failed](runbooks/backup-failed.md) |
| `stellarindex_pgbackrest_backup_metrics_absent` | `up{job="pgbackrest_exporter"} == 1 unless on (instance) pgbackrest_backup_since_last_completion_seconds{stanza!~"all-stanzas.*"}` | exporter up but no real-stanza backup series for 15 min — the two alerts above are structurally blind | page | [backup-failed](runbooks/backup-failed.md) |
| `stellarindex_pgbackrest_backup_unit_failed` | `node_systemd_unit_state{name="pgbackrest-backup.service",state="failed"}` | == 1 for 5 min | ticket | [backup-failed](runbooks/backup-failed.md) |
| `stellarindex_ch_schema_snapshot_stale` | `time() - stellarindex_ch_schema_snapshot_last_success_unix` (or `absent_over_time(...[36h])` — never / every-run-failed) | > 36 h, or series absent 36 h, for ≥ 30 min | ticket | [ch-schema-restore](runbooks/ch-schema-restore.md) |
| `stellarindex_ch_schema_snapshot_offsite_stale` | `time() - stellarindex_ch_schema_snapshot_offsite_last_success_unix` (or `absent_over_time(...[72h]) and on () stellarindex_ch_schema_snapshot_offsite_configured == 1`) | > 72 h, or never pushed since a target was configured, for ≥ 30 min | ticket | [ch-schema-restore](runbooks/ch-schema-restore.md) |
| `stellarindex_ch_schema_snapshot_unit_failed` | `node_systemd_unit_state{name="ch-schema-snapshot.service",state="failed"}` | == 1 for 5 min | ticket | [ch-schema-restore](runbooks/ch-schema-restore.md) |
| `stellarindex_restore_drill_stale` | `time() - stellarindex_restore_drill_last_success_unix` (or `absent_over_time(...[40d])`) | > 40 d for ≥ 30 min | ticket | [restore-drill-stale](runbooks/restore-drill-stale.md) |
| `stellarindex_zfs_pool_free_low` | `stellarindex_zfs_pool_free_bytes` (zpool free, textfile from `zfs-snapshot.sh`) | < 2.5 TiB for ≥ 15 min | ticket | [zfs-snapshots](runbooks/zfs-snapshots.md) |
| `stellarindex_zfs_pool_free_critical` | same | < 1.5 TiB for ≥ 5 min (below the snapshot job's 2 TiB guard floor) | page | [zfs-snapshots](runbooks/zfs-snapshots.md) |
| `stellarindex_zfs_snapshot_stale` | `time() - stellarindex_zfs_snapshot_latest_unix` per dataset (or `absent_over_time(...{dataset="data/clickhouse"}[36h])`) | > 36 h for ≥ 30 min | ticket | [zfs-snapshots](runbooks/zfs-snapshots.md) |
| `stellarindex_zfs_snapshot_pool_free_unreadable` | `stellarindex_zfs_snapshot_pool_free_unreadable` (error textfile; job refused to prune/snapshot) | == 1 for ≥ 10 min | ticket | [zfs-snapshots](runbooks/zfs-snapshots.md) |
| `stellarindex_restore_drill_failed` | `stellarindex_restore_drill_failures` | > 0 for ≥ 30 min (most recent run failed/aborted) | ticket | [restore-drill-failed](runbooks/restore-drill-failed.md) |
| `stellarindex_backup_offsite_stale` | `up{job="pgbackrest_exporter"} == 1 unless on (instance) (pgbackrest_backup_info{repo_key="2"} unless … offset 8d)` — no repo2 (S3 off-site) backup series younger than 8 d, or repo2 never written; repo1-fresh/repo2-stale is invisible to the two alerts above | for ≥ 1 h | ticket | [backup-offsite-stale](runbooks/backup-offsite-stale.md) |

## Cache / serving alerts

| Name | Metric | Condition | Severity | Runbook |
| ---- | ------ | --------- | -------- | ------- |
| `stellarindex_redis_master_down` | `redis_up` per master | == 0 for > 30 s | page | [redis-master-down](runbooks/redis-master-down.md) |
| `stellarindex_redis_memory_saturated` | `redis_memory_used_bytes / redis_memory_max_bytes * 100` | > 90 % for > 5 min | ticket | [redis-memory](runbooks/redis-memory.md) |
| `stellarindex_redis_evictions_high` | `rate(redis_evicted_keys_total[5m])` | > 100/s | ticket | [redis-memory](runbooks/redis-memory.md) |
| `stellarindex_redis_replication_broken` | `redis_connected_slaves` per master | < expected for > 2 min | ticket | [redis-replication](runbooks/redis-replication.md) |
| `stellarindex_redis_writes_blocked` | `redis_rdb_last_bgsave_status` per master (also surfaces as `MISCONF` errors in client logs) | == 0 for > 60 s | page | [redis-write-blocked-disk-full](runbooks/redis-write-blocked-disk-full.md) |

## API plane alerts

| Name | Metric | Condition | Severity | Runbook |
| ---- | ------ | --------- | -------- | ------- |
| `stellarindex_api_down` | `up{job=~"stellarindex[_-]api"}` across regions | == 0 for > 60 s | page | [api-down](runbooks/api-down.md) |
| `stellarindex_api_latency_p95_high` | `histogram_quantile(0.95, rate(http_request_duration_seconds_bucket{job="stellarindex-api"}[5m]))` | > 500 ms for 10 m (the p95 is already a 5 m percentile, so the longer `for` rides out a cold-cache deploy) | ticket | [api-latency](runbooks/api-latency.md) |
| `stellarindex_api_latency_p99_high` | `histogram_quantile(0.99, ...)` | > 2 s for 10 m | ticket | [api-latency](runbooks/api-latency.md) |
| `stellarindex_api_error_rate_high` | `rate(http_requests_total{status=~"5.."}[5m]) / rate(http_requests_total[5m])` | > 1 % for > 2 min | ticket | [api-5xx](runbooks/api-5xx.md) |
| `stellarindex_api_error_rate_critical` | same | > 5 % for > 2 min | page | [api-5xx](runbooks/api-5xx.md) |
| `stellarindex_api_price_stale` | `stellarindex_price_staleness_seconds` per asset | > 120 s sustained 5 min | ticket | [price-stale](runbooks/price-stale.md) |
| `stellarindex_api_cache_miss_rate_high` | `rate(stellarindex_api_cache_ops_total{result="miss"}[5m]) / rate(stellarindex_api_cache_ops_total{result=~"hit\|miss\|stale"}[5m])` per (cache, op) | > 50 % sustained 10 min on a hot op (≥ 0.1 req/s) | ticket | [cache-miss-rate-high](runbooks/cache-miss-rate-high.md) |

## Notify (transactional-email) alerts

`internal/notify` (the Resend client) sends the magic-link dashboard
login email and the API-signup confirmation email — the only two mail
paths (price alerts deliver via webhooks). The login handler swallows the
send error to stay enumeration-safe, so `stellarindex_notify_sends_total`
is the only signal a mail outage leaves.

| Name | Metric | Condition | Severity | Runbook |
| ---- | ------ | --------- | -------- | ------- |
| `stellarindex_notify_send_failure_ratio_high` | `sum by (template) (rate(stellarindex_notify_sends_total{result="failed"}[15m])) / sum by (template) (rate(stellarindex_notify_sends_total[15m]))` | > 0.5 for 15 min (a mail provider outage — new logins / signup confirmations stop delivering; existing sessions + keys unaffected) | ticket | [notify-send-failure](runbooks/notify-send-failure.md) |

## SLA-probe alerts

Source: `cmd/stellarindex-sla-probe` runs every 15 min via the
systemd timer in `configs/healthchecks/stellarindex-sla-probe.timer`; metrics emitted
to node_exporter's textfile_collector via `-textfile-output`.
Per the service SLA targets — these are the synthetic
counterparts to the API-plane alerts above.

| Name | Metric | Condition | Severity | Runbook |
| ---- | ------ | --------- | -------- | ------- |
| `stellarindex_sla_probe_p95_breach` | `stellarindex_sla_probe_latency_ms{quantile="0.95"}` | > 200 ms for ≥ 30 min | page | [sla-probe-p95-breach](runbooks/sla-probe-p95-breach.md) |
| `stellarindex_sla_probe_freshness_breach` | `stellarindex_sla_probe_freshness_sec` | > 30 s for ≥ 30 min | page | [sla-probe-freshness-breach](runbooks/sla-probe-freshness-breach.md) |
| `stellarindex_sla_probe_unit_failed_alert` | `stellarindex_sla_probe_unit_failed` | > 0 for ≥ 30 min | ticket | [sla-probe-unit-failed](runbooks/sla-probe-unit-failed.md) |
| `stellarindex_sla_probe_stale` | `time() - stellarindex_sla_probe_last_pass_timestamp` | > 90 min for ≥ 5 min | page | [sla-probe-stale](runbooks/sla-probe-stale.md) |

## SLO burn-rate alerts (multi-window)

Per [ADR-0009](../adr/0009-latency-budget.md). Pattern from the
Google SRE workbook: short + long windows must BOTH agree before
firing. Suppresses single-spike noise; catches both fast burns
(near-immediate budget consumption) and slow drifts (sustained
sub-target). Backstop direct-threshold alerts (above) stay live
for incident-time clarity.

| Name | SLO | Burn rate (× monthly budget) | Severity | Runbook |
| ---- | --- | ---------------------------- | -------- | ------- |
| `stellarindex_slo_latency_burn_fast` | 99.9% under 200ms | > 14.4× over 5m AND 1h | page | [slo-latency-burn-fast](runbooks/slo-latency-burn-fast.md) |
| `stellarindex_slo_latency_burn_medium` | same | > 6× over 30m AND 6h | page | [slo-latency-burn-medium](runbooks/slo-latency-burn-medium.md) |
| `stellarindex_slo_latency_burn_slow` | same | > 1× over 6h AND 24h | ticket | [slo-latency-burn-slow](runbooks/slo-latency-burn-slow.md) |
| `stellarindex_slo_availability_burn_fast` | 99.99% non-5xx | > 14.4× over 5m AND 1h | page | [slo-availability-burn-fast](runbooks/slo-availability-burn-fast.md) |
| `stellarindex_slo_availability_burn_medium` | same | > 6× over 30m AND 6h | page | [slo-availability-burn-medium](runbooks/slo-availability-burn-medium.md) |
| `stellarindex_slo_availability_burn_slow` | same | > 1× over 6h AND 24h | ticket | [slo-availability-burn-slow](runbooks/slo-availability-burn-slow.md) |

## Stellar / node alerts

> **Inert on r1 (2026-04-30).** The first four alerts in this table
> reference metrics produced by stellar-core / stellar-rpc / the
> stellar-core-prometheus-exporter. All three were removed from r1
> on 2026-04-23 ([r1-deployment-state.md](r1-deployment-state.md)),
> so these alerts have no producer and cannot fire on the current
> deployment posture. They remain in the rule file for Phase-3
> (Tier-1 validator rollout, ADR-0004); each runbook's *Deployment
> posture* callout explains the revival path.
>
> `archive-divergence` is **not** affected by that posture, but had
> its own producer gap until 2026-08-29 (issue #282): it consumes
> `stellarindex_verify_archive_mismatches_total`, which the
> verify-archive tier-a/tier-b timers now publish through
> node_exporter's textfile collector (`-textfile-output`). The
> previously-cited producer `scripts/ops/archive-cross-check.sh`
> never existed.

| Name | Metric | Condition | Severity | Runbook |
| ---- | ------ | --------- | -------- | ------- |
| `stellarindex_stellar_core_ledger_age` | `time() - stellarindex_stellar_core_last_ledger_time_unix` | > 60 s for > 2 min | page | [core-lag](runbooks/core-lag.md) |
| `stellarindex_stellar_core_peers_low` | `stellarindex_stellar_core_peer_count` | < 5 for > 5 min | ticket | [core-peers](runbooks/core-peers.md) |
| `stellarindex_stellar_rpc_lag` | `stellarindex_stellar_rpc_latest_ledger_age_seconds` | > 300 s for > 5 min | ticket | [rpc-lag](runbooks/rpc-lag.md) |
| `stellarindex_stellar_archive_publish_fail` | `increase(stellarindex_stellar_archive_publish_errors_total[1h])` | > 0 | informational | [archive-publish](runbooks/archive-publish.md) |
| `stellarindex_stellar_archive_divergence` | `increase(stellarindex_verify_archive_mismatches_total[26h])` (verify-archive tier-a/tier-b, via node_exporter textfile) | > 0 | page | [archive-divergence](runbooks/archive-divergence.md) |

## Stellar-stack version-lag alerts

Per the 2026-07-08 (core) and 2026-07-09 (galexie/CAP-0071) protocol-27
incidents: nothing watched whether the installed Stellar toolchain
(`stellar-core`, `stellar-galexie`, `stellar-archivist`) lagged
upstream. `stellar-stack-version-probe.timer` runs
`stellar-stack-version-probe.sh` daily (installed by
`configs/ansible/roles/archival-node/tasks/10-observability.yml`),
writing `stellarindex_stellar_stack_version_lag{component}` (0=current,
1=newer available, 2=newer PROTOCOL-MAJOR available),
`stellarindex_stellar_stack_installed_info{component,version}`, and
`stellarindex_stellar_stack_probe_success` to the node_exporter
textfile collector.

| Name | Metric | Condition | Severity | Runbook |
| ---- | ------ | --------- | -------- | ------- |
| `stellarindex_stellar_stack_lagging` | `stellarindex_stellar_stack_version_lag` per `component` | >= 1 for 2 d | ticket | [stellar-stack-version-lag](runbooks/stellar-stack-version-lag.md) |
| `stellarindex_stellar_stack_protocol_lag` | same | >= 2 for 6 h | page | [stellar-stack-version-lag](runbooks/stellar-stack-version-lag.md) |
| `stellarindex_ledger_meta_decode_failing` | `stellarindex_ledger_meta_decode_failures_total` per `unit` | > 0 for 10 m | page | [protocol-upgrades](protocol-upgrades.md) |
| `stellarindex_ledger_meta_decode_probe_stale` | `stellarindex_ledger_meta_decode_probe_updated_seconds` | age > 1 h for 30 m | ticket | [protocol-upgrades](protocol-upgrades.md) |

The two decode alerts are the REACTIVE backstop to the version-lag pair above.

## StellarIndex own-binary version-skew alerts

The section above watches THIRD-PARTY components. These watch **our
own** binaries, a gap that produced the same incident twice: F-1314
(2026-05-13) found `stellarindex-sla-probe` drifting because the deploy
workflow's default binary list omitted it, and on 2026-08-28 the
identical failure surfaced for `stellarindex-ops` — v0.44.7 on r1
against v0.46.1 everywhere else. A deploy that never touches a binary
still exits 0, so neither was ever reported.

`stellarindex-binary-version-probe.timer` runs
`stellarindex-binary-version-probe.sh` every 30 min (installed by
`configs/ansible/roles/archival-node/tasks/10-observability.yml`),
writing `stellarindex_binary_version_skew` (distinct installed versions
minus one), `stellarindex_binary_version_info{binary,version}`,
`stellarindex_binary_version_binaries_total` and
`stellarindex_binary_version_probe_success` to the node_exporter
textfile collector.

The alert compares binaries against **each other**, not against an
expected version — the host has no authoritative notion of the current
release, and hardcoding one would make the probe lie after every
legitimate deploy. It therefore also covers binaries added in future
without anyone maintaining a list.

| Name | Metric | Condition | Severity | Runbook |
| ---- | ------ | --------- | -------- | ------- |
| `stellarindex_binary_version_skew` | `stellarindex_binary_version_skew` | > 0 for 45 m | ticket | [binary-version-skew](runbooks/binary-version-skew.md) |
| `stellarindex_binary_version_probe_degraded` | `stellarindex_binary_version_probe_success` | == 0 for 2 h | ticket | [binary-version-skew](runbooks/binary-version-skew.md) |

Impact is indirect but one-way: `stellarindex-ops` backs the
data-integrity gates (`verify-archive` tier-a/b, `archive-completeness`,
`ch-schema-drift`, `restore-drill`). A stale gate validates against
retired rules, so it PASSES when it should fail — the one direction a
gate must never fail in.
The version probe compares released upstream versions, so it cannot know the
network has actually begun emitting new XDR — on 2026-08-27 testnet ingested 240
Protocol-28 ledgers on a galexie that could not decode P28, with no error at
all, because the breaking arm only appears once a ledger genuinely contains
parallel-execution structures. `..._decode_failing` watches for the decode
failure itself and names the cause; `..._decode_probe_stale` stops a dead probe
reading as "no failures". The failure is fail-closed (the component errors and
stops; no corrupt data is written), so this is an ingestion outage, not a
correctness incident.

## Archive completeness alerts

Per [ADR-0017](../adr/0017-archive-completeness-invariants.md). Both
the primary archive (`galexie-archive/` MinIO) and the cross-anchor
archive (`/srv/history-archive/`) have hard completeness contracts.
The daily `archive-completeness.timer` enforces them on R1; R2 + R3
delegate to R1 for cross-anchor checks but verify their own
chain-link locally. See [archive-completeness.md](archive-completeness.md).

| Name | Metric | Condition | Severity | Runbook |
| ---- | ------ | --------- | -------- | ------- |
| `stellarindex_archive_files_missing` | `archive_files_missing` per archive | > 0 for > 4 h | ticket | [archive-files-missing](runbooks/archive-files-missing.md) |
| `stellarindex_archive_completeness_stale` | `time() - archive_completeness_last_success_timestamp` | > 26 h | ticket | [archive-completeness-stale](runbooks/archive-completeness-stale.md) |
| `stellarindex_archive_completeness_critical_stale` | same | > 48 h on R1 (integrity leader) | page | [archive-completeness-stale](runbooks/archive-completeness-stale.md) |
| `stellarindex_archive_repair_source_degraded` | `increase(archive_completeness_repair_failures_total[25h]) / increase(archive_completeness_repair_attempts_total[25h])` per source | > 0.10 over one verify cycle (25 h) | informational | [archive-repair-source-degraded](runbooks/archive-repair-source-degraded.md) |
| `stellarindex_galexie_catchup_refused` | `stellarindex_galexie_catchup_refusals_5m` (journal probe textfile metric) | > 0 for 10 m | page | [galexie-catchup-refused](runbooks/galexie-catchup-refused.md) |
| `stellarindex_host_swap_activity` | `rate(node_vmstat_pswpout[10m])` | > 100 for 15 m | ticket | [galexie-catchup-refused](runbooks/galexie-catchup-refused.md) |
| `stellarindex_galexie_archive_tip_lag_high` | `galexie_archive_tip_lag_ledgers` (archive newest vs live newest) | > 64,000 for 90 m | ticket | [galexie-archive-tip-lag](runbooks/galexie-archive-tip-lag.md) |
| `stellarindex_galexie_archive_tip_lag_severe` | same | > 128,000 for 30 m | page | [galexie-archive-tip-lag](runbooks/galexie-archive-tip-lag.md) |
| `stellarindex_galexie_archive_tip_lag_metric_stale` | `time() - galexie_archive_tip_lag_updated_seconds` | > 30 m for 15 m | ticket | [galexie-archive-tip-lag](runbooks/galexie-archive-tip-lag.md) |
| `stellarindex_galexie_archive_gap` | `galexie_archive_unexpected_gaps` — partition-level holes/overlaps in the DR mirror that are NOT the declared capacity trim (tip-lag proves the newest edge; this proves the middle) | > 0 for 1 h | page | [galexie-archive-contiguity](runbooks/galexie-archive-contiguity.md) |
| `stellarindex_galexie_archive_contiguity_silent` | `absent_over_time(galexie_archive_unexpected_gaps[3h])` | for 15 m (hourly scan dark) | ticket | [galexie-archive-contiguity](runbooks/galexie-archive-contiguity.md) |

Defense-in-depth for `#26` — the original 23-day silent stall of
`galexie-archive`. The post-`#26` fix is the hourly
`galexie-archive-fill.timer`; these alerts page within hours if
that timer (or its `mc` aliases / aws-public IAM / MinIO
mtime-poison failure mode) silently breaks. Metric source:
node_exporter textfile_collector reads
`/var/lib/node_exporter/textfile_collector/galexie_archive_tip_lag.prom`,
refreshed every 5 min by `galexie-archive-tip-lag.timer`.

## HashDB drift-detector alerts

Per [ADR-0016](../adr/0016-per-region-storage-strategy.md)'s trust
model: the indexer's live LCM read loop appends sha256(LCM) per
ledger to an on-disk hashdb (`internal/hashdb`); a periodic sweep
re-reads a trailing window from the same bucket and compares,
catching upstream rewrites of a previously-fetched ledger's bytes
that a chain-link check alone can't see. Off by default
(`[hashdb].enabled = false`) — first production exposure 2026-07-08,
opt in per region. Both alerts are inert (no emitted series) on a
region that hasn't opted in.

| Name | Metric | Condition | Severity | Runbook |
| ---- | ------ | --------- | -------- | ------- |
| `stellarindex_hashdb_drift_detected` | `stellarindex_hashdb_drift_total` | > 0 | ticket | [hashdb-drift-detected](runbooks/hashdb-drift-detected.md) |
| `stellarindex_hashdb_verify_failing` | `rate(stellarindex_hashdb_verify_runs_total{outcome="error"}[6h]) > rate(...{outcome=~"ok\|drift"}[6h])` | sustained 30 min | ticket | [hashdb-drift-detected](runbooks/hashdb-drift-detected.md) |

## Data-freshness / completeness alerts

The "never get behind" watchdog. `data-freshness.sh` (every 15 min via
`data-freshness.timer`) emits per-domain ingest-freshness gauges + the per-source
ADR-0033 completeness verdict to the node_exporter textfile collector
(`data_freshness.prom`). Covers what the gap detector doesn't: reference oracles,
FX, supply, the issuer-metadata cron, and the verdict itself — the gaps that let
coingecko rot 11 days and sep1 metadata never populate, both unnoticed.

| Name | Metric | Condition | Severity | Runbook |
| ---- | ------ | --------- | -------- | ------- |
| `stellarindex_data_source_stale` | `stellarindex_data_freshness_stale{domain,source}` | == 1 for > 1h | ticket | [data-source-stale](runbooks/data-source-stale.md) |
| `stellarindex_supply_assets_stale` | `stellarindex_supply_assets_stale` | > 0 for > 2h — per-asset frozen supply the domain-level `supply` check cannot see, because that one measures max(time) across the whole table | ticket | [supply-assets-stale](runbooks/supply-assets-stale.md) |
| `stellarindex_completeness_incomplete` | `stellarindex_completeness_incomplete{source}` | == 1 for > 1h | ticket | [completeness-incomplete](runbooks/completeness-incomplete.md) |
| `stellarindex_twap_history_missing` | `stellarindex_twap_history_missing{view}` | == 1 for > 2h — a TWAP CAGG recreated WITH NO DATA (0081/0115/0126) whose manual `refresh_continuous_aggregate` follow-up was never run; the refresh policy re-fills only a recent sliver so newest-bar freshness reads green while back-history serves empty. Not visible to the ADR-0033 verdict (twap_* are derived CAGGs, not reconcile targets) | ticket | [twap-history-missing](runbooks/twap-history-missing.md) |
| `stellarindex_data_freshness_watchdog_silent` | `absent_over_time(stellarindex_data_freshness_stale[45m])` | for > 15m | ticket | [data-freshness-watchdog-silent](runbooks/data-freshness-watchdog-silent.md) |
| `stellarindex_serving_insert_frozen` | `time() - max(stellarindex_source_last_insert_unix)` | > 1800 s (no insert from ANY source) for 10 min | ticket | [data-source-stale](runbooks/data-source-stale.md) |
| `stellarindex_serving_insert_absent` | `absent(stellarindex_source_last_insert_unix)` | series missing for 15 min | ticket | [data-source-stale](runbooks/data-source-stale.md) |

## Ledgerstream tier alerts

Per [ADR-0027](../adr/0027-lcm-cache-tiering.md). R1's
`TieredDataStore` (`internal/ledgerstream/tiered.go`) reads each LCM
from the local `galexie-archive` MinIO bucket (hot) and falls back
on `NoSuchKey` to the AWS public bucket (cold). Pre-§3 of the
rollout (`storage.cold_tier_enabled = false`) the cold path never
runs and these alerts stay silent.

| Name | Metric | Condition | Severity | Runbook |
| ---- | ------ | --------- | -------- | ------- |
| `stellarindex_ledgerstream_tier_both_missing` | `stellarindex_ledgerstream_tier_read_total{outcome="both_missing"}` | `increase(...[5m]) > 0` for 5 m | page | [ledgerstream-tier-both-missing](runbooks/ledgerstream-tier-both-missing.md) |

`both_missing` is the cold-tier failure mode the rollout sequence
in ADR-0027 was designed to recover from cleanly: the runbook walks
the rehydrate-from-peer + disable-trim-timer steps.

## verify-archive timer alerts

Per the ADR-0016 per-region trust model: R1 runs verify-archive Tier A
(chain-link integrity) nightly at 03:23 UTC and Tier B (checkpoint anchor
against the local `/srv/history-archive` mirror) nightly at 04:37 UTC via
systemd; R2 + R3 trust R1 and run their own slower cadence. Tier A catches
internal corruption / dropped ledgers; Tier B catches single-source
corruption that is still chain-link-consistent (the failure mode Tier A is
blind to). node_exporter's `--collector.systemd` exports the unit state so
failures and stale runs trigger the alerts below. See
[verify-archive-tier-a.timer](https://github.com/Stellar-Index/StellarIndex/blob/main/deploy/systemd/verify-archive-tier-a.timer)
and
[verify-archive-tier-b.timer](https://github.com/Stellar-Index/StellarIndex/blob/main/deploy/systemd/verify-archive-tier-b.timer).

| Name | Metric | Condition | Severity | Runbook |
| ---- | ------ | --------- | -------- | ------- |
| `stellarindex_verify_archive_unit_failed` | `node_systemd_unit_state{name="verify-archive-tier-a.service",state="failed"}` | == 1 for > 5 min | ticket | [verify-archive-unit-failed](runbooks/verify-archive-unit-failed.md) |
| `stellarindex_verify_archive_run_stale` | `time() - node_systemd_timer_last_trigger_seconds{name="verify-archive-tier-a.timer"}` | > 36 h for > 10 min | page | [verify-archive-run-stale](runbooks/verify-archive-run-stale.md) |
| `stellarindex_verify_archive_tier_b_unit_failed` | `node_systemd_unit_state{name="verify-archive-tier-b.service",state="failed"}` | == 1 for > 5 min | ticket | [verify-archive-tier-b](runbooks/verify-archive-tier-b.md) |
| `stellarindex_verify_archive_tier_b_run_stale` | `time() - node_systemd_timer_last_trigger_seconds{name="verify-archive-tier-b.timer"}` | > 36 h for > 10 min | ticket | [verify-archive-tier-b](runbooks/verify-archive-tier-b.md) |

## Anomaly + freeze alerts

Per [ADR-0019](../adr/0019-anomaly-response-and-confidence-scoring.md).
The freeze policy fires only when `confidence < 0.45 AND z_score >
5σ AND source_count <= 1` — the extreme corner where multi-source
consensus can't help. (The bound is 0.45, not the ADR's original
0.10; see the 2026-07-25 amendment in the ADR for the recalibration.)
Operator runbook walks through review + override.

| Name | Metric | Condition | Severity | Runbook |
| ---- | ------ | --------- | -------- | ------- |
| `stellarindex_anomaly_freeze_engaged` | `stellarindex_anomaly_freeze_engaged_total` per class | rate > 0 over 5m | ticket | [anomaly-freeze-engaged](runbooks/anomaly-freeze-engaged.md) |
| `stellarindex_anomaly_freeze_sustained` | `stellarindex_anomaly_freeze_engaged_total` per class | rate > 0 sustained 1h+ | page | [anomaly-freeze-sustained](runbooks/anomaly-freeze-sustained.md) |
| `stellarindex_anomaly_freeze_recovery_stalled` | `stellarindex_anomaly_freeze_engaged_total` vs `_recovered_total` + `_recovery_sweeps_total{outcome!="ok"}` | engaged > recovered for 2h+ AND sweep errors in last 15m | ticket | [freeze-recovery-stalled](runbooks/freeze-recovery-stalled.md) |
| `stellarindex_amm_self_pair_swap_burst` | `stellarindex_amm_self_pair_swap_total` per source | increase > 10 over 15m, sustained 2m — a burst of self-pair (token_in==token_out) swaps, the 2026-08 Blend/Comet exploit primitive; normally zero | ticket | [amm-self-pair-swap-burst](runbooks/amm-self-pair-swap-burst.md) |

### Freeze lifecycle (ADR-0019 §"Freeze duration")

The rules above alert on the freeze DECISION. These alert on how
freezes END: a freeze holds for a minimum duration, extends by 30 min
at each expiry it has not earned its auto-unfreeze at, and escalates
to operator review after 4 extensions — from which point it does not
auto-unfreeze at all. Rules in
[`deploy/monitoring/rules/freeze-lifecycle.yml`](../../deploy/monitoring/rules/freeze-lifecycle.yml).

| Name | Metric | Condition | Severity | Runbook |
| ---- | ------ | --------- | -------- | ------- |
| `stellarindex_anomaly_freeze_escalated` | `stellarindex_anomaly_freeze_escalated_total` | increase > 0 over 15m — a freeze exhausted the 4-extension ladder and will NOT auto-unfreeze | page | [anomaly-freeze-sustained](runbooks/anomaly-freeze-sustained.md) |
| `stellarindex_anomaly_freeze_extension_rate` | `stellarindex_anomaly_freeze_extensions_total` | increase >= 3 over 1h, sustained 10m — freezes are climbing toward escalation | ticket | [anomaly-freeze-sustained](runbooks/anomaly-freeze-sustained.md) |
| `stellarindex_anomaly_freeze_active` | `stellarindex_anomaly_freeze_active` | > 0 for 5m — informational "N (pair, window) freezes held right now" | informational | [anomaly-freeze-engaged](runbooks/anomaly-freeze-engaged.md) |

## Divergence / quality alerts

| Name | Metric | Condition | Severity | Runbook |
| ---- | ------ | --------- | -------- | ------- |
| `stellarindex_price_divergence_warning` | `abs(our_price - ref_price) / ref_price` per pair | > 5 % for > 2 min | informational | [price-divergence](runbooks/price-divergence.md) |
| `stellarindex_price_divergence_critical` | same | > 10 % for > 2 min | ticket | [price-divergence](runbooks/price-divergence.md) |
| `stellarindex_oracle_stream_rows_unparsed` | `increase(stellarindex_oracle_stream_rows_unparsed_total[6h])` | > 0 for 10m | ticket | [oracle-stream-rows-unparsed](runbooks/oracle-stream-rows-unparsed.md) |
| `stellarindex_oracle_stale` | `time() - stellarindex_oracle_last_update_unix` per source | > 10× its resolution | ticket | [oracle-stale](runbooks/oracle-stale.md) |
| `stellarindex_divergence_refresh_error_dominant` | `rate(divergence_refresh_total{outcome="refresh_error"}[5m]) > rate(...{outcome="ok"}[5m])` | sustained 30 min | ticket | [divergence-refresh-error-dominant](runbooks/divergence-refresh-error-dominant.md) |
| `stellarindex_divergence_no_reference` | `rate(divergence_refresh_total{outcome="no_reference"}[5m]) > rate(...{outcome="ok"}[5m])` | sustained 30 min | ticket | [divergence-no-reference](runbooks/divergence-no-reference.md) |

## Aggregator alerts

| Name | Metric | Condition | Severity | Runbook |
| ---- | ------ | --------- | -------- | ------- |
| `stellarindex_aggregator_silent` | `rate(stellarindex_aggregator_vwap_writes_total[5m])` | == 0 for > 5 min | page | [aggregator-silent](runbooks/aggregator-silent.md) |
| `stellarindex_aggregator_outlier_storm` | `max by (pair) / min by (pair)` of `stellarindex_aggregator_venue_vwap{window="5m"}` − 1, ≥ 2 venues | > 1 % for > 15 min | ticket | [aggregator-outlier-storm](runbooks/aggregator-outlier-storm.md) |
| `stellarindex_aggregator_outlier_trim_fraction` | `1 − window_trades{stage="outlier"} / window_trades{stage="class"}` on the 24h window, ≥ 20 trades | > 0.2 for > 30 min | ticket | [aggregator-outlier-storm](runbooks/aggregator-outlier-storm.md) |
| `stellarindex_aggregator_outlier_trim_rate_legacy` | `sum by (pair) rate(stellarindex_aggregator_dropped_trades_total{reason="outlier"}[10m])` — the pre-2026-08-28 counter gate, renamed for a one-week overlap; **retire 2026-09-04** | > 10/s for > 2 h | ticket | [aggregator-outlier-storm](runbooks/aggregator-outlier-storm.md) |
| `stellarindex_aggregator_class_drop_spike` | `rate(stellarindex_aggregator_dropped_trades_total{reason="class"}[10m])` | > 10× baseline (offset 1h) for > 15 min | ticket | [aggregator-class-drop-spike](runbooks/aggregator-class-drop-spike.md) |
| `stellarindex_aggregator_fx_snap_fallback_dominant` | `rate(stellarindex_aggregator_fx_snap_fallback_total[15m]) / rate(stellarindex_aggregator_triangulations_total{outcome="ok"}[15m])` | > 0.5 for > 30 min | ticket | [aggregator-fx-snap-fallback-dominant](runbooks/aggregator-fx-snap-fallback-dominant.md) |
| `stellarindex_aggregator_triangulation_chains_dry` | `rate(stellarindex_aggregator_triangulations_total{outcome="missing_leg"}[15m])` > 0 **and** `rate(...{outcome="ok"}[15m])` == 0 | for > 30 min | ticket | [aggregator-triangulation-chains-dry](runbooks/aggregator-triangulation-chains-dry.md) |
| `stellarindex_aggregator_cache_write_errors` | `rate(stellarindex_aggregator_vwap_cache_write_errors_total[5m])` | > 0 for ≥ 2 min | page | [redis-write-blocked-disk-full](runbooks/redis-write-blocked-disk-full.md) |
| `stellarindex_customer_webhook_delivery_failing` | `rate(stellarindex_customer_webhook_delivery_attempts_total{outcome=~"server_error\|network_error"}[5m])` | > 0.1/s for ≥ 15 min | ticket | [customer-webhook-delivery-failing](runbooks/customer-webhook-delivery-failing.md) |
| `stellarindex_customer_webhook_delivery_exhausted` | `rate(stellarindex_customer_webhook_delivery_attempts_total{outcome="exhausted"}[1h])` | > 0 for ≥ 1h | informational | [customer-webhook-delivery-failing](runbooks/customer-webhook-delivery-failing.md) |
| `stellarindex_customer_webhook_fanout_failing` | `sum by (event_type, reason) (increase(stellarindex_customer_webhook_fanout_failures_total[1h]))` | > 0 for ≥ 5 min (a subscribed customer never got a delivery ROW — no retry exists) | ticket | [customer-webhook-fanout-failing](runbooks/customer-webhook-fanout-failing.md) |
| `stellarindex_usage_rollup_failing` | `rate(stellarindex_usage_rollup_sweeps_total{outcome=~"scan_error\|sink_error"}[15m])` | > 0 for ≥ 30 min | informational | [usage-rollup-failing](runbooks/usage-rollup-failing.md) |
| `stellarindex_dex_tvl_refresh_failing` | `rate(stellarindex_dex_tvl_refresh_total{outcome="error"}[15m])` > 0 **and** `rate(...{outcome="ok"}[15m])` == 0 | for > 30 min (served TVL is a carried-forward snapshot aging silently) | ticket | [dex-tvl-refresh-failing](runbooks/dex-tvl-refresh-failing.md) |
| `stellarindex_sdex_orderbook_maintain_failing` | `rate(stellarindex_sdex_orderbook_maintain_total{outcome=~"load_error\|advance_error"}[15m])` | > 0 for ≥ 30 min (load_error = endpoint stuck warming; advance_error = book drifting from tip) | ticket | [sdex-orderbook-maintain-failing](runbooks/sdex-orderbook-maintain-failing.md) |
| `stellarindex_protocol_events_rollup_failing` | `rate(stellarindex_protocol_events_rollup_sweeps_total{outcome="refresh_error"}[15m])` | > 0 for ≥ 30 min | informational | [protocol-events-rollup-failing](runbooks/protocol-events-rollup-failing.md) |
| `stellarindex_asset_volume_rollup_failing` | `rate(stellarindex_asset_volume_rollup_sweeps_total{outcome="refresh_error"}[15m])` | > 0 for ≥ 30 min | informational | [asset-volume-rollup-failing](runbooks/asset-volume-rollup-failing.md) |
| `stellarindex_nonstandard_decimals_correction_failing` | `increase(stellarindex_nonstandard_decimals_cache_refresh_failures_total[15m]) > 0 or increase(stellarindex_price_serve_declined_nonstandard_decimals_total[15m]) > 0` | > 0 for ≥ 5 min | ticket | [dex-nonstandard-decimals](runbooks/dex-nonstandard-decimals.md) |
| `stellarindex_price_alert_eval_failing` | `rate(stellarindex_price_alert_eval_total{outcome="list_error"}[5m]) > rate(...{outcome="ok"}[5m])` | sustained 30 min | ticket | [price-alert-eval-failing](runbooks/price-alert-eval-failing.md) |
| `stellarindex_assets_popular_priceless` | `stellarindex_assets_popular_priceless > 0` (count of market-popular, priceless, non-withheld assets; market-character volume — single-account-pair wash excluded) | > 0 for ≥ 1 h (a genuinely-traded asset renders priceless with no recorded reason) | ticket | [assets-popular-priceless](runbooks/assets-popular-priceless.md) |
| `stellarindex_priceless_coverage_check_stale` | `(time() - stellarindex_priceless_coverage_check_last_success_unix) > 1800` | for ≥ 30 min (the coverage tripwire wedged — blind to new gaps) | ticket | [assets-popular-priceless](runbooks/assets-popular-priceless.md) |
| `stellarindex_signup_reaper_failing` | `rate(stellarindex_signup_reaper_runs_total{outcome="error"}[6h]) > rate(...{outcome="ok"}[6h])` | sustained 30 min | ticket | [signup-reaper-failing](runbooks/signup-reaper-failing.md) |
| `stellarindex_ratelimit_fail_open` | `sum(rate(stellarindex_ratelimit_fail_open_total[5m]))` | > 0 for ≥ 10 min (rate limiter bypassing on a Redis error) | ticket | [ratelimit-fail-open](runbooks/ratelimit-fail-open.md) |
| `stellarindex_monthly_quota_fail_open` | `sum(rate(stellarindex_monthly_quota_fail_open_total[5m]))` | > 0 for ≥ 10 min (metered-spend ceiling bypassing on a counter read error) | ticket | [monthly-quota-fail-open](runbooks/monthly-quota-fail-open.md) |
| `stellarindex_admin_audit_write_failing` | `sum by (surface) (increase(stellarindex_admin_audit_write_failures_total[1h]))` | > 0 for ≥ 5 min (a privileged mutation committed with no durable audit row) | ticket | [admin-audit-write-failing](runbooks/admin-audit-write-failing.md) |
| `stellarindex_login_code_lockout_table_growing` | `stellarindex_login_code_lockout_rows` **or** `increase(stellarindex_login_code_lockout_errors_total{op="status_check"}[1h])` | rows > 10000, **or** any fail-open, for ≥ 30 min (a table an unauthenticated caller keys, or the code-brute-force bound not being enforced) | ticket | [login-code-lockout-table-growing](runbooks/login-code-lockout-table-growing.md) |
| `stellarindex_auth_reaper_stalled` | `time() - stellarindex_auth_reaper_last_sweep_unix{reaper}` vs `3 × stellarindex_auth_reaper_interval_seconds{reaper}` | a reaper (login_code / magic_link / signup) has not completed a sweep for > 3× its cadence, for ≥ 15 min (its rows gauge is frozen, not healthy) | ticket | [auth-reaper-stalled](runbooks/auth-reaper-stalled.md) |
| `stellarindex_tls_cert_expiring_soon` | `stellarindex_tls_cert_not_after_unix - time()` per host | < 14 days for ≥ 1 h | ticket | [tls-cert-expiring-soon](runbooks/tls-cert-expiring-soon.md) |

## Supply alerts

| Name | Metric | Condition | Severity | Runbook |
| ---- | ------ | --------- | -------- | ------- |
| `stellarindex_supply_cross_check_divergence` | `stellarindex_supply_cross_check_divergence_stroops` per `classic_key` | > 1 stroop for > 5 min | ticket | [supply-cross-check-divergence](runbooks/supply-cross-check-divergence.md) |
| `stellarindex_supply_divergence_high` | `stellarindex_supply_divergence_ratio` per `asset` × `reference` | > 1% for ≥ 1 h | ticket | [supply-divergence](runbooks/supply-divergence.md) |
| `stellarindex_supply_snapshot_unit_failed_alert` | `stellarindex_supply_snapshot_unit_failed` | > 0 for ≥ 30 min | ticket | [supply-snapshot-unit-failed](runbooks/supply-snapshot-unit-failed.md) |
| `stellarindex_supply_snapshot_stale` | `time() - stellarindex_supply_snapshot_last_success_timestamp` | > 36 h for ≥ 5 min | ticket | [supply-snapshot-stale](runbooks/supply-snapshot-stale.md) |
| `stellarindex_supply_snapshot_critical_stale` | same | > 72 h for ≥ 5 min | page | [supply-snapshot-stale](runbooks/supply-snapshot-stale.md) |
| `stellarindex_supply_snapshot_never_initialized` | `absent_over_time(stellarindex_supply_snapshot_last_success_timestamp[36h])` | == 1 for ≥ 5 min | ticket | [supply-snapshot-never-initialized](runbooks/supply-snapshot-never-initialized.md) |
| `stellarindex_supply_snapshot_circulating_zero` | `stellarindex_supply_snapshot_circulating_xlm{asset_key="XLM"}` | ≤ 0 for ≥ 5 min | page | [supply-snapshot-circulating-zero](runbooks/supply-snapshot-circulating-zero.md) |
| `stellarindex_aggregator_supply_refresh_stalled` | `time() - max(timestamp(stellarindex_aggregator_supply_refresh_total{outcome="ok"}))` | > 30 min for ≥ 5 min | page | [supply-refresh-stalled](runbooks/supply-refresh-stalled.md) |
| `stellarindex_aggregator_supply_refresh_error_dominant` | error-outcome rate / total-rate | > 50% for ≥ 30 min | ticket | [supply-refresh-error-dominant](runbooks/supply-refresh-error-dominant.md) |
| `stellarindex_aggregator_supply_refresh_never_initialized` | `absent_over_time(stellarindex_aggregator_supply_refresh_total{outcome="ok"}[36h])` | == 1 for ≥ 5 min | ticket | [aggregator-supply-refresh-never-initialized](runbooks/aggregator-supply-refresh-never-initialized.md) |
| `stellarindex_ch_supply_gapfill_failed` | `node_systemd_unit_state{name="ch-supply.service",state="failed"}` | == 1 for ≥ 10 min | ticket | [ch-supply-gapfill-failed](runbooks/ch-supply-gapfill-failed.md) |

## Infra / host alerts

| Name | Metric | Condition | Severity | Runbook |
| ---- | ------ | --------- | -------- | ------- |
| `stellarindex_host_down` | `up` for any host | == 0 for > 2 min | ticket | [host-down](runbooks/host-down.md) |
| `stellarindex_host_cpu_high` | `100 - (avg by (instance) (rate(node_cpu_seconds_total{mode="idle"}[5m])) * 100)` | > 90 % for > 10 min | informational | [host-cpu-high](runbooks/host-cpu-high.md) |
| `stellarindex_host_memory_high` | `(node_memory_MemTotal_bytes - node_memory_MemAvailable_bytes) / node_memory_MemTotal_bytes * 100` | > 90 % for > 10 min | informational | [host-memory-high](runbooks/host-memory-high.md) |
| `stellarindex_zfs_pool_degraded` | `node_zfs_pool_state{state=~"DEGRADED|FAULTED|UNAVAIL"}` | any, for > 60 s | page | [zfs-degraded](runbooks/zfs-degraded.md) |
| `stellarindex_zfs_pool_low_space` | `min by (instance) (node_filesystem_avail_bytes{fstype="zfs"})` | < 1.3 TB free for > 15 min | ticket | [zfs-pool-full](runbooks/zfs-pool-full.md) |
| `stellarindex_zfs_pool_critical_space` | `min by (instance) (node_filesystem_avail_bytes{fstype="zfs"})` | < 650 GB free for > 5 min | page | [zfs-pool-full](runbooks/zfs-pool-full.md) |
| `stellarindex_nvme_smart_warn` | `node_disk_io_errors_total` or SMART attributes | > 0 increase in 1 h | ticket | [nvme-smart](runbooks/nvme-smart.md) |
| `stellarindex_nvme_thermal_throttle` | NVMe `composite_temperature` | > 70 °C for > 5 min | ticket | [nvme-thermal](runbooks/nvme-thermal.md) |
| `stellarindex_nvme_wear_high` | `nvme_percentage_used_ratio` | > 0.80 for > 1 h | ticket | [nvme-smart](runbooks/nvme-smart.md) |
| `stellarindex_nvme_spare_low` | `nvme_available_spare_ratio` | < 0.20 for > 30 min | page | [nvme-smart](runbooks/nvme-smart.md) |
| `stellarindex_nvme_media_errors` | `increase(nvme_media_errors_total[24h])` | > 0 for > 5 min | ticket | [nvme-smart](runbooks/nvme-smart.md) |

## Observability / meta alerts

| Name | Metric | Condition | Severity | Runbook |
| ---- | ------ | --------- | -------- | ------- |
| `stellarindex_prometheus_scrape_failing` | `up{job=~"api\|indexer\|aggregator"}` | == 0 for any target > 2 min | informational | [scrape-failing](runbooks/scrape-failing.md) |
| `stellarindex_alertmanager_config_bad` | `alertmanager_config_last_reload_successful` | == 0 | ticket | [alertmanager-bad-config](runbooks/alertmanager-bad-config.md) |
| `stellarindex_deadmansswitch` | `vector(1)` constant | MUST fire every minute | informational | [deadmansswitch](runbooks/deadmansswitch.md) |
| `prometheus_down` (TSDB corruption) | systemd `prometheus.service` failed | exit-code != 0; runs ad-hoc, not a rule | **P1** | [prometheus-tsdb-corruption](runbooks/prometheus-tsdb-corruption.md) |
| `stellarindex_redis_exporter_down` | `up{job="redis_exporter"}` | == 0 for > 2 min OR series absent for 5 min | page | [exporter-down](runbooks/exporter-down.md) |
| `stellarindex_postgres_exporter_down` | `up{job="postgres_exporter"}` | == 0 for > 2 min OR series absent for 5 min | page | [exporter-down](runbooks/exporter-down.md) |
| `stellarindex_pgbackrest_exporter_down` | `up{job="pgbackrest_exporter"}` | == 0 for > 2 min OR series absent for 5 min | page | [exporter-down](runbooks/exporter-down.md) |
| `stellarindex_minio_exporter_down` | `up{job="minio"}` | == 0 for > 2 min OR series absent for 5 min | page | [exporter-down](runbooks/exporter-down.md) — or [minio-metrics-403](runbooks/minio-metrics-403.md) if the cause is a 403 (bearer-token gap) |
| `stellarindex_metrics_registry_absent` | `stellarindex_metrics_registry_present{component}` | == 0 for 15 min (component running Registry-less → its metrics + dependent alerts are dead) | informational | [metrics-registry-absent](runbooks/metrics-registry-absent.md) |

The four `*_exporter_down` rules close the F-0085 cascade-blindness gap surfaced by the 2026-05-26 audit — each exporter feeds an alert family whose detection silently fails if the exporter dies first. Adding the meta-alert ensures any future cascade surfaces immediately even when the metric-producing exporter is the same process tree dying alongside the failure it's meant to detect. The MinIO scrape specifically has a separate 403-shape failure (missing bearer-token file) documented in [minio-metrics-403](runbooks/minio-metrics-403.md) — operators paged by `stellarindex_minio_exporter_down` whose Prometheus `lastError` shows `HTTP status 403` should consult that runbook first.

The `deadmansswitch` alert is inverse-logic: AlertManager routes it
to a receiver that expects it every minute. If the receiver stops
seeing it, that's the alarm (catches AlertManager-down and
Prometheus-down scenarios).

`prometheus_down` is the disk-full / TSDB-corruption family — same
root cause as `redis-write-blocked-disk-full`. Doesn't have its own
Prometheus rule (Prometheus can't alert on its own absence — that's
what `deadmansswitch` is for); the runbook lives under the catalog
because the *recovery* needs documenting and the apt-shipped
systemd unit's `Restart=on-abnormal` doesn't auto-recover from it.

---

## Informational alerts — delivery register

**Every rule labelled `severity: informational` must have a row
here.** `scripts/ci/lint-alerts-catalog.py` fails CI if one is
missing, if a row names a rule that is no longer `informational`, or
if a row's Triage cell does not begin with one of the two tokens
below. The register exists because `informational` routes to
`receiver: silent`, which has no `*_configs` block and therefore
delivers to **nobody** — so putting a rule in this bucket is a
decision to have no human hear it, and that decision should be
written down rather than inherited from a copied YAML block.

Triage tokens:

- `silent-correct` — genuinely dashboard-only. Nobody needs to be
  told; the condition is unactionable, self-healing, structurally
  unable to fire, or its consequence already has a delivered
  (`ticket` / `page`) alert.
- `needs-delivery` — this should reach a human. It reports data
  loss, a stuck worker, a monitoring blind spot, or a
  customer-visible failure, and no delivered alert covers it.

`needs-delivery` here is a **recommendation, not a routing change**:
nothing in this repo re-routes on these tokens. Deciding what
`informational` should route to (and whether some of these rules
should simply be `ticket`) is issue **#485**, and is deliberately not
settled by this table. Counts as of 2026-09-02: 21 rules —
10 `silent-correct`, 11 `needs-delivery`.

<!-- informational-register:begin -->

| Alert | Component | What a firing tells an operator | Triage (#485) |
| ----- | --------- | ------------------------------- | ------------- |
| `stellarindex_anomaly_freeze_active` | aggregator | At least one ADR-0019 price freeze has been held for 5 min; the affected pairs serve last-known-good with `flags.frozen=true`. | `silent-correct` — a three-tier ladder already delivers around it: `stellarindex_anomaly_freeze_escalated` (page) and `stellarindex_anomaly_freeze_extension_rate` (ticket) cover the abnormal cases. A freeze inside its expected duration is a state, not a fault. |
| `stellarindex_archive_repair_source_degraded` | archive | One archive source failed more than 10% of its repair attempts over the last 25 h verify cycle; the fallback chain is still covering it. | `silent-correct` — the consequence has a delivered escalation (`stellarindex_archive_files_missing`). Caveat for whoever settles #485: the rule's own description says "ticket if the degradation persists > 24 h", which nobody can act on while delivery is nil — if it stays silent, that sentence should go. |
| `stellarindex_asset_volume_rollup_failing` | aggregator | The aggregator's asset-volume rollup worker has been failing its refresh for 30+ min; `/v1/assets` `volume_24h_usd` freezes at its last-good values. | `needs-delivery` — a stuck worker with no sibling at any severity, and the failure mode is stale-served-as-live: the column keeps returning a number that reads as current. Producer is live on r1. |
| `stellarindex_customer_webhook_delivery_exhausted` | api | A customer webhook hit the 15-attempt (~8 h) retry budget and was marked terminally failed; that customer never received the event. | `needs-delivery` — terminal, customer-visible delivery loss, and the rule's own description instructs an operator to contact the customer before they notice. The ticket sibling `stellarindex_customer_webhook_delivery_failing` needs more than 0.1 attempts/s sustained 15 min, which one low-volume customer endpoint never reaches, so this is the only signal for the permanent case. |
| `stellarindex_deadmansswitch` | meta | Fires constantly; the alarm is when it STOPS. | `silent-correct` — not actually silent. The alertname-matched route runs ahead of the severity matchers with `continue: false`, so this one reaches Healthchecks.io and never touches `receiver: silent`. It appears here only because its label is `informational`. No change wanted. |
| `stellarindex_external_poller_error_rate_high` | external-poller | More than half of one external poller's polls errored over 15 min; data is still flowing on the successes. | `silent-correct` — early-warning tier of a delivered alert: if it tips into full staleness, `stellarindex_external_poller_stale` (ticket) fires within 30 min. |
| `stellarindex_external_poller_stale_ecb` | external-poller | The ECB FX poller has not succeeded in 12+ h, i.e. it has missed a whole 6 h cycle. | `needs-delivery` — the generic `stellarindex_external_poller_stale` ticket carves ECB out explicitly (`source!="ecb"`), so this rule is ECB's ONLY coverage and ECB can be dead indefinitely with zero signal. ECB is the authority-sanity cross-check rather than a served price, so this is the lowest-stakes member of the set; but "silent AND carved out of the delivered rule" is a hole, not a decision. |
| `stellarindex_host_cpu_high` | infra | One host has been above 90% CPU for 10 min — either a runaway process or an undersized box; dashboards show the top consumer. | `silent-correct` — expected during backfills and heavy jobs, and the consequences that matter are delivered: `stellarindex_host_down` (ticket), `stellarindex_systemd_unit_failed` (ticket), `stellarindex_worker_panicked` (page). Delivering this would be pure noise. |
| `stellarindex_host_memory_high` | infra | One host has been above 90% memory for 10 min, so the next allocation spike risks an OOMKill; Postgres `shared_buffers` is the usual culprit on a shared box. | `silent-correct` — same ladder as CPU: an actual OOMKill surfaces as `stellarindex_systemd_unit_failed` (ticket) or `stellarindex_worker_panicked` (page), both delivered. Revisit only if a memory-pressure incident is ever missed. |
| `stellarindex_ingestion_decode_error` | ingestion | A source failed to decode more than 1 event/s for 5 min — usually a contract-event-schema change or a decoder regression. | `needs-delivery` — decode failures are silent DATA LOSS: the event is never recorded and no backfill knows to look for it. Nothing else covers them (`stellarindex_decoder_panicked` is for panics, not decode errors). r1 has already accumulated 41,118 decode errors on `sdex` and 3 on `bitstamp`; the counter is real and nobody has ever been told. |
| `stellarindex_ingestion_discovery_drops` | ingestion | The SEP-41 discovery sink dropped hits for 10 min under recorder pressure or buffer saturation. | `silent-correct` — saturation, not loss: a dropped contract re-appears on its next event, so the condition is self-healing. Its hard-failure twin `stellarindex_ingestion_discovery_record_failures` is the one that needs an answer. |
| `stellarindex_ingestion_discovery_record_failures` | ingestion | Writes to `discovered_assets` failed for 10 min; the table stops growing while it persists. | `needs-delivery` — the description's second cause, a `discovered_assets` schema or constraint fault, produces no other signal and stops asset discovery indefinitely. Only the first cause (a Postgres outage) lights other tickets. A stuck writer with a silent failure mode. |
| `stellarindex_ingestion_orphan_events` | ingestion | Events are arriving without their correlation partner (Soroswap swap-without-sync, a partial Phoenix 8-field swap) above 10/min for 15 min. | `needs-delivery` — an orphan is a trade that is never reconstructed, i.e. data loss, and it is the leading indicator of the in-place contract-upgrade class CLAUDE.md warns about. No sibling at any severity. Producer live on r1 for `source="soroswap"`. |
| `stellarindex_metrics_registry_absent` | observability | A component started WITHOUT a Prometheus Registry, so the metrics it would export are unregistered and every alert built on them can never fire. | `needs-delivery` — this is the meta-alert for monitoring blindness, and routing it to a receiver with no delivery is the defect describing itself. Nothing else detects the condition. It is inert on r1 today (the only `stellarindex_metrics_registry_present` series is the known `component="ledgerstream"` one the expr excludes), which means it fires only on a NEW regression — exactly when someone must hear it. |
| `stellarindex_price_divergence_warning` | divergence | Our aggregated price is more than 5% from the reference source, sustained 2 min. | `silent-correct` — two-tier by design: `stellarindex_price_divergence_critical` (ticket, 10%) is delivered. Both are inert anyway, because `stellarindex_our_price` and `stellarindex_reference_price` have no producer (no series on r1) — a separate defect from #485, already recorded in the rule file. |
| `stellarindex_prometheus_scrape_failing` | meta | A scrape target has been down 2+ min; visibility into that subsystem is gone until it returns. | `needs-delivery` — on r1 this is the ONLY `up == 0` rule covering `stellarindex-indexer`, `stellarindex-aggregator`, `galexie`, `caddy` and `prometheus` (the API has a page, `node_exporter` a ticket, the four exporters their own pages). A dead scrape target silently stops every alert built on that target's metrics: the same blindness class as `stellarindex_metrics_registry_absent`. Noted in passing for whoever picks this up — r1 also scrapes an `alertmanager` job that no `up == 0` rule covers at any severity. |
| `stellarindex_protocol_events_rollup_failing` | aggregator | The protocol-events rollup worker has been failing for 30+ min; `/v1/protocols` `events_24h` freezes at its last-good values. | `needs-delivery` — same shape as the asset-volume rollup: stuck worker, no sibling, stale numbers served as current. |
| `stellarindex_stellar_archive_publish_fail` | stellar | stellar-core failed to publish a checkpoint to our history archive. | `silent-correct` — structurally inert: nothing in the tree emits `stellarindex_stellar_archive_publish_errors_total` (allow-listed as known-inert in `lint-metric-refs.sh`; no series on r1) and r1 publishes no history archive. It cannot fire, so delivery is moot. Re-triage when the emitter lands with Phase-3 (ADR-0004). |
| `stellarindex_timescale_compression_lag` | storage | Chunks older than 7 days are still uncompressed after 24 h; the compression policy or the TimescaleDB job scheduler is misfiring. | `silent-correct` — conditional on the row below. The consequence is delivered (`stellarindex_zfs_pool_low_space` ticket, `stellarindex_zfs_pool_critical_space` page) and the root cause is `stellarindex_timescale_job_failures_climbing`, which this register recommends delivering. If that one stays silent, this one must not. |
| `stellarindex_timescale_job_failures_climbing` | storage | More than 10 failed TimescaleDB background-job runs in 6 h on one hypertable. | `needs-delivery` — the rule's own description says the caggs still look fresh and no staleness ticket will fire, and that "this is exactly how the 2026 background-worker starvation stayed invisible". An alert written specifically to make a silent failure mode visible, then delivered nowhere. 72 series live on r1. |
| `stellarindex_usage_rollup_failing` | api | The API's 5-minute usage-rollup sweeps (Redis scan or Timescale upsert) have failed for 30+ min; per-endpoint analytics and `/v1/account/usage` stop advancing. | `needs-delivery` — a stuck worker with a hard deadline: the counters survive in Redis on a 35-day TTL, after which the loss is permanent, and this is the per-customer usage surface. No sibling at any severity. |

<!-- informational-register:end -->

---

## Rules of thumb

- **Every alert has a runbook.** No exceptions. CI check enforces.
- **Alerts that page oncall must be actionable.** If the runbook
  is "wake up, check the dashboard, probably go back to bed", the
  alert belongs at `ticket`, not `page`. It does **not** belong at
  `informational` unless nobody needs to see it at all —
  `informational` is not a quieter ticket, it is no delivery (see
  the Severity legend and the
  [delivery register](#informational-alerts--delivery-register)).
- **Alerts fire on meaningful windows.** A 5-second blip that
  self-resolves should not page someone; the `for:` clause is
  mandatory on every rule.
- **Duplicate alerts are a smell.** If two rules fire on the same
  root cause, consolidate. Oncall shouldn't be paged twice for the
  same incident.
- **Every alert has a test.** Synthetic fixture → AlertManager →
  stub receiver → assert the right page fires. CI target
  `make test-alerts` (TBD) exercises this.

---

## Adding an alert

1. Define the metric in the code that exposes it
   (`internal/obs/*.go`); add to
   `docs/reference/metrics/README.md` (generated).
2. Write the Prometheus rule in `deploy/monitoring/rules/<area>.yml`.
3. Write the runbook at `docs/operations/runbooks/<name>.md` —
   copy `_template.md`.
4. Add a row to this catalogue.
5. Write an alert-firing test at `test/monitoring/<name>_test.yml`.

All five in one PR. The lint enforces the most-load-bearing
piece (`scripts/ci/lint-docs.sh` §9 — every rule's
`runbook_url` must point at an existing runbook file); the
metric-doc and catalogue-row checks catch the two next-most
common drifts. The alert-firing test at
`test/monitoring/<name>_test.yml` is not yet machine-checked
(`test/monitoring/` doesn't exist as a directory today) — write
it anyway as part of the same PR; the convention precedes the
enforcement.

---

## References

- [sev-playbook.md](sev-playbook.md) — response timelines each
  severity binds to.
- [runbooks/](runbooks/) — per-alert response steps.
- [runbooks/entry-walk-renumbering.md](runbooks/entry-walk-renumbering.md) —
  NOT an alert: a deploy-time procedure that must run whenever
  `dispatcher.EntryWalkVersion` is bumped. Listed here because it is the
  one non-alert runbook an on-call may be handed mid-incident — a skipped
  renumbering repair surfaces later as a widening
  `stellarindex_supply_divergence`, and the obvious fix (re-derive) is
  silently discarded by the `intra_ledger_seq` guard.
- [repo-hygiene-plan.md §16](../architecture/repo-hygiene-plan.md#16-observability-discipline) —
  "no alert without a runbook" rule.
- External:
  - Prometheus best practices — <https://prometheus.io/docs/practices/alerting/>
  - The "USE method" — utilisation / saturation / errors.
