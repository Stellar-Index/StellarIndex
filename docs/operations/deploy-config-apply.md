---
title: Post-deploy config-apply (the "config ships dead" guard)
status: operational
---

# Post-deploy config-apply

`deploy.yml` runs `configs/ansible/playbooks/deploy-binary.yml` — a
**binary-only** playbook. It stages + installs the release binaries, runs
migrations, and health-checks. It does **NOT**:

- render/apply the ansible `stellarindex.toml.j2` config,
- sync the Prometheus rule files,
- install/enable systemd units, or
- apply ClickHouse schema.

So when a release's diff touches any of those surfaces, the feature they
gate **ships dead and silent** unless an operator applies the config too.
This bit us twice on 2026-08-25: the v0.42.0 declared-peg map (`[pricing_guard]`
was absent from live `/etc/stellarindex.toml`, so AUDD/AUDR served no peg
despite "deploy OK"), and the v0.43.0 Prometheus rules (copied to the unused
`rules.d/` before the live dir `rules.r1/` was noticed).

The **config-apply gate** (`scripts/ci/config-apply-gate.sh`, run by
`deploy.yml`) fails the deploy job when a release changed a config surface
and the operator did not pass `-f config_acknowledged=true` — a forcing
function so the apply below is never forgotten.

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

### Prometheus rules
Live dir is **`/etc/prometheus/rules.r1/`** (prometheus.yml globs
`rules.r1/*.yml` — NOT `rules.d/`, which is unused). Source is the repo's
`configs/prometheus/rules.r1/*.yml`.
```
cp -a /etc/prometheus/rules.r1 /root/rules.r1.pre-<ver>        # backup
scp configs/prometheus/rules.r1/<changed>.yml r1:/etc/prometheus/rules.r1/
ssh r1 'promtool check rules /etc/prometheus/rules.r1/*.yml'   # MUST pass first
ssh r1 'systemctl reload prometheus'                           # SIGHUP, zero-drop
```
**Verify:** `curl -s localhost:9090/api/v1/rules` lists the new alert names.

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
