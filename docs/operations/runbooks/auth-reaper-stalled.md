---
last_verified: 2026-09-02
alert: stellarindex_auth_reaper_stalled
severity: ticket
---

# Runbook — `stellarindex_auth_reaper_stalled`

## At a glance

| | |
|---|---|
| **Fires when** | `time() - stellarindex_auth_reaper_last_sweep_unix{reaper}` exceeds `3 × stellarindex_auth_reaper_interval_seconds{reaper}` for 15 min |
| **Severity** | ticket (P3) — table growth is slow; hours, not minutes |
| **Component** | `api` — the reapers run inside `stellarindex-api` |
| **Blast radius** | one of `login_code_lockouts` / `magic_link_tokens` / speculative-account orphans is no longer bounded; its rows gauge is frozen, not healthy |
| **First action** | `journalctl -u stellarindex-api --since -6h | grep -iE "reaper|panic"` |

## Why this exists

Three background reapers in the API binary bound tables an unauthenticated
caller can grow: `login_code_lockouts` (keyed by attacker-chosen email,
[login-code-lockout-table-growing](login-code-lockout-table-growing.md)),
`magic_link_tokens` ([magic-link-token-table-growing](magic-link-token-table-growing.md))
and speculative-account orphans (`internal/signupreaper`). Each reported
WHAT it did — rows deleted, errors, a rows gauge — but none reported THAT
it ran. A reaper that dies (a recovered panic, a Postgres call that never
returns, a goroutine that was never started) leaves every one of those
signals frozen at the last healthy value: the rows gauge sits at 12, the
errors counter at 0, and the `_table_growing` alerts (rows > 10000) would
only fire once the table had actually filled — the alarm arriving after
the damage. #368 M5.

`stellarindex_auth_reaper_last_sweep_unix{reaper}` is stamped at the end
of every COMPLETED sweep, including failed ones (a failing reaper is alive
and its errors counter already says so); the ctx-cancelled early return
does not stamp it, because that IS the reaper going away.
`stellarindex_auth_reaper_interval_seconds{reaper}` is the configured
cadence, so the threshold follows the deployment's own interval.

## Symptoms

- One `reaper` label (`login_code`, `magic_link`, `signup`) in the alert.
- The matching rows gauge (`stellarindex_login_code_lockout_rows`,
  `stellarindex_magic_link_token_rows`) has been perfectly flat since the
  last sweep timestamp — flat is the symptom, not reassurance.
- `stellarindex_worker_panics_total{worker}` may have incremented at the
  same moment if the reaper died by panic.

## Quick diagnosis (≤ 5 min)

1. When did it last sweep, and what is its cadence?
   ```promql
   stellarindex_auth_reaper_last_sweep_unix
   stellarindex_auth_reaper_interval_seconds
   ```
2. Did the goroutine die or is it stuck?
   ```sh
   journalctl -u stellarindex-api --since -6h | grep -iE "reaper|panic|recovered"
   ```
   A `worker panicked` line names the reaper; nothing at all means the
   goroutine is blocked (almost always a Postgres call — check
   `pg_stat_activity` for a long-running `DELETE FROM login_code_lockouts`
   / `magic_link_tokens` and what it waits on).
3. Is it simply disabled? A reaper that is off in config publishes NO
   series and cannot fire this alert — if the series exists, it was
   constructed and running at some point.

## Mitigation (≤ 15 min)

- A dead goroutine does not come back on its own: restart the API
  (`systemctl restart stellarindex-api`) — the reapers sweep immediately
  on start, which also clears the alert. Zero customer impact beyond the
  restart itself.
- A STUCK Postgres call: find and terminate the blocking backend
  (`pg_terminate_backend`) before restarting, or the restart blocks on the
  same lock.
- If the table is already large, one manual sweep is safe:
  the reapers only delete settled / expired rows (see each reaper's
  retention doc comment) — never live lockouts or unexpired links.

## Root cause analysis

- Panic: the recovered stack is in the API log next to the
  `stellarindex_worker_panics_total` increment — file it against the
  reaper package; the fix is the bug, not the restart.
- Hang: a lock held by a long transaction (bulk restore, migration) is the
  usual cause; the reapers use no explicit lock timeout, which is a
  known gap.
- Never started: a wiring regression in `cmd/stellarindex-api/main.go` —
  the guard test there checks that reaper goroutines are supervised.

## Known false-positive patterns

- Immediately after a deploy with a NEW interval (the gauge carries the
  old cadence until the next restart stamps the new one): self-clears
  within one sweep.
- Prometheus itself down or the API scrape failing: `time() - gauge`
  keeps growing on stale samples; check `up{job="stellarindex-api"}`
  first.

## Related

- [login-code-lockout-table-growing](login-code-lockout-table-growing.md)
- [magic-link-token-table-growing](magic-link-token-table-growing.md)
- [worker-panicked](worker-panicked.md)
- Metrics: [`stellarindex_auth_reaper_last_sweep_unix`](../../reference/metrics/README.md#stellarindex_auth_reaper_last_sweep_unix)

## Changelog

- 2026-09-02 — created with the gauges + alert (#368 M5).
