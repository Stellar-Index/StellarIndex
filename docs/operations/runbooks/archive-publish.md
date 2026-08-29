---
title: Runbook — archive-publish
last_verified: 2026-08-29
status: current
severity: P3
---

# Runbook — `stellarindex_stellar_archive_publish_fail`

> **INERT ALERT (re-verified 2026-08-29).** This alert is inert
> **everywhere**, not just on r1: the metric
> `stellarindex_stellar_archive_publish_errors_total` has **no
> producer anywhere in the codebase** (F-1329; listed in
> `scripts/ci/lint-metric-refs.sh`'s `KNOWN_INERT` set). On the
> deployment side, there is no **standalone** stellar-core service
> on r1 (removed 2026-04-23) — the only core is galexie's
> captive-core subprocess, and it does **not** publish history.
> `/srv/history-archive` was filled by `stellar-archivist mirror`
> (one-shot, completed) and is read-only today via the
> verify-archive integrity tiers. No process actively *publishes*
> to it.
>
> The rule remains in both trees for Phase-3 (Tier-1 validator
> rollout, ADR-0004) when a stellar-core of ours resumes
> checkpoint-publish duty. Until then this runbook is
> *future-tense*.

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_stellar_archive_publish_fail` — **inert, see banner** |
| Severity | P3 (`severity: informational`) |
| Detected by | `configs/prometheus/rules.r1/stellar.yml` (group `stellarindex.stellar`, `severity: informational`, `for: 1h`) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/stellar.yml`. |
| Typical MTTR | 1–4 h |
| Impact | stellar-core couldn't publish a checkpoint to our history archive. Not customer-visible. Matters for Tier-1 posture (ADR-0004) — we advertise continuous archive publishing. Sustained failures reflect badly on validator quality scores. |

## Symptoms

- `increase(stellarindex_stellar_archive_publish_errors_total[1h]) > 0`
  for ≥ 1 h.
- `stellar-core/info` may show a `history_archive_state` that's
  not `Published` for recent checkpoints.
- Archive scanners (GitHub's archive-divergence checker, LOBSTR's
  validator scoring) flag us as lagging on history.

## Quick diagnosis (≤ 5 min)

> NB: the `mc` commands below are **Phase-3 placeholders** — no
> `myminio/history-archive` bucket exists today. The archive is
> the `/srv/history-archive` filesystem; r1's MinIO holds
> `galexie-live`, `galexie-archive`, and `backups`
> (`configs/ansible/roles/archival-node/tasks/09-minio.yml`).
> Adjust to the real publish target when Phase-3 lands.

```sh
# stellar-core publisher logs
ssh root@<val-host> "journalctl -u stellar-core -n 500 --no-pager" \
  | grep -iE 'history|publish|upload'

# Can we write to the archive backend? (S3 / MinIO — Phase-3 topology)
mc ls myminio/history-archive/live/ | tail   # adjust alias
mc stat myminio/history-archive/

# Space + permission on the archive bucket
mc admin info myminio
```

## Typical root causes

1. **Archive backend (MinIO / S3) outage or auth failure**.
   Credentials rotated without updating core, bucket policy
   changed, bucket full.
   - Mitigation: fix auth; confirm bucket has capacity.

2. **Network egress broken** from the stellar-core host to the
   archive endpoint.

3. **Core compiled / configured wrong**. The `HISTORY` archives
   section in `stellar-core.cfg` has wrong `put` / `get` / `mkdir`
   commands. Less common but has happened after an upgrade.

4. **Disk full on the core host** — it stages checkpoints before
   uploading. If `/tmp` or the staging dir is out of space, upload
   fails before it even starts.

## Mitigation

- [ ] Step 1 — look at core's log to see the specific upload
      error.
- [ ] Step 2 — fix the backend cause (auth / space / network).
- [ ] Step 3 — core retries the publish on its next checkpoint
      (every 64 ledgers, ~5 min). No manual retry needed.
- [ ] Step 4 — if gaps exist in the archive, run the
      archive-repair procedure: `stellar-core publish` for specific
      checkpoints. Node-recovery context in
      [`docs/operations/archival-node-bringup.md`](../archival-node-bringup.md).
- [ ] Verification: `increase(... errors_total[1h]) == 0`; archive
      scanners show us caught up.

## Known false-positive patterns

- **Very brief transient** — S3 returns a 503, core retries,
  successfully publishes on retry. Counter still went up. The
  alert's `for: 1h` threshold filters these.
- **Deliberate archive cutover** — during a storage migration we
  may temporarily disable publishing. Silence during the window.

## Related

- `archive-divergence.md` — when what we publish differs from
  other validators (much worse than not publishing at all; also
  inert-in-practice today — see its banner).
- `db-disk-full.md` — staging-dir disk-full variant.
- ADR-0004 (three-validator + independent archives).

## Changelog

- 2026-08-29 — re-verified against HEAD. Banner tightened: the
  alert is inert EVERYWHERE (no producer exists in the codebase at
  all — F-1329, KNOWN_INERT in scripts/ci/lint-metric-refs.sh),
  not merely inert-on-r1. "stellar-core is not running on r1"
  refined: no STANDALONE service — galexie's captive-core
  subprocess runs, and it does not publish history. Dead pointer
  `bootstrap-archival-node.md` → `archival-node-bringup.md`. The
  mc commands marked as Phase-3 placeholders — no
  myminio/history-archive bucket exists (archive is the
  /srv/history-archive filesystem; MinIO holds
  galexie-live/galexie-archive/backups). Rule citation →
  `rules.r1/stellar.yml` with group/severity/for.
- 2026-04-23 — initial draft.
- 2026-04-30 — top-of-file deployment-posture callout: this alert
  is inert on r1 (stellar-core removed 2026-04-23, no active archive
  publisher). Retained for Phase-3 validator rollout.
