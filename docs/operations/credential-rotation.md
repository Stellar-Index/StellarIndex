---
title: Credential rotation runbook
last_verified: 2026-10-05
status: current
---

# Credential rotation runbook

Rotating the load-bearing service credentials on an archival node (r1 today; R2/R3 the
same once they run their own MinIO). Only the MinIO family is written up; add a `##`
section per family (Postgres password, SEP-10 signing seed, webhook HMAC secrets) as
each gets a procedure.

Secrets live **ansible-vault encrypted** in
`configs/ansible/inventory/<region>.secrets.yml` (`r1.secrets.yml` for r1). The file is
**not tracked** (an encrypted vault in a public repo is still an offline-bruteforce
target): CI materializes it from the `ANSIBLE_VAULT_FILE_B64` Actions secret for
`ansible-drift.yml`; operators keep a local copy.

```sh
cd configs/ansible
ansible-vault edit inventory/r1.secrets.yml   # needs the vault password
```

## MinIO

Single-node MinIO (`configs/ansible/roles/archival-node/tasks/09-minio.yml`) backs
Galexie's S3 target.

| Identity | Vault variable(s) | Scope | Consumed by |
|---|---|---|---|
| MinIO root | `minio_root_user` / `minio_root_password` | full admin | the role's `mc alias set local ...` bootstrap; no running service |
| `galexie-writer` | `galexie_s3_access_key` / `galexie_s3_secret_key` | write-only, `galexie-live` (policy `galexie-writer.json`) | `galexie.service` via `/etc/default/galexie` |
| `galexie-archive-writer` | `galexie_archive_s3_access_key` (fixed literal in `defaults/main.yml`, not vaulted) / `galexie_archive_s3_secret_key` | write (no delete), `galexie-archive` (policy `galexie-archive-writer.json`) | archive-backfill galexie via `/etc/default/galexie-backfill`; `galexie-archive-fill` via `/etc/default/galexie-archive-fill`; the rehydrate procedure in [lcm-cache-tiering.md](lcm-cache-tiering.md) |
| `stellarindex-reader` ("the ops user") | `stellarindex_reader_access_key` (fixed literal) / `stellarindex_reader_secret_key` → `vault_stellarindex_reader_secret_key` | read-only, `galexie-live` + `galexie-archive` | indexer/aggregator/api `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY` via **`/etc/default/stellarindex`** (`14-stellarindex-services.yml`, tag `stellarindex`); `stellarindex-ops verify-archive` + the heavy-job wrapper via `/etc/default/stellarindex-ops` (`09-minio.yml`, tag `minio`). **Two files, two tags** |
| `stellarindex-archive-trimmer` | `stellarindex_archive_trimmer_access_key` (fixed literal) / `stellarindex_archive_trimmer_secret_key` → `vault_stellarindex_archive_trimmer_secret_key` | List + **Delete**, `galexie-archive` only (policy `stellarindex-archive-trimmer.json`) — the only identity with delete there | `galexie-archive-trim.service` via `/etc/default/galexie-archive-trim` (`14-stellarindex-services.yml`), loaded after `/etc/default/stellarindex-ops` so its read path stays `stellarindex-reader` |

Naming is not uniform: only `galexie-writer`'s access key is vaulted. Check
`configs/ansible/roles/archival-node/defaults/main.yml` (search `← vault`) before
scripting against these.

### Regenerating the galexie-writer (or archive-writer / ops-user) secret

1. **Generate:** `openssl rand -hex 32` (hex avoids `/` and `+` if it is ever pasted into a URL).
2. **Update the vault:**
   ```sh
   cd configs/ansible
   ansible-vault edit inventory/r1.secrets.yml
   #   galexie_s3_secret_key: "<new secret>"              (galexie-writer)
   #   galexie_archive_s3_secret_key: "<new secret>"       (archive-writer)
   #   vault_stellarindex_reader_secret_key: "<new secret>" (ops user)
   ```
