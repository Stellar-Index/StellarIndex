---
title: Runbook — verify-archive-tier-d
last_verified: 2026-09-28
status: ratified
severity: P3
---

# Runbook — `stellarindex_verify_archive_tier_d_run_stale`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_verify_archive_tier_d_run_stale` |
| Severity | P3 (ticket — defence-in-depth; Tier A's nightly chain-link carries the page-level signal) |
| Detected by | Prometheus rules in `deploy/monitoring/rules/verify-archive.yml` (`stellarindex_verify_archive_last_success_unix{tier="peers"}`, a node_exporter textfile) |
| Typical MTTR | 30 min (diagnosis) – re-run takes minutes to an hour |
| Impact | None immediate — the API serves correct data from existing bytes. A stale Tier D means the weekly multi-peer cross-check hasn't confirmed our archive against the network recently; a genuine fork would only be visible in journald until the next successful run. |

## What Tier D is

Tier D (`stellarindex-ops verify-archive -tier peers`) samples 50 peer
archives and compares their checkpoint hashes against ours — the failure
mode Tier A/B cannot see, since both anchor against our own mirror. If a
peer's bytes hash to a different chain than the network's signed reality,
Tier D catches it (ADR-0016 §7.4).

Tier D is installed as a weekly **cron** entry
(`stellarindex-verify-archive-tier-d`, Sunday 16:23 UTC —
`configs/ansible/roles/archival-node/tasks/14-stellarindex-services.yml`),
not a systemd timer, so there is no `node_systemd_unit_state` series to
alert a "run failed" pair off of. Staleness on
`stellarindex_verify_archive_last_success_unix{tier="peers"}` is the only
monitored signal, covering both "every recent run failed" and "the cron
entry was never installed" (gated on `verify_archive_tier_d_enabled` —
pubnet only, since a single-region testnet/futurenet host has no peer
region to cross-check).

## Symptoms

- `stellarindex_verify_archive_tier_d_run_stale`: no clean exit recorded
  in over 8 days (one missed weekly cycle plus slack) — or never
  recorded one.

## Quick diagnosis (≤ 5 min)

```sh
# Is the cron entry installed?
ssh r1 'crontab -l -u root | grep verify-archive-tier-d'

# What did the last run log?
ssh r1 'journalctl -t stellarindex-tier-d --since "-10 days" --no-pager | tail -80'
```

| Pattern | Cause |
| ------- | ----- |
| `PEERS DISAGREE` / mismatch reported | A sampled peer's checkpoint hash disagrees with ours. Escalate as a possible fork — compare the disputed checkpoint against a second peer before concluding which side is wrong. |
| `SELF DIVERGES FROM PEER CONSENSUS` / `our archive ... diverges` | The peers agree with each other but our `-archive-root` checkpoint JSON differs. Treat as a possible fork or corruption of OUR mirror — same STOP as peers disagreeing. |
| `our archive ... matched no consensus-verified checkpoint` / `missing from our archive` | Our mirror's `history/` tree lacks the sampled checkpoints (wrong `-archive-root`, or a hole inside the mirror's coverage). Check the mirror before re-running. |
| `verify-archive: ...` parse or config error | The rendered flags are wrong for this host shape; see `scripts/ci/verify-archive-tier-d-test.sh`. |
| no log entry at all in the window | The cron entry isn't installed, or `run-heavy-job.sh`'s lock was held by another heavy job for the entire window. Check `verify_archive_tier_d_enabled` in inventory. |

## Mitigation

- [ ] **Peers disagree**: STOP — do not auto-recover. Compare the
  disputed checkpoint against a second and third peer to determine
  which side diverges.
- [ ] **Cron entry missing**: re-run the archival-node role's
  `14-stellarindex-services.yml` tasks (tag `ops-jobs`) with
  `verify_archive_tier_d_enabled: true` in inventory.
- [ ] **Verification**: the next scheduled run (or a hand re-run)
  completes cleanly and advances
  `stellarindex_verify_archive_last_success_unix{tier="peers"}`.

## Related

- [verify-archive-run-stale](verify-archive-run-stale.md) — the Tier A
  staleness page (the higher-urgency chain-integrity counterpart).
- ADR-0016 §7.4 — Tier D in the cross-region trust model.

## Changelog

- 2026-09-28 — initial draft alongside the Tier D staleness alert.
