---
title: Runbook — stellar-core and stellar-rpc node alerts
last_verified: 2026-10-06
status: living (all three alerts inert on r1)
severity: P1
---

# Runbook — stellar-core / stellar-rpc node alerts

> **Deployment posture (2026-04-30).** Neither stellar-core nor stellar-rpc is running on r1; both daemons were removed 2026-04-23 ([r1-deployment-state.md §Services](../r1-deployment-state.md)). The metrics below have no producer, so all three alerts are *inert* on r1: there are no series to evaluate. Galexie's embedded captive-core is intentionally not exposed to the prometheus exporter (the exporter scraped the standalone daemon's `/info`; it connects out for ledger replay but exposes no `/peers` endpoint).
>
> Production ingest reads Galexie's MinIO output directly via `go-stellar-sdk/ingest.ApplyLedgerMetadata` ([architecture/ingest-pipeline.md](../../architecture/ingest-pipeline.md)), i.e. Galexie -> MinIO -> `internal/ledgerstream`. stellar-rpc is preserved only for the `rpc-probe` operator diagnostic and for fixture capture in `scripts/dev/`.
>
> The alerts remain in BOTH rule trees (`configs/prometheus/rules.r1/stellar.yml`, group `stellarindex.stellar`, the file r1 loads, and the multi-host twin `deploy/monitoring/rules/stellar.yml`) for the Phase-3 Tier-1 validator rollout (ADR-0004) and any future RPC facade (none on the roadmap today); their metrics are allowlisted as `KNOWN_INERT` in `scripts/ci/lint-metric-refs.sh`. Operators bringing a validator online reactivate the core signals by re-enabling `run_stellar_core` in the ansible role and exposing `stellar-core-prometheus-exporter`. Until then, an alert from these rules reaching you on r1 is a misconfiguration (for rpc_lag, a Prometheus misconfiguration, not an upstream lag), not a real signal. These sections are *future-tense*: kept discoverable rather than deleted.

## At a glance

