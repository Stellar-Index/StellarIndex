---
title: Credential rotation runbook
last_verified: 2026-11-17
status: current
---

# Credential rotation runbook

Rotating the load-bearing credentials on an archival node (r1; R2/R3 the same). [The full rotation batch](#the-full-rotation-batch)
lists the credentials in rotation order. Nothing checks it against the roles: after a
compromise, also read each role's `defaults/main.yml` and templates for secret-bearing
variables not named here.

Secrets live **ansible-vault encrypted** in `configs/ansible/inventory/<region>.secrets.yml`,
**not tracked** (an encrypted vault in a public repo is still bruteforce-able): CI
materializes it from the `ANSIBLE_VAULT_FILE_B64` Actions secret for `ansible-drift.yml`. Edit with `ansible-vault edit inventory/r1.secrets.yml` in `configs/ansible`.

## MinIO

Single-node MinIO (`configs/ansible/roles/archival-node/tasks/09-minio.yml`).

- **MinIO root:** `minio_root_user` / `minio_root_password`; full admin.
- **`galexie-writer`:** `galexie_s3_access_key` / `galexie_s3_secret_key`: write-only on `galexie-live`; `/etc/default/galexie`.
- **`galexie-archive-writer`:** `galexie_archive_s3_access_key` (literal) / `galexie_archive_s3_secret_key`; write, no delete, on `galexie-archive`; `/etc/default/galexie-backfill`, `/etc/default/galexie-archive-fill`. Rehydrate: [lcm-cache-tiering.md](lcm-cache-tiering.md).
- **`stellarindex-reader`** ("the ops user"): `stellarindex_reader_access_key` (literal) / `vault_stellarindex_reader_secret_key`; read-only on both buckets; **`/etc/default/stellarindex`** (`14-stellarindex-services.yml`, tag `stellarindex`) for the services, `/etc/default/stellarindex-ops` (`09-minio.yml`, tag `minio`) for ops tooling. **Two files, two tags.**
- **`stellarindex-archive-trimmer`:** `stellarindex_archive_trimmer_access_key` (literal) / `vault_stellarindex_archive_trimmer_secret_key`; List + **Delete** on `galexie-archive` only; `/etc/default/galexie-archive-trim`.

Only `galexie-writer`'s access key is vaulted (`defaults/main.yml`, `← vault`).

### Regenerating the galexie-writer (or archive-writer / ops-user) secret

1. **Generate:** `openssl rand -hex 32`.
2. **Update the vault:** `galexie_s3_secret_key`, `galexie_archive_s3_secret_key` or
   `vault_stellarindex_reader_secret_key`.
3. **Dry-run, then apply all three tags together:**
   ```sh
   ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml \
     --tags minio,galexie,stellarindex --check --diff
   ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml \
     --tags minio,galexie,stellarindex -e galexie_restart_ack=true
   ```

   > **A subset is an outage.** The `minio` tag rotates the secret at once; each env file
   > is re-rendered only by its own tag:
   >
   > - `galexie` tag (`07-galexie.yml`): `/etc/default/galexie`, `-backfill`, `-archive-fill`.
   > - `stellarindex` tag (`14-stellarindex-services.yml`): `/etc/default/stellarindex` (indexer, aggregator, api).
   > - `minio` tag (`09-minio.yml`): `/etc/default/stellarindex-ops` (`verify-archive`, `run-heavy-job.sh`).
   >
   > `minio` alone strands galexie; `minio,galexie` strands the three services (ingest
   > stops). The `stellarindex` tag is safe in a config apply (binary install and migrations
   > are gated on `manage_stellarindex_binaries`, `stellarindex_apply_migrations`). Without `-e galexie_restart_ack=true` the galexie restart waits for
   > a second, acknowledged run.

4. **Let the handlers restart; do NOT restart by hand.** A manual
   `systemctl restart stellarindex-*` before the `stellarindex` tag ran re-reads the OLD
   secret; if done, re-run step 3 with the full tag list.
5. **Verify:** `mc admin info local` (root creds) shows the user; `journalctl -u galexie -n 50`
   shows uploads with no `SignatureDoesNotMatch`; `/usr/local/bin/config-assertions.sh`
   prints no `FAIL galexie_writer_creds_valid`.

### Prometheus bearer-token regen (INV-0981/INV-1144 — now codified)

`16-prometheus-exporters.yml` (Group D) creates a `prometheus-read` policy and a service
account under root ([runbooks/infra.md#minio-metrics-403](runbooks/infra.md#minio-metrics-403)).
The secret is written once to `/etc/prometheus/minio.token` (`prometheus:prometheus`,
`0400`); the task runs only when the file is absent, since re-running `svcacct add`
invalidates the live secret. To rotate the
token:

```sh
ssh root@136.243.90.96
mc admin user svcacct rm local <the-access-key-shown-by-svcacct-info>
rm -f /etc/prometheus/minio.token
```

Then `ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml --tags exporters`. Symptom `minio_exporter_down`: [runbooks/meta.md#stellarindex_redis_exporter_down](runbooks/meta.md#stellarindex_minio_exporter_down).
`minio_prometheus_token_present` in `scripts/ops/config-assertions.sh` (hourly) catches a
missing or wrong-owner file.

### The `SignatureDoesNotMatch` drift symptom

A MinIO client on r1 logging `SignatureDoesNotMatch` means **credential-file/live-user
drift**, not an outage. Causes: a PARTIAL tag list (see the step 3 list); a consumer
restarted before its tag ran; a user rotated by hand; `AWS_ENDPOINT_URL` / `AWS_REGION`
missing from the rendered env file (see the `/etc/default/stellarindex-ops` template
comment in `09-minio.yml`).

Diagnosis: `journalctl -u galexie -n 200 --no-pager | grep -i signature`; compare the env
file with the vault (never paste or redirect vault output); on mismatch re-run step 3 with
the full tag list. `galexie_writer_creds_valid` in `scripts/ops/config-assertions.sh`
(hourly) auth-probes `/etc/default/galexie`, so a one-sided rotation fails within the hour
([runbooks/config-assertion-failed.md](runbooks/config-assertion-failed.md)).

### MinIO identity inventory

Reader check: `mc admin policy info local stellarindex-reader` (no `s3:DeleteObject`).

**Rotating MinIO root.**
```sh
cd configs/ansible
ansible-vault edit inventory/r1.secrets.yml   # new minio_root_user / minio_root_password
ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml --tags minio --check --diff
ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml --tags minio
```

The `Restart minio` handler restarts MinIO (short galexie write outage). Re-point operators'
`local` alias (`mc alias set local http://127.0.0.1:9000 <new root> <new secret>`).

**`archivewriter` alias.** `--tags minio` repairs drift in `galexie-archive-writer`
(no delete). Prove it:
```sh
mc alias set archivewriter http://127.0.0.1:9000 galexie-archive-writer <vaulted secret>
mc ls archivewriter/galexie-archive/ | head        # must list, not 403
```

**Fill job identity.** `galexie-archive-fill.sh` mirrors via `ARCHIVE_DEST`
(`archivewriter/galexie-archive`) and exits 1 if it cannot list; the `PARTIALS=…` sweep deletes through `ARCHIVE_DELETE_ALIAS` (`local`), both in
`/etc/default/galexie-archive-fill`. After deploying, confirm one timer cycle.

## The full rotation batch

One batch, in this order; each step's consumers are redeployed and checked before the next,
and the vault goes last so a failed step can still be rolled back. Which values are due is
tracked in the private inventory.

1. **Vendor API keys.** `vault_resend_api_key`, `vault_coingecko_api_key`,
   `vault_coingecko_demo_api_key`, `vault_dune_api_key`, `vault_massive_api_key`,
   `vault_tiingo_api_key`, `vault_exchangeratesapi_key`, `vault_openexchangerates_app_id`,
   `alertmanager_pagerduty_key`, `alertmanager_discord_webhook_url_pages`,
   `alertmanager_discord_webhook_url_alerts`, `alertmanager_discord_webhook_url_informational`
   (a leaked webhook URL lets anyone post into the paging channels),
   `alertmanager_healthchecks_deadmansswitch_url`, `alertmanager_healthchecks_alert_delivery_url`,
   the Healthchecks write key. Issue new, deploy, revoke old.
2. **MinIO.** `minio_root_password`, `galexie_s3_*`, `galexie_archive_s3_*`,
   `stellarindex_reader_*`, `stellarindex_archive_trimmer_*`, per [MinIO](#minio).
   Check: no `SignatureDoesNotMatch`.
3. **ClickHouse.** `clickhouse_live_daemon_password`, `clickhouse_ops_admin_password`,
   `clickhouse_ops_batch_password`, `clickhouse_ops_monitor_password`,
   `vault_clickhouse_serving_password`.
4. **Postgres, Redis, HA.** `postgres_pass_stellarindex` (the owner DSN every consumer
   renders into `/etc/default/stellarindex*`), `postgres_pass_stellarindex_api`,
   `postgres_pass_core`, `patroni_*` passwords, `redis_password`,
   `keepalived_vrrp_password`, `etcd_cluster_token`, `etcd_tls_key_pem` (with its cert).
   Check: `patronictl list`, and the API and indexer reconnect with the new DSN.
5. **Backup keys.** `ch_lake_backup_s3_key*`, `galexie_archive_mirror_s3_key*`,
   `pgbackrest_repo2_s3_key*`, `pgbackrest_repo1_sftp_private_key_file` (new keypair, then
   update `authorized_keys` on the repo host), the B2 application keys. Check: a backup of
   each kind completes before the old key is deleted.
   `pgbackrest_repo1_cipher_pass` and `pgbackrest_repo2_cipher_pass` are not rotated in
   place: changing one makes every existing backup in that repo unreadable. Rotating one
   means a new repo (or stanza) under the new passphrase and a fresh full backup, keeping
   the old passphrase until the old backups expire.
6. **Monitoring basic auth.** `prometheus_basic_auth_password_hash`,
   `alertmanager_basic_auth_password_hash`, `patroni_rest_basic_auth_password`. Check: `/-/healthy` on `:9090` and `:9093` answers 401
   without credentials and 200 with `curl -u`, Prometheus still scrapes (Targets page), and
   `patronictl list` still works.
7. **Application secrets.** `vault_dashboard_code_secret` invalidates every live sign-in
   code, login-device proof and passkey ceremony: rotate when no one is mid-sign-in.
   `vault_webhook_seal_key` has no re-seal path (the sealer holds one key and no ops
   command re-seals): rotating it makes every sealed webhook signing key unreadable, and
   deliveries to those webhooks fail terminally with no retry and no notice to the
   customer. Rotate it only when it is known exposed. Afterwards each customer must call
   rotate-secret on their webhook (the dashboard edit and delete still work), and
   deliveries failed in the meantime are lost. Nothing tells them: in the same cycle,
   notify every account in `SELECT DISTINCT account_id FROM customer_webhooks` through
   the incident-comms path ([sev-playbook.md §5.3](sev-playbook.md)), and keep the
   rotation open until each has rotated or been told. Restoring the old value makes the keys
   readable again for new deliveries.
8. **GitHub Actions secrets.** `DEPLOY_SSH_PRIVATE_KEY` with the matching `authorized_keys`
   entry on every host, `CLOUDFLARE_API_TOKEN`, `DEPLOY_PROTECTION_TOKEN`,
   `STELLARINDEX_LOAD_API_KEY`. Write each credential-bearing one as an **environment**
   secret on every Environment that reads it and delete any repository-level copy
   ([deploy-workflow.md §Per-region setup](deploy-workflow.md#per-region-setup)): a
   repository secret is readable from any branch's workflow.
9. **GCP service-account key.** Create, swap in, delete the old.
10. **The vault itself, last.** Rekey, then update the `ANSIBLE_VAULT_PASSWORD` and
    `ANSIBLE_VAULT_FILE_B64` Actions secrets (test nets: `TESTNETS_ANSIBLE_VAULT_PASSWORD`,
    `TESTNET_VAULT_FILE_B64`, `FUTURENET_VAULT_FILE_B64`), at environment scope as in
    step 8. Check: ansible-drift decrypts.

## Related

[ansible-drift.md](ansible-drift.md)
