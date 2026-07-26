---
title: Runbook — login-code-lockout-table-growing
last_verified: 2026-07-26
status: draft
severity: P3
---

# Runbook — `stellarindex_login_code_lockout_table_growing`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_login_code_lockout_table_growing` (P3 / ticket) |
| Detected by | Prometheus rules in `deploy/monitoring/rules/api.yml` + `configs/prometheus/rules.r1/api.yml` (C3-032 follow-up, audit-2026-07-23) |
| Typical MTTR | Minutes — either restart/repair the sweep, or apply the lagging migration |
| Impact | **Arm 1** (rows): a remote, unauthenticated caller is filling a table on a disk-fixed host. **Arm 2** (`status_check`): the brute-force bound on the 6-digit sign-in code is not being enforced, silently. |

## What this fires on

`login_code_lockouts` (migration 0122) is the durable per-email
failed-verify counter that bounds guessing of the dashboard's 6-digit
email code. Its primary key is **attacker-chosen**:
`POST /v1/auth/verify-code` is unauthenticated and accepts any
well-formed address, so

```
POST /v1/auth/verify-code {"email":"<random>@example.com","code":"000000"}
```

inserts a row for an address nobody owns — and the only thing that
deletes a row on the happy path is a *successful sign-in for that exact
address*, which can never happen. `internal/logincodereaper` (hourly,
48 h retention, live locks exempt) is what bounds the table.

Two arms:

| Arm | Expression | Meaning |
| --- | --- | --- |
| rows | `stellarindex_login_code_lockout_rows > 10000` | The sweep is not keeping up, is not running, or someone is filling faster than retention drains. Healthy is low tens. |
| fail-open | `increase(stellarindex_login_code_lockout_errors_total{op="status_check"}[1h]) > 0` | The pre-match lockout read errored and the handler **failed open** — the lockout was not enforced, and the HTTP response looked completely normal. |

Neither is an outage, and that is the point: without this alert the
first arm ends at a volume-level disk-full page that names the wrong
subsystem, and the second ends at an account takeover with no trace.

## Quick diagnosis (≤ 5 min)

1. **Which arm?**

   ```promql
   stellarindex_login_code_lockout_rows
   sum by (op) (increase(stellarindex_login_code_lockout_errors_total[1h]))
   ```

2. **Fail-open arm — is migration 0122 applied on every API node?** This
   is the cause the fail-open posture was chosen for.

   ```sh
   ssh r1 'sudo -u postgres psql stellarindex -c "\d login_code_lockouts"'
   ssh r1 "journalctl -u stellarindex-api --since '-2h' | grep 'login code lockout status unavailable'"
   ```

3. **Rows arm — is the sweep running at all?**

   ```sh
   ssh r1 "journalctl -u stellarindex-api --since '-6h' | grep 'login-code-lockout reaper'"
   ```

   Expect a start line at boot plus a `deleted settled rows` line
   whenever it removes anything. `op="sweep"` failures in the metric
   point at Postgres (statement timeout, lock contention, permissions).

4. **Rows arm — is it a fill or real traffic?** A fill is many distinct
   addresses each with a tiny `failed_count`:

   ```sh
   ssh r1 'sudo -u postgres psql stellarindex -c "
     SELECT failed_count, count(*) AS addresses, min(created_at), max(created_at)
       FROM login_code_lockouts GROUP BY 1 ORDER BY 2 DESC LIMIT 10;"'
   ```

   Thousands of addresses at `failed_count = 1` created in a tight
   window is a fill. A handful of addresses at `failed_count >= 10` is a
   targeted grinder — that is the control **working**; see
   `docs/audit/` C3-032 for the threat model.

## Remediation

1. **Fail-open arm: apply migration 0122** (or fix whatever made the
   read fail). Until then the durable lockout is off; the per-token
   `maxCodeAttempts = 5` cap still applies, so guessing is bounded per
   mint but not across mints.
2. **Rows arm, sweep broken:** fix the Postgres cause and confirm the
   next pass drains. A manual pass is safe and uses the same predicate:

   ```sql
   DELETE FROM login_code_lockouts
    WHERE updated_at < now() - interval '48 hours'
      AND (locked_until IS NULL OR locked_until <= now());
   ```

   **Keep both predicates.** The second is what protects a live lock: a
   row locked into the future must never be deleted at any age, or a
   grinder gets an early release.
3. **Rows arm, active fill:** the fill is bounded by
   `api.anon_rate_limit_per_min` (default 60). Check it is non-zero on
   the affected deployment, and block the source IP at the edge if it is
   a single origin. The rows themselves are harmless once retention
   catches up.

## Do NOT

- **Do not gate the INSERT on "this address has live tokens"** to stop
  the fill. It would work, and it would re-open C3-032: a grinder
  targeting a *real* address always has live tokens, but the whole
  purpose of the durable counter is to bound guessing **across mints**,
  and an address-existence gate is also an enumeration oracle. The
  table's retention is the right lever, not its write path.
- **Do not shorten retention below 24 h.** It must stay longer than
  `dashboardauth.durableCodeFailureWindow`, or the sweep starts
  truncating live counting windows and hands grinders a reset.
- **Do not "fix" the fail-open into a fail-closed.** Refusing sign-ins
  whenever this one table is unavailable turns a defence-in-depth blip
  into a dashboard-wide login outage — including during the exact
  migration-lag window that causes it. The counter, not a refusal, is
  the answer.

## Related

- [admin-audit-write-failing](admin-audit-write-failing.md) — the same
  pattern one surface over: a deliberately fail-soft control whose only
  evidence of degradation is its counter.
- [ratelimit-fail-open](ratelimit-fail-open.md) — the other fail-open
  control in the request path.
