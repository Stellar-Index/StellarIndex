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
