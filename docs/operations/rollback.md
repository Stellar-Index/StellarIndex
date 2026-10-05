---
title: Rollback procedures
last_verified: 2026-06-12
status: operator runbook
---

# Rollback procedures

Reversal mechanics for a launch or any later release. Rollback is a
**separate** decision from incident response (per
[`release-process.md`](release-process.md#post-flight), any first-hour alert
is SEV-2 minimum). **Roll back fast, write the postmortem after**: a wrong
rollback is recoverable; a slow one lets a broken release accumulate state.

**Migrations do not roll back** (CS-099). A binary rollback runs the previous
binary on the forward schema, which is safe only because every migration is
old-binary-safe ([migrations/README.md](../../migrations/README.md) rule 9).
`down.sql` is not a production lever. `make preflight-deploy` flags a dispatch
to an older tag as a rollback and points here.

## Decision tree — should we roll back?

```
                    ┌─ p99 latency > 1s sustained 5 min
                    │
Customer impact?    ├─ price returned with confidence < 0.05 for popular pair
                    │
                    ├─ /v1/healthz returns 5xx for > 60s
                    │
                    └─ ALL OF: any non-2xx > 1% rate, sustained 2 min
                              ↓
                        YES → ROLL BACK
                              (then file SEV-1 + open postmortem)

       ┌─ Single-component degraded (e.g. one source dropped)
       │
       ├─ Latency ≤ 500ms p95
       │
       └─ Documented graceful-degradation path engaged
                              ↓
                        NO → DO NOT ROLL BACK
                              (file SEV-2 + diagnose forward)
```

If unsure, **roll back**. An unnecessary rollback costs one release tag;
letting bad data accumulate costs corrupted history to backfill or truncate.

## Failure-mode triage

### A. The release didn't take

Symptoms: API binary won't start, indexer panics on boot, aggregator won't
connect to Redis. Diagnose with `systemctl status stellarindex-{api,indexer,
aggregator}` on r1. A release that crashes at startup never served traffic;
rollback is re-deploying the previous tag.

The deploy workflow keeps previous binaries as
`/usr/local/bin/<binary>.prev-<previous-tag>`
([`deploy-workflow.md`](deploy-workflow.md#backup-naming--rollback)).
Preferred path: re-trigger it with the previous known-good tag (host-side
backup→swap→restart→health-probe, automatic rollback on probe failure):

```sh
gh workflow run deploy.yml \
  -f region=r1 \
  -f version=vX.Y.Z \
  -f binaries=stellarindex-api
```

The previous tag is in `git tag` history or the "Running version" line in
[`r1-deployment-state.md`](r1-deployment-state.md). Confirm the
`.prev-<tag>` is still on disk first:

```sh
ssh root@<host> 'ls -lh /usr/local/bin/stellarindex-*.prev-* 2>/dev/null'
```

Manual fallback, only if the deploy workflow itself is broken
([`release-process.md` §Rollback](release-process.md#rollback)):

```sh
PREVIOUS=vX.Y.Z                               # the known-good tag
BINARY=stellarindex-api
ssh root@<host> "
  systemctl stop ${BINARY} && \
  cp /usr/local/bin/${BINARY}.prev-${PREVIOUS} /usr/local/bin/${BINARY} && \
  echo ${PREVIOUS} > /var/lib/stellarindex/deployed-versions/${BINARY} && \
  systemctl start ${BINARY} && \
  systemctl status ${BINARY} --no-pager | head -20
"
```

The sidecar must be a single word (the tag): the next deploy's backup step
errors on anything else. Then go to **§Post-rollback**.

### B. The release runs but breaks `/v1/price` correctness

Symptoms: wrong prices (ratio inverted, peg expansion off, FX leg unsnapped),
confidence collapsing to zero, freeze flags everywhere. Highest priority: bad
data accumulates in the trades hypertable and the CAGGs every minute.

r1 is the only production host ([ADR-0008](../adr/0008-ha-topology.md);
R2/R3 deferred):

```sh
# 1. Stop the aggregator on r1 — preserves the cache in its
#    last-good state while we swap binaries.
ssh root@<host> "systemctl stop stellarindex-aggregator"

# 2. Re-deploy all three binaries at the previous-known-good tag.
#    The deploy workflow does the per-host backup→swap→restart→
#    health-probe with automatic rollback on probe failure.
gh workflow run deploy.yml \
  -f region=r1 \
  -f version=vX.Y.Z \
  -f binaries=stellarindex-indexer,stellarindex-aggregator,stellarindex-api

# 3. (The aggregator was restarted by the workflow in step 2; if it
#    was left stopped because the workflow only re-deployed a subset,
#    start it explicitly.)
ssh root@<host> "systemctl start stellarindex-aggregator"

# 4. Smoke-check.
stellarindex-sla-probe -base-url https://api.stellarindex.io/v1 \
  -duration 30s -concurrency 1
```

Rows the broken release wrote to trades: decide after the rollback whether
to truncate or leave. A broken decoder usually produced *missing* rather than
*wrong* data, so leave-and-backfill is the cheap recovery; the
`(source, ledger, tx_hash, op_index)` primary key prevents duplicates on
re-ingest.

### C. The release runs but a single source is broken

Symptoms: `stellarindex_source_decode_errors_total{source="X"}` spiking,
`stellarindex_source_events_total{source="X"}` dropping to zero, the
`decode-errors` runbook firing.

DON'T roll back the release. Remove the source from the `[ingestion]`
allow-list; the indexer runs only the connectors named in `enabled_sources`:

```toml
# /etc/stellarindex.toml
[ingestion]
# Drop the broken source from this list (it's an allow-list, not a
# per-source enabled=false flag). Valid names: see config.KnownSources.
enabled_sources = ["soroswap", "aquarius", "phoenix", "..."]
```

```sh
# Apply on r1, then restart the indexer to pick up the new config.
scp stellarindex.toml root@<host>:/etc/stellarindex.toml
ssh root@<host> "systemctl restart stellarindex-indexer"
```

File a SEV-2 against the source's package; the source runs degraded until
the fix lands, then re-enable it.

### D. Public-repo content went wrong

Symptoms: unintended files, accidentally included secrets, wrong
license/CONTRIBUTING headers. The public repo IS the repo: there is no
private source of truth to re-cut from.

**Public-repo rollback rolls FORWARD.** Never rewrite history; never delete
or re-create the repository (that destroys history, issues, PRs and release
artifacts irreversibly).

```sh
# Revert the bad commit(s), oldest-first, and push normally.
git revert --no-edit <sha>            # or <oldest>^..<newest> for a range
git push origin main
```

> ⚠️ **Do not force-push `main`.** `main` carries no branch protection and no
> rulesets, so a force-push succeeds: it rewrites public history, breaks every
> clone, fork and open PR, and orphans the commits release tags point at.

A revert keeps the bad commit in history on purpose: it is the only rollback
every clone converges on without coordination.

If a revert cannot undo the damage (**secrets in history**, an unintended
file set), a force-push cannot either: the objects survive via pre-rewrite
refs, forks and the events API. Treat it as a security incident per
[SECURITY.md](../../SECURITY.md): **rotate the exposed credential first**,
then decide about history surgery with the maintainers.

### E. Status page misbehaving

Symptoms: `stellarindex.io/status` shows components down when production is
fine, or vice versa. Lowest stakes: the page is a derived view and carries no
production traffic. It is part of the explorer static export
(`web/explorer/src/app/status/`, Cloudflare Pages, deployed on push to
`main`); `web/status/` is now only a redirect stub.

1. **Edit + push** (preferred). Edit the incident corpus
   `internal/incidents/data/<YYYY-MM-DD>-<slug>.md`, the single source of
   truth: `web/explorer/src/lib/incidents.ts` reads it at build time and
   `/v1/incidents` serves it embedded in the Go binary. Cloudflare Pages
   redeploys in ~2 minutes; `/v1/incidents` reflects the edit only after
   `stellarindex-api` is re-deployed.
2. **Revert** if the page itself broke: `git revert <bad-sha>` on `main` and
   push; the previous good build redeploys.

If it cannot be corrected within the SEV-2 detection window:

```sh
# DNS revert — point status. at a previous Cloudflare Pages
# deployment by re-promoting an earlier successful build via the
# Pages dashboard, or aim status.stellarindex.io at a temporary
# maintenance page hosted elsewhere.
```

## Post-rollback

1. **Confirm it took.** Re-run the SLA probe; per-pair freshness gauges
   return to nominal.
2. **File the SEV.** Title `SEV-1: <vX.Y.Z> rolled back due to <symptom>`;
   body: which decision-tree branch fired, the rollback command, current state.
3. **Customer comms.** If the broken release was live for a non-trivial
   window, follow up on the launch-day thread from
   [`deploy/comms/rollback-update.md`](../../deploy/comms/rollback-update.md):
   what was wrong, what was rolled back, the customer-visible impact.
4. **Open the postmortem** (SEV-1 template), the same day.
5. **Block forward releases** until the postmortem names the root cause and
   a re-tested fix has landed.

## Cross-references

- [`launch-day-checklist.md`](launch-day-checklist.md): the cut-over this protects.
- [`release-process.md`](release-process.md): per-release procedure;
  §Post-flight has the rollback one-liner.
- [`sev-playbook.md`](sev-playbook.md): SEV escalation.
- [`public-flip.md`](public-flip.md): public-repo cut-over mechanics (shape D).
- [`postmortems/`](postmortems/): where the postmortem lands.
