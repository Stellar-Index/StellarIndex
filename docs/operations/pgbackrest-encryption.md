---
title: pgBackRest repository encryption
last_verified: 2026-07-25
status: current
---

# pgBackRest repository encryption (F4-F2)

**Status:** repo1 is UNENCRYPTED on r1 today. This document is the
procedure to fix that, and the honest statement of what it costs.

`configs/ansible/roles/archival-node/templates/pgbackrest.conf.j2` now
renders `repo1-cipher-type=aes-256-cbc` when
`pgbackrest_repo1_cipher_pass` is set, and
`configs/ansible/roles/archival-node/tasks/18-pgbackrest-backup.yml`
refuses to render an unencrypted repo1 unless
`pgbackrest_repo1_unencrypted_ack: true` says so out loud. Neither of
those changes encrypts anything on their own — see §2.

## 1. Why this matters

repo1 (`/var/lib/pgbackrest`) holds a full physical copy of the
production database plus its WAL stream. That copy includes every table
the API serves from and every table it authenticates with — API keys and
their scopes (migration 0073), the platform/billing schema with Stripe
identifiers (0027), price alerts and their delivery targets (0080).
pgBackRest's default is `cipher-type=none`, so all of it is on disk in
plaintext, protected by filesystem permissions alone.

Permissions are not the whole boundary here:

- the repo lives on the `data/pgbackrest` ZFS dataset, so any `zfs
  send`, snapshot copy, rsync, or disk-level backup of that pool carries
  the plaintext with it;
- repo1 shares a failure domain with the database it protects (the
  REL-DR finding that `pgbackrest_offsite_ack` already documents), so
  any host compromise that reaches the DB also reaches an offline,
  greppable copy of it;
- repo2 (offsite) has been encrypted since CS-111. repo1 never was —
  the weaker copy is the one on the box an attacker is already on.

## 2. Why this is not a config flip

**A pgBackRest repository's cipher is fixed when the stanza is created
and cannot be changed on an existing repo.** `stanza-upgrade` does not
re-encrypt — it exists for PostgreSQL version upgrades. Point
pgbackrest at an existing plaintext repo with `repo1-cipher-type` set
and it will refuse to read it, because the stored repo config and the
running config disagree.

So the only path from plaintext to encrypted is **create a new
repository**. Everything already in the old repo stays plaintext until
it is deleted, and the new repo starts with no backup history — the
retention window (`repo1-retention-full=2`) rebuilds from the first new
full backup forward.

Setting `pgbackrest_repo1_cipher_pass` and re-applying the role WITHOUT
doing §4 leaves the host with a config that cannot read its own
backups. Do not do half of this.

## 3. Key custody — read before generating anything

The passphrase is the only way to read an encrypted repo. There is no
recovery path, no escrow inside pgbackrest, and a restore drill will not
warn you that you have lost it until the day you need it.

- Generate with `openssl rand -base64 48`.
- Store in `configs/ansible/inventory/r1.secrets.yml` (ansible-vault) as
  `vault_pgbackrest_repo1_cipher_pass`, referenced from inventory as
  `pgbackrest_repo1_cipher_pass`.
- Store a SECOND copy outside this repository and outside r1 — the
  operator's password manager. A vault file whose only copy lives on
  the host the backups protect is not custody.
- Rotating it later has the same cost as §4: a new repo and a new full
  backup.

## 4. The procedure (operator, on r1)

**Do repo2 first.** r1 has no offsite copy today
(`pgbackrest_offsite_ack: true` in inventory). Provisioning the
already-encrypted repo2 BEFORE re-creating repo1 means the box is never
without a usable backup during the window in §4.4, which is otherwise
several hours with zero restorable copies. If repo2 is not ready,
accept that window deliberately and schedule it — do not discover it.

1. **Prove the current backup restores**, so the thing being discarded
   is known-good rather than assumed-good:
   `sudo bash /usr/local/bin/restore-drill.sh` (see
   `docs/operations/drills/restore-drills.md`). Non-destructive.
