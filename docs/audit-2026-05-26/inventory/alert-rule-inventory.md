# Alert Rule Inventory

Generated 2026-05-26T21:43:29Z.

## Multi-host rules: `deploy/monitoring/rules/`

### aggregator.yml
          - alert: ratesengine_aggregator_silent
          - alert: ratesengine_aggregator_outlier_storm
          - alert: ratesengine_aggregator_fx_snap_fallback_dominant
          - alert: ratesengine_aggregator_class_drop_spike
          - alert: ratesengine_aggregator_cache_write_errors

### anomaly.yml
          - alert: ratesengine_anomaly_freeze_engaged
          - alert: ratesengine_anomaly_freeze_sustained
          - alert: ratesengine_anomaly_freeze_recovery_stalled

### api.yml
          - alert: ratesengine_api_down
          - alert: ratesengine_api_latency_p95_high
          - alert: ratesengine_api_latency_p99_high
          - alert: ratesengine_api_error_rate_high
          - alert: ratesengine_api_error_rate_critical
          - alert: ratesengine_api_price_stale
          - alert: ratesengine_api_cache_miss_rate_high
          - alert: ratesengine_customer_webhook_delivery_failing
          - alert: ratesengine_customer_webhook_delivery_exhausted
          - alert: ratesengine_stripe_platform_sync_errors

### archive-completeness.yml
          - alert: ratesengine_archive_files_missing
          - alert: ratesengine_archive_completeness_stale
          - alert: ratesengine_archive_completeness_critical_stale
          - alert: ratesengine_archive_repair_source_degraded

### cache.yml
          - alert: ratesengine_redis_master_down
          - alert: ratesengine_redis_memory_saturated
          - alert: ratesengine_redis_evictions_high
          - alert: ratesengine_redis_replication_broken

### divergence.yml
          - alert: ratesengine_price_divergence_warning
          - alert: ratesengine_price_divergence_critical
          - alert: ratesengine_oracle_stale
          - alert: ratesengine_divergence_refresh_error_dominant

### external-pollers.yml
          - alert: ratesengine_external_poller_stale
          - alert: ratesengine_external_poller_stale_ecb
          - alert: ratesengine_external_poller_error_rate_high

### galexie-archive.yml
          - alert: ratesengine_galexie_archive_tip_lag_high
          - alert: ratesengine_galexie_archive_tip_lag_severe
          - alert: ratesengine_galexie_archive_tip_lag_metric_stale

### infra.yml
          - alert: ratesengine_host_down
          - alert: ratesengine_host_cpu_high
          - alert: ratesengine_host_memory_high
          - alert: ratesengine_zfs_pool_degraded
          - alert: ratesengine_nvme_smart_warn
          - alert: ratesengine_nvme_thermal_throttle

### ingestion.yml
          - alert: ratesengine_ingestion_source_stopped
          - alert: ratesengine_ingestion_source_stopped_low_volume_dex
          - alert: ratesengine_ingestion_source_stopped_daily_publisher
          - alert: ratesengine_ingestion_all_sources_stopped
          - alert: ratesengine_ingestion_cursor_stuck
          - alert: ratesengine_ingestion_orphan_events
          - alert: ratesengine_ingestion_decode_error
          - alert: ratesengine_ingestion_discovery_drops
          - alert: ratesengine_ingestion_insert_errors

### ledgerstream-tier.yml
          - alert: ratesengine_ledgerstream_tier_both_missing

### meta.yml
          - alert: ratesengine_prometheus_scrape_failing
          - alert: ratesengine_alertmanager_config_bad
          - alert: ratesengine_deadmansswitch

### sla-probe.yml
          - alert: ratesengine_sla_probe_p95_breach
          - alert: ratesengine_sla_probe_freshness_breach
          - alert: ratesengine_sla_probe_unit_failed_alert
          - alert: ratesengine_sla_probe_stale

