# W18 — Deployment, infrastructure, ansible roles

## Scope

`deploy/`, `docker/`, `configs/ansible/`, `configs/caddy/`,
`configs/loki/`, `configs/prometheus/`, `configs/alertmanager/`,
`configs/healthchecks/`, plus the actual r1 state.

## Inputs

- `deploy/systemd/` unit files (3 services + 8 timers/services)
- `deploy/docker-compose/dev.yaml`
- `docker/*.Dockerfile`
- `configs/ansible/{playbooks,roles,tasks,inventory}/`
- `configs/caddy/`
- `configs/loki/`, `configs/prometheus/`,
  `configs/alertmanager/`, `configs/healthchecks/`

## Checks

| # | Check | Method |
| --- | --- | --- |
| W18.1 | Every systemd unit on r1 has a matching `deploy/systemd/` source | r1 probe + grep |
| W18.2 | Every Dockerfile is multi-arch (or explicitly amd64) + pinned base | grep |
| W18.3 | Ansible playbooks: archival-node, deploy-binary, monitoring | playbook audit |
| W18.4 | Ansible roles: archival-node, haproxy, loki, patroni, prometheus, redis-sentinel | role audit |
| W18.5 | `deploy-one-binary.yml` semantics: stage → backup → atomic install → restart → health probe → rollback | task audit |
| W18.6 | Caddy: TLS termination, trusted-proxy list (ADR-0025), cert auto-renew | config + r1 probe |
| W18.7 | Loki: retention + WAL | config audit |
| W18.8 | Prometheus: scrape config + retention + rules + alertmanager wiring | config audit |
| W18.9 | Alertmanager: routing tree + receivers + secrets | config audit |
| W18.10 | Healthchecks.io: heartbeat + smoke + sla-probe timers | config audit |
| W18.11 | r1 live: every documented service is running; nothing unexpected listening | r1 probes |
| W18.12 | r1 live: `r1-ansible-drift-2026-05-22.md` drift register reconciled | per-row |
| W18.13 | NEW: galexie-archive-trim.timer (cold-tier W30) — present + reasonable schedule | unit file |
| W18.14 | NEW: customer-webhook delivery worker — systemd unit or part of api binary? | architecture clarity |
| W18.15 | NEW: stellar-core no longer runs as standalone service (per CLAUDE.md 2026-04-23) | r1 probe R1-P11 |
| W18.16 | NEW: every region (r1/r2/r3) docker-compose for dev mirrors prod | dev.yaml audit |

## Closure criteria

Every deploy artifact + r1 service confirmed. Findings on:
- any ansible-role drift vs r1 reality
- any unit on r1 that's not version-controlled
- any TLS cert <30 days from expiry
