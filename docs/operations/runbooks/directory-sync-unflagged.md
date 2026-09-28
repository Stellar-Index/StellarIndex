---
title: Runbook — directory-sync-unflagged
last_verified: 2026-09-28
status: draft
severity: P3
---

# Runbook — `stellarindex_directory_sync_unflagged`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_directory_sync_unflagged` (P3 / ticket) |
| Detected by | Prometheus rule in `deploy/monitoring/rules/api.yml` and the R1 single-host overlay `configs/prometheus/rules.r1/api.yml`. |
| Typical MTTR | 15–60 min: review the upstream change behind each cleared tag. Clears on its own when the next daily sync commits with no un-flags. |
| Impact | **Trust.** Each un-flagged issuer's price is no longer withheld by the scam-pricing gate and is served on every gated surface. |

## What this fires on

`stellarindex-ops directory-sync` mirrors the stellar-expert public
directory into `account_directory`. A scam-class tag there makes
`internal/pricingguard.ScamGate` withhold the issuer's price. When a
committed sync removes such a tag, or prunes a flagged row, it counts an
**un-flag** and writes the count to the node_exporter textfile
`directory_sync.prom` as
`stellarindex_directory_sync_rows_changed{source, kind="unflagged"}`.

The rule fires on `max by (instance, source) (...) > 0`. The gauge holds
the most recent committed run's count, so the alert stays active until
the next daily run commits with no un-flags. A run that fails or is
refused writes nothing, and the previous count stays in place.

One run can clear at most `max(10, ceil(5 % of the addresses flagged
before the run))` tags. `directory-sync` refuses a larger snapshot with
`ErrDirectoryChurnExceeded` unless it is run with `-accept-churn`.

## Quick diagnosis (≤ 5 min)

1. **What did the run do?**
   `journalctl -u directory-sync | grep 'Synced:'`. The line gives the
   upserted, pruned, newly-flagged and un-flagged counts.
2. **Which addresses?** Compare the upstream repository's history for
   the day of the run with the tags you expected to see. A tag removed
   in a reviewed upstream commit is a genuine correction.
3. **Was the run pinned?** `grep EXTRA_FLAGS /etc/default/directory-sync`.
   If it has no `-sha256`, the run synced whatever the upstream branch
   held at fetch time.

## Remediation

- **Genuine upstream correction**: no action. The alert clears after
  the next daily run.
- **Not a genuine correction**: pin the sync to the last good upstream
  commit. Set `EXTRA_FLAGS=-url
  https://github.com/stellar-expert/public-directory/archive/<commit>.tar.gz
  -sha256 <hex digest of that tarball>` in `/etc/default/directory-sync`
  and run `systemctl start directory-sync`. The pinned snapshot restores
  the tags, and the churn ceiling still applies to it.

## Do NOT

- **Do not re-run with `-accept-churn` to "get past" a refusal** unless
  you have confirmed the upstream mass change is genuine. That flag
  removes the per-run ceiling on prunes, new flags and un-flags.
- **Do not silence this alert.** It is the only signal that an issuer
  the directory flagged is being priced again.

## Related

- [scam-gate-fail-open](scam-gate-fail-open.md): the scam-pricing gate
  serving unguarded on a directory lookup error.
- [systemd-unit-failed](systemd-unit-failed.md): a failed or refused
  `directory-sync` run.
