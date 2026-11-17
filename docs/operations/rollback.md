---
title: Rollback procedures
last_verified: 2026-11-17
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
`down.sql` is not a production lever. `make preflight-deploy REGION=r1
VERSION=<previous-tag>` flags a dispatch to an older tag as a rollback and
points here.

## Rolling back the binaries

This is the one rollback command; the other runbooks link here.

```sh
gh workflow run deploy.yml \
  -f region=r1 \
  -f version=<previous-tag> \
  -f migrations_skip=true
sleep 10   # the dispatched run takes a few seconds to appear
gh run watch --exit-status \
  "$(gh run list --workflow=deploy.yml --event=workflow_dispatch --limit 1 --json databaseId -q '.[0].databaseId')"
```

- **No `-f binaries`.** The default is the region's whole set from
  `scripts/dev/region-binaries.tsv` (r1: indexer, aggregator, api,
  sla-probe, ops, migrate). The deploy's **Reconcile the binary set against
  the host** step refuses a partial set unless every omitted binary is
  already on the target tag, and after a release none of them is. A partial
  rollback is refused before anything on the host changes.
- **`migrations_skip=true` is required.** Without it the playbook syncs the
  old tag's `migrations/` (pruning the newer files) and runs `migrate up`,
  which fails with "no migration found for version N" whenever a migration
  landed after that tag, before any binary is swapped. The flag also skips
  the pgBackRest freshness gate, which only guards migrations.
- **Watch the run.** `gh workflow run` returns as soon as it dispatches; the
  next step of any procedure below waits on `gh run watch --exit-status`.
- The deploy does backup → swap → restart → health probe per binary and
  restores `.prev-<tag>` itself if a probe fails.
- If the release being rolled back changed a config surface (ansible
  `stellarindex.toml`, Prometheus rules, systemd units, schema), the run
  ends red at **Config-apply gate**, after the swap. The binaries are live:
  check **Run deploy playbook** passed and `/v1/version` reports the
  previous tag. Do not pass `config_acknowledged=true`: the host keeps the
  newer config, and the gate's marker should keep saying so.