### slo.yml
          - alert: ratesengine_slo_latency_burn_fast
          - alert: ratesengine_slo_latency_burn_medium
          - alert: ratesengine_slo_latency_burn_slow
          - alert: ratesengine_slo_availability_burn_fast
          - alert: ratesengine_slo_availability_burn_medium
          - alert: ratesengine_slo_availability_burn_slow

### stellar.yml
          - alert: ratesengine_stellar_core_ledger_age
          - alert: ratesengine_stellar_core_peers_low
          - alert: ratesengine_stellar_rpc_lag
          - alert: ratesengine_stellar_archive_publish_fail
          - alert: ratesengine_stellar_archive_divergence

### storage.yml
          - alert: ratesengine_timescale_primary_down
          - alert: ratesengine_timescale_replica_lag
          - alert: ratesengine_timescale_lock_table_pressure
          - alert: ratesengine_timescale_disk_full
          - alert: ratesengine_timescale_disk_warning
          - alert: ratesengine_node_root_disk_full
          - alert: ratesengine_node_root_disk_warning
          - alert: ratesengine_redis_writes_blocked
          - alert: ratesengine_timescale_connections_saturated
          - alert: ratesengine_timescale_cagg_stale
          - alert: ratesengine_timescale_compression_lag
          - alert: ratesengine_timescale_backup_failed
          - alert: ratesengine_timescale_backup_none_24h

### supply-refresh.yml
          - alert: ratesengine_aggregator_supply_refresh_stalled
          - alert: ratesengine_aggregator_supply_refresh_never_initialized
          - alert: ratesengine_aggregator_supply_refresh_error_dominant

### supply-snapshot.yml
          - alert: ratesengine_supply_snapshot_unit_failed_alert
          - alert: ratesengine_supply_snapshot_stale
          - alert: ratesengine_supply_snapshot_critical_stale
          - alert: ratesengine_supply_snapshot_never_initialized
          - alert: ratesengine_supply_snapshot_circulating_zero

### supply.yml
          - alert: ratesengine_supply_cross_check_divergence

### verify-archive.yml
          - alert: ratesengine_verify_archive_unit_failed
          - alert: ratesengine_verify_archive_run_stale

## R1 overlay: `configs/prometheus/rules.r1/`

### aggregator.yml
          - alert: ratesengine_aggregator_silent
          - alert: ratesengine_aggregator_outlier_storm
          - alert: ratesengine_aggregator_fx_snap_fallback_dominant
          - alert: ratesengine_aggregator_class_drop_spike
          - alert: ratesengine_aggregator_cache_write_errors

### anomaly.yml
          - alert: ratesengine_anomaly_freeze_engaged
          - alert: ratesengine_anomaly_freeze_sustained
          - alert: ratesengine_anomaly_freeze_recovery_stalled

### api.yml
          - alert: ratesengine_api_down
          - alert: ratesengine_api_latency_p95_high
          - alert: ratesengine_api_latency_p99_high
          - alert: ratesengine_api_error_rate_high
          - alert: ratesengine_api_error_rate_critical
          - alert: ratesengine_api_price_stale
          - alert: ratesengine_api_cache_miss_rate_high
          - alert: ratesengine_customer_webhook_delivery_failing
          - alert: ratesengine_customer_webhook_delivery_exhausted
          - alert: ratesengine_stripe_platform_sync_errors

### archive-completeness.yml
          - alert: ratesengine_archive_files_missing
          - alert: ratesengine_archive_completeness_stale
          - alert: ratesengine_archive_completeness_critical_stale
          - alert: ratesengine_archive_repair_source_degraded

### cache.yml
          - alert: ratesengine_redis_master_down
          - alert: ratesengine_redis_memory_saturated
          - alert: ratesengine_redis_evictions_high
          - alert: ratesengine_redis_replication_broken

### divergence.yml
          - alert: ratesengine_price_divergence_warning
          - alert: ratesengine_price_divergence_critical
          - alert: ratesengine_oracle_stale
          - alert: ratesengine_divergence_refresh_error_dominant

