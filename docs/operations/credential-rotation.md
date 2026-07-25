---
title: Credential rotation runbook
last_verified: 2026-07-05
status: current
---

# Credential rotation runbook

Procedure for rotating the load-bearing service credentials on an
archival node (r1 today; the same shape applies to R2/R3 once they
run their own MinIO). Born from the 2026-07-03 incident follow-up
(`docs/audit-2026-07-03-site/REGISTER.md`'s same-day note): a MinIO
root-password rotation invalidated the Prometheus scrape bearer
token and nobody had written down "when you rotate MinIO, also do
X" — the fix was applied by hand with no runbook and no drift guard.
This doc is that missing writedown; §MinIO below is the first
credential family covered. Extend it with a new `##` section per
credential family as they get their own rotation procedure (Postgres
password, SEP-10 signing seed, webhook HMAC secrets, etc. are not
yet written up here).

All secrets referenced below live **ansible-vault encrypted** in
`configs/ansible/inventory/<region>.secrets.yml` (`r1.secrets.yml`
for r1). That file is deliberately **not tracked in the repo** (the
2026-07-03 exposure response decided an encrypted vault in a public
repo is still an offline-bruteforce target) — CI materializes it
from the `ANSIBLE_VAULT_FILE_B64` GitHub Actions secret for the
`ansible-drift.yml` workflow, and operators keep their own local
copy for interactive `ansible-playbook` runs. Edit it with:

```sh
cd configs/ansible
ansible-vault edit inventory/r1.secrets.yml   # needs the vault password
```

## MinIO

r1 runs a single-node MinIO (`configs/ansible/roles/archival-node/tasks/09-minio.yml`)
backing Galexie's S3-compatible target. Four identities matter:

| Identity | Vault variable(s) | Scope | Consumed by |
|---|---|---|---|
| MinIO root | `minio_root_user` / `minio_root_password` | full admin | the ansible role's own `mc alias set local ...` bootstrap step (09-minio.yml); not used by any running service |
| `galexie-writer` | `galexie_s3_access_key` / `galexie_s3_secret_key` | write-only, `galexie-live` bucket (policy `galexie-writer.json`) | `galexie.service` via `/etc/default/galexie` |
| `galexie-archive-writer` | `galexie_archive_s3_access_key` (fixed literal `"galexie-archive-writer"` in `defaults/main.yml`, not vaulted) / `galexie_archive_s3_secret_key` (vaulted) | write (no delete), `galexie-archive` bucket — policy `galexie-archive-writer.json`, **ansible-managed only since 2026-07-25**, see §MinIO identity inventory | the one-shot archive-backfill galexie instance via `/etc/default/galexie-backfill`; the rehydrate procedure in [lcm-cache-tiering.md](lcm-cache-tiering.md) |
| `stellarindex-reader` (**"the ops user"**) | `stellarindex_reader_access_key` (fixed literal `"stellarindex-reader"`, not vaulted) / `stellarindex_reader_secret_key` → aliased in `defaults/main.yml` to `vault_stellarindex_reader_secret_key` (renamed 2026-07-03 drift audit) | read-only, both `galexie-live` + `galexie-archive` | `stellarindex-ops verify-archive`, and — per `/etc/default/stellarindex-ops` — the indexer/aggregator/api's `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY` env for the ledgerstream reader path |

Only the **access key** for `galexie-writer` is vault-sourced (the
identity itself, not just its secret) — `galexie-archive-writer` and
`stellarindex-reader` use fixed, non-secret access-key literals with
only the secret key vaulted. Don't assume a uniform naming scheme;
check `configs/ansible/roles/archival-node/defaults/main.yml` (search
`← vault`) before scripting against these.

### Regenerating the galexie-writer (or archive-writer / ops-user) secret

1. **Generate a new secret.** `openssl rand -hex 32` (or `-base64
   32`) — anything MinIO's `mc admin user add` accepts. Avoid `/`
   and `+`-heavy base64 if you'll ever hand-paste it into a URL;
   hex is the safer default here.
2. **Update the vault.**
   ```sh
   cd configs/ansible
   ansible-vault edit inventory/r1.secrets.yml
   # bump the relevant var:
   #   galexie_s3_secret_key: "<new secret>"              (galexie-writer)
   #   galexie_archive_s3_secret_key: "<new secret>"       (archive-writer)
   #   vault_stellarindex_reader_secret_key: "<new secret>" (ops user)
   ```
3. **Dry-run, then apply the `minio` tag.**
   ```sh
   ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml \
     --tags minio --check --diff
   ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml \
     --tags minio
   ```
   `09-minio.yml`'s `mc admin user add local <key> <new secret>`
   task (the "Create galexie-writer MinIO user" / "Create
   galexie-archive-writer MinIO user" / "Create stellarindex-reader
   MinIO user" tasks) **updates the secret in place** if the user
   already exists — this is the actual rotation step server-side. It
   is idempotent and safe to re-run.
4. **The env files re-template automatically** as part of the same
   `--tags minio` run:
   - `/etc/default/galexie` (galexie-writer) — has a `notify:
     Restart galexie` handler, so **galexie restarts automatically**
     when this task changes content.
   - `/etc/default/galexie-backfill` (archive-writer) — **no
     restart handler** (it's a one-shot backfill instance, normally
     not running); nothing to restart in steady state.
   - `/etc/default/stellarindex-ops` (ops user / `stellarindex-reader`)
     — templated by the same `09-minio.yml` task file but **has no
     `notify:` handler at all**. Re-applying `--tags minio` refreshes
     the file on disk, but `stellarindex-indexer` /
     `stellarindex-aggregator` / `stellarindex-api` **do not pick up
     the new secret until manually restarted** — they read this file
     only once via `EnvironmentFile=` at process start. Restart them
     explicitly:
     ```sh
     systemctl restart stellarindex-indexer stellarindex-aggregator stellarindex-api
     ```
5. **Verify.** `mc admin info local` (root creds) should show the
   user with the new secret's fingerprint; `journalctl -u galexie -n
   50` should show continued successful uploads with no
   `SignatureDoesNotMatch`; `/usr/local/bin/config-assertions.sh`
   should print no `FAIL galexie_writer_creds_valid` (see below).
6. **Rotating MinIO root** additionally invalidates the Prometheus
   scrape bearer token — see the next subsection. This is exactly
   the 2026-07-03 incident: root got rotated, `/etc/prometheus/minio.token`
   kept signing with the old root creds, and `minio_exporter_down`
   fired.

### Prometheus bearer-token regen (MinIO root rotation only)

Only needed when the **root** password changes — the bucket-scoped
writer/reader users above don't touch the metrics scrape path.

```sh
mc admin prometheus generate local > /etc/prometheus/minio.token
systemctl reload prometheus     # must reload to pick up the new bearer file
```

See [runbooks/exporter-down.md](runbooks/exporter-down.md#per-exporter-notes)
for the day-to-day symptom (`minio_exporter_down`) and
[runbooks/minio-metrics-403.md](runbooks/minio-metrics-403.md) for
the companion 403 case. This step is **not yet codified in
ansible** — it was applied by hand on 2026-07-03 (CLAUDE.md's "codify
every host change" rule is violated here until a task lands in
`09-minio.yml` to template `/etc/prometheus/minio.token` from a
`mc admin prometheus generate` run; tracked as a follow-up, not done
in this pass).

### The `SignatureDoesNotMatch` drift symptom

If galexie, the indexer, `stellarindex-ops verify-archive`, or any
other MinIO client on r1 starts logging `SignatureDoesNotMatch`,
suspect a **credential-file/live-user drift**, not a MinIO outage:
the server is up and answering — it's just rejecting requests
signed with a key it doesn't recognise (or with the AWS SigV4
region/endpoint fields absent, which produces the same symptom —
see the note in `09-minio.yml`'s `/etc/default/stellarindex-ops`
template and `feedback_minio_cred_drift`). This happens whenever
one side of a rotation moves without the other:

- The vault secret was updated but `--tags minio` was never
  re-applied (env file still has the old secret).
- `--tags minio` was applied but the corresponding service was
  never restarted (this is the `stellarindex-ops` env-file gap
  called out in step 4 above — there is no ansible handler wired to
  it).
- The MinIO user was rotated by hand (`mc admin user add ...`
  run directly on the host, bypassing ansible) without updating the
  vault — the next `--tags minio` re-apply would silently revert it,
  or a rebuild would ship the old secret.
- `AWS_ENDPOINT_URL` / `AWS_REGION` are missing from the rendered env
  file — the AWS SDK then signs for AWS proper instead of the local
  MinIO endpoint, which MinIO also rejects as `SignatureDoesNotMatch`
  (a real drift found live on 2026-07-03; see
  `configs/ansible/roles/archival-node/tasks/09-minio.yml`'s
  `/etc/default/stellarindex-ops` template comment).

Diagnosis: `journalctl -u galexie -n 200 --no-pager | grep -i
signature`; confirm which identity is failing (galexie-writer vs
ops-user) from the unit; compare `/etc/default/galexie` (or
`/etc/default/stellarindex-ops`) against what the vault currently
holds (`ansible-vault view inventory/r1.secrets.yml`) — a mismatch
means re-run `--tags minio` and restart the affected service(s) per
the steps above.

### Config-assertion backstop

`scripts/ops/config-assertions.sh`'s `galexie_writer_creds_valid`
check (hourly, `config-assertions.timer`) catches the galexie-writer
half of this drift automatically: it re-signs a real `mc ls` request
against the live MinIO server using whatever creds are currently in
`/etc/default/galexie`, so a rotation that only landed on one side
fails within the hour instead of waiting for galexie's own upload
loop to notice. See
[runbooks/config-assertion-failed.md](runbooks/config-assertion-failed.md).
This check is deliberately functional (auth-probe), not a content
diff — MinIO never exposes a stored secret to compare against, so
"does this credential still work" is the only thing that can be
asserted without either leaking the secret or maintaining a second
copy of it outside the vault.

There is currently no equivalent assertion for the
`stellarindex-reader` (ops-user) or `galexie-archive-writer`
credentials — both would follow the same `MC_HOST_*` auth-probe
pattern against their respective bucket if added.

### MinIO identity inventory + credential hygiene (2026-07-25)

Three findings from the 2026-07-25 rehydrate proof, and what each
needs. **All three need an operator on the host; none of them can be
finished from the repository.**

| Identity | Intended authority | Codified? | State |
| --- | --- | --- | --- |
| MinIO root (`minio_root_user`, still named `ratesengine-admin` from the pre-rename era) | full admin. Bootstrap + `mc admin` only; **no service should ever run as this** | env file + `local` alias | ⚠️ **exposed in an agent session transcript 2026-07-25 → must be rotated**, and is currently the identity the hourly archive-fill job writes as |
| `galexie-writer` | write on `galexie-live` | ✅ policy + user + attach | healthy; hourly auth-probe backstop |
| `galexie-archive-writer` | write (no delete) on `galexie-archive` | ✅ **as of 2026-07-25** — previously the vault var and the env file existed but no MinIO user, policy, or attach was ever created | the `archivewriter` mc alias fails `SignatureDoesNotMatch`; repaired by the next `--tags minio` apply, which re-syncs the secret from vault |
| `stellarindex-reader` | read-only on both buckets | ✅ policy + user + attach | codified policy grants **no** `s3:DeleteObject`, but the 2026-07-25 live test observed this identity successfully DELETING from `galexie-archive` — i.e. the live policy has drifted from the codified one. Verify with `mc admin policy info local stellarindex-reader` and re-apply if it disagrees |

**1. Rotate MinIO root.** Its credentials appeared in plaintext in an
agent session transcript on 2026-07-25. Treat as compromised even
though the endpoint is bound to `127.0.0.1` and the firewall exposes
:9000 only to `internal_cidrs` — the transcript is the wider blast
radius, not the port.

```sh
cd configs/ansible
ansible-vault edit inventory/r1.secrets.yml   # new minio_root_user / minio_root_password
ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml --tags minio --check --diff
ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml --tags minio
```

Root lives in `MINIO_ROOT_USER`/`MINIO_ROOT_PASSWORD` in
`/etc/default/minio`, so this **restarts MinIO** (the `Restart minio`
handler) — a short write outage for galexie. Then, in the same window:

- regenerate the Prometheus bearer token (§Prometheus bearer-token
  regen above) — root rotation always invalidates it, this is the
  2026-07-03 incident;
- re-point the `local` mc alias: `mc alias set local
  http://127.0.0.1:9000 <new root> <new secret>` (the ansible task does
  this, but any operator's own `~/.mc/config.json` needs it too);
- confirm the hourly `galexie-archive-fill.timer` run after the
  rotation succeeded — it authenticates via the `local` alias.

Take the opportunity to rename the user off `ratesengine-admin` while
you are creating a new one; the old brand name in an admin credential
is a small but free thing to retire.

**2. `archivewriter` — fixed, not removed.** The choice was
fix-or-remove; fix won, because the archive genuinely needs a write
identity that isn't root. `09-minio.yml` now renders
`galexie-archive-writer.json` (Put/Get/List + multipart on
`galexie-archive`, deliberately **no** `s3:DeleteObject`), creates the
user from the vaulted secret, and attaches the policy. `mc admin user
add` rewrites the secret of an existing user, so the drifted alias is
repaired by applying `--tags minio`. Afterwards, re-point the operator
alias and prove it:

```sh
mc alias set archivewriter http://127.0.0.1:9000 galexie-archive-writer <vaulted secret>
mc ls archivewriter/galexie-archive/ | head        # must list, not 403
```

**3. The hourly fill job still writes as root — least-privilege gap,
deliberately left open in this change.**
`galexie-archive-fill.sh` mirrors AWS→MinIO and sweeps empty partitions
using the `local` (root) alias, including `mc rm --recursive --force`.
Switching it to `galexie-archive-writer` needs the identity to exist on
the host first (step 2) and needs the delete sweep separated from the
mirror, since the writer policy grants no delete — a live cutover with a
verification step, not a config edit that can be proven in CI. Do it as
a follow-up after step 2 lands, and confirm one full timer cycle before
walking away.

## Related

- [runbooks/config-assertion-failed.md](runbooks/config-assertion-failed.md)
- [runbooks/exporter-down.md](runbooks/exporter-down.md)
- [runbooks/minio-metrics-403.md](runbooks/minio-metrics-403.md)
- [r1-ansible-drift-2026-07-03.md](r1-ansible-drift-2026-07-03.md)
- `configs/ansible/roles/archival-node/tasks/09-minio.yml`
