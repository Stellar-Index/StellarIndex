---
title: Runbook — core-lag
last_verified: 2026-08-29
status: current (alert inert on r1)
severity: P1
---

# Runbook — `stellarindex_stellar_core_ledger_age`

> **Deployment posture (2026-04-30).** stellar-core is **not running
> on r1** — the daemon was removed 2026-04-23
> ([r1-deployment-state.md §Services](../r1-deployment-state.md)).
> The metric `stellarindex_stellar_core_last_ledger_time_unix` has no
> producer, so this alert is *inert* on r1: there are no series to
> evaluate against. Galexie's embedded captive-core is intentionally
> not exposed to the prometheus exporter (the exporter scraped the
> standalone daemon's `/info`).
>
> The alert remains in BOTH rule trees
> (`configs/prometheus/rules.r1/stellar.yml` — the file r1 loads —
> and the multi-host twin `deploy/monitoring/rules/stellar.yml`)
> for Phase-3 (Tier-1 validator rollout, ADR-0004); both underlying
> metrics are allowlisted as `KNOWN_INERT` in
> `scripts/ci/lint-metric-refs.sh`. Operators bringing a validator
> online will reactivate this signal by re-enabling
> `run_stellar_core` in the ansible role and exposing
> `stellar-core-prometheus-exporter`. Until then this runbook is
> *future-tense*: keep it discoverable rather than delete it.

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_stellar_core_ledger_age` |
| Severity | P1 (page — SEV-1) |
| Detected by | `configs/prometheus/rules.r1/stellar.yml` (group `stellarindex.stellar`; `severity: page`, `for: 2m`) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/stellar.yml`. **Inert on r1**: `stellarindex_stellar_core_last_ledger_time_unix` has no producer (`KNOWN_INERT` in `scripts/ci/lint-metric-refs.sh`). |
| Typical MTTR | 10 min – 2 h |
| Impact | stellar-core (the Phase-3 validator) hasn't applied a ledger in > 60 s. **Ingest is unaffected** — Galexie's captive-core is an independent process and production ingest is Galexie → MinIO → `internal/ledgerstream`, not RPC. This alert concerns validator/quorum health and history-archive publishing. |

## Symptoms

- `time() - stellarindex_stellar_core_last_ledger_time_unix > 60`
  for ≥ 2 min.
- `stellar-core-dbinfo` / `info` endpoint shows an old `current_ledger`
  timestamp.
- `rpc-lag.md` (also inert today) fires downstream shortly after.

## Quick diagnosis (≤ 5 min)

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

## Typical root causes

1. **Stellar network itself is having issues**. Rare but has
   happened. Check SDF's status page / #stellar-core on Keybase /
   stellar.expert/explorer — if the whole network is halted, you
   wait. (F-1292, 2026-05-13: per ADR-0001, prefer stellar.expert
   or a public stellar-rpc endpoint for the cross-check;
   Horizon is not in our architecture.)

2. **We lost quorum** — too many of our configured quorum-set
   members are unreachable. Core refuses to close ledgers without
   quorum.
   - Signal: `info` shows `quorum` section with `disagree` count
     high; status stuck at "Syncing".

3. **captive-core catchup stalled** (for stellar-rpc). Out of RAM,
   out of disk, or hit a bug mid-replay.

4. **Corruption of the core DB**. Rare but possible after an
   unclean shutdown. `core new-db` + catchup is the recovery path.

## Mitigation

- [ ] Step 1 — network-wide or us? Cross-check via
      stellar.expert/explorer OR a public stellar-rpc endpoint
      (e.g. `curl -s https://mainnet.sorobanrpc.com -d '{"jsonrpc":"2.0","id":1,"method":"getLatestLedger"}'`).
      If the network is down, this is a P0 for Stellar, not for
      us. F-1292 (codex audit-2026-05-13): the earlier prose
      named SDF's Horizon, which ADR-0001 bans from our
      operational surfaces — replaced with the
      stellar.expert/stellar-rpc pair we already use elsewhere
      in this runbook tree.
- [ ] Step 2 — if quorum: verify our quorum set members are
      reachable. Update the quorum-set if a chosen validator is
      permanently offline.
- [ ] Step 3 — if catchup stalled: check disk space + memory +
      logs. Restart as a last resort (losing a few minutes of
      progress).
- [ ] Step 4 — if corruption: run `stellar-core new-db` + catchup
      per the upstream stellar-core operator docs (not captured in
      a local runbook).
- [ ] Verification: `info` status returns to "Synced"; ledger age
      drops below 30 s; `rpc-lag.md` (if it fired — also inert
      today) clears on its next evaluation.

## Root cause analysis

- Full `stellar-core` log for the incident window.
- Quorum-set config at time of incident (did someone change it
  recently?).
- Network status from external sources (SDF status page,
  stellar.expert's ledger-age panel).
- Hardware: was the host OOM / IOwait-pegged?

## Known false-positive patterns

- **Deliberate "catch up" mode after a restart** — stellar-core
  intentionally lags while it replays. The alert's `for: 2m` can
  absorb a quick boot, but a long catchup will trip it.
- **Network genuinely slow at times** — 5–6 s ledger close times
  during heavy traffic can tip the alert if you set the threshold
  too aggressively. Current threshold is 60 s which is well past
  any normal variation.

## Related

- `core-peers.md` — the "we're being cut off" variant.
- `rpc-lag.md` — downstream effect (also inert today).
- `archive-publish.md` — can cascade.

## Changelog

- 2026-08-29 — re-verified against HEAD. Detected-by row converted
  to the dual-tree convention (`rules.r1/stellar.yml`, group
  `stellarindex.stellar`, `severity: page`, `for: 2m`; both metrics
  `KNOWN_INERT` in lint-metric-refs.sh). Impact rewritten: the old
  "captive-core for stellar-rpc also stalls / source events via
  RPC stop" claim described the pre-2026-04-23 architecture —
  production ingest is Galexie → MinIO → ledgerstream and is
  unaffected by validator core lag; this alert concerns
  validator/quorum health + archive publishing only. Diagnosis
  curls converted to the `ssh root@val-01` shape (admin HTTP is
  localhost-only), matching core-peers.md. `rpc-lag.md` cross-refs
  qualified "(also inert today)". The `core new-db` recovery
  pointer at bootstrap-archival-node.md removed — that runbook
  never captured it; repointed at upstream stellar-core docs.

- 2026-04-23 — initial draft.
- 2026-04-30 — top-of-file deployment-posture callout: this alert
  is inert on r1 (stellar-core removed 2026-04-23) and is retained
  for Phase-3 validator rollout. Avoids on-call confusion when the
  runbook is opened from an unrelated incident.
