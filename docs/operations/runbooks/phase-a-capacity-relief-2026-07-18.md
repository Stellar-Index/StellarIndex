---
title: "Runbook — Phase A: capacity relief (execute-ready)"
last_verified: 2026-07-18
status: ready-pending-go
severity: P1
---

# Runbook — Phase A: capacity relief

Make room for the comprehensive backfill (Phase D) **without hitting the disk wall**, on measured numbers. Gate: `CH free ≥ Phase-D need + 15% margin`, proven, before any `[54M+]` / degraded-range walk. Nothing here changes served data; all steps are reversible.

## Starting state (live-verified 2026-07-18)
- ZFS pool `data` 94% full; `data/clickhouse` **available ~969 GiB**; no reclaimable snapshots.
- `ledger_entry_changes` = 4.93 TiB / 84 B rows; dominant cols `entry_xdr` 2.51 TiB + `key_xdr` 1.03 TiB, **codec = LZ4 (default)**.
- **Measured ZSTD(3) gain on `entry_xdr` = 1.75×** (LZ4 24.8 MiB → 14.2 MiB, identical rows).
- Phase 0 **halted** (its runaway loop `/root/phase0.sh` is stopped — do not restart it).

## Capacity math (why it fits)
| Item | ΔFree |
|---|---|
| Start (`data/clickhouse` available) | +0.95 TiB |
| **Step 1** pgbackrest prune (retention-diff → ~5 d) | **+~1.0 TiB** |
| **Step 3** recompress `entry_xdr`+`key_xdr` → ZSTD | **+~1.3–1.5 TiB** |
| = free after Phase A | **~3.3 TiB** |
| **Phase D** fill (10.5 B ops degraded ranges, as ZSTD) | −~1.5 TiB |
| = free after Phase D (comprehensive) | **~1.8 TiB** |
Gate `need+15%` = 1.5 × 1.15 ≈ **1.73 TiB**; free after Phase A ≈ 3.3 TiB → **holds with ~1.8 TiB to spare.**

---

## Step 0 — prerequisite: land the data-pool watchdog on the host
The codified `run-heavy-job.sh` now guards `/var/lib/clickhouse`, not just root (PR — `14-stellarindex-services.yml`). Apply **just that task** to R1 before any heavy job (targeted, safe — no heavy job running):
```
ansible-playbook -i inventory/r1.yml site.yml \
  --tags stellarindex-services --start-at-task="Install run-heavy-job.sh"  # (or the task name)
# verify on host:
grep -q /var/lib/clickhouse /usr/local/sbin/run-heavy-job.sh && echo "watchdog updated"
```
*(If a full targeted run is undesirable pre-capacity, a one-off `ansible.builtin.copy` of that single file is acceptable — but codify via the role, do not hand-edit the host.)*

## Step 1 — pgbackrest prune (fast ~1 TiB)  `[OP]` decision
2.52 TiB is ~13 daily diffs off one full (`20260705F`). Keep ~5 days:
```
# on R1, set in /etc/pgbackrest/pgbackrest.conf (codify in 18-pgbackrest-backup.yml):
#   repo1-retention-diff=5
sudo -u postgres pgbackrest --stanza=stellarindex expire
sudo -u postgres pgbackrest info   # confirm old diffs gone
zfs list -o name,used,avail data/pgbackrest data/clickhouse
```
Tradeoff: shortens differential PITR granularity to ~5 days (the full + WAL archive still give full-range recovery). **Your call on the retention number.**

## Step 2 — set the ZSTD codec (instant, metadata-only)
Applies to **new** parts immediately; existing parts convert on Step 3's rewrite. Lossless, reversible.
```
clickhouse-client --port 9300 -q "ALTER TABLE stellar.ledger_entry_changes MODIFY COLUMN entry_xdr String CODEC(ZSTD(3))"
clickhouse-client --port 9300 -q "ALTER TABLE stellar.ledger_entry_changes MODIFY COLUMN key_xdr  String CODEC(ZSTD(3))"
```
Also set it in `deploy/clickhouse/tier1_schema.sql` so the schema and new-ingest match.

## Step 3 — recompress existing partitions, one at a time (monitored)
`OPTIMIZE … PARTITION p FINAL` rewrites that partition's parts under the new codec (transient = that partition's current size ≤ 424 GiB; old parts freed after). **`OPTIMIZE` does not fire the MV** (only INSERTs do), so `ledger_entries_current` is untouched. CH reserves merge space up-front and **errors rather than fills** if short — the hard safety. Drive it under the heavy-job wrapper so the data-pool watchdog is active:
```
run-heavy-job.sh recompress-lec bash -c '
  set -euo pipefail
  for p in $(clickhouse-client --port 9300 -q "SELECT partition FROM system.parts WHERE database=\"stellar\" AND table=\"ledger_entry_changes\" AND active GROUP BY partition ORDER BY sum(bytes_on_disk) ASC"); do
    avail=$(df --output=avail -k /var/lib/clickhouse | tail -1 | tr -d " ")
    [ "$avail" -lt 524288000 ] && { echo "ABORT: <500 GiB free before partition $p"; exit 1; }   # ~500 GiB floor
    echo "=== OPTIMIZE partition $p ($(date -u +%FT%TZ), avail ${avail}KiB) ==="
    clickhouse-client --port 9300 --receive_timeout 36000 -q "OPTIMIZE TABLE stellar.ledger_entry_changes PARTITION \x27$p\x27 FINAL"
  done'
```
Smallest-first builds headroom early. Watch `stellarindex_zfs_pool_low_space` and `SELECT * FROM system.merges`. Reclaims ~1.3–1.5 TiB.

## Gate → proceed to Phase D only when ALL hold
- [ ] `data/clickhouse` available **≥ ~2.0 TiB** (need 1.5 + margin), measured.
- [ ] `entry_xdr`/`key_xdr` compressed size dropped ~40% (`system.columns`); ZFS pool < 85%.
- [ ] Watchdog live on host (`grep /var/lib/clickhouse /usr/local/sbin/run-heavy-job.sh`).
- [ ] `reconcile-balances` still green on the untouched `[38–54M]` (recompress is lossless — a canary).

## Rollback / safety
- Codec change is reversible: `MODIFY COLUMN … CODEC(LZ4)` + re-`OPTIMIZE` (needs the space back).
- pgbackrest prune is not reversible (expired backups are gone) — hence it's the one `[OP]`-gated step; the full + WAL remain.
- Any `OPTIMIZE` that would overflow errors out (CH space reservation) — safe to retry after freeing more.
- Abort the loop any time (Ctrl-C / `systemctl stop heavy-recompress-lec.scope`); partitions already done stay done.

## Related
Master plan: `../production-readiness-master-plan-2026-07-18.md` (Phase A/A0b). Phase D detail: `consolidated-deploy-plan-2026-07-18.md`.
