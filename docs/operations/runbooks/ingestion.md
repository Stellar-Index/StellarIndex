---
title: Runbook — ingestion
last_verified: 2026-10-06
status: living
severity: P1
---

# Runbook — ingestion alerts

Rules: `configs/prometheus/rules.r1/ingestion.yml` and `ledgerstream-tier.yml` (twins under `deploy/monitoring/rules/`). Indexer metrics port on r1 is `:9464` (loopback; query from r1). Production ingest is `Galexie MinIO -> ledgerstream -> dispatcher` ([ingest-pipeline.md](../../architecture/ingest-pipeline.md)); r1 runs no stellar-rpc, so use a public endpoint for `rpc-probe`.

## At a glance

- [`stellarindex_ingestion_all_sources_stopped`](#stellarindex_ingestion_all_sources_stopped)
- [`stellarindex_ingestion_source_stopped`](#stellarindex_ingestion_source_stopped)
- [`stellarindex_ingestion_decode_error`](#stellarindex_ingestion_decode_error)
- [`stellarindex_decoder_panicked`](#stellarindex_decoder_panicked)
- [`stellarindex_ingestion_lag_high`](#stellarindex_ingestion_lag_high)
- [`stellarindex_ledgerstream_tier_both_missing`](#stellarindex_ledgerstream_tier_both_missing)
- [`stellarindex_ingestion_discovery_drops`](#stellarindex_ingestion_discovery_drops)
- [`stellarindex_ingestion_dispatcher_tx_skips`](#stellarindex_ingestion_dispatcher_tx_skips)

## stellarindex_ingestion_all_sources_stopped

P1 page: `sum(rate(stellarindex_source_events_total[5m]))` = 0 for > 3 min. MTTR 5-20 min. Price staleness starts at the 60 s cache TTL and the API sets `stale_flag=true` globally; `stellarindex_api_price_stale` follows ~7 min later; past 30 min the 30 s freshness SLA is breached.

Check (shared upstream is Galexie + MinIO, then Timescale, then the indexer itself):

```sh
# 1. Is the indexer running?
systemctl status stellarindex-indexer      # systemd-managed binary on r1

# 2. What do its logs say?
journalctl -u stellarindex-indexer -n 200 --no-pager | tail -40
# Look for: "ledgerstream exited with error" (cmd/stellarindex-indexer/main.go),
# "insert trade failed" (internal/pipeline/sink.go),
# "postgres ping failed 3x — pool may be wedged" (main.go), and the
# ledgerstream tier-read outcome "both_missing" (see
# ingestion.md#stellarindex_ledgerstream_tier_both_missing).

# 3. Is Galexie producing fresh ledger objects?
sudo journalctl -u galexie -n 50 --no-pager
mc ls --json --recursive local/galexie-live/ \
  | jq -r 'select(.key|test("\\.xdr\\.zst$")) | .lastModified' | sort -r | head -1
# newest ledger object; one lands every ~5 s (galexie_ledgers_per_file: 1).
# The mc alias on r1 is `local`; a non-recursive `mc ls` only lists partition prefixes.
# Is the network itself closing ledgers (public endpoint)?
stellarindex-ops rpc-probe https://mainnet.sorobanrpc.com
# Expect: version info + latest ledger close time within 60s.

# 4. Is Timescale reachable + writable? (local peer auth)
sudo -u postgres psql -d stellarindex -c "INSERT INTO ingestion_cursors (source, sub_source, last_ledger) VALUES ('probe', 'healthcheck', 0) ON CONFLICT DO NOTHING;"
sudo -u postgres psql -d stellarindex -c "DELETE FROM ingestion_cursors WHERE source='probe';"  # remove the probe row (it otherwise shows in `stellarindex-ops list-cursors`)
```

Likely causes:

- No fresh objects in `galexie-live`: galexie's captive-core stalled or upstream network issue.
- Fresh objects but indexer not reading: firewall/DNS, or the `[storage]` section of `/etc/stellarindex.toml` (`s3_endpoint`, `s3_bucket_live`) / `STELLARINDEX_S3_ACCESS_KEY` / `STELLARINDEX_S3_SECRET_KEY` in `/etc/default/stellarindex`.
- psql INSERT fails: [timescale-primary-down](postgres.md#stellarindex_timescale_primary_down). Insert backpressure fills the `events` channel by design (ADR-0041: the cursor advances only once a ledger's events are enqueued): fix Postgres per [trade-insert-backpressure](ingestion-sink.md#stellarindex_ingestion_trade_insert_backpressure); only CEX/FX trades drop-oldest (`stellarindex_trade_insert_buffer_depth`, `stellarindex_source_insert_errors_total{kind="dropped"}`; split by `source`, it also counts permanent data faults and shutdown abandons).
- All probes pass, no events: indexer wedged, `ingestion.enabled_sources` set to `[]` (accepted by `internal/config/validate.go`; check startup logs), or a recent deploy.

Fix:

1. Galexie/MinIO: `systemctl status galexie`, `journalctl -u galexie --since="10 min ago"`, `df -h /var/lib/galexie` (ZFS dataset); a galexie restart clears most captive-core hangs. MinIO: `systemctl status minio`; [exporter-down](meta.md#stellarindex_redis_exporter_down), [minio-metrics-403](infra.md#minio-metrics-403). Newest object should be within ~1 min.
2. Wedged indexer, capture a goroutine dump first (`pgrep stellarindex-indexer` without `-f` matches nothing; signal via systemd):

   ```sh
   systemctl kill --signal=SIGQUIT stellarindex-indexer.service   # goroutine dump to journal, then exit(2)
   sleep 3
   journalctl -u stellarindex-indexer -n 300 --no-pager > /tmp/indexer-dump-$(date +%s).log
   ```

   The unit has `Restart=on-failure` / `RestartSec=10s` and self-restarts; if `systemctl is-active stellarindex-indexer` says otherwise: `systemctl restart stellarindex-indexer`. Recovery: `stellarindex_source_events_total` rate > 0 within 60 s, alert clears within 3 min.
3. Recent deploy broke it (last 4 h): find the previous binary and swap it. Preferred: `gh workflow run deploy.yml -f region=r1 -f version=<previous> -f binaries=stellarindex-indexer` (omit `binaries` to roll the whole set back). Manual fallback ([release-process.md](../release-process.md) "Rollback"; `.prev-<tag>` keeps the last 5):

   ```sh
   git tag --sort=-creatordate | head -10
   ssh root@r1 'for f in /var/lib/stellarindex/deployed-versions/stellarindex-*; do echo "$f: $(cat $f)"; done'
   ssh root@r1 'ls -lh /usr/local/bin/stellarindex-indexer.prev-*'
   PREVIOUS=v0.47.1                         # whichever tag was healthy
   ssh root@r1 <<EOF
     set -euxo pipefail
     test -f /usr/local/bin/stellarindex-indexer.prev-${PREVIOUS}
     systemctl stop stellarindex-indexer
     install -m 0755 /usr/local/bin/stellarindex-indexer.prev-${PREVIOUS} /usr/local/bin/stellarindex-indexer
     echo ${PREVIOUS} > /var/lib/stellarindex/deployed-versions/stellarindex-indexer
     systemctl start stellarindex-indexer
     systemctl status stellarindex-indexer --no-pager
   EOF
   ```

   Rolling back only the indexer trips `stellarindex_binary_version_skew` (ticket, `for: 45m`): expected, see [binary-version-skew](binary-version-skew.md). File a SEV-2 minimum plus a postmortem in `docs/operations/postmortems/`.
4. Gather for the RCA: the goroutine dump, `journalctl -u stellarindex-indexer --since "30 min ago"`, `stellarindex_source_events_total` by source (cliff or decay), `git log` of `cmd/stellarindex-indexer/` (72 h), `pg_stat_activity` lock waits, `journalctl -u galexie` / `-u minio`, `stellarindex_ledgerstream_tier_read_total{outcome="both_missing"}`.

Known false positives: galexie behind after a restart (cold catch-up ~9 min on mainnet; reader sees `both_missing`); the midnight UTC CAGG refresh briefly blocks trade inserts.

Escalate: still zero events after a restart and rollback.

## stellarindex_ingestion_source_stopped

Four alerts route here, all P2 (`severity: ticket`), MTTR 15-60 min. Each requires `stellarindex_source_enabled == 1` joined `on (source)`, so a deliberately disabled source stays quiet. A source in none of the allowlists is covered only by `all_sources_stopped`. Read the alert NAME first:

| Alert | Sources in its allowlist | Fires when | Sustain |
| ----- | ------------------------ | ---------- | ------- |
| `..._source_stopped` | `binance`, `bitstamp`, `coinbase`, `kraken`, `sdex`, `aquarius`, `reflector-dex`, `reflector-cex`, `reflector-fx`, `redstone`, `coingecko` | `rate(...[30m]) == 0` | 15 m |
| `..._source_stopped_low_volume_dex` | `comet`, `phoenix`, `soroswap`, `blend` | `rate(...[24h]) == 0` | 30 m |
| `..._source_stopped_daily_publisher` | `ecb` | `rate(...[30h]) == 0` | 1 h |
| `..._source_stopped_hourly_publisher` | `band` | `rate(...[6h]) == 0` | 1 h |

Check:

```sh
# :9464 is the indexer's metrics port and it is loopback-bound — query from r1.
ssh root@136.243.90.96 'curl -s http://localhost:9464/metrics | \
  grep -E "stellarindex_source_(events_total|enabled|last_event_unix)"'

# Health snapshot for every source's connection state:
stellarindex-ops list-cursors -config /etc/stellarindex.toml

# Is the network closing ledgers and the source contract still emitting?
stellarindex-ops rpc-probe https://mainnet.sorobanrpc.com
```

- On-chain and external sources flat together: [all_sources_stopped](#stellarindex_ingestion_all_sources_stopped).
- On-chain only: ledgerstream/indexer logs and cursor movement (dispatcher path).
- One source only: its filter rejects everything, it is legitimately idle, or a protocol change broke its decoder; check [decode_error](#stellarindex_ingestion_decode_error).

Cadence reference (check BEFORE assuming a source is stopped; "idle cap" is an operator judgement, not the alert threshold):

| Source | Cadence / idle cap | First look when stopped |
| ------ | ------------------ | ----------------------- |
| `sdex` | continuous in US/EU hours; 30 min off-hours | `stellarindex-ops hubble-check`; trades Hubble has that we lack = decoder regression. |
| `soroswap`, `aquarius` | pool-driven; 30 min | Soroban-RPC `getEvents` for the contract: on-chain events but none in ours = decoder regression or stuck cursor. |
| `phoenix` | low, off-peak gaps common; 45 min | As soroswap; its 8-event swap shape means partial decode-error storms mimic source-stopped. Measured 30-day max swap gap 8 h 28 m. |
| `comet` | one curated pool; 45 min | Decoder is contract-identity gated to `comet.MainnetGatedSet()` (today the Blend BLND/USDC backstop; ADR-0035/0040), so silence means that pool is quiet. Admitting a new pool is a code change plus redeploy; `seed-protocol-contracts -source comet` only re-upserts the in-code set. |
| `blend` | auction-driven; 90 min | Verify an auction window is active. |
| `band` | relayer push, hourly (divergence.md#stellarindex_oracle_stale); ~1 h | Band emits zero events; it is read from `InvokeContract` op args via the dispatcher's `ContractCallDecoder`. Verify that is wired and the contract is still relayed to. |
| `redstone` | ~1 min batches; 10 min active / 30 min off-peak | Event topic `"REDSTONE"`, no `feed_id` in the body (it lives in OpArgs); verify the OpArgs plumbing. |
| `reflector` (3 contracts) | 15 min DEX/CEX, 60 min FX | Confirm WHICH contract is silent. If the lake `contract_events` (topic_0=REFLECTOR) shows fresh events but every `oracle_updates` row has the same stale `ts`, Reflector's relayer is re-pushing the same `last_update_timestamp`: our decoder is right, data is stale upstream; raise it with Reflector. |
| `binance`, `kraken`, `bitstamp`, `coinbase` | sub-second; 60 s anomalous, 5 min broken | WebSocket streamers: silence usually means a silently dropped WS; check streamer-error metrics and reconnect logs. |
| `coingecko` | 60 s poll; 5 min | [CoinGecko 429 pattern](external-pollers.md#coingecko-429-pattern). |
| `ecb` | once per business day ~16:00 CET; 24 h weekdays / 72 h weekends and holidays | Cross-check TARGET2 closing days. |

Silence inside the idle cap for an off-peak source is a false positive; do not widen a window by hand. The rules ARE the cadence tiers: move the source between the allowlists in `configs/prometheus/rules.r1/ingestion.yml` AND `deploy/monitoring/rules/ingestion.yml` (the pair lint needs both), then `make monitoring-check`. Aquarius, in the 30 m tier, is the usual candidate for the low-volume-DEX tier. A firing low-volume or publisher tier already means well past the venue's normal quiet stretch: treat it as real.

Fix:

1. Isolated to one or a few sources and the host is healthy: restart the indexer (one `stellarindex-indexer.service` on r1):

   ```sh
   ssh root@136.243.90.96 "systemctl restart stellarindex-indexer && \
     systemctl status stellarindex-indexer --no-pager | head -10"
   ```

2. Events flow 1-2 min then stop again: source legitimately idle, misconfigured, or upstream schema drift.
3. `decode_error` also firing: the event shape changed, follow [decode_error](#stellarindex_ingestion_decode_error) Fix step 3.
4. Verify `rate(stellarindex_source_events_total{source=...}[5m]) > 0` within 2 min.

Immediately after a deploy a restart shows zero events briefly; every tier's sustain covers it. Escalate: several sources stop ([all_sources_stopped](#stellarindex_ingestion_all_sources_stopped)).

## stellarindex_ingestion_decode_error

P3: `rate(stellarindex_source_decode_errors_total{source=...}[5m]) > 1` for 5 min. MTTR hours to days. Each failure is one lost observation; the SCVal/XDR shape did not match, an amount was out of range, or an asset code/strkey failed validation. Distinct from `orphan-events` ([ingestion-events.md](ingestion-events.md#stellarindex_ingestion_orphan_events)) and `insert-errors` ([ingestion-sink.md](ingestion-sink.md#stellarindex_ingestion_insert_errors)).

Check:

```sh
ssh root@136.243.90.96 \
  'curl -s http://localhost:9464/metrics | grep stellarindex_source_decode_errors_total'

# Source logs rejections at debug; enable temporarily if the level is info.
ssh root@136.243.90.96 "journalctl -u stellarindex-indexer -n 500 --no-pager" \
  | grep -iE "decode|parse|malformed" | tail -30

# Does the contract still emit, and with what topic shape / protocolVersion?
stellarindex-ops rpc-probe https://mainnet.sorobanrpc.com
```

Likely causes, most frequent first:

- The contract upgraded its event shape (check the DEX's release notes).
- A Stellar protocol bump (CAP-67/P23 changed classic asset events); `rpc-probe`'s `protocolVersion` shows it.
- A decoder regression after an indexer deploy: `git log --oneline internal/sources/<source>/` over the post-deploy window; revert is the fastest fix.
- An off-schedule admin tx (pause, upgrade) emitting swap-like events; clears on its own.

Per-source first look:

| Source | Common decode-regression cause | First place to look |
| ------ | ------------------------------ | ------------------- |
| `soroswap` | `SwapEvent` has no post-state reserves; they come in the next `SyncEvent`. | The `orphan-events` rate. |
| `phoenix` | 8 events per swap (2-tuple topic `("swap", "<field>")`); a short group looks malformed. | Group by `(ledger, tx_hash, op_index)`. |
| `comet` | Shared `("POOL", <event>)` topic across every Balancer-v1 deployment; the decoder is contract-identity gated (ADR-0035/0040) and a copycat emitter fails closed, so a complaint is more often "a real pool we have not admitted" than a schema change. | Is the emitter in `comet.MainnetGatedSet()`? Admitting one is a code change plus redeploy. Foreign emitters surface in the completeness recognition audit. |
| `reflector` | Three separate contracts (DEX/CEX/FX); no on-chain `twap` or `x_*` methods. | Which contract is decoding. |
| `band` | Zero events; read from `relay()` / `force_relay()` op args via `ContractCallDecoder`. Pair rates are E18, relayed single-asset rates E9. | The `ContractCallDecoder` wiring. |
| `redstone` | Topic `"REDSTONE"`, no `feed_id` in the body; feed ids come from `write_prices(updater, feed_ids, payload)` op args (`events.Event.OpArgs`). A feed count mismatch first tries recovery (`resolveFeedAttribution`), and refuses the whole event only when recovery fails. | OpArgs plumbing, then which sentinel the log names: `ErrAmbiguousSubset` (recovery could not attribute uniquely), `ErrFeedIDCountMismatch`, `ErrStateWriteFeedMismatch` (claimed names disagree with what the contract stored). |
| `sdex` | Two post-P23 shapes handled (3-topic SEP-41 and 4-topic CAP-67 with `sep0011_asset`); a bump may add a third. No operations+effects fallback exists; pre-P23 classic movement is rebuilt from the lake (`internal/sources/classicmovements`, ADR-0047/0048). | `protocolVersion` from `rpc-probe`. |
| SEP-41 tokens | `transfer` data is a bare `i128` OR a map with `amount` + `to_muxed_id`. | Type-test before `MustI128()`. |
| `accounts`, `trustlines`, `claimable_balances`, `liquidity_pools`, `sac_balances` | LedgerEntry-based supply observers (`internal/pipeline/dispatcher.go`), not event decoders: a malformed entry change, not a schema shift. Not on the `compute-completeness`/`verify-reconciliation` axis; no re-derive tool and no ledger-gap target (rows only exist when an entry changes). Only the account observer has a watermark (`account_observer_watermark`, migration 0144); a dead observer cannot be told from a quiet one for the other four (open gap). | The indexer debug log for the LCM entry-change shape. |

After a protocol bump or DEX redeploy the WASM likely changed shape and backfill is unsafe across the boundary until the WASM-hash audit re-runs ([wasm-audits](../wasm-audits/), [contract schema evolution](../../architecture/ingest-pipeline.md#contract-schema-evolution)); flip `BackfillSafe = false` for that source until the audit log shows the new hash decodes cleanly.

Fix (no runtime mitigation; dropped events cannot be un-dropped):

1. Identify the cause above; if transient, wait.
2. Contract upgrade: update `internal/sources/<source>/decode.go` (one PR plus a golden-file fixture), then re-derive. Projected (Soroban) sources rewind the projector; dry run is the DEFAULT, `-write` is required (stderr banner `═══ DRY RUN — no writes; pass -write to apply ═══`); use `projected-rebuild` beyond ~1M ledgers:

   ```sh
   stellarindex-ops projector-replay -config /etc/stellarindex.toml -source <name> -from <ledger> -write
   ```

   Non-projected sources (`sdex`, external CEX/FX, `band`, `soroswap-router`) use `stellarindex-ops backfill -write -config /etc/stellarindex.toml -from N -to N -source <name>`, which refuses any source not `BackfillSafe` in `internal/sources/external/registry.go`. On r1 a re-derive is a heavy one-shot: `/usr/local/sbin/run-heavy-job.sh`, one at a time.
3. Regression: `git revert` the commit and deploy.
4. Verify the rate is under 1/s within 5 min.

If `stellarindex_aggregator_class_drop_spike` co-fires, the source dropped out of the VWAP for some pairs; remaining sources still price, but say in status comms that `flags.divergence_warning` may be elevated while price is correct ([drill log](../drills/README.md#drill-log)).

## stellarindex_decoder_panicked

P1 page: `stellarindex_decoder_panics_total > 0`. A decoder's `Matches` or `Decode` panicked; the dispatcher recovered, skipped that input and ingest continues (process up, cursor advancing). A `Decode` panic costs one event; a `Matches` panic aborts that dispatch chain, so other decoders for the same input are skipped too. The projector counts lake-row panics here too (journal `decoder panicked; row SKIPPED`). It pages because the decoder keeps silently dropping every event of that shape until a fixed binary ships. No permanent data loss: the raw event is in the ClickHouse lake.

The alert does not self-clear (`> 0` on a process-lifetime counter; `increase()` would never fire for a single poison event). Symptoms: journal `decoder panicked — input SKIPPED, ingest continues` with `decoder`, `ledger`, `tx_hash`, `op_index`, `panic`, `stack`; `stellarindex_source_decode_errors_total` moved by the same amount; later `/v1/coverage` `complete=false` once `compute-completeness` re-derives the window.

Check:

```sh
# Which decoder, which ledger, what was the fault?
ssh root@136.243.90.96 'journalctl -u stellarindex-indexer --since "-2h" --no-pager | grep -A25 "decoder panicked"'

# One-off or storm? (a storm means an event SHAPE, not one bad event)
ssh root@136.243.90.96 'curl -s "http://localhost:9090/api/v1/query?query=stellarindex_decoder_panics_total" | python3 -m json.tool | grep -E "source|value"'

# Ingest genuinely still moving? If not, this is a different incident.
ssh root@136.243.90.96 'curl -s "http://localhost:9090/api/v1/query?query=stellarindex_cursor_last_ledger" | python3 -m json.tool | grep -E "source|value"'
```

The skip stays visible (ADR-0033): the raw event is already in the lake (`dispatchOne` sinks it before the decoder pass); `compute-completeness` re-derives through the same decoder under `safeDecode` (`internal/completeness/reconcile.go`), marks the ledger `blind.Undecodable` and `/v1/coverage` reports `complete=false`; and `statsflush` persists the decode-error delta to `decoder_stats` every 5 min (`ledger_ingest_log`, migration 0051, is the decoder-independent census). For sdex, `stellarindex-ops verify-reconciliation -source sdex` shows the delta.

Likely causes: an unchecked slice index, `MustI128()` on a map-shaped `transfer`, a nil deref on an optional field; contract schema evolution (Soroswap, Phoenix, Aquarius, Reflector update in place; decode by Map-field-name, not position); attacker-shaped event bodies.

Fix (a deterministic panic needs a code fix):

1. Do NOT restart the indexer to clear the alert; it hides the bug and the unit is healthy.
2. Confirm the cursor advances and `stellarindex_ingestion_all_sources_stopped` is not firing. A climbing counter means a whole event shape: that source is effectively dark. If it must not run blind, remove it from `ingestion.enabled_sources` in `/etc/stellarindex.toml` (via the ansible role) and restart: a deliberate recorded gap.
3. File the bug with the stack and `(ledger, tx_hash, op_index)`; attach the raw lake event for that coordinate.
4. After the fix ships, re-derive from the lake (never a MinIO re-walk, invariant 8) and re-run the verdict:

   ```sh
   # Projected sources (soroswap, blend, phoenix, comet, defindex, sep41_*,
   # reflector/redstone oracle_updates, …):
   stellarindex-ops projector-replay -config /etc/stellarindex.toml \
     -source <name> -from <ledger> -write   # without -write this is a DRY RUN that exits 0

   # Non-projected sources (sdex, band, supply observers):
   stellarindex-ops ch-rebuild ...   # see docs/operations/backfill-procedure.md

   # Then re-run the verdict for the window and confirm it goes green:
   stellarindex-ops compute-completeness -write ...
   ```

See also [worker-panicked](infra.md#stellarindex_worker_panicked) (a panicking background worker stops; here one input is skipped) and [cursor-stuck](ledger-ingest.md#stellarindex_ingestion_cursor_stuck) if the cursor is NOT advancing.

## stellarindex_ingestion_lag_high

Retired: no rule in either tree defines this alert and the per-source lag gauge no longer exists (the current `ledgerstream -> dispatcher` indexer does not emit one). Per-source freshness now comes from `data-freshness.sh` and `source_coverage_snapshots`. Kept as operator context for a source that is behind but not stopped (`source_last_event_unix` still advancing).

Check (on r1 the metrics are at `localhost:9464`):

```sh
# Source processing rate vs production (~0.2 ledger/s); below that it never catches up.
curl -s http://prometheus:9090/api/v1/query --data-urlencode \
  'query=rate(stellarindex_source_events_total{source="<X>"}[5m])'

# Is persistence the bottleneck (insert errors rising)?
curl -s localhost:9464/metrics | grep insert_errors_total
```

Likely causes:

- Just restarted: the cursor resumes from `last_ledger`; lag shrinking means catch-up, wait and estimate ETA from the shrink rate.
- Source slower than the chain (decode + correlate + persist); needs a code PR (profile, batch inserts).
- Write-side bottleneck (`IO:DataFileRead` / `Lock:tuple` in `pg_stat_activity`): [connections_saturated](postgres.md#stellarindex_timescale_connections_saturated), [replica_lag](postgres.md#stellarindex_timescale_replica_lag), [disk_full](postgres.md#stellarindex_timescale_disk_full).
- Upstream RPC lagging: [rpc-lag](stellar-node.md#stellarindex_stellar_rpc_lag) (not on r1).

Fix: separate catching-up from staying-behind (plot lag over 15 min); if stuck, follow the linked runbook; for a large gap detect then backfill (`api.md#stellarindex_api_price_stale` may co-fire):

```sh
# 1. Identify the lagging cursor + the (from, to) range
stellarindex-ops detect-gaps -config /etc/stellarindex.toml \
    -threshold 50
# 2. Backfill the named range. -dry-run first to see scope.
stellarindex-ops backfill -config /etc/stellarindex.toml \
    -from <FIRST_LEDGER> -to <LAST_LEDGER> \
    -source <SOURCE_NAME> -dry-run
# 3. Drop -dry-run to commit.
stellarindex-ops backfill -write -config /etc/stellarindex.toml \
    -from <FIRST_LEDGER> -to <LAST_LEDGER> \
    -source <SOURCE_NAME> -resume
```

For projected Soroban sources (soroswap, blend, phoenix, comet, cctp, rozo, defindex, sep41, reflector/redstone, ...) catch-up is `stellarindex-ops projector-replay -source <name> -from <ledger>`, NOT `backfill` (ADR-0032); `backfill` refuses Soroban sources whose `BackfillSafe` flag is false. On r1 either runs under `/usr/local/sbin/run-heavy-job.sh <name> <cmd…>`. Verify lag under 100 ledgers sustained 15 min.

## stellarindex_ledgerstream_tier_both_missing

P1 page, rule in `ledgerstream-tier.yml`. MTTR 10-30 min (rehydrate) to hours. The indexer or a backfill cannot read an LCM from EITHER tier, so that cursor is stalled: a live-tip stall grows API freshness lag; a backfill stall does not affect live serving.

`internal/ledgerstream/tiered.go` reads each LCM from the local `galexie-archive` MinIO bucket (hot) and on `NoSuchKey` falls back to `s3://aws-public-blockchain/v1.1/stellar/ledgers/pubnet/` (cold), ADR-0027; `both_missing` is neither having it. On r1 the cold tier is ENABLED, so this alert can fire.

Symptoms: `stellarindex_ledgerstream_tier_read_total{outcome="both_missing"}` increased in 5 min; logs show `cold read: ... not found`; for live ingest `stellarindex_cursor_last_ledger{source="ledgerstream"}` stopped and `stellarindex_source_last_event_unix` is ageing; for a backfill, frozen progress in `stellarindex-ops list-cursors -config /etc/stellarindex.toml`. (No indexer ledger-lag or backfill-cursor gauge exists.)

Check:

```sh
# 1. Which ledgers are missing — the indexer log line names them.
ssh root@136.243.90.96 'journalctl -u stellarindex-indexer --since="10 min ago" | grep -iE "both.missing|tiered.*not.found" | tail -5'

# 2. Is the local hot tier intact for the affected range?
#    Partitions are named "%08X--<start>-<end>/" (hex = MaxUint32-start,
#    64000 ledgers each), so let mc find the object.
SEQ=...   # e.g. 23000000
ssh root@136.243.90.96 "mc find local/galexie-archive --name '*--${SEQ}.xdr.*'"

# 3. Is the AWS public bucket reachable from r1? (us-east-2, NOT us-east-1)
ssh root@136.243.90.96 'curl -sf -m 10 https://aws-public-blockchain.s3.us-east-2.amazonaws.com/v1.1/stellar/ledgers/pubnet/ -I | head -3'

# 4. Cold-read latency (AWS slow vs down).
ssh root@136.243.90.96 'curl -s localhost:9464/metrics | grep stellarindex_ledgerstream_cold_read_duration_seconds_count'
```

Likely causes:

- A. AWS unreachable (step 3 fails): if r1 lost outbound HTTPS, page the sysadmin; the cursor is not advanced past the failure and retries. If AWS Open Data is down, wait it out and record the window in `docs/operations/incidents/`; rehydration reads the SAME configured cold tier and cannot route around it.
- B. AWS reachable but the partition is missing: a recent partition not yet published (~30 min behind tip). Wait 30 min; past 1 h contact AWS Open Data (contact in the bucket's `README.md`).
- C. Hot tier trimmed too early (step 2 confirms the partition is gone and step 3 fails): `galexie-archive-trim` deleted a range not yet safely cold (`--verify-upstream` should prevent it).
- D. Mis-configured binary (partition present, AWS reachable, metric still increments): stale bucket or missing AWS config. r1's `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` are MinIO's, for the hot datastore; the cold client uses `s3_cold_access_key_env` / `s3_cold_secret_key_env`: both empty means anonymous reads (correct for `aws-public-blockchain`), exactly one set is a config error.

Fix:

1. For C, read the trim log, rehydrate (step 2), then disable the trim timer until the trim operator is fixed and file a ticket against stellarindex-ops:

   ```sh
   ssh root@136.243.90.96 'journalctl -u galexie-archive-trim.service --since="24 h ago" | tail -50'
   ssh root@136.243.90.96 'systemctl disable --now galexie-archive-trim.timer'
   ```

2. Rehydrate a trimmed range. Signature: `rehydrate-galexie-archive -config PATH -from N -to N [-write] [-dry-run]`. DRY RUN is the default (`-dry-run` is a no-op alias); only `-write` copies. There is no `--source` or peer selector: it reads the configured cold tier (`storage.s3_cold_*`) into the local hot tier, refuses if cold is unconfigured, and is idempotent. A dry run's `copied=N` only means "files absent from hot" (it never asks cold), so it reads `missing_in_cold=0 errors=0` even over a range cold lost; only a `-write` run's counts mean anything.

   ```sh
   # 1. Preview (also what you get if you forget -write):
   stellarindex-ops rehydrate-galexie-archive \
     -config /etc/stellarindex.toml -from <SEQ> -to <SEQ_END>
   # 2. Commit — heavy one-shots on r1 go through the wrapper:
   sudo /usr/local/sbin/run-heavy-job.sh rehydrate \
     /usr/local/bin/stellarindex-ops rehydrate-galexie-archive \
       -config /etc/stellarindex.toml -from <SEQ> -to <SEQ_END> -write
   ```

   Changing the cold tier itself (another provider, a peer mirror per ADR-0016) is an ansible change to `storage.s3_cold_*`.
3. For D, check the config, fix it, restart the binary:

   ```sh
   ssh root@136.243.90.96 'grep -E "s3_bucket|s3_endpoint|s3_cold" /etc/stellarindex.toml'
   ssh root@136.243.90.96 'grep -E "AWS_|S3_" /etc/default/stellarindex'
   ```

4. Once the gap is filled the alert resolves itself; confirm the cursor advances:

   ```sh
   ssh root@136.243.90.96 'curl -s localhost:9464/metrics | \
     grep -E "stellarindex_cursor_last_ledger|stellarindex_source_last_event_unix"'
   stellarindex-ops list-cursors -config /etc/stellarindex.toml
   ```

See ADR-0027, ADR-0016 and `docs/operations/lcm-cache-tiering.md`.

## stellarindex_ingestion_discovery_drops

P3: `increase(stellarindex_discovery_dropped_hits_total[10m]) > 0` for 10 min (`deploy/monitoring/rules/ingestion.yml`). The discovery sink (SEP-41 sightings plus oracle-suggestive hits, `docs/architecture/oracle-manipulation-defense.md`) is non-blocking: when its buffer fills, hits are dropped rather than stalling the hot path, so discovery is best-effort and `discovered_assets` may lag. Logs show `discovery: hits dropped`.

Check:

```sh
curl -s http://indexer:9464/metrics | grep stellarindex_discovery_dropped_hits_total

ssh root@indexer-01 "journalctl -u stellarindex-indexer -n 200 --no-pager" \
  | grep "discovery:"

curl -s http://indexer:9464/metrics | grep stellarindex_source_insert_errors_total
```

Likely causes: Postgres degraded and slowing recorder writes; discovery volume spiked past the buffer; a recorder timeout forcing drop/retry cycles.

Fix:

1. Timescale health first; restore storage before tuning discovery.
2. If storage is healthy, check for a sharp rise in discovery volume.
3. Raise the discovery buffer only for a sustained workload, not a transient outage.
4. Verify `increase(stellarindex_discovery_dropped_hits_total[10m])` returns to `0`.

See [insert_errors](ingestion-sink.md#stellarindex_ingestion_insert_errors), [all_sources_stopped](#stellarindex_ingestion_all_sources_stopped), `internal/canonical/discovery/sink.go`.

## stellarindex_ingestion_dispatcher_tx_skips

P3 ticket (`for: 0m`, group `stellarindex.ingestion`), MTTR 15-60 min. The dispatcher (`internal/dispatcher/dispatcher.go`) skipped a transaction, or a ledger's state-archival evictions, rather than decoding it. It reads as an empty ledger, so the completeness reconcile would still pass. All four counters are zero in steady state; any increase is a dispatcher gap. `statsflush` mirrors each delta as a WARN every 5-minute window.

| Metric | Meaning |
| ------ | ------- |
| `stellarindex_dispatcher_tx_read_errors_total` | The transaction was malformed and skipped; check the upstream ledger source for a corrupt or truncated ledger. |
| `stellarindex_dispatcher_tx_event_read_errors_total` | `GetTransactionEvents` failed (G15-06), so every Soroban event in that tx is dropped; usually a stellar-go/SDK behind a protocol upgrade of the events encoding. |
| `stellarindex_dispatcher_entry_meta_unsupported_total` | An unhandled `TransactionMeta` version stopped the apply-phase entry-change walk: every classic balance/trustline/offer/LP change in that tx is skipped; a protocol upgrade ahead of a stellar-go/dispatcher bump. |
| `stellarindex_dispatcher_evicted_keys_unreadable_total` | The ledger's evicted-key list failed to read, so its evictions never reached the entry decoders and each evicted balance stays served as live; an SDK behind a `LedgerCloseMeta` bump. The WARN names the ledger (`ledger=`). |

Check:

```sh
# Which of the four counters moved, and by how much?
ssh root@136.243.90.96 'curl -s localhost:9464/metrics | grep -E "stellarindex_dispatcher_(tx_read_errors|tx_event_read_errors|entry_meta_unsupported|evicted_keys_unreadable)_total"'

# The exact failing ledger/tx is in the indexer's logs.
journalctl -u stellarindex-indexer --since -2h | grep -E "dispatcher: (tx-read errors|tx-event read errors|unsupported TransactionMeta|evicted ledger keys unreadable)"
```

Fix:

1. Correlate the counter with any recent protocol upgrade or captive-core/RPC version change; note the deployed `stellar-go` version and the affected ledger range for the postmortem.
2. Protocol upgrade: bump the vendored `stellar-go` / protocol-support version and ship a dispatcher release. This is a code fix; the skip is permanent until patched.
3. Re-derive the range: raw Soroban events are durable in the CH lake (ADR-0034); apply-phase entry changes need a replay from the affected ledger (`stellarindex-ops projector-replay` per ADR-0032, or a full re-ingest if classic tables need it). Skipped evictions need the live dispatcher re-run over the WARN's ledgers: the lake walker has no eviction phase.

See [insert_errors](ingestion-sink.md#stellarindex_ingestion_insert_errors), ADR-0029, ADR-0032, ADR-0034.

## Related

- [rpc-lag](stellar-node.md#stellarindex_stellar_rpc_lag): only for deployments that route through stellar-rpc (not r1).
- [cursor-stuck](ledger-ingest.md#stellarindex_ingestion_cursor_stuck): events flowing but the cursor not advancing.
- [timescale-primary-down](postgres.md#stellarindex_timescale_primary_down), [binary-version-skew](binary-version-skew.md).
- `internal/dispatcher/`, `internal/pipeline/`, `cmd/stellarindex-indexer/main.go`.