3. **Dry-run, then apply `minio,galexie,stellarindex` together — never a subset:**
   ```sh
   ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml \
     --tags minio,galexie,stellarindex --check --diff
   ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml \
     --tags minio,galexie,stellarindex -e galexie_restart_ack=true
   ```

   > ⚠️ **A subset is a self-inflicted outage.** The `minio` tag's `mc admin user add`
   > rotates the secret server-side immediately, while each consumer's env file is
   > re-rendered only by its own tag:
   >
   > | Consumer | Credential file | Rendered by | Tag |
   > |---|---|---|---|
   > | `galexie.service` | `/etc/default/galexie` | `07-galexie.yml` | `galexie` |
   > | archive-fill / backfill one-shots | `/etc/default/galexie-backfill`, `/etc/default/galexie-archive-fill` | `07-galexie.yml` | `galexie` |
   > | **`stellarindex-indexer` / `-aggregator` / `-api`** | **`/etc/default/stellarindex`** | `14-stellarindex-services.yml` | **`stellarindex`** |
   > | `verify-archive`, `ch-live-catchup`, `run-heavy-job.sh`, interactive ops | `/etc/default/stellarindex-ops` | `09-minio.yml` | `minio` |
   >
   > `minio` alone strands galexie; `minio,galexie` strands the three services
   > (ledgerstream `SignatureDoesNotMatch`, ingest stops). The `stellarindex` tag is safe
   > in a config apply: binary build/install is gated on `manage_stellarindex_binaries`
   > and migrations on `stellarindex_apply_migrations` (both default `false`).
   > `-e galexie_restart_ack=true` is required for the galexie restart; without it the
   > role renders and leaves the restart to a second, acknowledged run.

4. **Let the handlers restart — do NOT restart by hand.** With all three tags:
   - `/etc/default/galexie` → `Restart galexie` handler (given `galexie_restart_ack=true`).
   - `/etc/default/galexie-backfill` / `-archive-fill` → no handler (one-shots).
   - `/etc/default/stellarindex` → `Restart stellarindex-indexer` / `-aggregator` / `-api`.
   - `/etc/default/stellarindex-ops` → read fresh by each one-shot.

   > ⚠️ A manual `systemctl restart stellarindex-*` before the `stellarindex` tag has run
   > re-reads the OLD secret. If done, re-run step 3 with the full tag list.

5. **Verify:** `mc admin info local` (root creds) shows the user; `journalctl -u galexie -n 50`
   shows uploads with no `SignatureDoesNotMatch`; `/usr/local/bin/config-assertions.sh`
   prints no `FAIL galexie_writer_creds_valid`.

### Prometheus bearer-token regen (INV-0981/INV-1144 — now codified)

`configs/ansible/roles/archival-node/tasks/16-prometheus-exporters.yml` (Group D) creates a
`prometheus-read` MinIO policy scoped to `admin:Prometheus` and a service account under root
carrying it (procedure: [runbooks/minio-metrics-403.md](runbooks/minio-metrics-403.md)).
`mc admin user svcacct add` prints the secret **once**; the task writes it to
`/etc/prometheus/minio.token` (`prometheus:prometheus`, `0400`) and notifies
`Restart prometheus`. The task is gated on the file's absence, because re-running
`svcacct add` mints a new secret and invalidates the live one.

The token belongs to a service account, so **rotating MinIO root does not invalidate it**
(the old `mc admin prometheus generate` root-signed JWT did, firing `minio_exporter_down`).

To rotate the scrape token itself (suspected leak):

```sh
ssh root@136.243.90.96
mc admin user svcacct rm local <the-access-key-shown-by-svcacct-info>
rm -f /etc/prometheus/minio.token
```

then `ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml --tags exporters`
(`--tags minio` does not reach Group D) to mint a new one and restart Prometheus.