### external-pollers.yml
          - alert: ratesengine_external_poller_stale
          - alert: ratesengine_external_poller_stale_ecb
          - alert: ratesengine_external_poller_error_rate_high

### galexie-archive.yml
          - alert: ratesengine_galexie_archive_tip_lag_high
          - alert: ratesengine_galexie_archive_tip_lag_severe
          - alert: ratesengine_galexie_archive_tip_lag_metric_stale

### infra.yml
          - alert: ratesengine_host_down
          - alert: ratesengine_host_cpu_high
          - alert: ratesengine_host_memory_high
          - alert: ratesengine_zfs_pool_degraded
          - alert: ratesengine_nvme_smart_warn
          - alert: ratesengine_nvme_thermal_throttle

### ingestion.yml
          - alert: ratesengine_ingestion_source_stopped
          - alert: ratesengine_ingestion_source_stopped_low_volume_dex
          - alert: ratesengine_ingestion_source_stopped_daily_publisher
          - alert: ratesengine_ingestion_all_sources_stopped
          - alert: ratesengine_ingestion_cursor_stuck
          - alert: ratesengine_ingestion_orphan_events
          - alert: ratesengine_ingestion_decode_error
          - alert: ratesengine_ingestion_discovery_drops
          - alert: ratesengine_ingestion_insert_errors

### ledgerstream-tier.yml
          - alert: ratesengine_ledgerstream_tier_both_missing

### meta.yml
          - alert: ratesengine_prometheus_scrape_failing
          - alert: ratesengine_alertmanager_config_bad
          - alert: ratesengine_deadmansswitch

### sla-probe.yml
          - alert: ratesengine_sla_probe_p95_breach
          - alert: ratesengine_sla_probe_freshness_breach
          - alert: ratesengine_sla_probe_unit_failed_alert
          - alert: ratesengine_sla_probe_stale

### slo.yml
          - alert: ratesengine_slo_latency_burn_fast
          - alert: ratesengine_slo_latency_burn_medium
          - alert: ratesengine_slo_latency_burn_slow
          - alert: ratesengine_slo_availability_burn_fast
          - alert: ratesengine_slo_availability_burn_medium
          - alert: ratesengine_slo_availability_burn_slow

### stellar.yml
          - alert: ratesengine_stellar_core_ledger_age
          - alert: ratesengine_stellar_core_peers_low
          - alert: ratesengine_stellar_rpc_lag
          - alert: ratesengine_stellar_archive_publish_fail
          - alert: ratesengine_stellar_archive_divergence

### storage.yml
          - alert: ratesengine_timescale_primary_down
          - alert: ratesengine_timescale_replica_lag
          - alert: ratesengine_timescale_lock_table_pressure
          - alert: ratesengine_timescale_disk_full
          - alert: ratesengine_timescale_disk_warning
          - alert: ratesengine_node_root_disk_full
          - alert: ratesengine_node_root_disk_warning
          - alert: ratesengine_redis_writes_blocked
          - alert: ratesengine_timescale_connections_saturated
          - alert: ratesengine_timescale_cagg_stale
          - alert: ratesengine_timescale_compression_lag
          - alert: ratesengine_timescale_backup_failed
          - alert: ratesengine_timescale_backup_none_24h

### supply-refresh.yml
          - alert: ratesengine_aggregator_supply_refresh_stalled
          - alert: ratesengine_aggregator_supply_refresh_never_initialized
          - alert: ratesengine_aggregator_supply_refresh_error_dominant

### supply-snapshot.yml
          - alert: ratesengine_supply_snapshot_unit_failed_alert
          - alert: ratesengine_supply_snapshot_stale
          - alert: ratesengine_supply_snapshot_critical_stale
          - alert: ratesengine_supply_snapshot_never_initialized
          - alert: ratesengine_supply_snapshot_circulating_zero

### supply.yml
          - alert: ratesengine_supply_cross_check_divergence

### verify-archive.yml
          - alert: ratesengine_verify_archive_unit_failed
          - alert: ratesengine_verify_archive_run_stale

