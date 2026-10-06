---
title: pgBackRest repository encryption
last_verified: 2026-07-25
status: current
---

# pgBackRest repository encryption (F4-F2)

**Status:** repo1 is UNENCRYPTED on r1 today. This is the procedure to fix
that and what it costs.

`templates/pgbackrest.conf.j2` renders `repo1-cipher-type=aes-256-cbc` when
`pgbackrest_repo1_cipher_pass` is set, and `tasks/18-pgbackrest-backup.yml`
(both under `configs/ansible/roles/archival-node/`) refuses to render an
unencrypted repo1 unless `pgbackrest_repo1_unencrypted_ack: true`. Neither
encrypts anything by itself (§2).

## 1. Why this matters

repo1 (`/var/lib/pgbackrest`) is a full physical copy of the production DB
plus WAL: API keys and scopes (migration 0073), the billing schema with
Stripe identifiers (0027), price alerts and delivery targets (0080). With
pgBackRest's default `cipher-type=none` it sits in plaintext behind
filesystem permissions alone, and:

- it lives on the `data/pgbackrest` ZFS dataset, so any `zfs send`,
  snapshot copy, rsync or disk-level backup of the pool carries the plaintext;
- it shares a failure domain with the DB (the REL-DR finding
  `pgbackrest_offsite_ack` documents), so a host compromise reaching the DB
  also reaches a greppable copy;
- repo2 (offsite) has been encrypted since CS-111; repo1 is the weaker copy
  on the box an attacker is already on.

## 2. Why this is not a config flip

**A repository's cipher is fixed at stanza creation and cannot change.**
`stanza-upgrade` does not re-encrypt (it is for PostgreSQL version upgrades).
Pointing pgbackrest at an existing plaintext repo with `repo1-cipher-type`
set makes it refuse to read it (stored vs running config disagree).

The only path is **create a new repository**. The old repo stays plaintext
until deleted, and the new one starts with no history; retention
(`repo1-retention-full=2`) rebuilds from the first new full backup.

Setting `pgbackrest_repo1_cipher_pass` and re-applying WITHOUT §4 leaves a
config that cannot read its own backups. Do not do half of this.

## 3. Key custody — read before generating anything

The passphrase is the only way to read an encrypted repo: no recovery, no
escrow in pgbackrest, and a restore drill will not warn you it is lost.

- Generate with `openssl rand -base64 48`.
- Store in `configs/ansible/inventory/r1.secrets.yml` (ansible-vault) as
  `vault_pgbackrest_repo1_cipher_pass`, referenced from inventory as
  `pgbackrest_repo1_cipher_pass`.
- Store a SECOND copy outside this repository and outside r1 (the operator's
  password manager). A vault whose only copy is on the host the backups
  protect is not custody.
- Rotating costs the same as §4: a new repo and a new full backup.

## 4. The procedure (operator, on r1)

**Do repo2 first.** r1 has no offsite copy today (`pgbackrest_offsite_ack:
true` in inventory). Provisioning the already-encrypted repo2 BEFORE
re-creating repo1 means no window with zero restorable copies (§4.4,
otherwise several hours). If repo2 is not ready, accept that window
deliberately and schedule it.

1. **Prove the current backup restores:**
   `sudo bash /usr/local/bin/restore-drill.sh` (see
   `docs/operations/drills/restore-drills.md`). Non-destructive.
2. **Add the secret**: `ansible-vault edit inventory/r1.secrets.yml`, add
   `vault_pgbackrest_repo1_cipher_pass`; set
   `pgbackrest_repo1_cipher_pass: "{{ vault_pgbackrest_repo1_cipher_pass }}"`
   and remove `pgbackrest_repo1_unencrypted_ack` from inventory. Leave
   `pgbackrest_manage_conf` as it is for now.
3. **Stop the backup timer and pause archiving to repo1:**
   ```sh
   systemctl stop pgbackrest-backup.timer
   sudo -u postgres pgbackrest --stanza=stellarindex stop
   ```
   `pgbackrest stop` makes `archive-push` a no-op, not an error, so
   PostgreSQL keeps recycling WAL. **This is the RPO window**: nothing is
   archived until step 6. Watch pg_wal free space; never leave the stanza
   stopped overnight.
4. **Delete the old stanza and repo:**
   ```sh
   sudo -u postgres pgbackrest --stanza=stellarindex --repo=1 stanza-delete --force
   ```
   *Destructive.* Every repo1 backup and WAL archive is gone; recovery to
   any point before step 6 is impossible from repo1 (repo2, if it exists, still has it).
5. **Render the encrypted config**: re-apply the role with
   `pgbackrest_manage_conf: true`, review the rendered diff of
   `/etc/pgbackrest/pgbackrest.conf`, confirm it carries
   `repo1-cipher-type=aes-256-cbc`.
6. **Create the stanza and take a full backup:**
   ```sh
   sudo -u postgres pgbackrest --stanza=stellarindex stanza-create
   sudo -u postgres pgbackrest --stanza=stellarindex start
   sudo -u postgres pgbackrest --stanza=stellarindex --type=full backup
   ```
   The ~273 GB full took ~15 min in the 2026-07-03 drill; the WAL gap from
   step 3 closes once `start` runs. `stellarindex_timescale_backup_none_24h`
   (SEV-1) fires if no backup completes within 24 h of the last old one: do
   the whole procedure inside one window.
7. **Re-enable the schedule**: `systemctl start pgbackrest-backup.timer`.
8. **Verify encryption, do not assume it**:
   ```sh
   sudo -u postgres pgbackrest --stanza=stellarindex info        # shows the new full
   sudo grep -c cipher /var/lib/pgbackrest/backup/stellarindex/backup.info
   sudo strings /var/lib/pgbackrest/backup/stellarindex/latest/... | head   # must be noise
   ```
   Then run the restore drill AGAINST THE NEW REPO
   (`sudo bash /usr/local/bin/restore-drill.sh`) and commit the appended
   entry in `docs/operations/drills/restore-drills.md`. An encrypted backup
   nobody has restored is worse than a plaintext one: the passphrase can be
   wrong too.

## 5. What ships in code vs what an operator must do

| Step | Where |
| --- | --- |
| Render `repo1-cipher-type`/`-pass` when the var is set | ✅ `templates/pgbackrest.conf.j2` |
| Refuse a silently-unencrypted repo1 | ✅ `tasks/18-pgbackrest-backup.yml` assert |
| Generate + vault the passphrase | ⬜ operator (§3) |
| stanza-delete / stanza-create / full backup | ⬜ operator (§4); needs SSH, never automated (destructive) |
| Post-change restore drill | ⬜ operator (§4.8) |

## Related

- `docs/operations/off-site-backup-plan.md` — repo2, and why it lands first.
- `docs/operations/drills/restore-drills.md` — the drill evidence log.
- `docs/operations/runbooks/infra.md#stellarindex_timescale_backup_none_24h` — the alert this can trip if it runs long.
- `docs/adr/0043-backup-and-restore-strategy.md` — the strategy this closes a gap in.
