---
title: Post-deploy config-apply (the "config ships dead" guard)
last_verified: 2026-09-01
status: operational
---

# Post-deploy config-apply

`deploy.yml` runs `configs/ansible/playbooks/deploy-binary.yml` — a
**binary-only** playbook. It stages + installs the release binaries, runs
migrations, and health-checks. It does **NOT**:

- render/apply the ansible `stellarindex.toml.j2` config,
- install/enable systemd units, or
- apply ClickHouse schema.

(Prometheus **rule files** used to be on this list. Since 2026-09-01 they
are the one surface `deploy.yml` applies and verifies itself, in a step
*outside* the binary playbook — see [below](#prometheus-rules--applied-automatically-no-operator-action).)

So when a release's diff touches any of those surfaces, the feature they
gate **ships dead and silent** unless an operator applies the config too.
This bit us twice on 2026-08-25: the v0.42.0 declared-peg map (`[pricing_guard]`
was absent from live `/etc/stellarindex.toml`, so AUDD/AUDR served no peg
despite "deploy OK"), and the v0.43.0 Prometheus rules (copied to the unused
`rules.d/` before the live dir `rules.r1/` was noticed).

The **config-apply gate** (`scripts/ci/config-apply-gate.sh`, run by
`deploy.yml`) fails the deploy job when a release changed a config surface
and the operator did not pass `-f config_acknowledged=true` — a forcing
function so the apply below is never forgotten. Its surfaces are the whole
`configs/ansible/roles/` tree (templates, tasks, files, **defaults**,
handlers — a `defaults/main.yml` change re-renders every template that reads
it), `configs/ansible/inventory/`, `configs/healthchecks/`, the Prometheus
rules, `deploy/systemd/`, `deploy/clickhouse/`, and the repo scripts the role
copies onto the host verbatim (`scripts/ops/config-assertions.sh`,
`ch-schema-snapshot.sh`, `restore-drill.sh`, `scripts/dev/r1-smoke.sh`).

The gate diffs the deploying tag against the **previous release tag by
ancestry** unless it is given the host's live version as a 3rd argument —
so on a **skip-ahead** deploy (host on v0.45.0, deploying v0.47.2) or a
**rollback**, the ancestry diff is the wrong interval. Run it by hand
against the host's real baseline before acknowledging:

```
ssh r1 'cat /var/lib/stellarindex/deployed-versions/stellarindex-api'   # → live version
bash scripts/ci/config-apply-gate.sh v0.47.2 false v0.45.0
```

## The apply procedure (per surface)

r1: `ssh -i ~/.ssh/si_deploy root@<r1-host>`. Each step: **back up → apply →
verify the key landed** (never trust "deploy OK" — grep the live surface).

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
`rules.r1/*.yml` — NOT `rules.d/`, which is unused). Source is the repo's
`configs/prometheus/rules.r1/*.yml`.

**Since 2026-09-01 this surface applies itself.** `deploy.yml`'s
*Apply Prometheus rules (r1)* step runs
[`configs/prometheus/apply-rules.sh`](../../configs/prometheus/apply-rules.sh)
on every r1 deploy, and the config-apply gate no longer asks you to
acknowledge it. The step runs **unconditionally**, not only when a release
changed a rule file — the host's rule set becomes a function of the repo
rather than of who remembered to run what.

What it does that the old manual procedure did not:

- **Reconciles deletions.** The manual `scp <changed>.yml` only ever added
  files. A rule deleted from the repo stayed on the host and kept firing —
  on 2026-09-01 `stellarindex_recognition_unattributed_jump` did exactly
  that for hours after #465 removed it.
- **Verifies by POLLING** `/api/v1/rules` until every expected alert is
  loaded *and* its rule health is ok, restoring its backup and failing if
  not. The old "verify: lists the new alert names" was a single sample;
  Prometheus re-reads on SIGHUP asynchronously, so one sample inside that
  window reports a good apply as a failure — and an operator who saw it
  pass once had no evidence the rules were *evaluating*.
- **Refuses an empty rule set**, so a path typo cannot silently delete
  every alert (the same F-1357 guard the ansible prometheus role carries).
- Keeps 5 timestamped backups at `/etc/prometheus/rules.r1.bak-*`.

If the step fails, its surface stays in the config-apply gate and the gate
goes red — the binaries are already live, so that is a signal to act, not a
rollback. To apply by hand (or from a laptop for a hotfix):

```
scp configs/prometheus/apply-rules.sh r1:/tmp/
scp -r configs/prometheus/rules.r1 r1:/tmp/rules.r1.incoming
ssh r1 'bash /tmp/apply-rules.sh /tmp/rules.r1.incoming'
```

`--check-only` validates without installing (this is what CI runs).

**Verify:** the script does it for you and exits non-zero if it could not.
Independently: `curl -s localhost:9090/api/v1/rules | grep -c alert`.

> The multi-host tree `deploy/monitoring/rules/` is **not** covered — it
> belongs to the ansible `prometheus` role, which requires a two-host
> `prometheus_pair` inventory group r1 does not have. It remains a gated
> surface.

### systemd units
```
scp deploy/systemd/<unit>.{service,timer} r1:/etc/systemd/system/
ssh r1 'systemctl daemon-reload && systemctl enable --now <unit>.timer'
```
**Verify:** `systemctl is-active <unit>.timer` and `systemctl list-timers <unit>.timer`.

### ClickHouse / Timescale schema
Apply idempotent DDL (`CREATE … IF NOT EXISTS`) via `clickhouse-client` /
`psql`. Heavy backfills are a SEPARATE monitored job, not part of the deploy.
**Verify:** the table/MV exists (`system.tables` / `\dt`).

## Durable fix (roadmap)
`ansible-drift.yml` already `--check --diff`s the full archival-node playbook
against live r1 — the authoritative drift check — but it is **credential-broken
since 2026-07-20** (stale `ANSIBLE_VAULT_PASSWORD` / `ANSIBLE_VAULT_FILE_B64`).
Restoring those secrets (operator item) makes it the real post-deploy gate;
until then, this runbook + the `config-apply-gate` forcing function are the
guard.
