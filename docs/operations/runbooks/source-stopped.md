---
title: Runbook — source-stopped
last_verified: 2026-04-23
status: draft
severity: P2
---

# Runbook — `ratesengine_ingestion_source_stopped`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `ratesengine_ingestion_source_stopped` |
| Severity | P2 (ticket) |
| Detected by | `deploy/monitoring/rules/ingestion.yml` |
| Typical MTTR | 15–60 min |
| Impact | One configured source has stopped producing events for 5+ minutes. API clients querying that pair see price staleness creep up. If multiple sources stop, escalate to `all-ingestion-down.md` (P1). |

## Symptoms

- `sum by (source) (rate(ratesengine_source_events_total[5m])) == 0` AND `ratesengine_source_enabled == 1` sustained 5 min.
- Dashboard: *Ingestion → Events per source* panel shows a flat line for the offending source while other sources are still producing.
- `ratesengine_source_lag_ledgers` may be climbing (or frozen at the last reported value — the lag metric also stops updating when the source stops polling).

## Quick diagnosis (≤ 5 min)

```sh
# Confirm which source: the alert label tells you, but dashboards
# sometimes drop the label on flat-line queries.
curl -s http://api:9464/metrics | \
  grep -E "ratesengine_source_(events_total|enabled|last_event_unix)"

# Health snapshot for every source's connection state:
ratesengine-ops list-cursors -config /etc/ratesengine/config.toml

# Is upstream the issue?
ratesengine-ops rpc-probe http://stellar-rpc:8000
```

Key signals:
- **`Connected=false` in health / `rpc-probe` failing** → upstream is down. Jump to `rpc-lag.md`.
- **`Connected=true` but events==0** → source connection is alive, the contract just isn't emitting. Rare on SDEX / Aquarius; more common for Phoenix during low-volume hours.
- **Per-source-only issue (others fine)** → the source's filter is rejecting everything, OR its pair-cache is empty, OR a protocol upgrade broke its decoder. Check `decode-errors` alert for correlation.

## Mitigation

- [ ] Step 1 — if the RPC is healthy but THIS source has flat-lined: restart the indexer pod. Often resolves cases where a source goroutine panicked and silently died (we recover, but a hanging state can still surface as no-events).
  ```sh
  kubectl rollout restart deploy/ratesengine-indexer
  ```
- [ ] Step 2 — if events flow for 1-2 min post-restart then stop again: the contract is probably offline or dead. Compare the source's target contract to stellar.expert for recent activity. If truly dead, remove from `ingestion.enabled_sources` + open an incident to replace with a live contract.
- [ ] Step 3 — if decode-errors is also firing: the contract's event shape changed. Follow `decode-errors.md` Step 3 (update decoder + backfill).
- [ ] Verification: `rate(ratesengine_source_events_total{source=...}[5m]) > 0` within 2 min of mitigation.

## Known false-positive patterns

- **Low-volume sources during quiet windows**. Phoenix in particular can genuinely see zero swaps for 5+ minute stretches during off-peak hours. The alert can't distinguish "idle" from "stuck" without cross-checking the tip; once the source emits ANY event the alert clears. If this fires during known-quiet windows repeatedly, extend the `for:` window to 10 min.
- **Immediately post-deploy**. A restart briefly shows zero events while the source boots. The alert's 5 min window gives enough headroom, but very slow bootstraps (stellar-core catchup) can trip it.

## Related

- `all-ingestion-down.md` — P1 escalation when multiple sources stop.
- `rpc-lag.md` — upstream root cause.
- `decode-errors.md` — adjacent failure mode that can masquerade as source-stopped if every event is being rejected.
- `cursor-stuck.md` — persistence-layer sibling (events flowing but cursor not advancing).

## Changelog

- 2026-04-23 — initial draft.
