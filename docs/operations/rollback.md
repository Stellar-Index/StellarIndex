---
title: Rollback procedures
last_verified: 2026-06-12
status: operator runbook
---

# Rollback procedures

What to do when a launch — or any subsequent release — needs to
be reverted. Per
[`release-process.md`](release-process.md#post-flight) the
default escalation is "any first-hour alert ⇒ SEV-2 minimum",
but rollback is a **separate** decision from incident response —
this doc covers the reversal mechanics.

The principle: **roll back fast, write the postmortem after.**
A wrong rollback is recoverable; a slow rollback that lets a
broken release accumulate state isn't.

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

If unsure, **roll back**. The cost of an unnecessary rollback
is one extra release tag; the cost of letting bad data
accumulate is corrupted history that has to be backfilled-
or-truncated later.

## Failure-mode triage

### A. The release didn't take

Symptoms: API binary won't start, indexer panics on boot,
aggregator won't connect to Redis.

Diagnosis is fast — `systemctl status stellarindex-{api,indexer,
aggregator}` on r1. If the new release crashes at startup, it never
served real traffic; rollback is just re-deploying the previous tag.

The deploy workflow keeps the last 5 previous binaries on disk as
`/usr/local/bin/<binary>.prev-<previous-tag>` (see
[`deploy-workflow.md`](deploy-workflow.md#backup-naming--rollback)).
Preferred path is to re-trigger the deploy workflow with the
previous-known-good tag — it does the host-side
backup→swap→restart→health-probe with automatic rollback on probe
failure:

```sh
gh workflow run deploy.yml \
  -f region=r1 \
  -f version=vX.Y.Z \
  -f binaries=stellarindex-api
```

Find the previous tag from `git tag` history or the "Running
version" line in [`r1-deployment-state.md`](r1-deployment-state.md).
Confirm the `.prev-<tag>` is still on disk first:

```sh
ssh root@<host> 'ls -lh /usr/local/bin/stellarindex-*.prev-* 2>/dev/null'
```

Manual fallback (only if the deploy workflow itself is broken — see
[`release-process.md` §Rollback](release-process.md#rollback)):

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

Skip directly to **§Post-rollback** below.

### B. The release runs but breaks `/v1/price` correctness

Symptoms: prices reading wrong values (ratio inverted, peg
expansion off, FX leg unsnapped), confidence scores
collapsing to zero, freeze flags fired everywhere.

Highest-priority rollback. Bad data accumulates in the trades
hypertable + the CAGGs every minute the broken release runs.

R1 is the only production host today (single-host per
[ADR-0008](../adr/0008-ha-topology.md); R2/R3 deferred). The sequence:

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

If any rows landed in the trades hypertable from the broken
release, decide post-rollback whether to truncate or leave —
typically the broken decoder produced *missing* data rather
than *wrong* data, so leave-and-backfill is the cheap
recovery. The trades schema's `(source, ledger, tx_hash,
op_index)` primary key prevents duplicate inserts on
re-ingest.

### C. The release runs but a single source is broken

Symptoms: `stellarindex_source_decode_errors_total{source="X"}`
spiking; `stellarindex_source_events_total{source="X"}` dropping
to zero; the `decode-errors` runbook fires.

DON'T roll back the whole release. Instead disable just the
broken source by removing it from the `[ingestion]` allow-list in
`/etc/stellarindex.toml` — the indexer only runs the connectors
named in `enabled_sources`:

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

Then file a SEV-2 against the broken source's package. The
release stands; the source enters degraded mode. Re-enable
once the fix lands.

### D. Public-flip went wrong

Symptoms: public repo content doesn't match private; orphan-
branch initial commit had unintended files; secrets accidentally
included; license/CONTRIBUTING/etc. headers wrong.

Public-repo rollback is a `git push --force` to an empty repo
or — safer — delete the public repo entirely and re-create.
Either way, do NOT touch the private repo (it's the source of
truth).

```sh
# OPTION A — repo is empty enough that nobody cloned it:
gh repo delete StellarIndex/stellar-index --yes
# Then re-do the cut-over per public-flip.md from step 5.

# OPTION B — repo has been observed (someone might have cloned):
# Force-push a corrected initial commit. Coordinate with anyone
# who already cloned to re-pull.
git push origin +public-v1:main
```

Per [`public-flip.md`](public-flip.md), the
`git clone --no-local --no-hardlinks` step makes Option A
genuinely safe — the private repo is untouched.

### E. Status page misbehaving

Symptoms: the public page at `status.stellarindex.io` shows
components down when production is fine, or vice versa.

Lowest-stakes rollback. The status page is a derived view; it
doesn't affect production traffic. F-1211 (codex audit-2026-05-12):
the page is a static Next.js export at [`web/status/`](../../web/status/)
deployed to Cloudflare Pages on push to `main`. Earlier docs
mentioned an Upptime / GitHub-Pages pipeline that no longer
exists.

Two paths:

1. **Edit + push** (preferred). Edit the incident Markdown corpus
   under `internal/incidents/data/<YYYY-MM-DD>-<slug>.md` (this is
   the single source of truth — `web/status/src/lib/incidents.ts`
   reads it at build time, and `/v1/incidents` serves the same
   corpus from the Go binary), commit, push. Cloudflare Pages
   redeploys the status page in ~2 minutes. Note a corpus edit also
   requires re-deploying `stellarindex-api` for `/v1/incidents` to
   reflect it (the corpus is embedded in the binary).
2. **Revert** if the page itself broke. `git revert <bad-sha>` on
   `main` and push — the previous-known-good build redeploys.

If the page is fundamentally broken and can't be corrected within
the SEV-2 detection window:

```sh
# DNS revert — point status. at a previous Cloudflare Pages
# deployment by re-promoting an earlier successful build via the
# Pages dashboard, or aim status.stellarindex.io at a temporary
# maintenance page hosted elsewhere.
```

## Post-rollback

After any rollback above:

1. **Confirm rollback took.** Re-run the SLA probe; verify the
   per-pair freshness gauges return to nominal.
2. **File the SEV.**
   - Title: `SEV-1: <vX.Y.Z> rolled back due to <symptom>`
   - Body: which decision-tree branch fired; what the rollback
     command was; current state.
3. **Customer comms.** If the broken release was live for any
   non-trivial window, send a follow-up to the launch-day comm
   thread. Honest is better than apologetic — say what was
   wrong, what was rolled back, what the customer-visible
   impact was.
4. **Open the postmortem.** Same template as any other SEV-1.
   Bias toward writing it the same day; details fade.
5. **Block forward releases.** Until the postmortem identifies
   the root cause and a fix has landed + been re-tested,
   pause the release-cut cadence. A second cut on top of an
   un-fixed problem is a force-multiplier on the original
   incident.

## Cross-references

- [`launch-day-checklist.md`](launch-day-checklist.md) — the
  cut-over runbook this rollback procedure protects.
- [`release-process.md`](release-process.md) — the per-release
  procedure; §Post-flight has the rollback one-liner this doc
  expands on.
- [`sev-playbook.md`](sev-playbook.md) — incident escalation
  for the SEV file step.
- [`public-flip.md`](public-flip.md) — public-repo cut-over
  mechanics; rollback shape D references this.
- [`docs/operations/postmortems/`](postmortems/) — where the
  postmortem lands after the dust settles.
