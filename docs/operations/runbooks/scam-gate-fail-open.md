---
title: Runbook — scam-gate-fail-open
last_verified: 2026-09-23
status: draft
severity: P3
---

# Runbook — `stellarindex_scam_gate_fail_open`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_scam_gate_fail_open` (P3 / ticket) |
| Detected by | Prometheus rule in `deploy/monitoring/rules/api.yml` and the R1 single-host overlay `configs/prometheus/rules.r1/api.yml`. |
| Typical MTTR | 5–30 min: clears on its own once `account_directory` is readable again; the fix is whatever made it unreadable. |
| Impact | **Trust.** Every price surface that consults the scam-pricing gate serves directory-flagged issuers' prices at 200 for as long as the lookups fail. |

## What this fires on

`internal/pricingguard.ScamGate` withholds the aggregated price of any
pair whose issuer carries a scam-class tag in the curated
`account_directory` table. When the directory lookup **errors**, the gate
deliberately **fails open**: failing closed would blank every asset's
price on a database blip. A failed lookup is not cached, so the next
request asks again.

Every fail-open consultation increments
`stellarindex_scam_gate_lookup_failures_total{surface}` and logs
`scam pricing gate: directory lookup failed — serving unguarded`. A
cancelled client request is not counted, because it serves nothing. The
rule fires on `sum by (surface) (rate(...[5m])) > 0` sustained
`for: 5m`, so one error during a failover does not ticket.

`surface` names the serving path (`price_read`, `tip`, `oracle`,
`price_at`, `asset_headline`, `vwap`, `twap`, `chart`, `price_stream`,
`dex_tvl`, …). `price_alert` is the aggregator's customer price-alert
evaluator; every other surface is the API. A single surface firing alone
points at that path's context budget; every surface firing together
points at the database.

## Quick diagnosis (≤ 5 min)

1. **Is Postgres reachable?** Check the API's `/readyz`. If the
   Timescale checker is red, this is a database incident; treat its
   runbook as primary and this alert as a consequence.
2. **What is the error?**
   `journalctl -u stellarindex-api | grep 'scam pricing gate'` (or
   `-u stellarindex-aggregator` for `price_alert`) — the WARN
   line's `err` field names the cause (lock wait, `relation does not
   exist` mid-migration, pool exhaustion, `context deadline exceeded`).
3. **Is the table readable?**
   `psql "$DATABASE_URL" -c 'SELECT count(*) FROM account_directory'`.
   A lock held by a long `directory-sync` transaction or a migration
   shows here as a hang.

## Remediation

- **Database down or degraded** → follow the database recovery path. The
  gate self-heals on the next successful lookup; verdicts are cached for
  60 s per issuer after that.
- **Lock contention from `directory-sync`** → let the sync finish or
  cancel it. It is safe to re-run.
- **Pool exhaustion** → the gate shares the API's Postgres pool; relieve
  the load that is exhausting it.

## Do NOT

- **Do not "fix" this by failing closed.** A directory error would then
  withhold every classic asset's price on every surface. The fail-open
  is deliberate; this alert exists so the unguarded window is observed.

## Related

- [ratelimit-fail-open](ratelimit-fail-open.md) and
  [monthly-quota-fail-open](monthly-quota-fail-open.md) — the other
  API fail-open paths, alerted the same way.
- `stellarindex_price_serve_scam_withheld_total` in the
  [metrics reference](../../reference/metrics/README.md) — the gate's
  success-side counter. A drop to zero while this alert fires is the
  same outage seen from the other side.
