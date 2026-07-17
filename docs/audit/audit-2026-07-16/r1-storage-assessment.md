# R1 disk / storage assessment (2026-07-16)

First-pass from repo config + docs — **no live r1 access during the Phase-0 freeze**, so every "current" number is a doc snapshot (mostly `storage-considerations.md`, last_verified 2026-05-20) and the live checks at the end are `[OP]`. Where the doc is stale I say so.

## TL;DR (the two things that matter)
1. **Runway is the concern.** The ZFS pool was **93% full (12.5 / 13.85 TB usable) as of 2026-05-20** — and that snapshot PREDATES the ADR-0034 ClickHouse-lake pivot (2026-06-05) and the current Phase-0 backfill, both of which add multi-TB. **Current usage is very likely higher, possibly near-full.** A raidz2 pool that hits ~100% during an active backfill will fail writes and wedge ingest (Phase 0 + live). **Run a live `zpool list data` / `df -h` now (see [OP]).** Biggest reversible lever: trim `/srv/history-archive` (6.95 TB → pool 93%→~43%, ZFS-snapshot-reversible for 7 days).
2. **The RAID/ZFS switch itself looks correct** — stable by-id device paths, raidz2, ZFS upgraded to 2.3.4 — but the **drive-shuffle-to-expand plan did NOT complete** (details below); the pool is the original 4-drive raidz2.

## Disk topology (which physical disks, which in the array)
4 × **Samsung MZQL27T6HBLA-00A07** 7.68 TB datacenter NVMe (PM9A3). Referenced by **stable `/dev/disk/by-id/` serials** (nvme names are NOT boot-stable — the inventory documents that the live OS mirror is `nvme1n1+nvme3n1` and `nvme0n1` is a whole-disk pool member, opposite the naive assumption).

| Drive (serial suffix) | Role | In OS mirror? | In ZFS pool? |
|---|---|---|---|
| …717048 | OS-mirror + ZFS | yes (p1 ESP / p2 swap / p3 root, mdadm RAID1) | yes (**p4** partition) |
| …717042 | OS-mirror + ZFS | yes (p1/p2/p3, mdadm RAID1) | yes (**p4** partition) |
| …716944 | ZFS only | no | yes (**whole disk**) |
| …718947 | ZFS only | no | yes (**whole disk**) |

- **All 4 drives feed the ZFS `data` raidz2 pool** (2 via `p4`, 2 whole-disk). 2 of them double as the OS mirror.
- **raidz2 = tolerates 2 drive failures.** OS mirror (2 members, both on the dual-duty drives) tolerates 1. A single dual-duty-drive failure is survivable (raidz2 loses 1 of 4 = fine; OS mirror degraded-but-alive). This is a sound layout.
- Mixed partition+whole-disk raidz2 created with `zpool create -f` → sized to the smallest member (~7.63 TB after OS slots) → ~15 TB raw-usable, ~13.85 TB after overhead, **~100 GB trailing waste on the 2 whole-disk members** (mildly wasteful, not dangerous).
- **The RAID switch was done properly** and hardened after two near-misses: (a) 2026-07-03 drift-audit caught the inventory pointing the carve task at a whole-disk pool member (boot-unstable naming) → fixed to by-id + a SAFETY gate refusing to carve any drive in `zpool status` or lacking the OS layout; (b) a zfsutils downgrade nearly bricked the pool ("one reboot from a poolless host") → r1 pinned to locally-built **OpenZFS 2.3.4** (`apt-mark hold`), ansible refuses to install packages when a zfs userspace already exists. `zfs_os_drives_needing_data_partition: []` on r1 → the carve task is inert (partitions pre-exist), so ansible can't re-partition.

## Your expansion question: did the "free a drive → add it → expand the pool" plan complete?
**Almost certainly NOT — and the repo explains why.** `storage-considerations.md:194` records this exact plan as a BLOCKED option:
> "OpenZFS 2.2.2 (current) doesn't support raidz expansion. Would need 2.3+ upgrade first. Even with that, the 12.5 TB data doesn't fit on a single drive (7.68 TB) for the 1-drive-transit pattern → requires **2-drive transit** (zero-parity window during migration)."

So the plan had two prerequisites:
1. **ZFS 2.3+ (for raidz expansion)** — ✅ DONE (upgraded to 2.3.4, 2026-05-21, the day after that doc).
2. **The actual drive-shuffle expansion** — ❌ the repo shows it as blocked/not-done: the 1-drive-transit pattern you remember can't work because 12.5 TB doesn't fit on one 7.68 TB transit drive, so it needs a **2-drive transit with a zero-parity window** (risky), and there's no evidence in the repo that this was executed.

The pool today is the **original 4-drive raidz2 at 13.85 TB usable** — i.e. all 4 physical drives are already IN the raidz2 (there is no held-out 5th drive; the box only has 4 bays). So there's nothing left to "add" unless a physically new drive is provisioned. Your memory is right that the *plan existed and its first step (the ZFS upgrade) happened*; the final expand step did not.

**Two caveats worth a live check:**
- If a raidz *expansion* (attach-to-vdev) was ever run, **old data keeps its pre-expansion parity ratio until rewritten**, so reported free space can be lower than a fresh vdev of the same width — a rewrite (send/recv or rebalance) would be needed to reclaim it. `zpool history` will show whether an expand/attach ever ran.
- Real capacity relief on a 4-bay box is: (a) **trim** (history-archive / LCM cold-tier, below), or (b) a **physically larger/additional drive** from Hetzner (then a real raidz expansion on 2.3.4, or a rebuild).

