---
title: Post-deploy config-apply (the "config ships dead" guard)
last_verified: 2026-09-07
status: operational
---

# Post-deploy config-apply

`deploy.yml` runs `configs/ansible/playbooks/deploy-binary.yml`, a
**binary-only** playbook: stage + install release binaries, run migrations,
health-check. It does **NOT**:

- render/apply the ansible `stellarindex.toml.j2` config,
- install/enable systemd units, or
- apply ClickHouse schema.

(Prometheus **rule files** are the one surface `deploy.yml` applies and
verifies itself, in a step outside the binary playbook — see
[below](#prometheus-rules--applied-automatically-no-operator-action).)

When a release's diff touches those surfaces, the feature they gate **ships
dead and silent** unless an operator applies the config. Precedents: the
v0.42.0 declared-peg map (`[pricing_guard]` absent from live
`/etc/stellarindex.toml`, so AUDD/AUDR served no peg despite "deploy OK") and
the v0.43.0 Prometheus rules (copied to the unused `rules.d/`, not the live
`rules.r1/`).

The **config-apply gate** (`scripts/ci/config-apply-gate.sh`, run by
`deploy.yml`) fails the deploy job when a release changed a config surface
that nothing has cleared and the operator did not pass
`-f config_acknowledged=true`. Surfaces: the whole
`configs/ansible/roles/` tree (templates, tasks, files, **defaults**,
handlers; a `defaults/main.yml` change re-renders every template reading
it), `configs/ansible/inventory/`, `configs/healthchecks/`, the Prometheus
rules, `deploy/systemd/`, `deploy/clickhouse/`, and the repo scripts the role
copies onto the host verbatim (`scripts/ops/config-assertions.sh`,
`ch-schema-snapshot.sh`, `restore-drill.sh`, `scripts/dev/r1-smoke.sh`).

## Three cases, not two

"Comment-only, so acknowledge" is **false** as a general rule:
`v0.61.1..v0.62.0` added `CREATE TABLE stellar.account_creators_ops` to
`account_creators_rollup.sql` **and** `tier1_schema.sql`; the acknowledgement
was correct only because the DDL had been applied by hand and the objects
confirmed present.

Each changed surface lands in exactly one case:

| Case | What it means | Who clears it |
| --- | --- | --- |
| **comment-only** | every added and removed line is blank or a comment in that file's syntax, so nothing the host renders or executes differs | the gate itself, no operator action |
| **applied** | substantive, and proven live on this host by a step that fails closed | the step that proved it, via the gate's `[applied]` argument |
| **substantive** | everything else | the operator, with `-f config_acknowledged=true`, after applying **and verifying** |

Classification is conservative: a file type with no known comment convention
is substantive, as is any non-comment payload on either side of the diff. A
comment-only surface still differs *textually* from the host copy until
applied; the weekly `ansible-drift` job reports that.

### What "applied" is allowed to mean

The gate reads git, not the host. `[applied]` is evidence its caller
produced; `deploy.yml` produces two kinds:

- **`configs/prometheus/rules.r1/`**: `apply-rules.sh` installs, then POLLS
  `/api/v1/rules` until every expected alert is loaded and healthy,
  restoring its backup if never.
- **`deploy/clickhouse/*.sql`, only some.** The *Verify ClickHouse DDL is
  applied* step asks the target whether every object such a diff creates is
  in `system.tables`. A file qualifies only when its whole diff is additive
  whole statements: every hunk opens a `CREATE
  TABLE/MATERIALIZED VIEW/VIEW/DICTIONARY`, nothing substantive was removed,
  and no other DDL/DML verb appears (`config-apply-gate.sh --ddl-objects`
  decides; the self-test pins it).

Deliberately **not** checked (no cheap check exists), never auto-cleared:

- a column added inside an existing `CREATE TABLE IF NOT EXISTS`;
- an `ALTER` / `DROP` / `EXCHANGE`, or any diff removing a statement;
- a systemd unit, ansible template, inventory var, healthcheck script.

Existence proves the DDL **ran**, not that a new materialized view is
backfilled; backfills are a separate monitored job.

The gate diffs the deploying tag against the **previous release tag by
ancestry** unless given the host's live version as a 3rd argument. On a
**skip-ahead** deploy (host v0.45.0, deploying v0.47.2) or a **rollback**
that interval is wrong; run it by hand against the real baseline before
acknowledging:

```
ssh r1 'cat /var/lib/stellarindex/deployed-versions/stellarindex-api'   # → live version
bash scripts/ci/config-apply-gate.sh v0.47.2 false v0.45.0
```

## The apply procedure (per surface)

r1: `ssh -i ~/.ssh/si_deploy root@<r1-host>`. Each step: **back up → apply →
verify the key landed** (never trust "deploy OK"; grep the live surface).

### `stellarindex.toml` (ansible template changed)
The full render needs the ansible vault (operator-gated). For a small
additive change, surgically edit `/etc/stellarindex.toml` to match the
template's new keys (back up first: `cp /etc/stellarindex.toml
/root/stellarindex.toml.pre-<change>`), validate the parse with the ops
binary, restart the affected service. **Verify:** grep the live config for
the new key AND confirm the runtime behavior (e.g. the API serves the new
field), not just that the process restarted.

### Prometheus rules — APPLIED AUTOMATICALLY, no operator action

Live dir is **`/etc/prometheus/rules.r1/`** (prometheus.yml globs
`rules.r1/*.yml`, NOT `rules.d/`, which is unused). Source:
`configs/prometheus/rules.r1/*.yml`.

`deploy.yml`'s *Apply Prometheus rules (r1)* step runs
[`configs/prometheus/apply-rules.sh`](../../configs/prometheus/apply-rules.sh)
on every r1 deploy, **unconditionally**, and the gate no longer asks for
acknowledgement. It:

- **Reconciles deletions.** A manual `scp <changed>.yml` only adds; a rule
  deleted from the repo kept firing (`stellarindex_recognition_unattributed_jump`
  after #465 removed it).
- **Verifies by POLLING** `/api/v1/rules` until every expected alert is
  loaded *and* healthy, else restores its backup and fails. Prometheus
  re-reads on SIGHUP asynchronously, so a single sample can report a good
  apply as a failure.
- **Refuses an empty rule set** (F-1357 guard, as in the ansible
  prometheus role), so a path typo cannot delete every alert.
- Keeps 5 timestamped backups at `/etc/prometheus/rules.r1.bak-*`.

If the step fails its surface stays in the gate and the gate goes red; the
binaries are already live, so that is a signal to act, not a rollback. By
hand (or from a laptop for a hotfix):

```
scp configs/prometheus/apply-rules.sh r1:/tmp/
scp -r configs/prometheus/rules.r1 r1:/tmp/rules.r1.incoming
ssh r1 'bash /tmp/apply-rules.sh /tmp/rules.r1.incoming'
```

`--check-only` validates without installing (what CI runs).

**Verify:** the script exits non-zero if it could not. Independently:
`curl -s localhost:9090/api/v1/rules | grep -c alert`.

> The multi-host tree `deploy/monitoring/rules/` is **not** covered: it
> belongs to the ansible `prometheus` role, which needs a two-host
> `prometheus_pair` inventory group r1 lacks. It remains a gated surface.

### systemd units

> `deploy/systemd/` is **mixed authority** (`scripts/ci/lint-deploy-systemd-authority.sh`):
> most units there are a REFERENCE copy only; the archival-node role ships its
> own `.j2` for the same unit and that template is what runs. Scp-ing the
> reference copy deploys stale content (no `run-heavy-job.sh` wrap, wrong
> `User=`, wrong resource caps; see `deploy/systemd/DIVERGENCES`) that the
> next `ansible-playbook` run silently overwrites.
> Before scp-ing, confirm the unit is AUTHORITATIVE:
> `python3 scripts/ci/deploy-systemd-authoritative.py configs/ansible/roles/archival-node`.
> If not listed, it is templated: apply it via the `archival-node` ansible
> role, or template the `.j2` in
> `configs/ansible/roles/archival-node/templates/systemd/` by hand.

```
scp deploy/systemd/<unit>.{service,timer} r1:/etc/systemd/system/
ssh r1 'systemctl daemon-reload && systemctl enable --now <unit>.timer'
```
**Verify:** `systemctl is-active <unit>.timer` and `systemctl list-timers <unit>.timer`.

### ClickHouse / Timescale schema
Apply idempotent DDL (`CREATE … IF NOT EXISTS`) via `clickhouse-client` /
`psql`. Heavy backfills are a SEPARATE monitored job, not part of the deploy.
**Verify:** the table/MV exists (`system.tables` / `\dt`).

Apply it **before** the dispatch where you can: the deploy's ClickHouse
evidence step then finds the objects present, clears those surfaces itself,
and no acknowledgement is needed.

## Durable fix (roadmap)
`ansible-drift.yml` `--check --diff`s the full archival-node playbook against
live r1 (the authoritative drift check) but is **credential-broken since
2026-07-20** (stale `ANSIBLE_VAULT_PASSWORD` / `ANSIBLE_VAULT_FILE_B64`).
Restoring those secrets (operator item) makes it the real post-deploy gate;
until then this page + the gate are the guard.
