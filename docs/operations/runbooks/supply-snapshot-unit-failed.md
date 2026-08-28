---
title: Runbook — supply-snapshot-unit-failed
last_verified: 2026-08-29
status: ratified
severity: P3
---

# Runbook — `stellarindex_supply_snapshot_unit_failed_alert`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_supply_snapshot_unit_failed_alert` |
| Severity | P3 (`severity: ticket`) |
| Detected by | `configs/prometheus/rules.r1/supply-snapshot.yml` (group `stellarindex.supply_snapshot`, `severity: ticket`, `for: 30m`) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/supply-snapshot.yml`. |
| Typical MTTR | 15–30 min |
| Impact | `/v1/assets/{id}` F2 fields (total / circulating / max / market_cap_usd / fdv_usd) keep serving the previous good value, so bounded — but they go stale until the writer recovers. |

## Coverage caveat — timer-path-only alert

`stellarindex_supply_snapshot_unit_failed` is written by the
binary itself: there is **no wrapper script** — the
`supply-snapshot.service` unit ExecStarts
`stellarindex-ops supply snapshot` directly, and on failure the
subcommand emits the gauge via its own
`supplySnapshotMaybeEmitFailure` helper
(`internal/ops/supply/supply.go` + `internal/supply/textfile.go`),
gated on `-textfile-output` / the unit's `TEXTFILE_OUTPUT` env.
The aggregator-resident goroutine path (gated by
`[supply] aggregator_refresh_enabled = true`) doesn't run via
systemd-unit semantics, so this alert **cannot fire** on a
goroutine-only deployment. The equivalent failure signal there
is `supply-refresh-error-dominant.md` (≥ 50 % of refresher ticks
have a non-`ok` outcome). See
[supply-pipeline.md](../../architecture/supply-pipeline.md) for
the two-path overview.

## Symptoms

- `stellarindex_supply_snapshot_unit_failed{asset_key=…} > 0` for ≥
  30 min.
- The most-recent `supply-snapshot.service` invocation in journald
  exited non-zero.
- `last_success_timestamp` for the named asset is older than the
  daily cadence target (24 h).

## Quick diagnosis (≤ 5 min)

```sh
ssh root@136.243.90.96

# 1. Last run output.
journalctl -u supply-snapshot.service -n 100 --output=cat

# 2. Dry-run the writer to reproduce.
runuser -u stellarindex -- /usr/local/bin/stellarindex-ops supply snapshot \
  -config /etc/stellarindex.toml -dry-run

# 3. Validate config.
runuser -u stellarindex -- /usr/local/bin/stellarindex-ops docs-config | \
  head  # confirm parses cleanly
grep -E "sdf_reserve_accounts|reserve_balances_stroops" /etc/stellarindex.toml
```

## Typical root causes (roughly in frequency order)

1. **Missing balance entry in `reserve_balances_stroops` — but only
   when the fallback path is consulted.** `SupplyConfig.Validate`
   deliberately no longer requires a balance entry per
   `sdf_reserve_accounts` account (`internal/config/config.go` —
   the live AccountEntry observer may cover it, and Validate has no
   DB access to check). The error fires only at READ time, when the
   chained reader falls back to the static map for an
   observer-uncovered account
   (`internal/supply/config_reader.go`).
   - Signal: error `supply: ConfigReserveBalanceReader: no balance
     configured for account G...`.
   - Mitigation: add the missing balance entry (bring-up fallback),
     or backfill the AccountEntry observer so the live path covers
     the account; re-run.

2. **Postgres unavailable.** The writer's `timescale.Open` or
   `InsertSupply` call failed. Same diagnostic flow as
   `pg-conns-saturated.md` — confirm reachability and pool depth.
   (The writer also dials ClickHouse for the ledger's close time —
   `-ch-addr`, default `127.0.0.1:9300` — a down ClickHouse fails
   the run the same way.)

3. **No ingestion cursors yet.** On a fresh box without indexed
   ledgers, `resolveSnapshotLedger` errors with "no ingestion
   cursors yet — pass -ledger explicitly until the indexer has
   produced a cursor."
   - Mitigation: ride out the indexer's first cursor, or set
     `EXTRA_FLAGS="-ledger <known-good>"` in
     `/etc/default/supply-snapshot` until it lands.

4. **Operator config edit broke parsing.** Trailing-comma in the
   TOML, mistyped key, etc. The writer-start config-load fails.
   - Signal: `config:` prefix in the error message — but see the
     blind spot below.
   - Mitigation: fix the TOML, reload.

   > **BLIND SPOT — a config parse error never trips THIS alert.**
   > Config-load and flag errors return BEFORE the first
   > failure-gauge emit (`supplySnapshotMaybeEmitFailure` is first
   > reachable only after `config.LoadWithEnv` +
   > `cfg.Supply.Validate` succeed — `internal/ops/supply/supply.go`),
   > so a parse error exits non-zero WITHOUT ever setting
   > `unit_failed=1`. Nothing watches `node_systemd_unit_state` for
   > this unit, so the failure is silent at alert level and only
   > surfaces ~36 h later via `_stale` (or `_never_initialized` on
   > a box that never succeeded). If you're here from one of those
   > staleness alerts, check journald for a `config:`-prefixed
   > error first.

## Mitigation

- [ ] Step 1 — Walk Quick diagnosis to reproduce the failure mode.
- [ ] Step 2 — Apply the matching root-cause fix.
- [ ] Step 3 — Force a manual run: `systemctl start supply-snapshot.service`.
- [ ] Verification: `unit_failed` returns to 0 and `last_success_timestamp` updates.

## Known false-positive patterns

- **First run after a fresh deploy**, before the first daily cron
  fire. The `for: 30m` window typically absorbs this.

## Related

- `supply-snapshot-stale.md` — when no recent successful run exists
  (and where the config-parse blind spot above eventually surfaces).
- `supply-snapshot-never-initialized.md` — where a box that has
  never emitted the gauge at all surfaces.
- `supply-cross-check-divergence.md` — when the value itself looks wrong.
- `pg-conns-saturated.md` — Postgres reachability.

## Changelog

- 2026-08-29 — re-verified against HEAD. "Emitted by the unit's
  wrapper script" corrected — no wrapper exists; the unit
  ExecStarts `stellarindex-ops supply snapshot` directly and the
  binary writes the failure gauge itself
  (`supplySnapshotMaybeEmitFailure` + `internal/supply/textfile.go`,
  gated on `-textfile-output`/`TEXTFILE_OUTPUT`). New blind spot
  documented under root cause 4: config-load/flag errors return
  BEFORE the first failure-gauge emit, so a TOML parse error fails
  the unit silently (nothing watches `node_systemd_unit_state`) and
  surfaces only ~36 h later via `_stale`/`_never_initialized`.
  Root cause 1 rewritten: `SupplyConfig.Validate` deliberately no
  longer requires a per-account balance entry (observer may cover
  it); the error fires at READ time on fallback with message
  `supply: ConfigReserveBalanceReader: no balance configured for
  account G...`. Rule citation → `rules.r1/supply-snapshot.yml`;
  commands use r1 shapes.
- 2026-04-30 — initial draft alongside #295 (textfile + alerts).
- 2026-04-30 — coverage caveat added: this alert is timer-path-
  only and cannot fire on aggregator-resident-only deployments;
  goroutine-path equivalent is supply-refresh-error-dominant.md.
