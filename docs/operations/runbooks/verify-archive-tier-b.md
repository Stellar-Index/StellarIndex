---
title: Runbook — verify-archive-tier-b
last_verified: 2026-08-25
status: ratified
severity: P3
---

# Runbook — `stellarindex_verify_archive_tier_b_*`

Covers both Tier B (checkpoint-anchor) alerts:
`stellarindex_verify_archive_tier_b_unit_failed` and
`stellarindex_verify_archive_tier_b_run_stale`.

## At a glance

| Field | Value |
| ----- | ----- |
| Alerts | `stellarindex_verify_archive_tier_b_unit_failed`, `stellarindex_verify_archive_tier_b_run_stale` |
| Severity | P3 (ticket — defence-in-depth over the page-level Tier A chain check) |
| Detected by | Prometheus rules in `deploy/monitoring/rules/verify-archive.yml` (node_exporter systemd collector) |
| Typical MTTR | 30 min (diagnosis) – hours (mirror re-sync / re-run) |
| Impact | None immediate — the API serves correct data from existing bytes. A failed/stale Tier B means the single-source-corruption anchor check is not confirming our LCM header-hashes against the local archive mirror. |

## What Tier B is

Tier B (`stellarindex-ops verify-archive -tier checkpoint`) cross-checks,
at every 64-ledger checkpoint, our LedgerCloseMeta's header-hash against
the canonical header-hash recorded in the **local** rs-stellar-archivist
mirror at `/srv/history-archive`. It catches **single-source corruption
that is still chain-link-consistent** — a failure mode Tier A (chain-link
integrity) is blind to, because a self-consistent-but-wrong chain passes
Tier A. The two run as separate nightly timers on R1 (Tier A 03:23 UTC,
Tier B 04:37 UTC), sharing one incremental state file keyed per tier.

The unit was added in task #33 / W8 recon 14a — before that, only Tier A
was scheduled, so the anchor check never ran.

## Symptoms

- `stellarindex_verify_archive_tier_b_unit_failed`:
  `verify-archive-tier-b.service` exited `failed`, sustained 5 min.
- `stellarindex_verify_archive_tier_b_run_stale`: the `tier-b.timer` hasn't
  fired cleanly in 36h+ (either disabled or every recent run failed).

## Quick diagnosis (≤ 5 min)

```sh
# Unit + timer state
ssh r1 'systemctl status verify-archive-tier-b.service verify-archive-tier-b.timer'

# Why did the last run fail?
ssh r1 'journalctl -u verify-archive-tier-b.service --since "yesterday" --no-pager | tail -60'

# Is the local mirror populated for the checked range? An empty/stale
# mirror makes every checkpoint "missed" (DAT-09 inconclusive-fatal).
ssh r1 'ls /srv/history-archive/ledger | head; df -h /srv/history-archive'
```

The journal's last lines indicate the failure mode:

| Pattern | Cause |
| ------- | ----- |
| `checkpoint anchor ... MISMATCH` | Our LCM header-hash disagrees with the mirror's canonical hash. Single-source corruption — escalate. |
| `checkpoint anchor inconclusive — N missed, 0 matched` | The local mirror is missing the checked range (not synced far enough). Not corruption — a mirror-sync gap. |
| `context deadline exceeded` / watchdog | Run hit its runtime bound; investigate per-chunk progress. |
| `access denied` / `403` | galexie-archive S3 creds in `/etc/default/stellarindex-ops` rotated/wrong. |

## Mitigation

- [ ] **Anchor MISMATCH**: STOP — this is the single-source-corruption signal
  Tier B exists to surface. Do NOT auto-recover. Compare the offending
  checkpoint's header-hash against a peer archive (SDF / Lobstr / SatoshiPay)
  and escalate per the RCA in
  [verify-archive-unit-failed](verify-archive-unit-failed.md).
- [ ] **Inconclusive / all-missed (mirror gap)**: the `/srv/history-archive`
  mirror hasn't synced the checked range. Advance the mirror (the
  rs-stellar-archivist sync that backfills `/srv/history-archive`), then
  re-run Tier B manually:
  ```sh
  ssh r1
  set -a; source /etc/default/stellarindex-ops; set +a
  /usr/local/bin/stellarindex-ops verify-archive \
    -config /etc/stellarindex.toml \
    -tier checkpoint -archive-root /srv/history-archive \
    -from 2 -workers 8 -max-runtime 1h
  ```
- [ ] **`run_stale`**: confirm the timer is enabled
  (`systemctl enable --now verify-archive-tier-b.timer`). Note that during a
  long Tier A bootstrap the shared singleton lock skips Tier B fires — the
  timer still records a trigger, so staleness only fires on genuine
  non-firing / repeated failure.
- [ ] **Verification**: the next scheduled `verify-archive-tier-b.service`
  run completes cleanly (unit state `active`/`inactive`, not `failed`).

## Related

- [verify-archive-unit-failed](verify-archive-unit-failed.md) — the Tier A
  (chain-link) sibling; the RCA for a hash mismatch is shared.
- [verify-archive-run-stale](verify-archive-run-stale.md) — the Tier A
  staleness page (the higher-urgency chain-integrity counterpart).
- ADR-0016 — per-region trust model that these nightly checks anchor.
- ADR-0017 — archive-completeness invariants (checkpoint tiers, DAT-09).
- `docs/operations/archival-node-bringup.md` §"Per-region trust +
  verification model".

## Changelog

- 2026-08-25 — initial draft alongside the task #33 / W8 recon 14a Tier B
  timer.
