---
title: "Audit — un-audited surfaces (2026-07-18) + remediation ledger"
status: remediation-in-progress
date: 2026-07-18
scope: "The surfaces the 2026-07-16 audit did not cover: explorer frontend (forensic + analytics), the deploy/CI-CD pipeline, and dependencies/config security."
method: "Cold adversarial audit (audit-suite auditors) → skeptic verification of money/security → fixers → fix-verifiers → integration."
---

# Un-audited surfaces audit + remediation (2026-07-18)

Extends the 2026-07-16 audit to the surfaces it explicitly left out. Four cold auditors, findings skeptic-verified where money/security, remediated by fixers, verified, and integrated as the PRs noted. This is the durable record.

## Result at a glance

| Surface | Finding | Sev | Disposition |
|---|---|---|---|
| Frontend forensic | Operation amounts rendered raw stroops (10⁷ too large) on tx/op/account | **HIGH** | Fixed — frontend money PR |
| Frontend analytics | Currency converter renders the **inverted** rate (100 USD→108 EUR) | **HIGH** | Fixed — frontend money PR |
| Frontend forensic | i128 transfer amounts via `Number()` (KALIEN-class precision loss) + lending sibling | MED/HIGH | Fixed — frontend money PR |
| Frontend forensic | Account positions `Number(raw)/1e7` precision loss | LOW | Fixed — frontend money PR |
| Frontend analytics | Lending APR mislabeled as APY (list vs detail) | MED | Fixed — frontend money PR |
| Frontend analytics | SearchModal accepts protocol-relative `//host` (open-redirect) | LOW | Fixed — frontend money PR |
| API (backend) | C3-1 8s read-timeout only on 2/15 explorer handlers (unauth pool-exhaustion DoS) | HIGH | Fixed — PR #16 |
| Storage (backend) | C2-4c sibling `argMax(*, ledger_seq)` same-ledger tie ambiguity (3 readers) | MED | Fixed — PR #16 |
| Ansible | Jinja `${#...}` mis-parse — **hard-breaks the deploy** | HIGH | Fixed — PR #13 (merged) |
| CI/CD | `ansible-drift.yml` live-keyscans host (MITM) + inlines secrets + no cleanup | MED | Fixed — PR #15 (merged) |
| CI/CD | Pages deploys interpolate `github.ref_name` into wrangler command (script-injection) | MED | Fixed — PR #15 (merged) |
| CI/CD | `cf-project-rename` ignores API success (silent-outage) | MED | Fixed — PR #15 (merged) |
| CI/CD | 2 unpinned `actions/checkout@v7` | LOW | Fixed — PR #15 (merged) |
| Config | Loki binds 0.0.0.0 + auth off; MinIO binds all interfaces | MED/LOW | Fixed (codify) — PR #14 (merged); **apply post-Phase-0** |
| Config | galexie drift-guard constants inconsistent (hard-fails the drift assert) | MED | Comment fixed — PR #14; **constant reconcile = [OP]** |
| Docs | VERSIONS.md stale (galexie v26→v27, go shortlist) | MED | Fixed — PR #14 (merged) |

**Verified SOUND (no findings):** `deploy.yml`/`release.yml` hardening, go.mod (no forks/`replace`), pnpm lockfiles (registry+integrity), XSS chokepoints (JSON-LD escaping, `isSafeHref`/`isSafeHomeDomain`), auth flows (open-redirect-guarded, no client secrets), and most of the analytics money surface (decimals scaling, pre-scaled strings, percentages).

## Root causes found

- **Dead cron schedules** (the every-2h `ci-health` red-main tripwire, weekly security + drift scans, k6, site-crawl — 0 runs ever) trace to the **org transfer** `StellarIndex/stellar-index` → `Stellar-Index/StellarIndex`. GitHub requires schedule re-registration after a transfer. **[OP]**
- The **galexie version-lag** (R1 v26 vs pinned v27) + inconsistent drift-guard constants (a v0.0.0-2026-06-10 version-string vs a 2026-07-09 sha256) are why the ansible-drift assert hard-fails.

## Operator ([OP]) items — need you / off-repo

1. **Re-register the GitHub Actions cron schedules** (org-transfer fix) — else the red-main tripwire + weekly security/drift scans stay dead.
2. **Confirm the post-2026-07-03 vault rotation was complete** — the encrypted vault is retrievable from public git history (commit `9c8afc61`); rotate the vault passphrase too.
3. **`CLOUDFLARE_API_TOKEN`** repo secret still not taking (account ID resolves, token empty) — blocks the frontend deploy.
4. **Build galexie v27** and reconcile `galexie_expected_version_string` + `galexie_expected_binary_sha256` from that one binary (currently target different builds).
5. **Version bumps needing release SHAs** (MinIO ~20mo old, node_exporter) + **Docker base-image digest pins** — need registry access.