## Your compression question: the deferred compression task
It's **TimescaleDB hypertable compression**, not ClickHouse (and yes, TimescaleDB is in use — it's the served/Postgres tier). Evidence in `perf-todo.md` + the incident narrative:
- A Timescale **compression policy (job 1034)** compresses rebuilt history; during a 2026-06 heavy-load incident it "tried to compress all rebuilt history in one [pass]" and hammered the shared 93%-full ZFS pool, so **compression was DEFERRED / paused** ("compression deferred, fills paused, stacked queries cancelled"; "the 22:00 UTC compression will run"; "compression job 1034, recreate 6 indexes").
- Separately, `perf-todo.md §2`: `/v1/oracle/latest` reads one **compressed chunk `compress_hyper_11_1126_chunk`** doing a 280 ms Seq Scan (stale/missing segment-by index) — a `recompress_chunk(...)` fixes it (operator-gated; Redis cache hides it, so deferred).

**Why it matters for runway:** if job 1034 was paused with a backlog, a chunk of `trades`/`oracle`/CAGG history may still be **UNCOMPRESSED** — Timescale zstd compression is typically 5–20×, so an uncompressed backlog is directly wasted disk on a 93%-full pool. **Completing the deferred compression would reclaim space** and is lower-risk than trimming the archive. (ClickHouse lake tables are separately ZSTD-coded; a `ROADMAP #66` item mentions `OPTIMIZE FINAL` on dup partitions 25/45/62 — a different, smaller lever.)

## Reclaim levers (biggest first; all `[OP]`, all deferred by the Phase-0 freeze)
1. **Complete the deferred Timescale compression (job 1034)** — reclaim = size of the uncompressed backlog × (compression ratio − 1). Low-risk, in-place. VERIFY FIRST (below).
2. **Trim `/srv/history-archive`** (6.95 TB) — reclaim ~7.1 TB, pool 93%→~43%. ZFS-snapshot-reversible (7-day window); DR rebuild 4–10 h from SDF (ADR-0016 amendment needed). The big lever.
3. **LCM cold-tiering (ADR-0027)** — `trim-galexie-archive` moves MinIO galexie-archive (4.96 TB) to aws-public-blockchain cold. Config-gated.
4. **ClickHouse `OPTIMIZE FINAL` on dup partitions 25/45/62** (ROADMAP #66) — smaller, collapses ReplacingMergeTree duplicates.

## Dataset breakdown (2026-05-20 snapshot — verify live)
| Dataset | Mount | Used | Notes |
|---|---|---|---|
| data/archive | /srv/history-archive | 6.95 TB | SDF history mirror (bucket/ 4.2 TB + transactions/ 2.0 TB). Biggest trim target. |
| data/minio | /var/lib/minio | 4.96 TB | galexie-archive + galexie-live. LCM cold-tier candidate. |
| data/postgres | /var/lib/postgresql | 606 GB | TimescaleDB served tier (compression job 1034 applies here). |
| data/clickhouse | /var/lib/clickhouse | (grew post-2026-06-05) | The lake — NOT in the 2026-05-20 snapshot; the main current grower (Phase 0 writes here). |
| os/loki/prometheus/pgbackrest/restore-drill | various | small–moderate | all zstd; prometheus ~12×. |

All datasets already use **zstd** at the ZFS layer (good) + tuned recordsize (8K postgres, 1M galexie/minio/archive, 128K os/clickhouse/loki/prometheus).

## [OP] — live checks to run on r1 (post-Phase-0, or now read-only ones if urgent)
```sh
# 1. HEADROOM (urgent — is the pool near-full during Phase 0?)
zpool list -v data          # SIZE / ALLOC / FREE / CAP% / FRAG
df -h /var/lib/clickhouse /var/lib/postgresql /srv/history-archive /var/lib/minio
zfs list -o name,used,avail,refer,compressratio -r data

# 2. RAID/EXPANSION integrity (your question, definitively)
zpool status -v data        # all 4 members ONLINE? degraded/removed? resilver/expand in progress?
zpool history data | grep -iE 'create|add|attach|replace|expand'   # was an expansion ever run?
cat /proc/mdstat            # OS mirror (mdadm RAID1) healthy / not degraded?
zpool get version,feature@raidz_expansion data                     # expansion feature active?

# 3. Deferred compression (reclaim opportunity)
psql -c "SELECT hypertable_name, count(*) FILTER (WHERE is_compressed) comp, count(*) FILTER (WHERE NOT is_compressed) uncomp FROM timescaledb_information.chunks GROUP BY 1 ORDER BY uncomp DESC;"
psql -c "SELECT job_id, last_run_status, next_start FROM timescaledb_information.job_stats WHERE job_id = 1034;"   # did 1034 complete? backlog?
zfs get compressratio data/postgres data/clickhouse                # actual ratio achieved

# 4. Drive health (hard-to-expand box → catch a failing drive early)
for d in 717048 717042 716944 718947; do smartctl -a /dev/disk/by-id/nvme-SAMSUNG_*_S6CKNN0Y$d 2>/dev/null | grep -iE 'Percentage Used|Media.*Errors|Critical Warning|Power_On_Hours'; done
```

## Verdict
- **RAID switch: done correctly** (by-id serials, raidz2, ZFS 2.3.4, hardened against the two near-misses). Sound and fault-tolerant (survives 1 drive loss comfortably, 2 with data intact).
- **Expansion plan: not completed** — ZFS-upgrade prerequisite met, but the drive-shuffle expand step wasn't executed (blocked by the transit-fit problem), and the box is at 4/4 bays so real expansion needs new hardware.
- **Runway: the live risk.** 93%-and-growing on a hard-to-expand box during an active Phase-0 backfill. Confirm headroom live; if tight, the deferred-compression completion + a history-archive trim are the reversible relief valves — but both are `[OP]` and Phase-0-gated.
