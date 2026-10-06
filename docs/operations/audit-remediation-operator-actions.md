---
title: Audit remediation — items requiring operator (human) action
last_verified: 2026-10-05
status: living — populated as remediation proceeds
---

# Operator-action register (audit remediation 2026-07)

Findings from the correctness/security and maintainability audits that need an
account, a secret, a legal call, prod infra or an owner decision. Code-side fixes
are in the commit log; closed items are dropped from this page.

> **Before any ansible playbook against r1:** read
> [r1-ansible-drift-2026-07-03](r1-ansible-drift-2026-07-03.md) and always run
> `--check --diff` first. The hourly `config-assertions.timer` alerts on
> regressions of the load-bearing subset.

## Repo / CI settings (highest leverage — one setting unlocks every guard)
- [ ] **Branch-protect `main` + require status checks** (CS-097) — NOT DONE. Live state:
  `GET /repos/Stellar-Index/StellarIndex/branches/main/protection` returns 404 and
  `GET .../rulesets` returns `[]` (no `main-integrity` / `main-required-checks`);
  `GET .../actions/permissions` shows `allowed_actions: "all"`, `sha_pinning_required: false`.
  1. Settings → Rules → Rulesets: create `main-integrity` (block force-push + deletion,
     no bypass) and `main-required-checks` (core CI jobs as required checks; admins may
     bypass).
  2. Settings → Actions → General: allow owner actions + require full-SHA pinning, or
     `allowed_actions=selected` + `sha_pinning_required=true` via
     `gh api --method PUT repos/Stellar-Index/StellarIndex/actions/permissions`.
  3. Re-run the `gh api` calls; tick only when they no longer 404/return empty.
  Pinned by `scripts/ci/audit-remediation-operator-actions-test.sh`. CS-098 is unaffected:
  `scripts/ci/lint-baseline-growth.sh` keeps baselines shrink-only (growth needs a
  `Baseline-Growth:` commit trailer).

## Accounts / secrets (launch-blocking, operator-only)
- [ ] **Buy CoinGecko Pro** → set `COINGECKO_API_KEY` on r1 + restart indexer (P0-3).
- [ ] **Create Healthchecks.io account + Discord webhooks** → set `DISCORD_WEBHOOK_URL_PAGES`/
  `_ALERTS` + the 4× `HEALTHCHECKS_URL_*` in r1 env files; rerun `pre-launch-check.sh`.
- [ ] **Relocate + rotate the GCP service-account key** (CS-001) — `rates-engine-data-
  validation-*.json` sits in the repo working tree (gitignored, never committed). Move it
  out of the repo dir; rotate if it was ever shared; confirm the SA is still used.

## Set config values (code is ready; values are yours)
- [ ] **Re-raise `min_usd_volume` to the 10000 default** once CEX/CoinGecko data flows
  (0 today to serve on-chain-only micro-volume; CS-040 makes the gate scale-correct).

## r1 migration to non-root services (CS-118/CS-119)
The role creates the `stellarindex` user (`04-users.yml`) and runs daemons + timer
oneshots as `User=stellarindex`. `--tags users,minio,observability,stellarindex` does most
of it; on the running host do the ownership flips in this order:

1. `useradd --system --shell /usr/sbin/nologin --home-dir /var/lib/stellarindex --no-create-home stellarindex || true`
2. Confirm no oneshot is mid-run
   (`systemctl list-units 'stellarindex-*' '*completeness*' 'ch-supply*' 'sep1-*' 'data-freshness*' 'supply-snapshot*' 'verify-archive-*'`),
   then `systemctl stop stellarindex-api stellarindex-aggregator stellarindex-indexer`
3. Chown state + env files (binaries in `/usr/local/bin` stay root:root 0755):
   - `chown -R stellarindex:stellarindex /var/lib/stellarindex`
   - `chown -R stellarindex:stellarindex /var/lib/node_exporter/textfile_collector`
   - `chgrp stellarindex /etc/default/stellarindex /etc/default/stellarindex-ops && chmod 0640 /etc/default/stellarindex /etc/default/stellarindex-ops`