## Post-Phase-0 apply queue (codified, awaiting deploy)

- The Loki/MinIO loopback binds (need a service restart).
- The full drift-apply (33 tasks): api_serving CH profile, CH tuning, ch-supply→journald, migrations 0109–0114 + the C2-4c/argMax reprojects, captive-core T1, pool alerts, galexie v26→v27.
- The C2-4c + argMax historical **reprojects** of `ledger_entry_changes`/`ledger_entries_current` (new ingest already correct).
- The C2-11 soroban topic re-ingest; the INV-3 corrected re-derives.

## Follow-ups (identified, not fixed — need a decision or backend change)

- **Movements per-row decimals** — `AccountMovements` hardcodes 7 decimals; correct non-7-decimal Soroban scaling needs the backend to carry `decimals` on the `/v1/accounts/{id}/movements` wire.
- **Weighted-APY dilution** (lending list) — null-rate reserves stay in the denominator; "include-idle-at-0 vs exclude-unknown" is a product decision.
- **Trivy** scans ignore MEDIUM/unfixed + no image scan; **SSH 0.0.0.0/0** (mitigated, blocked on a jump host that doesn't exist yet).
- Tx/op event tables render only topic_0 (incomplete forensic detail).

## HA / infra roles audit (2026-07-18) — the last un-audited surface

The ratified-but-**undeployed** HA roles (patroni/haproxy/redis-sentinel/prometheus) + systemd units + CI lints. **The failover safety itself is SOUND** — Patroni `synchronous_mode` + `remote_apply` + `pg_rewind` + a preflight-enforced 3-node cluster; Redis/Sentinel bind-loopback + `requirepass` + ACL; Prometheus exposes no admin/lifecycle API; secrets are 0600/0640; supply-chain SHAs pinned; the galexie-trim destructive path is fail-closed.

### LIVE (fixed now — systemd/CI hardening PR)
- **F-005 [MED] StartLimit in the wrong section.** `stellarindex-{api,aggregator,indexer}.service` put `StartLimitInterval/Burst` under `[Service]`; systemd ≥230 ignores them (they're `[Unit]` keys), so a broken service hot-loops forever and **never enters `failed`** — the alert those units *claim* to trip never fires. → moved to `[Unit]`.
- **F-007 [LOW]** three root oneshots (`ch-live-catchup`, `config-assertions`, `stellarindex-completeness`) run unsandboxed → add Protect*/NoNewPrivileges.
- **F-008 [LOW]** vacuous-pass CI lints (`lint-docs.sh` skips a renamed input; `lint-rule-structure.py` exits 0 if PyYAML absent / dir moved; `lint-openapi-urls` no `paths` guard) → fail-closed on missing input.

### LATENT — fix before the FIRST HA deploy (undeployed roles)
- **F-001 [HIGH — deploy-blocking]** Patroni bootstrap/join/idempotency probes hit `127.0.0.1:8008` but the REST API binds `{{ ansible_host }}:8008` → the play aborts on first deploy, and re-runs churn a Patroni restart every time. Fix: bind `0.0.0.0:8008` (already firewalled) or probe `{{ ansible_host }}:8008` (as the redis role correctly does).
- **F-004 [MED — security]** etcd (the Patroni DCS) runs **plaintext HTTP, no auth/TLS/RBAC**, reachable by all of RFC1918 → anyone in `10.0.0.0/8` can rewrite leader keys (forced failover / split-brain). Fix: etcd client-cert + peer TLS (or ≥ RBAC), tighten the firewall to peer IPs only.
- **F-002 / F-003 [MED — observability]** Prometheus scrapes node_exporter `:9100` and HAProxy `:8404`, but those roles' default-drop firewalls never open them (and node_exporter isn't installed on HA hosts; HAProxy stats bind loopback) → the HA tier + failover gauges are unscraped. Fix: open `:9100`/`:8404` to the Prometheus host + install node_exporter + bind HAProxy metrics on the private IP.
- **F-006 [MED]** keepalived uses multicast VRRP with no `unicast_peer` → on cloud VPCs (no L2 multicast) both LBs claim the VIP. Fix: template `unicast_src_ip`/`unicast_peer`.
- **F-009 [LOW]** redis/patroni `first_run_only: true` skips ALL config re-renders on a live cluster — so the ACL-lockdown security flip can never be applied via the role. Fix: always re-render, compute the primary from live Sentinel state.
- **F-010 [INFO]** patroni hardcodes `synchronous_standby_names` while `synchronous_mode` manages it (and assumes host[0] is primary). Fix: drop the static value.

### NOT-EXAMINED (adjacent, flagged)
- `configs/alertmanager/` (live alertmanager config — outside `deploy/monitoring` literal scope), the HA roles' `02-install`/`05-systemd` tasks, and `.timer` cadences (surveyed, not line-traced).
