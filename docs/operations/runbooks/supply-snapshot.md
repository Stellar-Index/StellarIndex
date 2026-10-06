---
title: Runbook — supply-snapshot
last_verified: 2026-10-06
status: living
severity: P3
---

# Runbook — supply-snapshot alerts

Rule file: `configs/prometheus/rules.r1/supply-snapshot.yml` (group `stellarindex.supply_snapshot`, `component: supply`; the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/supply-snapshot.yml`). Severities: `circulating_zero` and `critical_stale` are `severity: page` (P2); the other three are `severity: ticket` (P3).

All five alerts watch the **systemd-timer path**: `supply-snapshot.timer` -> `supply-snapshot.service` -> `stellarindex-ops supply snapshot` (native XLM, daily). There is no wrapper script. The binary writes `/var/lib/node_exporter/textfile_collector/supply_snapshot.prom` itself (`internal/supply/textfile.go`, gated on `-textfile-output` / the unit's `TEXTFILE_OUTPUT` env; metric names and labels are defined there). On r1 `/etc/default/supply-snapshot` is ansible-managed with `TEXTFILE_OUTPUT` set (`10-observability.yml`); finding it unset is config drift to codify and fix.

The second producer of `asset_supply_history` is the aggregator-resident goroutine (`runSupplyRefresh` in `cmd/stellarindex-aggregator`, gated by `[supply] aggregator_refresh_enabled = true`, emits `stellarindex_aggregator_supply_refresh_total{outcome=...}`). It is tracked by [supply-refresh.md#stellarindex_aggregator_supply_refresh_stalled](supply-refresh.md#stellarindex_aggregator_supply_refresh_stalled) / `supply-refresh.md#stellarindex_aggregator_supply_refresh_error_dominant`, not by these alerts. A goroutine-only deployment cannot trip `unit_failed` or `circulating_zero` (no textfile, series absent). On r1 BOTH paths are live (timer for native XLM; goroutine at 5 min cadence for the watched classic assets, `stellarindex.toml.j2` `[supply]` block). They are complementary: do NOT silence an alert here on the theory that the goroutine path covers it. Overview: [supply-pipeline.md](../../architecture/supply-pipeline.md).

Shared impact: `/v1/assets/{id}` F2 fields (total / circulating / max / market_cap_usd / fdv_usd) go stale or wrong.

Shared commands (r1 shapes):

```sh
ssh root@136.243.90.96
journalctl -u supply-snapshot.service -n 100 --output=cat
systemctl status supply-snapshot.timer; systemctl list-timers supply-snapshot.timer
systemctl start supply-snapshot.service        # force a one-off run
runuser -u stellarindex -- /usr/local/bin/stellarindex-ops supply snapshot -config /etc/stellarindex.toml -dry-run
```

The writer also dials ClickHouse for the ledger's close time (`-ch-addr`, default `127.0.0.1:9300`); a down ClickHouse fails the run (`unit_failed` territory).

## At a glance

- [`stellarindex_supply_snapshot_unit_failed_alert`](#stellarindex_supply_snapshot_unit_failed_alert) - last run exited non-zero (P3, 30 m)
- [`stellarindex_supply_snapshot_stale`](#stellarindex_supply_snapshot_stale) - no success for > 36 h (P3)
- [`stellarindex_supply_snapshot_critical_stale`](#stellarindex_supply_snapshot_critical_stale) - no success for > 72 h (P2)
- [`stellarindex_supply_snapshot_never_initialized`](#stellarindex_supply_snapshot_never_initialized) - gauge absent for 36 h (P3)
- [`stellarindex_supply_snapshot_circulating_zero`](#stellarindex_supply_snapshot_circulating_zero) - XLM circulating <= 0 (P2)

## stellarindex_supply_snapshot_unit_failed_alert

`stellarindex_supply_snapshot_unit_failed > 0`, `for: 30m`, `severity: ticket`. Typical MTTR 15-30 min. Impact bounded: F2 fields keep serving the previous good value but go stale until the writer recovers.

The gauge is emitted by the subcommand's own `supplySnapshotMaybeEmitFailure` (`internal/ops/supply/supply.go` + `internal/supply/textfile.go`) on failure.

Symptoms:

- `stellarindex_supply_snapshot_unit_failed{asset_key=...} > 0` for 30 min or more.
- Latest `supply-snapshot.service` run in journald exited non-zero.
- `last_success_timestamp` for the asset is older than the 24 h daily cadence.

Diagnosis (5 min): shared commands above (journal, dry-run), then validate config:

```sh
runuser -u stellarindex -- /usr/local/bin/stellarindex-ops docs-config | head   # parses cleanly?
grep -E "sdf_reserve_accounts|reserve_balances_stroops" /etc/stellarindex.toml
```

Root causes, roughly by frequency:

1. **Missing entry in `reserve_balances_stroops`, only when the fallback path is consulted.** `SupplyConfig.Validate` deliberately does not require a balance per `sdf_reserve_accounts` account (`internal/config/config.go`; the live AccountEntry observer may cover it and Validate has no DB access). The error fires at READ time, when the chained reader falls back to the static map for an observer-uncovered account (`internal/supply/config_reader.go`).
   - Signal: `supply: ConfigReserveBalanceReader: no balance configured for account G...`.
   - Fix: add the balance entry (bring-up fallback) or backfill the AccountEntry observer so the live path covers the account; re-run.
2. **Postgres unavailable.** `timescale.Open` or `InsertSupply` failed. Same flow as `pg-conns-saturated.md`: confirm reachability and pool depth. A down ClickHouse fails the run the same way.
3. **No ingestion cursors yet.** Fresh box: `resolveSnapshotLedger` errors "no ingestion cursors yet - pass -ledger explicitly until the indexer has produced a cursor."
   - Fix: wait for the indexer's first cursor, or set `EXTRA_FLAGS="-ledger <known-good>"` in `/etc/default/supply-snapshot`.
4. **Operator config edit broke parsing** (trailing comma, mistyped key). Signal: `config:` prefix in the error. Fix the TOML, reload.

   > **Blind spot: a config parse error never trips THIS alert.** Config-load and flag errors return BEFORE the first failure-gauge emit (`supplySnapshotMaybeEmitFailure` is reachable only after `config.LoadWithEnv` + `cfg.Supply.Validate` succeed), so the unit exits non-zero without setting `unit_failed=1`. Nothing watches `node_systemd_unit_state` for this unit, so it surfaces only ~36 h later via [`_stale`](#stellarindex_supply_snapshot_stale) (or [`_never_initialized`](#stellarindex_supply_snapshot_never_initialized) on a box that never succeeded). If you arrive from one of those, check journald for a `config:`-prefixed error first.

Mitigation: reproduce via diagnosis, apply the matching fix, `systemctl start supply-snapshot.service`. Verify `unit_failed` returns to 0 and `last_success_timestamp` updates.

False positive: first run after a fresh deploy, before the first daily fire; the 30 m `for` usually absorbs it.

## stellarindex_supply_snapshot_stale

`(time() - stellarindex_supply_snapshot_last_success_timestamp{asset_key=...}) > 36*3600`, `for: 5m`, `severity: ticket`, `alert_family: supply_snapshot_stale`. 36 h = 24 h cadence + 12 h cushion. Typical MTTR 15 min. Impact: F2 fields visibly old; `observed_at` more than a day behind chain state. This alert tracks only the timer-path gauge; see the shared intro for why it is real on r1.

Diagnosis (5 min):

```sh
systemctl status supply-snapshot.timer ; systemctl list-timers supply-snapshot.timer   # scheduled?
journalctl -u supply-snapshot.service --since "3 days ago" -n 50                       # last run?
ls -la /var/lib/node_exporter/textfile_collector/supply_snapshot.prom                  # textfile written?
systemctl start supply-snapshot.service                                                # force a run
```

Root causes:

1. **Timer disabled** (stopped for maintenance, not re-enabled). Fix: `systemctl enable --now supply-snapshot.timer`.
2. **Service unit failing every run.** Fires alongside [`unit_failed_alert`](#stellarindex_supply_snapshot_unit_failed_alert); follow that section.
3. **`TEXTFILE_OUTPUT` unset means this alert CANNOT be what fired.** With no textfile the series is absent and `time() - <missing>` is no data; that state belongs to [`_never_initialized`](#stellarindex_supply_snapshot_never_initialized). On r1 an unset value is ansible drift to fix.
4. **Clock skew.** A backwards host clock jump makes a recent run look ancient. Signal: `node_time_seconds` deviates from real time. Fix ntp, then run the writer once with a fresh clock.

Mitigation: identify the silent stage (timer / unit / textfile), apply the fix, force a run. Verify `last_success_timestamp` updates within 60 s after a successful run reaches node_exporter.

False positive: none from a never-initialized gauge. An absent series evaluates to no data, so if `_stale` fired the gauge existed and a run HAS succeeded at some point. r1 sat in the absent state silently for 24+ days before the 2026-05-08 audit, which is why `_never_initialized` exists.

## stellarindex_supply_snapshot_critical_stale

Same expression and diagnosis as [`_stale`](#stellarindex_supply_snapshot_stale) with threshold `> 72*3600`, `for: 5m`, `severity: page` (P2), same `alert_family`. Three days without a snapshot makes F2 fields visibly old to customers; escalate per the SEV-3 playbook. It firing means `_stale` has been firing for 36 h unanswered: follow the `_stale` section (and `unit_failed_alert` if runs are failing).

## stellarindex_supply_snapshot_never_initialized

`absent_over_time(stellarindex_supply_snapshot_last_success_timestamp[36h]) == 1`, `for: 5m`, `severity: ticket`. Typical MTTR 10 min (one-shot operator action). Impact: F2 fields (circulating / total / max / market_cap_usd / fdv_usd) render as `null` for every asset.

Why separate from `_stale`: `time() - <missing>` is no data, not infinity, so a deployment that never wrote a snapshot is invisible to `_stale`. The 36 h window matches `_stale`'s cushion so a fresh install awaiting its first daily fire does not false-positive. The aggregator alert `stellarindex_aggregator_supply_refresh_never_initialized` (`rules.r1/supply-refresh.yml`) deliberately routes its `runbook_url` to this page; its own section lives in `supply-refresh.md`.

Symptoms:

- Annotation: "supply snapshot has never published - pipeline uninitialized."
- `/v1/assets/native` and `/v1/assets/xlm` omit `circulating_supply` / `market_cap_usd` (omitted when null per the wire-shape contract).
- `psql ... -c "SELECT count(*) FROM asset_supply_history"` returns 0.

Diagnosis (5 min):

```sh
systemctl is-enabled supply-snapshot.timer     # "not-found" = never installed; "enabled" = installed
systemctl status supply-snapshot.timer --no-pager
systemctl status supply-snapshot.service --no-pager   # most recent run
grep -A 1 'aggregator_refresh_enabled' /etc/stellarindex.toml   # absent/false = goroutine path off
journalctl -u stellarindex-aggregator --since '2 days ago' | grep -E 'supply-refresh|supply.*ok|supply.*err'
```

Resolution. Only the ops-CLI textfile writer (Path A) emits `last_success_timestamp`, so only Path A clears this alert. Both paths populate `asset_supply_history`; r1 runs both (see shared intro), so they are not mutually exclusive.

Path A, systemd timer (daily):

```sh
sudo cp deploy/systemd/supply-snapshot.{service,timer} /etc/systemd/system/
sudo systemctl daemon-reload

# CRITICAL: wire the textfile output BEFORE starting. The shipped unit defaults
# `Environment=TEXTFILE_OUTPUT=` (EMPTY, no metrics emitted). Started as-is the
# snapshot writes to Postgres but never publishes last_success_timestamp, so this
# alert never clears and the verify grep below fails on a working writer.
cat <<'EOT' | sudo tee /etc/default/supply-snapshot
TEXTFILE_OUTPUT=/var/lib/node_exporter/textfile_collector/supply_snapshot.prom
EOT

sudo systemctl enable --now supply-snapshot.timer
sudo systemctl start supply-snapshot.service
sudo journalctl -u supply-snapshot.service --no-pager -n 50
# Expect: `Wrote snapshot for asset_key=XLM ledger=<N> basis=<...>` and zero exit.
curl -s http://localhost:9100/metrics | grep stellarindex_supply_snapshot_last_success_timestamp
# Expect one line with a recent unix timestamp (after node_exporter rescrapes the .prom).
```

The alert clears within 5 min of a successful first run.

Path B, aggregator goroutine (sub-minute cadence capable):

```sh
# /etc/stellarindex.toml
[supply]
aggregator_refresh_enabled = true
# optional: aggregator_refresh_cadence = "5m" (default)

sudo systemctl restart stellarindex-aggregator
sudo journalctl -u stellarindex-aggregator -f | grep -E 'supply-refresh'     # tick within one cadence
curl -s http://localhost:9465/metrics | grep stellarindex_aggregator_supply_refresh_total
# Expect at least one outcome="ok" line per watched asset_key.
```

Path B does NOT silence this alert. It clears the sibling aggregator `_never_initialized` alert and populates the goroutine-path metrics tracked by [supply-refresh.md#stellarindex_aggregator_supply_refresh_stalled](supply-refresh.md#stellarindex_aggregator_supply_refresh_stalled). If a deployment intentionally runs Path B exclusively, silence this textfile-path alert; `_stale` cannot fire there (its series is absent), so no silence is needed for it.

Why neither path is the default: the supply pipeline ships dormant by design. The operator-managed `reserve_balances_stroops` config is the source of truth for SDF reserves (subtracted from total to get circulating); without it the writer would emit nonsense, so the gate forces operator review before first publish. Wiring guide: [docs/operations/supply-snapshot.md](../supply-snapshot.md).

Verify (within `max(36 h, aggregator_refresh_cadence)`):

```sh
sudo -u postgres psql -d stellarindex -c \
  "SELECT asset_key, count(*) AS rows, max(time) AS latest
   FROM asset_supply_history GROUP BY asset_key ORDER BY asset_key"
curl -s 'https://api.stellarindex.io/v1/assets/native' | jq '.data.circulating_supply'   # numeric string, not null
```

`internal/supply/refresher.go`: the `OutcomeKindMissingFreshness` outcome surfaces when the opt-in `[supply].strict_freshness_required` is enabled.

## stellarindex_supply_snapshot_circulating_zero

`stellarindex_supply_snapshot_circulating_xlm{asset_key="XLM"} <= 0`, `for: 5m`, `severity: page` (P2). Typical MTTR 15-60 min. Impact: `/v1/assets/native` reports `circulating_supply: 0`, a customer-visible data-quality incident. Timer-path-only (live on r1): the gauge comes from `internal/supply/textfile.go`; an absent series is owned by [`_never_initialized`](#stellarindex_supply_snapshot_never_initialized).

Per ADR-0011 native XLM circulating = total - sum(SDF reserves). Non-positive means either the reserve-balance sum (live observer OR static fallback) equals or exceeds the frozen total, or the XLMComputer math is broken (regression).

Diagnosis (5 min). The writer reads reserve balances from the live LCM AccountEntry observer FIRST (chained-fallback reader, L2.12a, PRs #411-#413), so check the database before the TOML:

```sh
# 1. Latest snapshot (flags before the positional; Go's flag package stops at the first positional)
stellarindex-ops supply audit -config /etc/stellarindex.toml native

# 2. Which reserve source did the writer use? The reader consults the
#    account_observations hypertable first; the TOML map is used only when at
#    least one watched account has NO observation at-or-before the snapshot
#    ledger (the whole call then drops to the static map, no mixing).
runuser -u postgres -- psql -d stellarindex -c \
  "SELECT account_id, max(ledger) AS latest_obs,
          (SELECT balance_stroops FROM account_observations b
            WHERE b.account_id = a.account_id
            ORDER BY ledger DESC LIMIT 1) AS latest_balance
     FROM account_observations a
    WHERE account_id IN (SELECT unnest(ARRAY['G...','G...']))  -- paste sdf_reserve_accounts
    GROUP BY account_id;"

# 3. ONLY if step 2 shows an uncovered account (fallback in play): sum the TOML
#    balances vs the frozen total. A wrong TOML value is harmless while the
#    observer covers every account.
grep -A 100 "^\[supply" /etc/stellarindex.toml
python3 -c "
import re, sys
content = open('/etc/stellarindex.toml').read()
balances = re.findall(r'^\\s*\"?([A-Z0-9]+)\"?\\s*=\\s*\"?(\\d+)\"?', content, re.M)
total = sum(int(b) for _, b in balances if len(_) == 56)
print(f'sum of reserve balances: {total} stroops = {total/1e7:.2f} XLM')
print(f'frozen total:           500018068120000000 stroops = 50,001,806,812.00 XLM')
print(f'difference:             {500018068120000000 - total} stroops')
"

# 4. Dry-run to confirm reproduction
stellarindex-ops supply snapshot -config /etc/stellarindex.toml -dry-run
```

Root causes:

1. **Corrupt or inflated observer balances.** The live `account_observations` rows are summed first (`internal/supply/config_reader.go`, chained reader in `internal/ops/supply/supply.go`). A decode/backfill bug inflating a reserve's `balance_stroops` drives circulating <= 0 with a correct TOML.
   - Signal: step 2 `latest_balance` implausibly large vs stellar.expert.
   - Fix: file a P2 against the AccountEntry observer; as a stopgap correct the rows from chain state and re-run the writer.
2. **Reserve balance overstated in the TOML fallback** (only with an uncovered account). Operator copied an SDF value with the wrong scale (USD or XLM instead of stroops, inflating by 10^7). Signal: step 3 reserve total ~ 10^7 x frozen total. Fix: divide the entry by 10^7, re-run.
3. **All-reserve config.** `sdf_reserve_accounts` includes the issuer or a payment account; poisons BOTH paths since the observer sums whatever the list names. Signal: extra G-strkeys vs SDF's announcement. Fix: remove them.
4. **XLMComputer bug** (should not happen; algorithm is trivial). Signal: `supply snapshot -dry-run` gives the same wrong value with verified-correct inputs on both reserve paths. Fix: roll back the writer binary; file a P2 bug.

Mitigation: identify cause (observer coverage FIRST, TOML only if fallback is in play); observer-data error: correct/re-derive the affected `account_observations` rows and file the observer bug; config error: fix TOML and force a run; algorithm bug: roll back. In every case verify `circulating_supply > 0` on the next snapshot; the alert clears within 5 min of a corrected snapshot.

False positive: none realistic. A zero would be correct only if every XLM were burned; ADR-0011's zero-is-a-valid-answer note does not apply to native XLM (hard-capped, indestructible by design).

## Related

- `supply-cross-check-divergence.md` - when the value itself looks wrong (classic vs SAC divergence).
- `pg-conns-saturated.md` - Postgres reachability.
- [supply-refresh.md#stellarindex_aggregator_supply_refresh_stalled](supply-refresh.md#stellarindex_aggregator_supply_refresh_stalled), `supply-refresh.md#stellarindex_aggregator_supply_refresh_error_dominant` - goroutine-path counterparts.
- `archive-completeness.md#stellarindex_archive_completeness_stale` - same shape on the archive side.
- [supply-pipeline.md](../../architecture/supply-pipeline.md) (incl. "The chained-fallback reader pattern"), [ADR-0011](../../adr/0011-supply-algorithm.md) (Algorithm 1, native XLM), ADR-0021 (chained-fallback reserve reader).
