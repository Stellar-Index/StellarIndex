---
title: Runbook — ledger ingest alerts (meta decode failing, cursor stuck, ledger stalled)
last_verified: 2026-10-06
status: active
severity: P1 | P2 | P3
---

# Runbook — ledger ingest alerts

Alerts on the Galexie -> ledgerstream -> dispatcher -> cursor path. Cursor and stall rules: `configs/prometheus/rules.r1/ingestion.yml` (twin in `deploy/monitoring/rules/ingestion.yml`). Ledger-meta rules: `stellar-stack-version.yml` in the same trees.

## At a glance

| Alert | Severity | Detected by |
| ----- | -------- | ----------- |
| [`stellarindex_ledger_meta_decode_failing`](#stellarindex_ledger_meta_decode_failing) | P1 page | `stellarindex_ledger_meta_decode_failures_total > 0` for 10m |
| [`stellarindex_ledger_meta_decode_probe_stale`](#stellarindex_ledger_meta_decode_failing) | P3 ticket | probe metric age > 1h for 30m |
| [`stellarindex_ingestion_cursor_stuck`](#stellarindex_ingestion_cursor_stuck) | P2 ticket, `for: 5m` | per-SOURCE cursor flat |
| [`stellarindex_ingestion_ledger_stalled`](#stellarindex_ingestion_ledger_stalled) | P1 page, ~10 min to page | `ledgerstream` cursor flat |

## stellarindex_ledger_meta_decode_failing

Covers `stellarindex_ledger_meta_decode_failing` (page) and `stellarindex_ledger_meta_decode_probe_stale` (ticket: the probe stopped reporting for over an hour, so a real failure would go unseen; check `ledger-meta-decode-probe.timer` / `.service`).

Symptom: a component cannot decode ledger meta the network now produces (we are behind a protocol upgrade). No data loss; the decode is fail-closed and the component has STOPPED ingesting and will not resume on its own.

Fix:

1. Identify the affected unit from the `unit` label.
2. Check `stellarindex_stellar_stack_version_lag` (see the [stellar-stack-version-lag runbook](stellar-node.md#stellar-stack-version-lag)) for the component and target version.
3. Follow the [protocol-upgrade procedure](../protocol-upgrades.md) to bump `galexie_version` / `stellar_core_version` / the release binary's `go-stellar-sdk` and re-apply.
4. Confirm `stellarindex_ledger_meta_decode_failures_total` stops increasing and the ingest cursor advances.

## stellarindex_ingestion_cursor_stuck

Symptom: `increase(stellarindex_cursor_last_ledger{source=...}[5m]) == 0` AND `stellarindex_source_enabled == 1`. If the cursor froze hours ago, a restart replays from it (slow, expensive).

Check:

```sh
# Which source + how far back is the cursor?
stellarindex-ops list-cursors -config /etc/stellarindex.toml

# How does that compare to the network tip?
stellarindex-ops detect-gaps -config /etc/stellarindex.toml -threshold 100
```

Likely causes:

- Cursor flat + `stellarindex_source_events_total` rising: cursor upserts failing or the dispatcher rejecting ledgers before commit (indexer logs: `cursor upsert`, `dispatcher rejected ledger`, `dispatcher panicked`).
- Cursor flat + events == 0: source legitimately quiet; check `source-stopped` (`ingestion.md#stellarindex_ingestion_source_stopped`).
- Cursor flat + repeated indexer errors: a live ingest fault.
- Indexer just started and has not yet committed a ledger.

Fix:

1. Upstream first: confirm Galexie is producing fresh objects (`ssh root@136.243.90.96 'mc ls local/galexie-live | tail'`). If MinIO/Galexie is the problem, go to [all-ingestion-down](ingestion.md#stellarindex_ingestion_all_sources_stopped).
2. If events flow but the cursor is flat, capture logs, then restart:

   ```sh
   journalctl -u stellarindex-indexer -n 500 --no-pager > /tmp/indexer.log
   ssh root@136.243.90.96 systemctl restart stellarindex-indexer
   ```

3. A cursor that regressed should be impossible (advance-only guard): snapshot it before restart with `runuser -u postgres -- psql -d stellarindex -c 'TABLE ingestion_cursors'` (on r1) and attach to the postmortem.
4. Verify `stellarindex_cursor_last_ledger{source=...}` climbs again.

Escalate: cursor regression, or a flat cursor surviving a restart.

## stellarindex_ingestion_ledger_stalled

Symptom: no ledger ingested for >= 10 min; every on-chain surface (trades, events, supply, coverage, explorer) is frozen while CEX/FX prices keep updating, so the API looks partly alive. `stellarindex_cursor_last_ledger{source="ledgerstream"}` is flat or absent. `stellarindex_ingestion_all_sources_stopped` and `stellarindex_ingestion_cursor_stuck` stay quiet for this fault (CEX/FX connectors share the binary; `ledgerstream` is a cursor namespace, not a source), and the unit never parks in `failed` (StartLimit), so `stellarindex_systemd_unit_failed` does not fire either.

Check:

```sh
# Is the process alive, or restart-looping on a dependency?
ssh root@136.243.90.96 'systemctl status stellarindex-indexer --no-pager | head -20'
ssh root@136.243.90.96 'journalctl -u stellarindex-indexer -n 200 --no-pager | tail -60'

# Is the lake readable, and is Galexie still writing?
ssh root@136.243.90.96 'mc ls local/galexie-live | tail'
ssh root@136.243.90.96 'systemctl is-active minio galexie'

# Where is the committed cursor vs the bucket?
ssh root@136.243.90.96 "runuser -u postgres -- psql -d stellarindex -c \
  \"SELECT source, sub_source, last_ledger, last_updated FROM ingestion_cursors WHERE source = 'ledgerstream'\""
```

Likely causes:

- MinIO/Galexie down or credential drift (`SignatureDoesNotMatch` in the journal). A climbing `stellarindex_ledgerstream_live_start_retries_total` confirms the live tail cannot open the lake; nothing is skipped while it climbs.
- Lake healthy, cursor flat, no restarts: dispatch wedged or sink blocked. Check `stellarindex_decoder_panics_total`, `stellarindex_postgres_ping_failure_streak`, `stellarindex_trade_insert_buffer_depth`.
- Unit restart-looping every ~5 min on a broken dependency or config.
- Process not running (`systemctl status` gives the reason).

Fix:

1. Fix the upstream first (MinIO, then Galexie); the indexer resumes from its committed cursor. See [all-sources-stopped](ingestion.md#stellarindex_ingestion_all_sources_stopped).
2. Lake healthy and cursor still flat: capture the journal BEFORE restarting (`journalctl -u stellarindex-indexer -n 1000 --no-pager > /tmp/indexer-stall.log`), then `ssh root@136.243.90.96 systemctl restart stellarindex-indexer`.
3. If Postgres is the blocker, work that incident first.
4. Verify the cursor climbs within ~1 min; the alert clears ~10 min later. Then run `stellarindex-ops detect-gaps` for the gap the stall left.

Escalate: a stall surviving a restart with a healthy lake and Postgres. Silence a deploy or reboot longer than 10 min rather than widening the rule.

## Related

- [stellar-stack-version-lag runbook](stellar-node.md#stellar-stack-version-lag), [Protocol upgrades](../protocol-upgrades.md), [Alerts catalog](../alerts-catalog.md).
- [ingestion.md#stellarindex_ingestion_source_stopped](ingestion.md#stellarindex_ingestion_source_stopped), [ingestion.md#stellarindex_ingestion_all_sources_stopped](ingestion.md#stellarindex_ingestion_all_sources_stopped).
- [infra.md#stellarindex_systemd_unit_failed](infra.md#stellarindex_systemd_unit_failed).
- Implementation: `cmd/stellarindex-indexer/main.go` (`processAndPersistCursor`), `internal/ledgerstream/`.
