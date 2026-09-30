---
title: r1 deployed versions — source of record + snapshot
last_verified: 2026-09-29
status: reference
---

# r1 deployed versions

**Deployed-vs-tagged is answered here.** Historically (REC-07, audit-2026-08-14)
the only place the *deployed* version of each binary could be resolved was r1's
on-host sidecar files — no in-repo doc recorded deploys past the bringup era, so
"what is actually running on r1?" was unanswerable from the repo for the
v0.31.0..v0.33.2 window. This file closes that gap: it names the authoritative
source and carries a periodically-refreshed snapshot.

## Authoritative source (always current)

Every `ansible` binary deploy writes the version it just installed to a sidecar
on r1 (`configs/ansible/tasks/deploy-one-binary.yml` — the pre-deploy step reads
the previous value from it for the rollback path, the post-deploy step
overwrites it). (The from-scratch bootstrap path — `manage_stellarindex_binaries: true`
in the test-net inventories, `14-stellarindex-services.yml` — writes the
same sidecars with the controller checkout's `git describe --tags
--always --dirty`, e.g. `v0.47.2-3-g1a2b3c4-dirty`, so a later `deploy.yml`
run labels its rollback copy truthfully instead of `untracked-<ts>`.)
So the **live source of record** is:

```
/var/lib/stellarindex/deployed-versions/<binary>
```

Query it directly (read-only) for the current truth:

```sh
ssh root@r1 'for f in /var/lib/stellarindex/deployed-versions/stellarindex-*; do \
  printf "%-26s %s  (%s)\n" "$(basename "$f")" "$(cat "$f")" "$(stat -c %y "$f" | cut -d. -f1)"; done'
```

The sidecar is written atomically at deploy time, so its mtime is the deploy
timestamp and its contents are the tag that was deployed — this is what
distinguishes *deployed* from merely *tagged* (a `git tag` / release cut does
not imply the fleet moved to it).

## Snapshot (2026-09-29, from the sidecars above)

Point-in-time; the sidecars are the live truth. The release-managed set
(`aggregator`/`api`/`indexer`/`migrate`/`ops`/`sla-probe`) is monitored for
skew — `stellarindex_binary_version_skew`
(`deploy/monitoring/rules/binary-version-skew.yml`) pages after 45m if they
disagree. A brief mismatch mid-rollout is expected as binaries swap one at a
time; a mismatch that persists is the F-1314 / 2026-08-28 drift class the
[runbook](runbooks/binary-version-skew.md) covers, not a state to leave alone.

| Binary                   | Deployed version | Deployed (host mtime) |
|--------------------------|------------------|-----------------------|
| stellarindex-api         | v0.92.1          | 2026-09-29            |
| stellarindex-indexer     | v0.92.1          | 2026-09-29            |
| stellarindex-ops         | v0.92.1          | 2026-09-29            |
| stellarindex-aggregator  | v0.92.1          | 2026-09-29            |
| stellarindex-sla-probe   | v0.92.1          | 2026-09-29            |
| stellarindex-migrate     | v0.92.1          | 2026-09-29            |

Notes:
- `migrate` is in `deploy.yml`'s default binary set and its "Reconcile the
  binary set against the host" step refuses to dispatch a release that would
  leave `migrate` behind (i.e. omitting it is only accepted when it is
  already on the version being deployed) — so `migrate` redeploys with every
  release, on the same cadence as `api`/`indexer`/`ops`, not on its own
  schedule. `aggregator` and `sla-probe` are the ones that still roll
  independently; a snapshot taken mid-cycle can catch them a release apart
  from the rest — that gap should close on their next deploy. Any gap
  involving `migrate`, or one that does not close on the next deploy of the
  lagging binary, is drift, and `stellarindex_binary_version_skew` will be
  paging on it.
- The config-apply gate (`.github/workflows/deploy.yml`, "config baseline")
  separately excludes `migrate` from the *config* version-baseline minimum
  (#427): that exclusion is about `migrate` gating no config surface, not
  about deploy cadence, and stays regardless of the above — it does not
  reintroduce a "migrate lags by design" policy.
- The same minimum runs over the region's manifest set only
  (`scripts/dev/region-binaries.tsv`): a sidecar for a binary the manifest
  excludes at that region — testnet's `stellarindex-aggregator`, unit
  disabled, sidecar frozen at v0.63.0 — is reported as ignored and does not
  drag the baseline back. Nothing deploys that binary there, so its sidecar
  says nothing about which config the host has applied.
- Legacy `ratesengine-*` sidecars may also be present on the host — those predate
  the binary rename and are NOT the current fleet; ignore them.

## Planned maintenance

| Date (UTC) | Release | Service | Window | Duration |
|------------|---------|---------|--------|----------|
| 2026-09-29 | v0.92.1 (schema 162→187) | API | 03:35:19–03:36:54, plus two restarts of seconds each (binary swap 03:42:37, config apply ~03:49:50) | 1m35s |
| 2026-09-29 | v0.92.1 | aggregator (prices, MEV detection) | 03:35:19–03:41:13 | 5m54s |
| 2026-09-29 | v0.92.1 | cap67 movements writer | 03:35:19–~03:49:50 | ~14m30s |

## Keeping this answerable

- The sidecars are automatic — no action needed for the *live* answer.
- Refresh the snapshot above (and `last_verified`) when reconciling deploy state
  (e.g. during a release or an audit), so the repo carries a recent, greppable
  deployed-vs-tagged record without an SSH round-trip. This is a snapshot, not
  the source of truth — when in doubt, read the sidecars.

See also: [deploy-workflow.md](deploy-workflow.md), [release-process.md](release-process.md),
[rollback.md](rollback.md), and the bringup-era [r1-deployment-state.md](r1-deployment-state.md)
(historical only).