4. Install the units (`--tags stellarindex`), `systemctl daemon-reload`.
5. `systemctl start stellarindex-indexer stellarindex-aggregator stellarindex-api`; check
   `ps -o user= -p $(systemctl show -p MainPID --value stellarindex-api)` says
   `stellarindex` (per unit); `bash scripts/dev/r1-smoke.sh`.
6. Rollback: reinstall the root units, `daemon-reload`, start (the chowns are
   backwards-compatible).

`archive-completeness.service` stays `User=root`: `run-heavy-job.sh` creates the
`MemoryMax=20G` scope and disk watchdog only for a root caller.

## Disaster recovery (CS-110/111/112 — ADR-0043; your half:)
- [ ] **Provision the offsite bucket for `repo2`** → set the `pgbackrest_repo2_*` vars +
  cipher pass in vault, flip `pgbackrest_manage_conf: true` after reviewing the rendered
  `pgbackrest.conf` diff, run `stanza-upgrade` + a first full to repo2.
- [ ] **Run `scripts/ops/restore-drill.sh` by hand twice** (`DRILL_REPO=1`, then `=2` once
  repo2 exists; add `DRILL_CH_WINDOW=100000` on one run to measure the CH re-derive RTO).
  Commit the appended `docs/operations/drills/restore-drills.md` entries, then enable the
  timer (monthly, first Saturday 04:00 UTC).
- [ ] **Point the CH schema snapshot offsite** — `ch_schema_snapshot_mc_target` is empty
  and `ch_schema_snapshot_offsite_ack: true` is set in `inventory/r1.yml`, so the daily
  snapshot lands only on the pool it protects. The role refuses `--tags backup` without a
  target and `stellarindex_ch_schema_snapshot_offsite_stale` tickets r1 until a push
  lands. Procedure: [v1-launch-plan](v1-launch-plan.md) row 1.12; drop the ack in the same
  edit. Interim (after any DDL change, at least monthly; does not clear the alert):
  `OUT_DIR=deploy/clickhouse/schema-snapshot TEXTFILE_DIR=/dev/null scripts/ops/ch-schema-snapshot.sh`,
  then commit the result.
- [ ] **Full-CH-backup decision** — waits on the drill's measured re-derive throughput.
- [ ] **Encrypt pgBackRest repo1** (F4-F2) — repo1 is `cipher-type=none`: a plaintext DB
  copy (API keys, Stripe ids) on local disk and in any ZFS send. The role renders
  `repo1-cipher-type=aes-256-cbc` when `pgbackrest_repo1_cipher_pass` is set and refuses an
  unencrypted repo1 otherwise. pgbackrest cannot re-encrypt in place: `stanza-delete` +
  `stanza-create` + full backup discards repo1's history, so do repo2 first. Procedure:
  [pgbackrest-encryption.md](pgbackrest-encryption.md).

## `R1_INVENTORY_B64` — DO THIS BEFORE MERGING (C6-041)

`configs/ansible/inventory/r1.yml` is untracked (it published r1's IP,
`allowed_ssh_cidrs: 0.0.0.0/0`, disk serials and the operator SSH pubkey).
`.github/workflows/ansible-drift.yml` materializes it from an Actions secret, so the
drift guard is RED until the secret exists.

- [ ] **Create the `R1_INVENTORY_B64` Actions secret** from the working-tree copy:

  ```bash
  base64 -w0 configs/ansible/inventory/r1.yml    # macOS: base64 -i configs/ansible/inventory/r1.yml
  # repo Settings → Secrets and variables → Actions → New repository secret
  #   Name: R1_INVENTORY_B64
  ```

  Re-run `ansible-drift` (`workflow_dispatch`) and confirm it passes the "Inventory" step.
  It still fails at "Vault password" until `ANSIBLE_VAULT_PASSWORD` /
  `ANSIBLE_VAULT_FILE_B64` are restored (CID-24) — three secrets, one green run.

