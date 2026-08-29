---
title: Runbook — redis-replication
last_verified: 2026-08-29
status: current (alert inert on r1)
severity: P2
---

# Runbook — `stellarindex_redis_replication_broken`

> **INERT ON R1 (re-verified 2026-08-29).** r1 is a SINGLE-NODE
> Redis — no Sentinel, no replicas — so there is no replication to
> monitor. The r1 overlay (`configs/prometheus/rules.r1/cache.yml`)
> DELIBERATELY keeps the phantom expr
> `redis_connected_slaves < on(instance) redis_expected_slaves` —
> `redis_expected_slaves` has no producer anywhere (F-1329), so the
> rule can never fire on r1; it is tracked in
> `scripts/ci/lint-metric-refs.sh`'s `KNOWN_INERT` list. The
> multi-host tree (`deploy/monitoring/rules/cache.yml`) carries the
> LIVE form, `redis_connected_slaves < 2`, against the ADR-0024
> 1-primary-2-replica topology. Everything below describes that
> future multi-host shape.

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_redis_replication_broken` |
| Severity | P2 (`severity: ticket`) |
| Detected by | Deliberate expr split between the trees: `deploy/monitoring/rules/cache.yml` (group `stellarindex.cache`, `severity: ticket`, `for: 2m`) carries the live `redis_connected_slaves < 2`; the r1 overlay `configs/prometheus/rules.r1/cache.yml` keeps the never-firing `redis_expected_slaves` form so the alert stays INERT on the single-node r1 (see banner). |
| Typical MTTR | 15–45 min |
| Impact | No immediate customer impact — reads and writes continue on the master. But Sentinel needs at least one healthy replica to promote on failover; without that, **a master failure becomes a full cache outage** (`redis-master-down.md`). |

## Symptoms

- `redis_connected_slaves < 2` on the master for ≥ 2 min (the
  multi-host rule; the ADR-0024 topology expects 2 replicas per
  master — HA plan §3.4). There is no `redis_expected_slaves`
  metric anywhere (F-1329): the old symptom line quoting it
  described a series that never existed.
- `redis-cli info replication` on the master shows
  `connected_slaves:` lower than configured.
- Sentinel logs: `+sdown slave ...`.

## Quick diagnosis (≤ 5 min)

```sh
# View from the master
redis-cli -h redis-master info replication

# Each replica's own view
for shard in redis-1 redis-2; do
  echo "=== $shard ==="
  redis-cli -h $shard info replication | grep -E 'role|master_link_status|slave_read_only'
done

# Is a replica wedged on an initial sync?
redis-cli -h redis-1 info replication | grep -E 'master_sync_in_progress|master_sync_total_bytes|master_sync_left_bytes'

# Sentinel's view
redis-cli -h redis-sentinel -p 26379 sentinel replicas <mastername>
```

## Typical root causes

1. **Replica process died.** Hardware / OOM / crash. Check pod
   state + logs on the affected replica.

2. **Replica is behind the master's repl-backlog and can't catch up
   incrementally**, so it's doing a full sync — during which it's
   counted as connected but `master_link_status:down` can flap.
   - Signal: `master_sync_in_progress:1` on the replica.
   - Mitigation: wait. Full sync on a multi-GB Redis takes minutes.
     If it doesn't complete, your `repl-backlog-size` or
     `client-output-buffer-limit replica` are too small — bump
     both.

3. **Network-level flapping** between master and replica. TCP
   connection repeatedly drops.
   - Signal: master log shows repeated `Connecting to MASTER ... /
     Partial resynchronization not possible` cycles.
   - Mitigation: network diagnosis (MTU, packet loss, firewall
     between zones).

4. **Authentication drift.** After secret rotation, one replica's
   `requirepass` / `masterauth` didn't get updated.
   - Signal: replica log says `NOAUTH Authentication required`.
   - Mitigation: re-roll the replica with the correct secret.

## Mitigation

- [ ] Step 1 — identify which replica is missing and why (above).
- [ ] Step 2 — if redis-server on a replica host is down:
      `ssh root@cache-NN "systemctl restart redis-server"`.
      Sentinel detects the recovered replica and re-adds it to
      replication on its next discovery cycle.
- [ ] Step 3 — if sync is in progress: monitor
      `master_sync_left_bytes`; ETA = leftBytes / network bandwidth.
- [ ] Step 4 — if auth drift: restart the replica with the correct
      secret mounted.
- [ ] Verification: `connected_slaves` on the master returns to the
      expected count; Sentinel's replica list shows all healthy.

## Root cause analysis

- Master log around the disconnect.
- Replica log of the affected instance.
- Sentinel log across the window (sdown / odown events).
- Was there a network/firewall change deploy around the incident
  window?

## Known false-positive patterns

- **Rolling `systemctl restart redis-server` across the (future)
  cache hosts** (one-host-at-a-time via the `redis-sentinel`
  ansible role with `--limit`): a replica is briefly absent while
  its instance restarts, and the `for: 2m` threshold can trip
  during a deliberately slow rollout. Silence the alert for the
  duration of a planned maintenance.
- **New replica provisioning**: adding a third replica to a shard
  produces a period of `connected_slaves == expected - 1` while
  initial-sync completes. Expected.

## Related

- `redis-master-down.md` — the downstream consequence if a master
  fails while replication is broken.
- ADR-0024 — `docs/adr/0024-redis-ha-via-sentinel.md` (the
  1-primary-2-replica Sentinel topology this alert guards).
- HA plan §3.4 Redis topology.

## Changelog

- 2026-04-23 — initial draft.
- 2026-08-29 — re-verified against HEAD: the symptom quoted
  `redis_expected_slaves`, a metric with no producer anywhere
  (F-1329 dead alert) — replaced with the multi-host tree's
  literal `redis_connected_slaves < 2`; INERT-on-r1 banner added
  (the r1 overlay deliberately keeps the phantom expr,
  KNOWN_INERT-tracked); StatefulSet/PodDisruptionBudget
  false-positive rewritten as a rolling systemctl restart via the
  redis-sentinel role; ADR-0024 cited; dual-tree Detected-by
  noting the deliberate expr split. Status draft → current
  (alert inert on r1).