2. **Add the secret**: `ansible-vault edit inventory/r1.secrets.yml`,
   add `vault_pgbackrest_repo1_cipher_pass`; set
   `pgbackrest_repo1_cipher_pass: "{{ vault_pgbackrest_repo1_cipher_pass }}"`
   and remove `pgbackrest_repo1_unencrypted_ack` from inventory. Leave
   `pgbackrest_manage_conf` as it is for now.
3. **Stop the backup timer and pause archiving to repo1** so nothing
   writes to a stanza that is about to be deleted:
   ```sh
   systemctl stop pgbackrest-backup.timer
   sudo -u postgres pgbackrest --stanza=stellarindex stop
   ```
   `pgbackrest stop` makes `archive-push` a no-op rather than an error,
   so PostgreSQL keeps recycling WAL. **This is the RPO window**:
   nothing is being archived until step 6. Watch pg_wal free space; do
   not leave the stanza stopped overnight.
4. **Delete the old stanza and repo**:
   ```sh
   sudo -u postgres pgbackrest --stanza=stellarindex --repo=1 stanza-delete --force
   ```
   *This is the destructive step.* Every existing repo1 backup and its
   WAL archive are gone; recovery to any point before step 6 is no
   longer possible from repo1. If repo2 exists, it still is.
5. **Render the encrypted config**: re-apply the role with
   `pgbackrest_manage_conf: true`, review the rendered diff of
   `/etc/pgbackrest/pgbackrest.conf`, and confirm it now carries
   `repo1-cipher-type=aes-256-cbc`.
6. **Create the stanza and take a full backup**:
   ```sh
   sudo -u postgres pgbackrest --stanza=stellarindex stanza-create
   sudo -u postgres pgbackrest --stanza=stellarindex start
   sudo -u postgres pgbackrest --stanza=stellarindex --type=full backup
   ```
   The full backup of the ~273 GB set took ~15 min in the 2026-07-03
   drill; the WAL gap from step 3 closes as soon as `start` runs.
   `stellarindex_timescale_backup_none_24h` (SEV-1) fires if this has
   not completed within 24 h of the last old backup — do the whole
   procedure inside one window, not across days.
7. **Re-enable the schedule**: `systemctl start pgbackrest-backup.timer`.
8. **Verify encryption, do not assume it**:
   ```sh
   sudo -u postgres pgbackrest --stanza=stellarindex info        # shows the new full
   sudo grep -c cipher /var/lib/pgbackrest/backup/stellarindex/backup.info
   sudo strings /var/lib/pgbackrest/backup/stellarindex/latest/... | head   # must be noise
   ```
   Then run the restore drill AGAINST THE NEW REPO —
   `sudo bash /usr/local/bin/restore-drill.sh` — and commit the appended
   entry in `docs/operations/drills/restore-drills.md`. An encrypted
   backup nobody has restored is a strictly worse hope than a plaintext
   one, because now the passphrase can be wrong too.

## 5. What ships in code vs what an operator must do

| Step | Where |
| --- | --- |
| Render `repo1-cipher-type`/`-pass` when the var is set | ✅ `templates/pgbackrest.conf.j2` |
| Refuse a silently-unencrypted repo1 | ✅ `tasks/18-pgbackrest-backup.yml` assert |
| Generate + vault the passphrase | ⬜ operator (§3) |
| stanza-delete / stanza-create / full backup | ⬜ operator (§4) — needs SSH; never automated, it is destructive |
| Post-change restore drill | ⬜ operator (§4.8) |

## Related

- `docs/operations/off-site-backup-plan.md` — repo2, and why it should
  land first.
- `docs/operations/drills/restore-drills.md` — the drill evidence log.
- `docs/operations/runbooks/backup-failed.md` — the alert this procedure
  can trip if it runs long.
- `docs/adr/0043-backup-and-restore-strategy.md` — the strategy this
  closes a gap in.