The previous tag is in `git tag` history or the "Running version" line in
[`r1-deployment-state.md`](r1-deployment-state.md). Backups are kept as
`/usr/local/bin/<binary>.prev-<tag>`, the last 5
([`deploy-workflow.md`](deploy-workflow.md#backup-naming--rollback)):

```sh
ssh root@<host> 'ls -lh /usr/local/bin/stellarindex-*.prev-* 2>/dev/null'
```

### Manual fallback

Only if the deploy workflow itself is broken. Per binary, for every binary
the region runs:

```sh
PREVIOUS=<previous-tag>
BINARY=stellarindex-api
ssh root@<host> "
  systemctl stop ${BINARY} && \
  cp /usr/local/bin/${BINARY}.prev-${PREVIOUS} /usr/local/bin/${BINARY} && \
  echo ${PREVIOUS} > /var/lib/stellarindex/deployed-versions/${BINARY} && \
  systemctl start ${BINARY} && \
  systemctl status ${BINARY} --no-pager | head -20
"
```

The CLI binaries (`stellarindex-sla-probe`, `stellarindex-ops`,
`stellarindex-migrate`) have no unit: run only the `cp` and the sidecar
`echo`. The sidecar must be a single word (the tag): the next deploy's
backup step errors on anything else. Swapping only some binaries leaves the
host split across releases and fires `stellarindex_binary_version_skew`
until every binary is on one tag.

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
roll back with [the rollback command](#rolling-back-the-binaries), then go to
**§Post-rollback**.

### B. The release runs but breaks `/v1/price` correctness

Symptoms: wrong prices (ratio inverted, peg expansion off, FX leg unsnapped),
confidence collapsing to zero, freeze flags everywhere. Highest priority: bad
data accumulates in the trades hypertable and the CAGGs every minute.

r1 is the only production host ([ADR-0008](../adr/0008-ha-topology.md);
R2/R3 deferred):

```sh
# 1. Stop the aggregator so it stops computing prices with the bad binary.
#    If the wrong numbers come from ingest (the trade rows themselves are
#    wrong), stop stellarindex-indexer too; it resumes from its cursor.
ssh root@<host> "systemctl stop stellarindex-aggregator"

# 2. Roll back the whole set and wait for the result.
gh workflow run deploy.yml \
  -f region=r1 \
  -f version=<previous-tag> \
  -f migrations_skip=true
sleep 10
gh run watch --exit-status \
  "$(gh run list --workflow=deploy.yml --event=workflow_dispatch --limit 1 --json databaseId -q '.[0].databaseId')"

# 3. Only if step 2 succeeded: the deploy restarted every unit on the
#    previous tag. Confirm.
ssh root@<host> "systemctl is-active stellarindex-aggregator stellarindex-indexer stellarindex-api; \
  cat /var/lib/stellarindex/deployed-versions/stellarindex-aggregator"

# 4. Smoke-check.
stellarindex-sla-probe -base-url https://api.stellarindex.io/v1 \
  -duration 30s -concurrency 1
```

If step 2 fails, do **not** start the aggregator: it would run the bad
release again. Read the failed step's `::error::` line, correct the dispatch
and re-run it, or use the [manual fallback](#manual-fallback) for every
binary. A stopped aggregator is a freshness alert; a running bad one is
wrong prices.

#### Correcting what the broken release wrote

The rollback stops new bad rows; it does not touch the ones already
stored. Decide once the rollback has taken:

- **Missing rows** (a decoder that dropped events): replay the range. The
  command depends on who writes the domain:
  [the replay decision rule](../architecture/ingest-pipeline.md#the-replay-decision-rule).
- **Wrong rows**: `trades` upserts on `(source, ledger, tx_hash, op_index,
  ts)` (`ON CONFLICT … DO UPDATE`), so a replay over the range overwrites
  rows with the same key. A row whose key itself was wrong (for example a
  shifted `ts`) is not displaced: delete the range first, then replay.
- **Aggregates**: a `ch-rebuild` replay refreshes the trade and oracle
  aggregates over its range; after any other replay, refresh the CAGGs over
  the affected window ([`cagg-broad-recompute.md`](cagg-broad-recompute.md)
  for a wide one).
- Record the window and what was corrected in the SEV and the postmortem,
  and add an incident to the corpus (§E) if customers saw wrong numbers.

### C. The release runs but a single source is broken

Symptoms: `stellarindex_source_decode_errors_total{source="X"}` spiking,
`stellarindex_source_events_total{source="X"}` dropping to zero, the
`decode-errors` runbook firing.

DON'T roll back the release. Remove the source from the indexer's
allow-list: `/etc/stellarindex.toml` `[ingestion] enabled_sources` names the
only connectors the indexer runs. Ansible renders that file from
`stellarindex_enabled_sources`
(`configs/ansible/roles/archival-node/templates/stellarindex.toml.j2`), so
change the variable, not the file; a hand edit is overwritten by the next
playbook run and the broken source silently comes back.

```sh
# from configs/ansible/: drop the source from stellarindex_enabled_sources
# in the host's inventory (default: roles/archival-node/defaults/main.yml)
ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml \
  --tags stellarindex --check --diff            # always --check --diff first
ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml \
  --tags stellarindex
ssh root@<host> "grep -n '^enabled_sources' /etc/stellarindex.toml"
```

The template task hides its diff (`diff: false`: the file carries DSNs), so
the `grep` is the check. A changed file restarts the indexer, aggregator
and API through their handlers. Codify the change in a PR the same day. File a SEV-2 against the source's
package; the source runs degraded until the fix lands, then re-enable it the
same way.

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
`main`); status.stellarindex.io is the same build on the `stellarindex-status` project.

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

- [`v1-launch-plan.md`](v1-launch-plan.md#1-launch-sequence-and-gate-rationale): the cut-over this protects.
- [`release-process.md`](release-process.md): per-release procedure;
  §Post-flight has the rollback one-liner.
- [`sev-playbook.md`](sev-playbook.md): SEV escalation.
- [`postmortems/`](postmortems/): where the postmortem lands.
