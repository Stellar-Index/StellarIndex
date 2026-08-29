---
title: Runbook — alertmanager-bad-config
last_verified: 2026-08-29
status: current
severity: P2
---

# Runbook — `stellarindex_alertmanager_config_bad`

> **R1 DEPLOYMENT REALITY (re-verified 2026-08-29).** `mon-01` /
> `mon-02` **do not exist** — the `prometheus` ansible role and the
> `monitoring.yml` playbook are the **multi-host** shape and are
> not runnable against r1's inventory (OBS-02, audit-2026-07-23;
> the playbook itself says so). On r1, Prometheus and Alertmanager
> run as **apt-packaged systemd units on the one box**, managed by
> `configs/prometheus/rules.r1/` (rule files) and
> `configs/alertmanager/{alertmanager.r1.yml,apply.sh}` (AM
> config). The alert itself **IS live on r1** — the
> `alertmanager` self-scrape job in
> `configs/prometheus/prometheus.r1.yml` scrapes
> `localhost:9093`, which exports
> `alertmanager_config_last_reload_successful`. The r1 procedure
> is the body of this runbook; the multi-host pair procedures are
> kept at the bottom as the future playbook.

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_alertmanager_config_bad` |
| Severity | P2 (`severity: ticket`) |
| Detected by | `configs/prometheus/rules.r1/meta.yml` (group `stellarindex.meta`, `severity: ticket`, `for: 5m`) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/meta.yml`. |
| Typical MTTR | 5–30 min |
| Impact | AlertManager reload after a config push failed, so any config changes since the last successful load are **not live**. Existing routes keep working from the previous in-memory config. New alerts you expected to route go nowhere. |

## Symptoms

- `alertmanager_config_last_reload_successful == 0` for ≥ 5 min.
- AlertManager log: `error loading config: ...` at the reload time.
- A recent edit to `configs/alertmanager/alertmanager.r1.yml` (or a
  hand apply) whose expected new route doesn't fire.

## Quick diagnosis (≤ 5 min) — r1

