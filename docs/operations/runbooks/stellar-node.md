---
title: Runbook — stellar-core and stellar-rpc node alerts
last_verified: 2026-10-06
status: living (all three alerts inert on r1)
severity: P1
---

# Runbook — stellar-core / stellar-rpc node alerts

Neither stellar-core nor stellar-rpc runs standalone on r1 ([r1-deployment-state.md](../r1-deployment-state.md)); the three node alerts below have no producer and are inert there (`KNOWN_INERT` in `scripts/ci/lint-metric-refs.sh`). Production ingest is Galexie -> MinIO -> `internal/ledgerstream` ([ingest-pipeline.md](../../architecture/ingest-pipeline.md)); stellar-rpc survives only for `rpc-probe` and fixture capture. The rules stay in `configs/prometheus/rules.r1/stellar.yml` and `deploy/monitoring/rules/stellar.yml` for the Phase-3 validator rollout (ADR-0004). One of these alerts reaching you on r1 is a misconfiguration, not a real signal. `stellar-stack-version-lag` is live.

## At a glance

| Alert | Severity | Metric |
| ----- | -------- | ------ |
| [`stellarindex_stellar_core_ledger_age`](#stellarindex_stellar_core_ledger_age) | P1 (page), `for: 2m` | `stellarindex_stellar_core_last_ledger_time_unix` |
| [`stellarindex_stellar_core_peers_low`](#stellarindex_stellar_core_peers_low) | P2 (ticket), `for: 5m` | `stellarindex_stellar_core_peer_count` |
| [`stellarindex_stellar_rpc_lag`](#stellarindex_stellar_rpc_lag) | P2 (ticket), `for: 5m` | `stellarindex_stellar_rpc_latest_ledger_age_seconds` |
| [`stellar-stack-version-lag`](#stellar-stack-version-lag) | P3 -> P1 | version-probe textfile |

## stellarindex_stellar_core_ledger_age

`time() - stellarindex_stellar_core_last_ledger_time_unix > 60` for 2 min: the validator has applied no ledger. Ingest is unaffected (Galexie's captive-core is independent).

**Check**

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

**Likely causes**

- Network-wide halt: cross-check stellar.expert/explorer or `curl -s https://mainnet.sorobanrpc.com -d '{"jsonrpc":"2.0","id":1,"method":"getLatestLedger"}'` (never Horizon, ADR-0001). A network halt is not ours to fix.
- Lost quorum: `info` shows a high `disagree` count, status stuck at "Syncing".
- Catchup stalled (RAM, disk) or a deliberate post-restart catchup.
- Core DB corruption after an unclean shutdown.

**Fix**

1. Quorum: verify quorum-set members are reachable; update the quorum set if one is permanently offline.
2. Catchup stalled: check disk, memory and logs; restart as a last resort.
3. Corruption: `stellar-core new-db` plus catchup per the upstream operator docs.
4. Verify `info` returns to "Synced" and ledger age is under 30 s.

**Escalate** Capture the core log for the window and the quorum-set config.

## stellarindex_stellar_core_peers_low

`stellarindex_stellar_core_peer_count < 5` for 5 min. Pre-emptive ticket: tracking may be fine, but quorum view is fragile. Core runs as `stellar-core.service` (archival-node role, ADR-0008); no Kubernetes.

**Check**

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

**Likely causes**

- Firewall or egress change dropping outbound 11625.
- Stale `KNOWN_PEERS` (peers changed IPs or retired).
- Upstream network partition.
- Our peer flagged for misbehaviour (rare).

**Fix**

1. Confirm reachability (`nc -zv <known-peer> 11625`). Firewall: check `iptables -L -n`, `ufw status` and the colo egress policy.
2. Stale peers: update `KNOWN_PEERS` in the role's `stellar-core.cfg.j2` and apply with `--limit val-XX`, rolling, so quorum stays up.
3. Being dropped: look for invalid-ledger warnings in our core log, then contact quorum-set operators.
4. Verify peer count sustains >= 10. A cold boot or rolling restart briefly lowers it.

**Escalate** If the host is down, see `infra.md#stellarindex_host_down`.

## stellarindex_stellar_rpc_lag

`stellarindex_stellar_rpc_latest_ledger_age_seconds > 300` for 5 min. Only meaningful for an RPC facade; production ingest does not read stellar-rpc.

**Check**

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

**Likely causes**

- `rpc-probe` health `latency ... is too high (>30s)`: captive-core catching up after a restart (10-30 min).
- Connection refused or timeout: RPC process dead.
- `latestLedger` lagging with health OK: clock skew on the RPC host (compare `date`).
- `latestLedger` equal to the tip: RPC is fine; go to `ingestion.md#stellarindex_ingestion_source_stopped`.

**Fix**

1. Catching up: wait.
2. Resource exhaustion (CPU/RAM): scale or restart (a restart triggers another catchup).
3. Partition: check peer connectivity and firewall.
4. Verify the age drops under 300 s. Version drift against upstream shows in the `rpc-probe` version.

## stellar-stack-version-lag

Alerts, rules in `deploy/monitoring/rules/stellar-stack-version.yml` (and `configs/prometheus/rules.r1/`):

- `stellarindex_stellar_stack_lagging` (P3, ticket): lag >= 1 for 2 d.
- `stellarindex_stellar_stack_protocol_lag` (P1, page): lag >= 2 for 6 h. A full protocol major behind: the next protocol activation vote will freeze or crash-loop the lagging component.
- `stellarindex_stellar_stack_probe_degraded` (P3, ticket): the probe itself cannot answer.

Metric source: `/var/lib/node_exporter/textfile_collector/stellar_stack_version.prom`, written daily by `stellar-stack-version-probe.timer` -> `/usr/local/sbin/stellar-stack-version-probe.sh`. Components via the `component` label: `core` (stellar-core), `galexie` (stellar-galexie), `archivist` (stellar-archivist). Clearing it means the coordinated-upgrade checklist below, a deliberate maintenance-window operation.

**Check**

```sh
# 1. See exactly which component(s) are lagging + by how much, and
#    the raw installed version string for each.
ssh r1 'grep "^stellarindex_stellar_stack_" \
  /var/lib/node_exporter/textfile_collector/stellar_stack_version.prom'

# 2. Confirm the probe itself is healthy (ran recently, wrote fresh
#    output) rather than serving a stale value from a broken timer.
ssh r1 'systemctl status stellar-stack-version-probe.timer'
ssh r1 'systemctl status stellar-stack-version-probe.service'
ssh r1 'stat /var/lib/node_exporter/textfile_collector/stellar_stack_version.prom'

# 3. Cross-check against the live upstream state directly.
dpkg-query -W -f='${Version}\n' stellar-core stellar-archivist   # (run ON r1)
apt-cache policy stellar-core stellar-archivist                  # (run ON r1)
curl -s https://api.github.com/repos/stellar/stellar-galexie/releases/latest | jq -r '.tag_name, .published_at'
/usr/local/bin/galexie version                                   # (run ON r1)
```

| What fired | Action |
| ---------- | ------ |
| `stellarindex_stellar_stack_lagging` only, lag=1 | Same-protocol patch release: upgrade in ordinary maintenance. |
| `stellarindex_stellar_stack_protocol_lag`, lag=2 | New protocol major: P1. Find the announced activation date (stellar.org, SDF announcements) and schedule the upgrade window now, well before it. |
| `stellarindex_stellar_stack_probe_success == 0`, no lag change | Probe degraded: `ssh r1 'sudo systemctl start stellar-stack-version-probe.service'`, re-check the textfile. |

**Upgrade checklist.** Do all applicable components in ONE window; upgrading only one just moves the incident.

1. Confirm the target protocol version and its activation date.
2. stellar-core (apt). `stellar-core-auto-upgrade.timer` normally does this within 6 h and records `stellarindex_stellar_core_autoupgrade_result`; run by hand only when that alert fired, the host is pinned, or the stop file is set.
   ```sh
   ssh r1
   sudo apt-get update
   apt-cache policy stellar-core   # confirm the candidate is the target version
   sudo systemctl stop galexie     # captive-core will restart cleanly once core is upgraded
   sudo apt-get install --only-upgrade stellar-core
   sudo systemctl start galexie
   ```
   Then update any `stellar-core` version reference in `configs/ansible/roles/archival-node/tasks/06-stellar-core.yml` comments and `VERSIONS.md` in the same PR.
3. stellar-galexie (build from tag; no apt package; see `configs/ansible/roles/archival-node/tasks/07-galexie.yml`):
   ```sh
   # On r1, or any build host with the same Go toolchain:
   git clone https://github.com/stellar/stellar-galexie.git
   cd stellar-galexie
   git checkout galexie-vNN.N.N        # the target tag
   CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
     go build -trimpath -o galexie .
   # Verify before installing:
   ./galexie version

   # Install with a rollback backup:
   sudo cp /usr/local/bin/galexie /usr/local/bin/galexie.prev
   sudo systemctl stop galexie
   sudo install -o root -g root -m 0755 galexie /usr/local/bin/galexie
   sudo systemctl start galexie
   sudo systemctl status galexie      # confirm it's running + past initial catchup
   /usr/local/bin/galexie version     # record the new pseudo-version string
   ```
   Codify in the SAME PR: in `configs/ansible/roles/archival-node/defaults/main.yml` bump `galexie_version` AND `galexie_expected_version_string` (the exact `galexie version` output; the drift-guard assert and this probe compare it); update the `stellar/stellar-galexie` row in `VERSIONS.md`; then `ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml --tags galexie --check --diff` must show zero diff.
4. stellar-archivist (apt; no daemon, invoked on demand by `stellarindex-ops verify-archive -tier archivist`):
   ```sh
   ssh r1
   sudo apt-get update
   sudo apt-get install --only-upgrade stellar-archivist
   ```
5. go-stellar-sdk: if the upgrade changed XDR shapes our decoders touch, bump `github.com/stellar/go-stellar-sdk` in `go.mod` through a normal PR (land it BEFORE the operator-side upgrades if the new shapes are unparseable without it), run `go build ./...` and the unit suite, and audit `internal/scval`.
6. Verify: `sudo systemctl start stellar-stack-version-probe.service` on r1; every upgraded component's `stellarindex_stellar_stack_version_lag` reads 0 (briefly absent if GitHub rate-limits the re-check). Watch `stellarindex_source_decode_errors_total` and the completeness verdict (`data-freshness.md#stellarindex_completeness_incomplete`) for an hour.

The galexie comparison is date-based: `galexie version` reports a Go pseudo-version, never the release tag, so the probe compares the embedded commit date with the release's target-commit date. Logic is commented in `stellar-stack-version-probe.sh`.

**`stellarindex_stellar_stack_probe_degraded`** is not a lag: the two lag alerts are evaluating over absent or frozen series and cannot fire. Arms:

- `stellarindex_stellar_stack_probe_success` 0: degraded run (GitHub releases API unreachable or rate-limited, `jq` missing, or apt with no candidate). One day is usually transient.
- `node_textfile_mtime_seconds` for `stellar_stack_version.prom` older than 48 h: probe or timer not running.
- `probe_success` absent > 3 d: the textfile was never written on this host.

Run the Check block, then re-run the probe by hand.

**Likely causes (false positives)**

- `probe_success == 0` with stale lag: the probe's unauthenticated GitHub API limit (60 requests/hour per source IP) was hit; self-heals the next day. Escalate only if it stays 0 for several days.
- `core` or `archivist` lag absent: `apt-cache policy` found no candidate, usually because `apt.stellar.org` is not configured (`06-stellar-core.yml` is gated behind `run_stellar_core`, false on r1; known gap, see [r1-ansible-drift-2026-07-03.md](../r1-ansible-drift-2026-07-03.md)).
- `galexie` lag flipping 1<->2 without an upgrade: the major-gap heuristic looks at the two most recent upstream releases; trust the date gap (`stellarindex_stellar_stack_installed_info` vs the releases page).

## Related

- `ingestion.md#stellarindex_ingestion_source_stopped`, `archive.md#stellarindex_stellar_archive_publish_fail`, `infra.md#stellarindex_host_down`, [galexie catchup refused](archive.md#stellarindex_galexie_catchup_refused).
- `configs/ansible/roles/archival-node/tasks/07-galexie.yml`, `configs/ansible/roles/archival-node/defaults/main.yml`, `VERSIONS.md`.
