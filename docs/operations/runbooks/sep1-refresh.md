---
title: Runbook — SEP-1 refresh alerts
last_verified: 2026-10-14
status: draft
---
# Runbook — SEP-1 refresh

All alerts here are `severity: ticket` (P3), identical in `configs/prometheus/rules.r1/sep1-refresh.yml` and `deploy/monitoring/rules/sep1-refresh.yml`.

The hourly `sep1-refresh.timer` runs `stellarindex-ops sep1-refresh` over the 750 stalest issuers and stamps every run's counters, an empty queue included, into `/var/lib/node_exporter/textfile_collector/sep1_refresh.prom` (temp file + rename). Every series is a gauge of the most recent run, so a rate alert is one run's fraction held for `2h30m`, i.e. three consecutive runs. A targeted `-issuer` run never writes the file. Resolution mechanics and the retry ladder: [sep1-resolution.md](../sep1-resolution.md).

## At a glance

- [`stellarindex_sep1_refresh_timeouts_high`](#stellarindex_sep1_refresh_timeouts_high): egress, DNS or the resolver budget; more than 30% of previously reached domains timed out.
- [`stellarindex_sep1_refresh_failing`](#stellarindex_sep1_refresh_failing): more than 30% of previously reached domains failed for any reason and the run's own systemic guard did not trip.
- [`stellarindex_sep1_refresh_stale`](#stellarindex_sep1_refresh_stale): no stamp for 3 h; the timer, the unit or the textfile path is broken.

### Quick diagnosis

```sh
systemctl list-timers sep1-refresh.timer --no-pager
systemctl status sep1-refresh.service --no-pager | grep -E '^ +Active:'
journalctl -u sep1-refresh -n 60 --no-pager -o cat | grep -E '^(FAIL|MOVED|[0-9]+ succeeded)'
cat /var/lib/node_exporter/textfile_collector/sep1_refresh.prom | grep -vE '^#' | grep -vE ' 0$'
```

The last command lists only the non-zero series: `failures{reason=…,reached=…}` says which reason dominates and whether the failing domains had ever served a toml.

## stellarindex_sep1_refresh_timeouts_high

**Impact:** issuer names, logos and org verification stop refreshing for the affected issuers; served identity goes stale but does not change. The retry ladder backs each failed issuer off, so a long outage on our side delays their next attempt by hours after the cause is fixed.

**Scope:** the reference host only; one run's `failures{reason="timeout",reached="true"}` over `reached`, held for three runs.

**Metric source:** `sep1_refresh.prom`, stamped by `sep1-refresh` at the end of each run. `timeout` covers the run deadline, a DNS lookup that timed out and any dial or response timeout.

**Steady state:** a healthy hourly run on the reference host stores 745–748 of 750 issuers; about half of the handful of failures are timeouts, well under 1% of reached.

1. Confirm it is not one issuer: the alert needs at least 20 reached issuers and 30% of them, so a single dead domain cannot trip it.
2. Check egress from the host: `curl -sS -m 10 -o /dev/null -w '%{http_code}\n' https://www.stellar.org/.well-known/stellar.toml` and `resolvectl query stellar.org`. A failing resolver or a saturated uplink shows here.
3. Check the host is not starved: `uptime` and `systemctl list-jobs`; a run beside a heavy catch-up job can push every fetch past the 10 s per-request budget.
4. If the run itself was cut short, `stellarindex_sep1_refresh_aborted` is 1 and the journal has `Aborted at N/750 (deadline)`; raise `RUN_TIMEOUT` in `/etc/default/sep1-refresh` only after the cause is understood.
5. Once the cause is fixed, the backed-off issuers return on the ladder's schedule; `stellarindex-ops sep1-refresh -issuer <G…> -write` refreshes one urgently.

## stellarindex_sep1_refresh_failing

**Impact:** as above, for any failure reason. The run's own guard unwinds the ladder when 90% or more of reached issuers fail (`stellarindex_sep1_refresh_systemic` 1, and the unit exits non-zero so the systemd-failed alert fires); this alert covers the band below that, where the ladder is still backing issuers off.

**Scope:** one run's `regressed` over `reached`, held for three runs, suppressed while the systemic flag is set.

**Metric source:** `sep1_refresh.prom`.

**Steady state:** `regressed` is single digits out of ~750 reached.

1. Read the reason split: `grep failures /var/lib/node_exporter/textfile_collector/sep1_refresh.prom | grep -v ' 0$'`.
2. `timeout` or `dns` dominant: follow [timeouts_high](#stellarindex_sep1_refresh_timeouts_high).
3. `tls` dominant: check the host clock (`timedatectl`) and the CA bundle (`ls -la /etc/ssl/certs/ca-certificates.crt`); an expired bundle fails every HTTPS domain at once.
4. `http` dominant: a CDN or hosting provider blocking the resolver's user agent fails many issuers together; the journal's `FAIL … HTTP 403` lines name the domains.
5. `lake` or `write` dominant: the ClickHouse auth-flags read or the Postgres write is failing; these are `our_fault` outcomes and the ladder step was already unwound for them. Check the stores first.
6. `parse`, `too_large`, `too_deep`, `payload`: the domains' documents are hostile or broken; expected in small numbers, never 30% of reached.

## stellarindex_sep1_refresh_stale

**Impact:** no refresh is happening at all; issuer identity ages silently. The data-freshness watchdog on `issuers.sep1_payload_fetched_at` catches the same condition later and more coarsely.

**Scope:** `time() - stellarindex_sep1_refresh_last_run_unix > 3 h`, or the series absent for 6 h on a host node_exporter has been scraped through.

**Metric source:** `sep1_refresh.prom`; the stamp happens after the queue is read, so a run that fails before its loop never stamps.

**Steady state:** the stamp advances every hour.

1. `systemctl list-timers sep1-refresh.timer --no-pager`: a missing or inactive timer means the unit was never enabled on this host.
2. `journalctl -u sep1-refresh -n 40 --no-pager -o cat`: a failure before the loop (config, ClickHouse or Postgres unreachable) exits before the stamp.
3. `ls -la /var/lib/node_exporter/textfile_collector/sep1_refresh.prom`: the unit writes as `stellarindex` through `ReadWritePaths`; a mode or ownership change on the directory fails the write with `sep1-refresh: WARN textfile:` in the journal while the run itself still succeeds.
4. If the file is fresh but the series absent, node_exporter is not reading the directory: check `curl -s localhost:9100/metrics | grep node_textfile_scrape_error`.

## Related

- [Alerts catalogue](../alerts-catalog.md)
- [SEP-1 resolution](../sep1-resolution.md)
- [Data freshness runbook](data-freshness.md)