On r1 the unit is `prometheus-alertmanager` (the Debian apt
package's name). The live config is
`/etc/prometheus/alertmanager.yml` (mode `0640 root:prometheus` —
it embeds the Discord webhook URLs). The source of truth is
`configs/alertmanager/alertmanager.r1.yml`, rendered (secrets
substituted from `/etc/default/alertmanager-secrets`), validated
with amtool, atomically installed, and reloaded by
`bash configs/alertmanager/apply.sh`.

```sh
# AlertManager's own logs around the failed reload
ssh root@136.243.90.96 "journalctl -u prometheus-alertmanager -n 100 --no-pager | grep -iE 'reload|error'"

# Validate the LIVE config in place
ssh root@136.243.90.96 "amtool check-config /etc/prometheus/alertmanager.yml"
```

## Typical root causes

1. **YAML typo**. Indent error, missing colon, unquoted special
   character. `amtool check-config` catches these. Note that a
   malformed edit that breaks `apply.sh`'s block-stripper
   indentation assumptions can also produce a validating-but-
   receiver-less config — the pre-#275 incident class — which is a
   silent no-fanout, not a `config_bad` firing.

2. **Template-expansion error**. Malformed `{{ ... }}` in a
   receiver template. These validate at parse time but reference
   errors (field doesn't exist) only fire at send time — watch
   for silent "template expanded to empty string" behaviour too.

3. **Secret resolution — NOT a load failure on r1.** An unset
   secret in `/etc/default/alertmanager-secrets` does **not** fail
   the config load: `apply.sh`'s stripper drops that receiver's
   `*_configs` block, degrading it to a no-op stub. The failure
   mode is **silent no-fanout** (alerts accumulate in the AM UI
   but don't reach Discord/Healthchecks), not `config_bad`. If
   fan-out is missing but this alert is green, check the env file
   for empty URLs and re-run `apply.sh`.

4. **Version skew** — a new AlertManager binary with a syntax the
   old config doesn't use, or vice versa. `config.file` parsed
   differently across versions (the apt package pins the distro's
   AM version; a hand-upgraded amtool can also disagree with the
   running binary).

## Mitigation — r1

- [ ] Step 1 — validate the checked-in source locally: CI's
      `monitoring-rules` job already runs
      `apply.sh --check-only` (see below), so reproduce with
      `ALERTMANAGER_SECRETS=/dev/null bash configs/alertmanager/apply.sh --check-only`.
      Fix syntax in `alertmanager.r1.yml`.
- [ ] Step 2 — confirm the real secrets exist on r1:
      `/etc/default/alertmanager-secrets` should set
      `HEALTHCHECKS_DEADMANSSWITCH_URL`,
      `DISCORD_WEBHOOK_URL_PAGES`, `DISCORD_WEBHOOK_URL_ALERTS`
      (any may be empty — but empty means that receiver becomes a
      no-op stub; see root cause 3).
- [ ] Step 3 — apply via the normal flow on r1:
      `bash configs/alertmanager/apply.sh` (renders + amtool-
      validates + installs `0640 root:prometheus` + reloads).
      Don't hand-edit `/etc/prometheus/alertmanager.yml` — the
      next apply will overwrite it, and per CLAUDE.md every r1
      config change lands in the repo in the same PR.
- [ ] Step 4 — manual reload if needed:
      `systemctl reload prometheus-alertmanager` (what `apply.sh`
      does). The classic `curl -XPOST http://localhost:9093/-/reload`
      assumes the unit runs with `--web.enable-lifecycle`, which
      is unverified on the apt unit — prefer systemctl.
      `TODO(ash): check whether r1's prometheus-alertmanager unit
      passes --web.enable-lifecycle; drop this note either way.`
- [ ] Verification:
      `alertmanager_config_last_reload_successful == 1`; the alert
      clears after one evaluation interval.

## CI guard — already in place (#275)

The `monitoring-rules` CI job installs amtool and runs
`configs/alertmanager/apply.sh --check-only` on **both** render
branches — empty URLs (exercising the block-stripper stub path)
and dummy URLs (exercising substitution) — validating the
**rendered** config, not just the template. Pre-gate, amtool ran
solely inside `apply.sh` at apply time, and a malformed edit that
broke the stripper's indentation assumptions shipped a silently
receiver-less Alertmanager. A PR that breaks either render branch
fails CI before it can reach r1.

## Known false-positive patterns

- **Reload during unit startup** — `last_reload_successful` is 0
  until the very first load completes. During a cold start this
  can briefly trip; `for: 5m` absorbs normal startup.

## Multi-host pair procedure (future — not runnable today)

When the monitoring tier exists (`mon-01`/`mon-02` per the
`prometheus` role and ADR-0008 §3), the shape becomes: live config
at `/etc/alertmanager/alertmanager.yml` rendered from the role's
`alertmanager.yml.j2`; push via `ansible-playbook` with the
prometheus role (handler reloads); diff the live config across the
pair (`diff <(ssh root@mon-01 cat …) <(ssh root@mon-02 cat …)`)
— they must agree; verify
`alertmanager_config_last_reload_successful == 1` on both. Until
OBS-02 is resolved, none of this applies to r1.

## Related

- `deadmansswitch.md` — the watchdog that catches a totally-broken
  AlertManager (this alert relies on AM being up enough to serve
  metrics; the heartbeat is routed via
  `configs/alertmanager/alertmanager.r1.yml`).
- `scrape-failing.md` — if the AM metrics endpoint is what's
  failing, not the config.

## Changelog

- 2026-08-29 — re-verified against HEAD. R1 DEPLOYMENT REALITY
  banner added: mon-01/mon-02 don't exist and the prometheus
  role/playbook is not runnable against r1 (OBS-02); r1's AM is
  the apt `prometheus-alertmanager` unit, live config
  `/etc/prometheus/alertmanager.yml` (0640 root:prometheus),
  source of truth `alertmanager.r1.yml` via `apply.sh` (secrets
  from `/etc/default/alertmanager-secrets`, reload via
  `systemctl reload prometheus-alertmanager`); alert confirmed
  live via the prometheus.r1.yml alertmanager self-scrape.
  Diagnosis/mitigation rewritten for that shape; multi-host pair
  procedure retained as the future playbook. "Add an amtool CI
  step" replaced — it EXISTS since #275 (monitoring-rules job runs
  apply.sh --check-only on both render branches). Secret-resolution
  root cause refined: an unset secret degrades the receiver to a
  no-op stub (silent no-fanout), it does not fail the load. The
  /-/reload curl flagged as unverified on the apt unit (TODO(ash)).
  Rule citation → `rules.r1/meta.yml`.
- 2026-04-23 — initial draft.
- 2026-05-02 — diagnosis converted from kubectl ConfigMap +
  pod-logs to systemd / journalctl on `mon-01..02` running
  `alertmanager.service` per the `prometheus` ansible role
  (ADR-0008). The cited path `deploy/monitoring/alertmanager.yml`
  was wrong — the source-of-truth template is in the role
  (`alertmanager.yml.j2`); live config sits at
  `/etc/alertmanager/alertmanager.yml`.
