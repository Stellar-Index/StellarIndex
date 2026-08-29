---
title: r1 deployed versions — source of record + snapshot
last_verified: 2026-08-20
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

## Snapshot (2026-08-20, from the sidecars above)

Point-in-time; the sidecars are the live truth. Binaries deploy independently,
so a mixed fleet is normal.

| Binary                   | Deployed version | Deployed (host mtime) |
|--------------------------|------------------|-----------------------|
| stellarindex-api         | v0.38.2          | 2026-08-19            |
| stellarindex-indexer     | v0.38.2          | 2026-08-19            |
| stellarindex-ops         | v0.38.2          | 2026-08-19            |
| stellarindex-aggregator  | v0.36.0          | 2026-08-17            |
| stellarindex-sla-probe   | v0.36.0          | 2026-08-17            |
| stellarindex-migrate     | v0.28.1          | 2026-08-08            |

Notes:
- **Mixed versions are expected**, not drift: `migrate` only re-deploys when a
  new migration ships (its schema head is what matters, not its own tag), and
  `aggregate`/`sla-probe` roll on their own cadence.
- Legacy `ratesengine-*` sidecars may also be present on the host — those predate
  the binary rename and are NOT the current fleet; ignore them.

## Keeping this answerable

- The sidecars are automatic — no action needed for the *live* answer.
- Refresh the snapshot above (and `last_verified`) when reconciling deploy state
  (e.g. during a release or an audit), so the repo carries a recent, greppable
  deployed-vs-tagged record without an SSH round-trip. This is a snapshot, not
  the source of truth — when in doubt, read the sidecars.

See also: [deploy-workflow.md](deploy-workflow.md), [release-process.md](release-process.md),
[rollback.md](rollback.md), and the bringup-era [r1-deployment-state.md](r1-deployment-state.md)
(historical only).