Symptoms: `minio_exporter_down` in
[runbooks/exporter-down.md](runbooks/exporter-down.md#per-exporter-notes); the 403 case in
[runbooks/minio-metrics-403.md](runbooks/minio-metrics-403.md). The hourly
`minio_prometheus_token_present` check in `scripts/ops/config-assertions.sh` catches a
missing, empty or wrong-owner file (stat only; never reads the token).

### The `SignatureDoesNotMatch` drift symptom

Any MinIO client on r1 logging `SignatureDoesNotMatch` means **credential-file/live-user
drift**, not an outage. Causes:

- Vault updated but the apply ran a PARTIAL tag list (see the step 3 table).
- The right tag ran but the consumer was restarted before it, so it holds the old
  `EnvironmentFile=` value.
- The MinIO user was rotated by hand (`mc admin user add ...` on the host) without updating
  the vault — the next `--tags minio` reverts it.
- `AWS_ENDPOINT_URL` / `AWS_REGION` missing from the rendered env file — the SDK signs for
  AWS proper, which MinIO also rejects (see the `/etc/default/stellarindex-ops` template
  comment in `09-minio.yml`).

Diagnosis: `journalctl -u galexie -n 200 --no-pager | grep -i signature`; identify the
failing identity from the unit; compare `/etc/default/galexie` (or
`/etc/default/stellarindex-ops`) with the vault via `ansible-vault view inventory/r1.secrets.yml`
(never paste or redirect its output into a log or transcript). On mismatch re-run step 3
with the full tag list.

### Config-assertion backstop

`galexie_writer_creds_valid` in `scripts/ops/config-assertions.sh` (hourly,
`config-assertions.timer`) re-signs a real `mc ls` with the creds in `/etc/default/galexie`,
so a one-sided rotation fails within the hour
([runbooks/config-assertion-failed.md](runbooks/config-assertion-failed.md)). It is an
auth-probe, not a diff: MinIO never exposes a stored secret. There is no equivalent check
for `stellarindex-reader` or `galexie-archive-writer`; both would use the same `MC_HOST_*`
auth-probe pattern.

### MinIO identity inventory

| Identity | Intended authority | Codified? | State |
| --- | --- | --- | --- |
| MinIO root (`minio_root_user`, now `stellarindex-admin`) | full admin; bootstrap + `mc admin` only, no service runs as it | env file + `local` alias | rotated; old access key rejected. Used only by the operator-run `PARTIALS` delete |
| `galexie-writer` | write on `galexie-live` | policy + user + attach | healthy; hourly auth-probe |
| `galexie-archive-writer` | write (no delete) on `galexie-archive` | policy + user + attach | repaired; `mc ls archivewriter/galexie-archive/` lists |
| `stellarindex-reader` | read-only on both buckets | policy + user + attach | live policy grants no `s3:DeleteObject`, matching the codified one. Re-check with `mc admin policy info local stellarindex-reader` |

**Rotating MinIO root.** Treat any transcript exposure as compromise, even though the
endpoint binds `127.0.0.1` and :9000 is open only to `internal_cidrs`.

```sh
cd configs/ansible
ansible-vault edit inventory/r1.secrets.yml   # new minio_root_user / minio_root_password
ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml --tags minio --check --diff
ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml --tags minio
```

Root lives in `MINIO_ROOT_USER`/`MINIO_ROOT_PASSWORD` in `/etc/default/minio`, so the
`Restart minio` handler restarts MinIO (short galexie write outage). Then re-point every
operator's `local` alias (`mc alias set local http://127.0.0.1:9000 <new root> <new secret>`;
the role does root's). The hourly `galexie-archive-fill.timer` is unaffected.

**`archivewriter` alias.** `09-minio.yml` renders `galexie-archive-writer.json`
(Put/Get/List + multipart on `galexie-archive`, no `s3:DeleteObject`), creates the user from
the vaulted secret and attaches the policy; `mc admin user add` rewrites an existing user's
secret, so `--tags minio` repairs drift. Prove it:

```sh
mc alias set archivewriter http://127.0.0.1:9000 galexie-archive-writer <vaulted secret>
mc ls archivewriter/galexie-archive/ | head        # must list, not 403
```

**Fill job identity.** `galexie-archive-fill.sh` lists and mirrors through `ARCHIVE_DEST`
(`archivewriter/galexie-archive`, set in `/etc/default/galexie-archive-fill`) and exits 1 if
it cannot list it. The only delete, the operator-run `PARTIALS=…` sweep, goes through
`ARCHIVE_DELETE_ALIAS` (`local`, same file); the run stops before deleting if that alias is
not configured. After deploying, confirm one full timer cycle.

## Related

- [runbooks/config-assertion-failed.md](runbooks/config-assertion-failed.md)
- [runbooks/exporter-down.md](runbooks/exporter-down.md)
- [runbooks/minio-metrics-403.md](runbooks/minio-metrics-403.md)
- [r1-ansible-drift-2026-07-03.md](r1-ansible-drift-2026-07-03.md)
- `configs/ansible/roles/archival-node/tasks/09-minio.yml`
