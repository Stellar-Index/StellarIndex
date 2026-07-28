---
name: deploy-r1
description: Deploy a released Stellar Index version to the r1 production host — the workflow invocation, migration semantics, post-deploy verification battery, and rollback. Use when asked to deploy, roll out, or roll back on r1.
---

# /deploy-r1

Full runbook: `docs/operations/deploy-workflow.md`. Deploys are
operator-triggered, never automatic on tag. Confirm with @ash before
deploying anything he hasn't asked to ship.

## Invoke

```sh
gh workflow run deploy.yml \
  -f region=r1 \
  -f version=vX.Y.Z \
  -f binaries=stellarindex-indexer,stellarindex-aggregator,stellarindex-api
```

Include `stellarindex-ops`/`stellarindex-migrate`/`stellarindex-sla-probe`
when the release changed them — r1's ops binary has drifted
out-of-band before; deploying it keeps timer units honest.

## What the workflow does (know before you press)

Downloads release binaries → verifies SHA256SUMS → stages migrations
FROM THE TAG's tree (binary↔migration parity) → applies migrations
BEFORE binary swap → per-binary: stage → backup `.prev-<tag>` →
atomic rename → restart → health probe → **automatic rollback of the
binary on probe failure**. Two sharp edges:

- **Migrations are NOT rolled back on a failed binary deploy**
  (CS-099, still open): a rollback leaves old-binary-on-new-schema.
  Before deploying a release with migrations, read them for
  old-binary compatibility (additive = fine; renames/drops = not).
- `migrations_skip` is a string→bool footgun with history — leave it
  alone unless you know why you're setting it.

## Post-deploy verification (ALWAYS, in order)

```sh
bash scripts/dev/r1-smoke.sh                       # 13 shape-asserted GETs (exit = failures)
API_BASE_URL=https://api.stellarindex.io bash scripts/dev/r1-smoke.sh   # through the edge

# COVERAGE checks — the smoke above is 13 hand-picked routes and will
# stay green while an entire subsystem is dark. Both exit non-zero on
# failure; treat a regression in either as deploy-blocking.
bash scripts/ops/route-sweep.sh                    # every OpenAPI GET; exit = 5xx count
bash scripts/ops/reconcile-supply-vs-horizon.sh    # served supply vs Horizon; exit = assets outside tolerance

ssh root@136.243.90.96 'systemctl status stellarindex-indexer stellarindex-aggregator stellarindex-api | grep -E "Active|●"'
ssh root@136.243.90.96 'bash -s' < scripts/ops/pre-launch-check.sh   # NOT installed on r1 — pipe it
```

**Why the two coverage checks exist** (2026-07-27/28): the 13-GET smoke
and the SLA probe both passed continuously while **21 of 94 GET routes
returned 503** — the whole explorer tier (`/accounts`, `/contracts`,
`/ledgers`, `/tx`, `/operations`) was dark and no check noticed, because
none of them touch those routes. Separately, the supply figures were
verified for months against the *trustline sum only*, which is exact and
therefore blind to the claimable/LP/SAC components — hiding a 13.2%
AQUA understatement. A check that only covers what already works
manufactures confidence; these two are the antidote. Baselines to
compare against: `route-sweep` server_5xx should be **0**;
`reconcile-supply` outside-tolerance should be **0** (was 3).

Then watch for 10–15 min: the freshness watchdog + verdict
(`/v1/coverage`, `/v1/status`) and the cursor advancing — a restart
once reset the ledgerstream cursor 65k ledgers back (2026-06-01);
first stuck-cursor hypothesis is ALWAYS `mc stat` the bucket for
cursor+1 (see /diagnose-stellarindex).

## Rollback

The workflow keeps the previous 5 binaries at
`/usr/local/bin/<binary>.prev-<tag>`. Manual rollback: stop unit,
`mv` the prev binary into place, restart, re-run the smoke. Runbook:
deploy-workflow.md §rollback. Remember the migration caveat above.
