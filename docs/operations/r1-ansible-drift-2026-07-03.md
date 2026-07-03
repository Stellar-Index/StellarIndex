---
title: r1 ↔ ansible drift audit
last_verified: 2026-07-03
status: current
---

# r1 ↔ ansible drift audit (2026-07-03)

**Trigger:** the 2026-06-11 incident's rsyslog suppression rules turned
out to be codified in ansible but **never applied to r1** (the
postmortem recorded codified-as-applied). That raised the reverse
question: what lives on r1 by hand that ansible would **erase** if the
playbook ran? Both directions were audited: every `dest:` in
`configs/ansible/roles/*/tasks` plus the r1 overlay surfaces
(prometheus, alertmanager, caddy, systemd units) diffed against the
live host.

## ⚠️ Standing rule

**Do NOT run `ansible-playbook playbooks/archival-node.yml` against r1
until the gaps below are reconciled — and always `--check --diff`
first.** Ansible does not auto-run against r1; nothing self-heals in
either direction. The hourly `config-assertions.sh` timer (+
`stellarindex_config_assertion_failed` alert) watches the load-bearing
subset.

## Findings — live-on-r1, absent-from-ansible (would be ERASED)

| # | Surface | Live state | Codified? |
|---|---|---|---|
| 1 | `[supply]` in `/etc/stellarindex.toml` | 16 `sdf_reserve_accounts` + `reserve_balances_stroops` table (CS-010 fix, 2026-07-02) | ✅ 2026-07-03: template renders the balances table; accounts + balances now in `inventory/r1.yml` vars |
| 2 | Redis `maxmemory 1gb` (2026-06-16 sweep) | Debian-packaged redis, hand-edited conf; the redis-sentinel role is the future HA shape and does NOT manage it | ✅ 2026-07-03: archival-node lineinfile task |
| 3 | nftables nft-drop log tweak (`5/second … level info`, 2026-06-30) + `10-nft-drop.conf` rsyslog + logrotate pair | Hand observability addition | ❌ minor — template logs drops at `1/second flags all`; reconcile when convenient |
| 4 | nftables `11625 accept` (F-1201, future validator) | Hand rule; template gates it on `run_stellar_core`, which is `false` in r1.yml | ❌ deliberate divergence — flip the var when Phase 3 lands, or accept the rule disappearing (nothing listens today) |
| 5 | Caddy (public TLS edge) | Entirely hand-managed; live still binds legacy `api.ratesengine.net` alias that `configs/caddy/Caddyfile.api` dropped | ❌ no ansible role exists for caddy at all |
| 6 | systemd units (`stellarindex-*.service`) | Live = root-user shape; repo `deploy/systemd/` = non-root future shape (task #30) | pending the non-root migration — do not sync units without executing the migration steps |
| 7 | sshd | Live = stock Ubuntu (root-with-key); template needs `ssh_permit_root_login` | ✅ 2026-07-03: pinned `"prohibit-password"` in r1.yml (deploy workflow + agents SSH as root) |

## Findings — repo-ahead-of-r1 (apply-gaps, now closed)

- rsyslog loki/clickhouse suppression (2026-06-11 fix) — **applied
  2026-07-03**, probe-verified.
- Prometheus rules: `served_value_drift`/`_check_stale` (board #14),
  `divergence_no_reference` (CS-088), `ch_live_sink_drops`/`_sustained`
  (ADR-0041), plus rebrand wording in anomaly/api/sla-probe — **synced
  2026-07-03**. Live rules.r1 now matches the repo tree exactly.
- prometheus.yml / alertmanager.yml: live is older but strictly a
  subset (no live-only material lines). Alertmanager sync is gated on
  the Discord/Healthchecks env vars existing (operator account item).

## False positives worth remembering

Raw-text diffs against `.j2` templates flag loop/var-rendered content
as "missing" — the nftables 80/443/SSH-limit rules ARE in the role
(`public_allow_ports_base` defaults) despite not appearing in the
template text. Render (`ansible-playbook --check --diff`) before
believing a template gap.

## The durable fix

Make ansible the actual deployment path for r1 (run with
`--check --diff`, reconcile the table above, then apply for real and
keep applying). Until then: every hand fix on r1 gets codified in the
same PR (this audit is the enforcement backstop), and
`config-assertions.sh` alerts on regressions of the load-bearing
subset.