| Alert | Severity | Rule | Metric (no producer on r1) |
| ----- | -------- | ---- | -------------------------- |
| [`stellarindex_stellar_core_ledger_age`](#stellarindex_stellar_core_ledger_age) | P1 (page, SEV-1) | `severity: page`, `for: 2m` | `stellarindex_stellar_core_last_ledger_time_unix` |
| [`stellarindex_stellar_core_peers_low`](#stellarindex_stellar_core_peers_low) | P2 (ticket) | `severity: ticket`, `for: 5m` | `stellarindex_stellar_core_peer_count` |
| [`stellarindex_stellar_rpc_lag`](#stellarindex_stellar_rpc_lag) | P2 (`severity: ticket`) | `severity: ticket`, `for: 5m` | `stellarindex_stellar_rpc_latest_ledger_age_seconds` |

## stellarindex_stellar_core_ledger_age

Typical MTTR 10 min - 2 h. stellar-core (the Phase-3 validator) hasn't applied a ledger in > 60 s. **Ingest is unaffected**: Galexie's captive-core is an independent process and production ingest is Galexie -> MinIO -> `internal/ledgerstream`, not RPC. This alert concerns validator/quorum health and history-archive publishing.

Symptoms:

- `time() - stellarindex_stellar_core_last_ledger_time_unix > 60` for >= 2 min.
- `stellar-core-dbinfo` / `info` endpoint shows an old `current_ledger` timestamp.
- The rpc_lag alert (also inert today) fires downstream shortly after.

Quick diagnosis (<= 5 min):

```sh
# Core's own view — the admin HTTP port (11626) is localhost-only,
# so go via SSH to the validator host.
ssh root@val-01 "curl -s http://localhost:11626/info | jq"
#   Look at: status (Synced vs Syncing vs Catching up), current ledger,
#   quorum info, last close time.

# How many peers are we connected to?
ssh root@val-01 "curl -s http://localhost:11626/peers | jq '.peers | length'"

# Any catastrophic log lines?
ssh root@<val-host> "journalctl -u stellar-core -n 200 --no-pager" \
  | grep -iE 'panic|fatal|deadlock|corrupt'
```

Typical root causes:

1. **Stellar network itself is having issues.** Rare but has happened. Check SDF's status page / #stellar-core on Keybase / stellar.expert/explorer; if the whole network is halted, you wait. (F-1292, 2026-05-13: per ADR-0001, prefer stellar.expert or a public stellar-rpc endpoint for the cross-check; Horizon is not in our architecture.)
2. **We lost quorum**: too many of our configured quorum-set members are unreachable. Core refuses to close ledgers without quorum. Signal: `info` shows `quorum` section with `disagree` count high; status stuck at "Syncing".
3. **captive-core catchup stalled** (for stellar-rpc). Out of RAM, out of disk, or hit a bug mid-replay.
4. **Corruption of the core DB.** Rare but possible after an unclean shutdown. `core new-db` + catchup is the recovery path.

Mitigation:

- [ ] Step 1: network-wide or us? Cross-check via stellar.expert/explorer OR a public stellar-rpc endpoint (e.g. `curl -s https://mainnet.sorobanrpc.com -d '{"jsonrpc":"2.0","id":1,"method":"getLatestLedger"}'`). If the network is down, this is a P0 for Stellar, not for us. (F-1292, codex audit-2026-05-13: the earlier prose named SDF's Horizon, which ADR-0001 bans from our operational surfaces; replaced with the stellar.expert/stellar-rpc pair we already use elsewhere in this runbook tree.)
- [ ] Step 2: if quorum: verify our quorum set members are reachable. Update the quorum-set if a chosen validator is permanently offline.
- [ ] Step 3: if catchup stalled: check disk space + memory + logs. Restart as a last resort (losing a few minutes of progress).
- [ ] Step 4: if corruption: run `stellar-core new-db` + catchup per the upstream stellar-core operator docs (not captured in a local runbook).
- [ ] Verification: `info` status returns to "Synced"; ledger age drops below 30 s; the rpc_lag alert (if it fired; also inert today) clears on its next evaluation.

Root cause: the full `stellar-core` log for the incident window; the quorum-set config at time of incident (did someone change it recently?); network status from external sources (SDF status page, stellar.expert's ledger-age panel); hardware (was the host OOM / IOwait-pegged?).

False positives:

- **Deliberate "catch up" mode after a restart**: stellar-core intentionally lags while it replays. The alert's `for: 2m` can absorb a quick boot, but a long catchup will trip it.
- **Network genuinely slow at times**: 5-6 s ledger close times during heavy traffic can tip the alert if you set the threshold too aggressively. Current threshold is 60 s which is well past any normal variation.

## stellarindex_stellar_core_peers_low

Typical MTTR 15-60 min. stellar-core connected to < 5 peers. It may still be tracking the ledger fine, but its view of quorum is fragile: any further partition and we lose sync. Pre-emptive ticket, not an outage.

Symptoms:

- `stellarindex_stellar_core_peer_count < 5` for >= 5 min.
- `stellar-core/peers` endpoint shows < 5 connected peers.
- Dashboard: *Stellar -> peer count* sitting well below steady state (usually 20+).

Quick diagnosis (<= 5 min). When this alert un-inerts (Phase-3 Tier-1 validator rollout), stellar-core will run as the `stellar-core.service` systemd unit on each validator host (per the `archival-node` ansible role's `templates/systemd/stellar-core.service.j2` and ADR-0008). The procedure below assumes that bare-metal shape; there is no Kubernetes deployment for stellar-core anywhere in this architecture.

```sh
# Per-validator host — stellar-core's HTTP admin port is 11626 by
# default; localhost-only.
ssh root@val-01 "curl -s http://localhost:11626/peers | jq"
# Look at: total count, which peers we're connected to, any
#   "attempting" that aren't making it to "authenticated".

# Are we network-reachable? (11625 is the stellar-core P2P port.)
ssh root@val-01 "nc -zv <a-random-quorum-peer> 11625"
# Should report "succeeded", or refuse / timeout cleanly.

# Did we recently change the known-peer list? (The ansible role
# renders the config to /etc/stellar/stellar-core.cfg and its
# template defines KNOWN_PEERS — there is no PREFERRED_PEERS
# anywhere in our config.)
ssh root@val-01 "grep -A20 'KNOWN_PEERS' /etc/stellar/stellar-core.cfg"

# Is a firewall dropping our outbound?
ssh root@val-01 "journalctl -u stellar-core -n 200 --no-pager \
  | grep -iE 'connect|refused|timed out'"
```

Typical root causes:

1. **Firewall / egress change**: a new iptables / ufw rule on the validator host or the colo perimeter dropped our outbound 11625.
2. **Known-peer list drift.** The SDF / LOBSTR / Satoshipay peers we explicitly trust changed IPs or retired them. Mitigation: update `KNOWN_PEERS` in the `archival-node` role's `stellar-core.cfg.j2` template + re-apply.
3. **Large-scale network partition.** We can reach a few peers but not most. Usually correlates with BGP / upstream issues.
4. **Our own peer has been flagged for misbehaviour** by the wider network (too many invalid ledger submissions etc.) and peers are dropping us. Rare but has happened historically in the network.

Mitigation:

- [ ] Step 1: identify whether we can *reach* the wider network (`nc -zv <known-peer> 11625` from the validator host).
- [ ] Step 2: if firewall: unblock. Check the host's local rules (`iptables -L -n`, `ufw status`) and the colo perimeter egress policy.
- [ ] Step 3: if the `KNOWN_PEERS` list is stale: update the template in the `archival-node` ansible role + apply with `--limit val-XX` rolling across hosts so quorum stays up.
- [ ] Step 4: if we're the one getting dropped: check our own core logs for invalid-ledger or misbehaviour warnings, then reach out to the quorum-set operators.
- [ ] Verification: peer count climbs back to >= 10 sustained.

False positives:

- **Cold boot**: stellar-core needs a few minutes to authenticate all peers. The `for: 5m` threshold covers normal boot; a long catchup can tip it.
- **Rolling restart of our own validators**: we run three (ADR-0004); if you're restarting them in sequence, briefly each instance has fewer peers.

## stellarindex_stellar_rpc_lag

Typical MTTR 5-30 min. Historical/future-tense: in an RPC-facade deployment, consumers of that facade would lag as its `latestLedger` fell behind wall-clock. Production ingest (Galexie -> MinIO -> `internal/ledgerstream`) does not read from stellar-rpc at all and is unaffected.

Symptoms:

- `stellarindex_stellar_rpc_latest_ledger_age_seconds > 300` sustained 5 min.
- Upstream: all per-source lag signals climb together (historically the `source_lag_ledgers` gauge, deleted 2026-07-02; today: data-freshness ages across sources), the hallmark of a shared-upstream issue (vs single-source).
- API `/v1/readyz` may still return 200 (we only probe Timescale + Redis there, not the RPC).

Quick diagnosis (<= 5 min):

```sh
# Direct probe — version + latestLedger + event retention
stellarindex-ops rpc-probe http://stellar-rpc:8000

# Check the RPC's own self-reported health. It returns an
# error envelope when stale rather than HTTP error:
curl -s -XPOST http://stellar-rpc:8000 \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"getHealth"}' | jq

# Is the RPC's captive-core process alive + caught up?
# stellar-rpc ships as a systemd unit per the archival-node role.
ssh root@<host> "journalctl -u stellar-rpc -n 50 --no-pager"
```

Key signals:

- `rpc-probe` reports `health: ⚠ latency ... is too high (>30s)`: RPC's captive-core is catching up. Wait or investigate captive-core logs.
- `rpc-probe` reports connection refused / times out: RPC process is dead. Check the orchestrator / restart.
- `rpc-probe` returns `latestLedger` that equals the tip on stellar.expert: RPC is fine, something else is wrong. Go back to `source-stopped.md`.
- `rpc-probe` returns lagging `latestLedger` but health=OK: possible clock skew on the RPC host. Compare with `date` on the host.

Typical root causes:

1. **captive-core catching up after a restart.** Usually resolves on its own within 10-30 min. Stellar-core replays from the last ledger header it has; fresh boots or volume loss force a full catchup.
2. **Network partition between RPC host and peer validators.** captive-core can't advance ledgers it hasn't heard about.
3. **Host resource exhaustion.** captive-core is CPU-bound on replay + memory-bound at steady state. OOM or CPU throttle stalls it.
4. **Our own dep is behind.** The upstream stellar-rpc project ships breaking changes quarterly. Version drift shows up first as subtle lag, then total failure. `rpc-probe` prints the version; compare to the upstream's current mainline.

Mitigation:

- [ ] Step 1: identify the root cause via the probes above.
- [ ] Step 2: if captive-core catching up: wait. Inform stakeholders the API's price-freshness SLA is in a degraded window.
- [ ] Step 3: if host resource issue: scale the RPC node or restart. Keep in mind a restart triggers another catchup window.
- [ ] Step 4: if network partition: check peer connectivity, firewall rules, and core-peers metric if you have one.
- [ ] Verification: `stellarindex_stellar_rpc_latest_ledger_age_seconds` drops back under 300 s; source-stopped alerts that tracked this clear on their own within the next poll cycle.

## Related

- [core_peers_low](#stellarindex_stellar_core_peers_low) and [core_ledger_age](#stellarindex_stellar_core_ledger_age): losing peers escalates to losing sync.
- `source-stopped.md`: downstream effect when the RPC is completely unavailable vs just lagging.
- `all-ingestion-down.md`: its "RPC failure takes ALL sources down" branch is historical; production ingest no longer flows through stellar-rpc.
- `archive.md#stellarindex_stellar_archive_publish_fail`: a core_ledger_age stall can cascade into it.
- `infra.md#stellarindex_host_down`: if the host hosting stellar-core is down, peers-low is redundant (the bigger problem).
- ADR-0004 (three-validator aspiration). stellar-rpc docs: `https://developers.stellar.org/docs/data/rpc/` for operator-side recovery guidance.