- [ ] **Treat the historical contents as public** — every revision is readable via
  `git log -p -- configs/ansible/inventory/r1.yml`. Mitigate the SSH surface by narrowing
  `allowed_ssh_cidrs` (C6-018/C6-028/C6-060/C6-092); rotate anything credential-shaped
  that ever appeared in it per [credential-rotation.md](credential-rotation.md). Do not
  rewrite history: forks and clones keep the old objects anyway.

- [ ] **Keep a second copy of `r1.yml` outside the repo.** The Actions secret is
  write-only, not a backup.

## MinIO credential hygiene
Identity record: [credential-rotation.md §MinIO identity inventory](credential-rotation.md#minio-identity-inventory).
- [ ] **Deploy `galexie-archive-fill` off the root (`local`) alias** — code done: the fill
  reads and mirrors through `ARCHIVE_DEST` (`archivewriter/galexie-archive`) and fails fast
  if it cannot list it; only an operator run with `PARTIALS=…` deletes, via
  `ARCHIVE_DELETE_ALIAS` (`local`). Remaining: apply `--tags archive-fill` and confirm one
  full timer cycle succeeds.

## Supply cross-check P3 on BLND / EURC / KALE / PHO (E4/N-F3)
- [x] `supply seed-sac-balances -full-history` ran for all 38 assets. Verify per asset via
  `sac_balance_seed_provenance` (`source='full_history'`); `min_ledger_seen` is not a
  success signal (archived balances are tombstoned, so it reports tip − 2,073,600).
  Record per-asset outcomes of
  [runbooks/supply.md#stellarindex_supply_cross_check_divergence](runbooks/supply.md#stellarindex_supply_cross_check_divergence) here.

## Multi-region / HA (gated on hosts existing — P3)
- [ ] Provision R2 (AWS) + R3 (Vultr); then the `redis-sentinel`/patroni/bringup roles run.
- [ ] **Patroni REST auth** (CS-122) — set `patroni_rest_basic_auth_user/password` in vault
  before Patroni deploys. The role fails the play if they are empty and listens on
  `ansible_host`, not 0.0.0.0.
- [ ] Narrow `allowed_ssh_cidrs` from `0.0.0.0/0` once a stable admin range exists.
- [ ] Optional: Cloudflare orange-cloud in front of `api.` (WAF/L7).

## Legal / vendor (before commercial launch — CS-115/116)
- [ ] **Vendor-ToS review of raw CEX data redistribution** — `/v1/history` and
  `/v1/observations?source=<cex>` serve per-trade source-attributed records (data-vendor
  `source=` filters return 400; exchange venues stay selectable by owner decision);
  Binance/Kraken/Coinbase terms generally prohibit this. Blended outputs
  (`/v1/price|vwap|…`) are defensible. Decide whether the source-attributed surfaces stay
  for restricted venues.
- [ ] **External security review** booking (P2-3).
- [ ] Confirm CoinGecko Pro redistribution terms at purchase. (`github.com/xdrpp/goxdr`, via
  `txnbuild`, is dual GPL-3/Apache-2.0; we take it under Apache-2.0.)

## Launch cutover (operator/DNS — P2-5/P2-6)
- [ ] DNS flip finalization, public rate-limit tier, announcement, 24h watch (endpoints
  are already DNS+TLS live; this is the go-live decision + comms).

---
_CS-### ids refer to
https://github.com/Stellar-Index/StellarIndex/tree/0023bb9aefa96fb8231d9eabd160e6133eca39e9/docs/audit-2026-06-30
and
https://github.com/Stellar-Index/StellarIndex/tree/0023bb9aefa96fb8231d9eabd160e6133eca39e9/docs/maintainability-audit-2026-07-01._
