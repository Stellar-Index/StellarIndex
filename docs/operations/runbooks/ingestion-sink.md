---
title: Runbook — ingestion persistence (insert errors, backpressure, duplicate flood, undrained rows)
last_verified: 2026-10-06
status: living
severity: ticket (P2 for insert_errors, persist_drop, duplicate_flood; P3 for backpressure)
---

# Runbook — ingestion persistence alerts

Alerts on the path from decoded events to the Postgres served tier (ADR-0041 durability semantics). Rules: `configs/prometheus/rules.r1/ingestion.yml` (group `stellarindex.ingestion`, the file r1 actually loads) with the multi-host twin `deploy/monitoring/rules/ingestion.yml` (same exprs). Indexer metrics port on r1 is 9464 (the API serves its own on :3000).

Loss model since ADR-0041 (2026-07-06): infrastructure faults on a write are NOT lost; the sink retries with backpressure and the cursor stalls ([trade_insert_backpressure](#stellarindex_ingestion_trade_insert_backpressure)). A firing of [insert_errors](#stellarindex_ingestion_insert_errors) therefore means genuine loss.

## At a glance

| Alert | Severity | Meaning |
| ----- | -------- | ------- |
| [`stellarindex_ingestion_insert_errors`](#stellarindex_ingestion_insert_errors) (+ `stellarindex_ingestion_persist_drop`, `stellarindex_ingestion_trustline_observation_drop`) | P2 (`severity: ticket`); `for: 5m` for insert_errors, `for: 0m` for persist_drop and trustline_observation_drop | events failing to persist: genuine loss |
| [`stellarindex_ingestion_trade_insert_backpressure`](#stellarindex_ingestion_trade_insert_backpressure) | P3 ticket, 10 min | trade sink retrying: Postgres unreachable, ingest intentionally stalled |
| [`stellarindex_ingestion_duplicate_flood`](#stellarindex_ingestion_duplicate_flood) | P2 ticket, `for: 10m` | trades stop landing new rows though cursor and decoder look healthy |
| [`stellarindex_ingestion_sink_undrained_rows`](#stellarindex_ingestion_sink_undrained_rows) | ticket, `for: 0m` | shutdown drain abandoned buffered rows the cursor had already passed |

## stellarindex_ingestion_insert_errors

Also covers the sensitive `stellarindex_ingestion_persist_drop` sibling on the money-flow kinds, and `stellarindex_ingestion_trustline_observation_drop` on `kind="trustline_observation"`; they share this runbook. Typical MTTR 15-60 min.

Impact: events failing to persist. Post-ADR-0041 infrastructure faults are NOT lost; they retry with backpressure (the cursor stalls; see the backpressure section). A firing here therefore means **GENUINE loss**: a permanent data fault (`kind=trade`) or external retry-buffer overflow (`kind=dropped`), mirroring the alert's own annotation. Downstream: price staleness, missing rows.

Symptoms:

- `stellarindex_source_insert_errors_total{source=...,kind=...}` rises above 6/min sustained. `kind` is not just `trade|oracle`: `trade` is a permanently dropped trade and `trade_abandoned` a retry abandoned on shutdown / cycle timeout (cursor held, re-derivable, not a loss); the counter also carries `panic` (unhandled decode/persist panic, recovered in the sink), `dropped` (external retry-buffer overflow, ADR-0041), and the per-domain persist kinds. The money-flow set `trade` / `soroswap_router_swap` / `defindex_flow_strategy` / `defindex_flow_vault` has its own SENSITIVE any-nonzero tripwire (`stellarindex_ingestion_persist_drop`, `increase(...[15m]) > 0`) that shares this runbook_url, because a low-rate silent drop sits below the 0.1/s threshold here. `kind="trustline_observation"` (classic-supply, no retry path) has the same shape of companion tripwire, `stellarindex_ingestion_trustline_observation_drop` (INV-0786; ~17,663 errors / 30 days on r1 sat under the 0.1/s gate with no alert at all before it).
- `stellarindex_source_events_total` may still rise: the consumer is pulling events, it's the writer that's failing.
- Dashboard view: *Ingestion -> Insert errors* panel non-zero for > 5 min.
- The offending source's `stellarindex_source_last_event_unix` may freeze (if persistence blocks until retry).

Quick diagnosis (<= 5 min):

```sh
# Which source + which kind is failing? (9464 = the indexer's
# metrics port on r1; the API serves its own metrics on :3000.)
ssh root@136.243.90.96 'curl -s localhost:9464/metrics | grep stellarindex_source_insert_errors_total'

# Is it the storage layer itself?
stellarindex-ops rpc-probe https://mainnet.sorobanrpc.com   # rules out upstream — r1 has no local stellar-rpc, point at a public endpoint
ssh root@136.243.90.96 'runuser -u postgres -- psql -d stellarindex -c "SELECT now(), pg_is_in_recovery();"'

# Actual failure reason is in the indexer's logs:
journalctl -u stellarindex-indexer --since -2h | grep -E "insert (trade|oracle update) failed" | tail
```

If the log line says:

- `connection refused`: Timescale is down or network partitioned. Jump to `postgres.md#stellarindex_timescale_primary_down`. For `kind=trade` an infrastructure fault like this now lands in `stellarindex_ingestion_trade_insert_backpressure` (the ADR-0041 retry path), not here; if you ARE seeing it here, the retry buffer overflowed (`kind=dropped`).
- `disk full` / `no space`: Timescale volume out of space. Free space on the ZFS pool or evict old chunks (see db-disk-full.md).
- `duplicate key value`: should be impossible; the idempotent ON CONFLICT swallows these. If you see this, the primary-key invariant is broken and this is a data-integrity incident, not a capacity one. Escalate.
- `violates check constraint`: a source sent malformed data (negative amounts, bad tx_hash). Decode bug, not a storage bug; check `stellarindex_source_decode_errors_total` on the same source.

Mitigation (<= 15 min). Events counted HERE are genuinely lost (permanent data fault `kind=trade`, or retry-buffer overflow `kind=dropped`). Prioritise:

- [ ] Step 1: stop the bleeding. If Timescale is the root cause, follow `postgres.md#stellarindex_timescale_primary_down` first; insert errors are a symptom.
- [ ] Step 2: if disk-full: extend the underlying volume (the production deployment uses bare-metal NVMe + ZFS per [ADR-0008](../../adr/0008-ha-topology.md), not Kubernetes; grow via `zpool` / Hetzner volume-resize console). Let the indexer auto-retry once `df` reports headroom; then backfill the gap:

  ```sh
  # 1. Identify the affected range. detect-gaps is cursor-vs-tip
  #    only; find-data-gaps reads the data tables and reports the
  #    actual missing (from, to) ledger ranges.
  stellarindex-ops detect-gaps -config /etc/stellarindex.toml \
      -threshold 50
  stellarindex-ops find-data-gaps -config /etc/stellarindex.toml \
      -source <SOURCE_NAME> -output text
  # 2. Backfill the named range — dry-run first to confirm scope.
  stellarindex-ops backfill -config /etc/stellarindex.toml \
      -from <FIRST_LEDGER> -to <LAST_LEDGER> \
      -source <SOURCE_NAME> -dry-run
  # 3. Drop -dry-run to commit.
  stellarindex-ops backfill -write -config /etc/stellarindex.toml \
      -from <FIRST_LEDGER> -to <LAST_LEDGER> \
      -source <SOURCE_NAME> -resume
  ```

- [ ] Step 3: if the underlying issue is fixed: watch the rate decline. Alert clears when the 5m rate drops below 0.1/s.
- [ ] Verification: `stellarindex_source_insert_errors_total` stops incrementing (use `rate()[1m]` to see it go to 0).

Root cause (for the postmortem): Timescale logs from `/var/log/postgresql/` covering the affected window; `stellarindex_source_insert_errors_total` series by `(source, kind)` over the incident; indexer stderr with the full error strings (Timescale wraps them verbosely); did the alert fire for ONE source or ALL? One-source = decode/schema issue; all-sources = shared storage issue.

False positives: none yet. The threshold (6/min sustained 5 min) was chosen to ride through a 1-commit-at-a-time restart; no legitimate brief spikes seen.

## stellarindex_ingestion_trade_insert_backpressure

P3 ticket. Detected by `sum(rate(stellarindex_trade_insert_retries_total{outcome="retry"}[5m])) > 0` for 10 min. Typical MTTR: minutes (Postgres restart / pressure passes).

Impact: the served tier is FROZEN: the on-chain ledger cursor is held and no new trades/prices land while this fires. **No data is lost**: on-chain trades block-and-retry (cursor gating), external CEX/FX trades buffer in memory. External price freshness degrades if it persists.

Why this exists: 2026-07-06 incident: during a 17-minute Postgres outage the trade sink DROPPED writes (`insert trade failed` / `connection refused`) while the ledger cursor kept advancing, a ~205-ledger sdex hole (healed from the lake) plus unrecoverable CEX drops. The sink now classifies the failure ([`timescale.IsInfraError`](../../../internal/storage/timescale/errors.go)) and, on an infrastructure fault, RETRIES with backpressure instead of dropping. This alert is the visible signal that the retry path is active, i.e. Postgres is unreachable and ingest is intentionally stalled rather than losing data (ADR-0041).

Symptoms:

- `stellarindex_trade_insert_retries_total{outcome="retry"}` climbing; `outcome="recovered"` flat (hasn't recovered yet).
- The on-chain ledger cursor (`stellarindex_cursor_last_ledger`) is not advancing; `stellarindex_ingestion_cursor_stuck` may also fire.
- `stellarindex_trade_insert_buffer_depth` climbing (external CEX/FX trades queuing in the bounded retry buffer).
- Indexer journal: repeated `infrastructure fault on sink write — retrying with backpressure`.

Quick diagnosis (<= 5 min):

```sh
# Is Postgres actually up? (the near-universal cause)
systemctl status postgresql
sudo -u postgres psql -d stellarindex -c 'SELECT 1'

# Retry vs recovery mix + external buffer depth
curl -s localhost:9464/metrics | grep -E 'trade_insert_retries_total|trade_insert_buffer_depth'

# Indexer's own view
journalctl -u stellarindex-indexer --since -15m | grep -iE 'backpressure|connection refused|abandoned'
```

If `SELECT 1` fails: a Postgres outage (expected trigger); go to Mitigation. If Postgres is healthy but retries persist: suspect connection-pool exhaustion (`too_many_connections`, SQLSTATE 53300) or a network partition to the DB host.

Mitigation (<= 15 min):

- [ ] Bring Postgres back (restart the service / clear disk / restore the network path). This is the only real fix; the sink recovers on its own the instant writes succeed.
- [ ] If the cause is pool exhaustion, find and kill the hog: `SELECT pid, state, query_start, left(query,80) FROM pg_stat_activity ORDER BY query_start;`
- [ ] Verification: within ~1 retry interval of Postgres recovering, `stellarindex_trade_insert_retries_total{outcome="recovered"}` increments, the cursor resumes advancing, and `stellarindex_trade_insert_buffer_depth` drains to 0. The alert clears within its 10 min window.

Root cause:

- **Nothing to re-derive for on-chain trades**: they were held in memory and landed on recovery. If the indexer was HARD-restarted while this fired (not a graceful shutdown), the in-flight buffer is lost; but those ledgers are re-derivable from the CH lake and the ADR-0033 completeness verdict will flag any residue. A graceful shutdown logs the exact abandoned ledger range at ERROR (`abandoned on shutdown … re-derive this ledger range`).
- **External (CEX/FX) trades** dropped only if the outage outlasted the bounded buffer (`stellarindex_source_insert_errors_total{kind="dropped"}` bumped). Those are vendor-refillable via the connector backfill path.

## stellarindex_ingestion_duplicate_flood

P2 ticket. Detected by the same expr in both rule trees. Typical MTTR 30-90 min. Status of this procedure: draft.

Impact: cursor advances but the trades hypertable falls behind. `/v1/price` returns stale-but-flagged data; freshness SLA fails. No data loss (events were never persisted from this code path; if the source produces fresh events again, they'll land).

Symptoms:

- `stellarindex_trade_insert_outcome_total{source=...,outcome="duplicate"}` > 0.5/sec for >= 10 min.
- `stellarindex_trade_insert_outcome_total{source=...,outcome="new"}` == 0 over the same window, **or absent entirely**: the counter is call-site-seeded, so a source that has landed no new row since process start has no `outcome="new"` child at all. The rule uses `unless on (source) rate(new[10m]) > 0` (#302) precisely so absent and zero read the same; don't read a missing line in the curl below as "the alert must be wrong".
- `stellarindex_source_events_total{source=...}` still climbing: events ARE being decoded.
- `stellarindex_cursor_last_ledger{source="ledgerstream"}` still advancing.
- `psql trades` shows `max(ts) WHERE source = <X>` frozen for hours.
- `/v1/markets?source=<X>` returns `last_trade_at` matching the frozen `max(ts)`.

The combination is the diagnostic signature: cursor + decoder healthy, persistence apparently working (no errors), but no INSERT is landing a new row. Live r1 evidence on 2026-05-28: 157 SDEX dupes/min, `max(ts) = 14:29:17 UTC` for 11 hours.

**`outcome="duplicate"` is a CONFLATION; read it carefully.** The trade upsert is **not** `ON CONFLICT DO NOTHING` any more. The INV-3 keystone fix (migration 0109) made it `ON CONFLICT … DO UPDATE … WHERE trades.derive_generation <= EXCLUDED.derive_generation`, so the counter's `duplicate` label now covers THREE different outcomes that all score "no fresh row inserted": (1) a true duplicate (the row was already there, unchanged); (2) a generation-guarded **corrective UPDATE** that rewrote an existing row in place, i.e. the system working exactly as intended; (3) a guard-**skipped** write (a lower generation refusing to revert a higher-generation correction).

**Operational consequence: a running corrective re-derive produces this alert's exact signature** (`duplicate` climbing with `new` at zero) because a re-derive legitimately updates rows in place rather than inserting them. Before treating a firing as a stuck cursor, check whether a heavy job is in flight:

```sh
ssh root@136.243.90.96 'systemctl list-units "heavy-*.scope" --all --no-pager; pgrep -a stellarindex-ops'
```

If one is, the alert is expected for the duration of the job and the freshness SQL below (step 2) is the signal that matters, not the counter.

Quick diagnosis (<= 5 min):

```sh
ssh root@136.243.90.96

# 1. Confirm the duplicate vs new split.
curl -sS localhost:9464/metrics | grep stellarindex_trade_insert_outcome_total

# 2. Confirm the trades hypertable is actually stale.
sudo -u postgres psql stellarindex -c "
  SELECT source, max(ts) AT TIME ZONE 'UTC' AS max_ts,
         count(*) FILTER (WHERE ts > NOW() - INTERVAL '1 hour') AS rows_last_hour
    FROM trades
   WHERE ts > NOW() - INTERVAL '24 hours'
   GROUP BY source
   ORDER BY max_ts DESC;"

# 3. Confirm the indexer cursor IS advancing.
curl -sS localhost:9464/metrics | grep cursor_last_ledger

# 4. Look at the ingestion_cursors table for stuck backfills shadowing live ingest.
sudo -u postgres psql stellarindex -c "
  SELECT source, sub_source, last_ledger, last_updated
    FROM ingestion_cursors
   ORDER BY last_updated DESC
   LIMIT 20;"
```

Likely causes:

1. **Cursor jumped past data without persisting events.** A back-pressure event (postgres outage, slow sink) caused ProcessLedger to return cleanly because the channel buffer absorbed the events, but the events were never drained before shutdown; they got dropped. The cursor was upserted regardless. Subsequent live walking starts past the gap; the events for that gap range have to be backfilled.
2. **Live indexer running with a stale event channel from a prior process.** Extremely unlikely under the current architecture (single sink goroutine) but possible if a refactor introduces a leak.
3. **A backfill process replaying the same range repeatedly.** Inspect `ingestion_cursors` for a `backfill` row whose `last_ledger` is below its sub_source's upper bound and check if a `stellarindex-ops backfill` process is running.

Remediation. For cause 1 (most common): identify the gap, run a targeted backfill. The trades hypertable's PK is `(source, ledger, tx_hash, op_index, ts)` so a backfill is idempotent; re-walking a range that already has data is harmless.

```sh
# Determine the gap: lowest ledger to backfill is one past max_ts;
# upper ledger is the current cursor.
sudo -u postgres psql stellarindex -t -c "SELECT max(ledger) FROM trades WHERE source = 'sdex';"
# vs cursor_last_ledger metric.

# Print the plan first — `-dry-run` validates config + sources +
# range and prints the chunk split, then exits without writing.
stellarindex-ops backfill -config /etc/stellarindex.toml \
  -from <max_ledger+1> -to <current_cursor> \
  -source sdex,aquarius,soroswap,phoenix,comet -parallel 4 -dry-run

# Run the targeted backfill. The flag is -source (SINGULAR, comma-
# separated) — `-sources` does not parse. `-parallel N` splits the
# range into N contiguous non-overlapping chunks, each with its own
# dispatcher + sink + chunk-specific cursor row (so -resume picks up
# per chunk); the flag's own guidance is 4-16 on a 16-core box, above
# which postgres max_connections or galexie S3 list throughput is the
# bottleneck. Heavy one-shots on r1 ALWAYS go through the wrapper
# (docs/operations/maintainer-workflow.md, "Heavy one-shot jobs"), one at a time. The
# wrapper's flock is per job NAME and its MemoryMax=20G scope cap
# (MemorySwapMax=0 — kill, not swap) applies to the whole wrapped
# process, chunks included: raise -parallel and you divide that
# budget, you don't multiply it.
sudo /usr/local/sbin/run-heavy-job.sh dupflood-backfill \
  /usr/local/bin/stellarindex-ops backfill -write \
    -config /etc/stellarindex.toml \
    -from <max_ledger+1> -to <current_cursor> \
    -source sdex,aquarius,soroswap,phoenix,comet \
    -parallel 4
```

`backfill` **refuses** any source that isn't `BackfillSafe` in `internal/sources/external/registry.go`; for on-chain Soroban sources that gate is the per-WASM-hash audit. For a *projected* (Soroban-derived) source the ADR-0032 catch-up path is `projector-replay`, not `backfill`, and note it carries the shared write gate, so **dry run is the default and you must pass `-write`** to actually rewind:

```sh
sudo /usr/local/sbin/run-heavy-job.sh dupflood-replay \
  /usr/local/bin/stellarindex-ops projector-replay \
    -config /etc/stellarindex.toml -source <name> -from <ledger> -write
```

Beyond roughly 1M ledgers use `projected-rebuild` instead (same `-write` gate, plus `-workers` / `-window`).

For cause 2: restart the indexer to clear any goroutine leak: `systemctl restart stellarindex-indexer`.

For cause 3: identify the looping backfill via `ps`, decide whether to stop it (`systemctl stop` for service-managed, `kill` for ad-hoc).

Verification. After remediation, the metric should flip back:

```sh
# Wait at least 2× scrape interval (60s default) then check:
curl -sS localhost:9464/metrics | grep stellarindex_trade_insert_outcome_total
# outcome=new should be climbing again.

# And the trades table should accept fresh rows:
sudo -u postgres psql stellarindex -c "
  SELECT max(ts) AT TIME ZONE 'UTC' FROM trades WHERE source = 'sdex';"
# Should be within the last few minutes.
```

The alert will clear after `for: 10m` elapses with healthy `outcome=new` rates.

Reference: `internal/storage/timescale/trades.go:Store.InsertTrade` (where the outcome metric is emitted); `docs/reference/metrics/README.md#stellarindex_trade_insert_outcome_total`; F-0028 audit finding (audit-2026-05-26) for the original observation of soroban_events ingest tip lag, similar shape; F-0020 audit finding for the postgres back-pressure cause. Since 2026-07-06 infrastructure faults retry rather than drop, so a back-pressure episode is visible in the backpressure section before it can show up here.

## stellarindex_ingestion_sink_undrained_rows

Ticket. `increase(stellarindex_sink_undrained_rows_total[1h]) > 0`, `for: 0m`.

What it means: the Postgres pipeline sink (`sink="persist_events"`) was shut down with rows still buffered, its bounded drain could not land them before the budget expired, and the ledger cursor had ALREADY advanced past them. Those rows are a served-tier gap the indexer will never revisit on its own. `kind` says what was lost: `trade` rows or non-trade `event` rows.

First action: pull the `abandoned on shutdown` ERROR lines from the indexer journal (they carry the exact ledger range for trades, or source for events), then re-derive that range from the ClickHouse lake. Why ticket: nothing is on fire (live ingest resumed on restart and the rows are recoverable, ADR-0034), but the gap does not heal itself and no other alert names it; only the completeness verdict would, hours later.

Why this exists: the indexer upserts the per-ledger cursor when the producer has ENQUEUED a ledger's events to the sink, before the sink writes them (`cmd/stellarindex-indexer`, `internal/pipeline/sink.go`). On SIGTERM the sink drains what is buffered under a budget derived from `pipeline.ShutdownDeadline` (30 s): 20 s of bounded drain, a 5 s best-effort final pass, 5 s reserved for the loss report, then `main` hard-exits. A drain that trips that budget (Postgres slow from VACUUM or a lock convoy, restarting, or down during the deploy) abandons whatever is left, and every abandoned row is a served-tier row the cursor says was written.

Until 2026-09-18 that loss was visible only as ERROR log lines (`trade batch abandoned on shutdown — recoverable from the CH lake (ADR-0034); re-derive this ledger range` and `served-tier event abandoned on shutdown — re-derive this source's tail`). Nothing alerted on either, so a bad deploy lost rows silently. The ClickHouse live-sink half of the same class already had `stellarindex_ch_live_sink_ledgers_total{outcome="dropped"}` and the [ch-live-sink](ch-live-sink.md#stellarindex_ingestion_ch_live_sink_drops) rules; this counter and alert are the served-tier twin. The counter increments ONLY in the three functions that emit those ERROR lines (`reportAbandonedTrades`, `reportAbandonedEvent`, and the external retry buffer's `finalDrain`), by row, so the alert's value is the size of the gap and a steady-state flush that hands its rows to the shutdown drain to retry is never counted. Until 2026-09-20 the external buffer's final pass only logged a Warn and never incremented, so a vendor-refillable loss was invisible to the alert; its final pass now counts what it could not land (`kind="trade"`, `venues=` on its ERROR line), so Resolution C is reachable from the alert, not only from the journal.

**Scrape-window caveat.** The increment lands 20-25 s after SIGTERM and the process exits by 30 s. r1 scrapes the indexer every 15 s, so a single loss can fall between two scrapes and the alert stays silent. The journal ERROR line is the authoritative record; the alert is the machine-readable best-effort signal on top of it. After any deploy during which Postgres was unhealthy, run the Quick diagnosis grep even if nothing fired.

Symptoms:

- `stellarindex_ingestion_sink_undrained_rows{sink="persist_events",kind="trade"|"event"}` firing.
- Indexer journal, around the last restart, one or more of:
  - `trade batch abandoned on shutdown — recoverable from the CH lake (ADR-0034); re-derive this ledger range` with `phase=`, `batch_size=`, `ledger_from=`, `ledger_to=`;
  - `served-tier event abandoned on shutdown — re-derive this source's tail` with `kind=`, `source=`;
  - `PersistEvents drain deadline exceeded — made a final best-effort persist pass` with `undrained_events=`, `undrained_trades=`, `ledger_from=`, `ledger_to=`;
  - `external trade retry buffer not fully drained at shutdown — remaining entries are vendor-refillable (ADR-0041); record the venues and window` with `remaining=`, `venues=`.
- Often alongside: the backpressure ticket in the minutes before the deploy (`stellarindex_trade_insert_retries_total{outcome="retry"}`), or a Postgres restart in the deploy's own log.

Quick diagnosis (<= 5 min):

```sh
# The authoritative record: what was abandoned, and the range / source to re-derive.
ssh root@136.243.90.96 'journalctl -u stellarindex-indexer --since "-3h" --no-pager | grep -E "abandoned on shutdown|drain deadline exceeded|retry buffer not fully drained"'

# What the counter saw (may be smaller than the journal — see the scrape-window caveat).
ssh root@136.243.90.96 'curl -s "http://localhost:9090/api/v1/query?query=increase(stellarindex_sink_undrained_rows_total%5B1h%5D)" | python3 -m json.tool | grep -E "\"sink\"|\"kind\"|value"'

# Why the drain tripped: was Postgres healthy across the restart?
ssh root@136.243.90.96 'journalctl -u postgresql -u stellarindex-indexer --since "-3h" --no-pager | grep -iE "shutting down|ready to accept|connection refused|infrastructure fault" | tail -40'
```

Write down, per ERROR line: `ledger_from`/`ledger_to` (trades) or `source` (events), and the wall-clock time of the restart.

Triage tree:

1. **`kind="trade"`, ERROR line carries a non-zero ledger range**: on-chain trades (SDEX and Soroban DEXes). Recoverable from the lake: Resolution A.
2. **`kind="trade"`, `ledger_from=0`, or the `retry buffer not fully drained` line**: external CEX/FX trades (they carry no ledger; the retry-buffer line names the `venues=`). No lake copy exists; Resolution C.
3. **`kind="event"`**: a non-trade served-tier write (oracle update, supply observation, blend / cctp / rozo row). The line names the `source`; Resolution B.
4. **Alert silent, but the journal shows the lines**: the scrape missed the increment (known caveat). Proceed exactly as if it fired.
5. **Alert firing, no ERROR line in the journal**: both are emitted by the same function, so this is a log-shipping / journald problem, not a phantom loss. Fix the log path, then read the range from the `PersistEvents drain deadline exceeded` line once it appears.

Resolution A, on-chain trades: re-derive the ledger range from the lake. ADR-0034 keeps every raw operation in ClickHouse, so the served-tier rows are rebuilt from it. `-sdex-gaps` restricts the pass to ledgers the served tier is short on, which is exactly the shape a shutdown loss leaves. Dry-run first (no `-write`), then write; on r1 run the write under `/usr/local/sbin/run-heavy-job.sh`.

```sh
stellarindex-ops ch-rebuild -config /etc/stellarindex.toml \
  -from <ledger_from> -to <ledger_to> -sdex -sdex-gaps
stellarindex-ops ch-rebuild -config /etc/stellarindex.toml \
  -from <ledger_from> -to <ledger_to> -sdex -sdex-gaps -write
```

Soroban DEX trades (soroswap / aquarius / phoenix / comet) land through the same command's default event pass, so the range covers them too. Then confirm the served tier is whole over the range:

```sh
stellarindex-ops compute-completeness -config /etc/stellarindex.toml -ch -skip-recognition -source sdex -from <ledger_from>
```

[sdex-gap-detected](ingest-gap.md#sdex-gap-detected) has the longer form if the range is wide or the writer is still unhealthy.

Resolution B, non-trade events: re-derive the source's tail. `consumer.Event` carries no ledger, so the window comes from the restart: take `ledger_from`/`ledger_to` from the `drain deadline exceeded` line in the same journal burst, or the `ingestion_cursors` value at the restart minus a generous margin. Then re-derive by source:

- **Oracle / ContractCall sources (band, soroswap-router):** `stellarindex-ops ch-rebuild -config /etc/stellarindex.toml -from <from> -to <to> -contract-calls -sources <source>` (dry-run, then `-write`), as in [oracle-unknown-symbols](ingestion-events.md#stellarindex_ingestion_oracle_unknown_symbols).
- **Event-based sources:** the same `ch-rebuild` without `-contract-calls`, scoped with `-sources <source>`.
- **Anything else the line names:** follow that source's own recovery in the insert_errors section above; the per-source gap detector ([ingest-gap-detected](ingest-gap.md#stellarindex_ingest_gap_detected)) and the completeness verdict are how you confirm the tail is whole afterwards.

Resolution C, external CEX/FX trades: record the loss. External trades have no ledger and no lake copy; the source poller does not replay history. Note the venue (`venues=` on the retry-buffer line, or the batch's sources) and window in the incident record. Served prices are unaffected beyond that window (the next poll refills the feed), so no further action.

Resolution D, stop it recurring. The drain tripped because Postgres could not absorb a few hundred buffered rows in 20 s. Find out why before the next deploy: a `VACUUM`/autovacuum on `trades`, a lock convoy ([pg-lock-convoy](pg-lock-convoy.md)), or the deploy stopping Postgres before the indexer. If the answer is "the deploy order", fix the playbook; if it is "Postgres was genuinely down", the loss was the designed outcome (bounded drain over unbounded hang, ADR-0041) and the re-derive above is the whole remedy.

False positives: none. The counter increments only where a row has nowhere left to go. A steady-state flush interrupted by shutdown carries its rows into the bounded drain and is counted only if THAT also fails; a projector-owned event the sink was never going to write is skipped before counting (`TestDrainFinalPass_SkipInSinkExcludedFromReport`); the external retry buffer counts only what is still in its ring AFTER its final pass, never the rows a steady-state tick re-queued for the next retry (`TestExternalRetryBuffer_FinalDrainDoesNotCountLandedRows`).

## Related

- [ch-live-sink](ch-live-sink.md): the ClickHouse half of the same class. Different remedy: those drops are healed by the `ch-live-catchup` timer; undrained-rows loss is healed by nothing but the re-derive above.
- `ledger-ingest.md#stellarindex_ingestion_cursor_stuck`: the cursor-not-advancing symptom backpressure produces on purpose. `all-ingestion-down.md`: if the outage is total and prolonged, the SEV-1 page.
- `postgres.md#stellarindex_timescale_primary_down` (root cause when shared storage is the issue); `decode-errors.md` (decode failures look similar but are source-side).
- ADR-0003 (i128 precision): check-constraint violations in insert_errors indicate a decoder sending values that violate NUMERIC bounds.
- [ADR-0041](../../adr/0041-ingest-durability-semantics.md) (durability semantics; why the drain is bounded rather than blocking); `docs/adr/0034-tiered-clickhouse-architecture.md` (why the range is recoverable).
- `internal/pipeline/trade_sink.go` (`reportAbandonedTrades`, `externalRetryBuffer.finalDrain`) and `internal/pipeline/sink.go` (`reportAbandonedEvent`): the only three undrained-counter increment sites; `pipeline.ShutdownDeadline` is where the budget comes from.
