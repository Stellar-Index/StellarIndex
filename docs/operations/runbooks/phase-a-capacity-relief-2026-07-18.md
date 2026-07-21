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

## Capacity math (live-updated 2026-07-18 — supersedes the first estimate)

**Hardware reality (verified):** all 4× 7.68 TB NVMes are fully allocated — 2 hold OS (boot/swap/root on md-RAID1) + a 6.9 TB ZFS partition, 2 are whole-disk in the pool; `parted` shows **0 GB unpartitioned** and there is **no spare drive**. Adding *raw* capacity needs a **5th NVMe** — this pool already did a ZFS raidz-expansion, so a 5th drive can join the vdev (+~6.9 TiB usable → pool 94% → ~65%). Durable fix; a Hetzner/`[OP]` action (not doable from software).

**Software reclaim levers (no hardware):**
| Lever | ΔFree | Status |
|---|---|---|
| now (`data/clickhouse` avail) | ~0.9 TiB | fluctuating at 91–94% |
| recompress `ledger_entry_changes` XDR → ZSTD (measured **1.75×** on `entry_xdr`; p43 canary −31%) | **+~1.3–1.5 TiB** | **in progress** (1 of 16 partitions) |
| recompress the OTHER big tables — `operations` 3.1T / `operation_results` 2.3T / `contract_events` 1.2T (all **LZ4**) | **+~1.5–2.5 TiB** (canary MEASURED **2.04×** on `operations.body_xdr` — worth it; skip/test `transactions` — ratio 1.5, signature-heavy) | next, after LEC |
| pgbackrest diff prune (13→5 d) | +~1.0 TiB | **deferred** until S3 off-site exists (currently the *only* backup copy) — held as an emergency lever |
| **Phase D** comprehensive fill (as ZSTD) | −~1.5 TiB | after Phase A |

**Net (honest):** software-only — recompress-LEC, no prune, no hardware → after Phase A **~2.3 TiB** free → after Phase D **~0.8 TiB spare (pool ~87%)** — *works, but tight.* The earlier "~3.3 TiB after Phase A" assumed the pgbackrest prune, **now deferred → ~2.3 TiB.** Gate to start Phase D unchanged: free ≥ need (1.5) + 15% ≈ **1.73 TiB** — so we need the other-table recompress **or** the prune to clear the gate with margin; recompress-LEC alone doesn't.

**Projection — "will we be full after the backfills?" (answer: no, ~83–85%):**
| Stage | Usable free | Pool |
|---|---|---|
| now (mid-recompress) | ~0.9 TiB | 94% |
| after LEC recompress | ~2.4 TiB | ~88% |
| after other-tables recompress (2.04× lever) | ~4–5 TiB | ~76–80% |
| **after Phase D comprehensive backfill** | **~2.5–3.5 TiB** | **~83–85%** |

**Capacity decision (Ash, 2026-07-19):** a few months of headroom is the bar, and it's **met** — Phase D lands at ~83–85% with ~2.5–3.5 TiB free ≈ **~6 months** of live-growth runway (~5.5 TiB/yr). **A second server will be procured later** for durable growth; the **S3 offload** (galexie archive 5.56 TiB + pgBackRest off-site → frees ~7.5 TiB) is a **deferred lever, not needed now.** ⚠️ The margin depends on the **other-tables recompress** delivering ~1.5–2.5 TiB — without it Phase D lands at ~91% (fits, no headroom), so it's a required Phase A step, not optional.

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

## Step 3b — recompress the other big tables (after Step 3, sequenced not concurrent)
Canary MEASURED **2.04×** on `operations.body_xdr` → worth ~1.5–2.5 TiB. Same mechanism, run **after** LEC finishes (avoid concurrent merge pressure). For each of `operations`, `operation_results`, `contract_events`:
```
# 1. set ZSTD on the blob/XDR columns (instant metadata). Identify them first:
clickhouse-client --port 9300 -q "SELECT name,type FROM system.columns WHERE database='stellar' AND table='operations' AND (name LIKE '%_xdr' OR type LIKE '%String%') ORDER BY position"
clickhouse-client --port 9300 -q "ALTER TABLE stellar.operations MODIFY COLUMN body_xdr String CODEC(ZSTD(3))"   # + other _xdr cols
# 2. recompress per partition, biggest-first, disk-guarded — reuse the recompress-lec.sh pattern (change the table name + [range])
```
**Skip / test `transactions`** — its ratio is 1.5 (signature/result XDR is high-entropy; ZSTD won't help much; canary before spending the transient). Keep `max_bytes_to_merge_at_max_space_in_pool` at 150 GiB and the disk floor active throughout.

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
