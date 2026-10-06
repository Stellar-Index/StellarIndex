---
title: Runbook — ingestion
last_verified: 2026-10-06
status: living
severity: P1
---

# Runbook — ingestion alerts

Ingestion pipeline alerts: all sources stopped, per-source stopped, decode errors, decoder panics, retired lag alert, ledgerstream tier misses, discovery drops and dispatcher tx skips. Merged from eight former pages; each `##` section keeps one page's content under its alert name.

## At a glance

- [`stellarindex_ingestion_all_sources_stopped`](#stellarindex_ingestion_all_sources_stopped)
- [`stellarindex_ingestion_source_stopped`](#stellarindex_ingestion_source_stopped)
- [`stellarindex_ingestion_decode_error`](#stellarindex_ingestion_decode_error)
- [`stellarindex_decoder_panicked`](#stellarindex_decoder_panicked)
- [`stellarindex_ingestion_lag_high`](#stellarindex_ingestion_lag_high)
- [`stellarindex_ledgerstream_tier_both_missing`](#stellarindex_ledgerstream_tier_both_missing)
- [`stellarindex_ingestion_discovery_drops`](#stellarindex_ingestion_discovery_drops)
- [`stellarindex_ingestion_dispatcher_tx_skips`](#stellarindex_ingestion_dispatcher_tx_skips)

## stellarindex_ingestion_all_sources_stopped

_Source page `ingestion.md#stellarindex_ingestion_all_sources_stopped`: status ratified, severity P1, last verified 2026-08-28._

### At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_ingestion_all_sources_stopped` |
| Severity | **P1** (SEV-1) |
| Detected by | `sum(rate(stellarindex_source_events_total[5m]))` = 0 for > 3 min |
| Typical MTTR | 5–20 min depending on root cause |
| Impact | Price staleness begins at the 60 s cache TTL; API sets `stale_flag=true` globally. If the outage lasts > 30 min we breach the Freighter 30 s freshness SLA. |

### Symptoms

- Alert `stellarindex_ingestion_all_sources_stopped` fires.
- `stellarindex_api_price_stale` (ticket) follows ~7 min later
  (rule: `stellarindex_price_staleness_seconds > 120` with `for: 5m`,
  `configs/prometheus/rules.r1/api.yml`).
- Indexer logs show no activity, repeated MinIO read errors, or
  Galexie producing no fresh objects in `galexie-live`.

> The legacy `stellarindex_ingestion_lag_high` companion alert was
> retired with the move off the orchestrator topology
> ([alerts-catalog.md](../alerts-catalog.md) §Ingestion historical
> note); don't expect to see it fire today.

### Quick diagnosis (≤ 5 min)

> **Architecture reminder.** Production ingest reads Galexie's MinIO
> output directly via `go-stellar-sdk/ingest.ApplyLedgerMetadata`
> ([architecture/ingest-pipeline.md](../../architecture/ingest-pipeline.md));
> stellar-rpc was removed from r1 on 2026-04-23 and is no longer in
> the data path. The "shared upstream" is now Galexie + MinIO, not
> stellar-rpc. Sections C / D of the legacy "stellar-rpc is the
> problem" branch below are retained as future-tense for any
> deployment that still routes through RPC; on r1 today, jump to
> Galexie / MinIO checks first.

The "all sources down" shape usually means one of three common roots: Galexie's MinIO output (the shared upstream), the shared storage (Timescale), or the indexer process itself.

```sh
# 1. Is the indexer running?
systemctl status stellarindex-indexer      # systemd-managed binary on r1

# 2. What do its logs say?
journalctl -u stellarindex-indexer -n 200 --no-pager | tail -40
# Look for: "ledgerstream exited with error" (cmd/stellarindex-indexer/main.go),
# "insert trade failed" (internal/pipeline/sink.go),
# "postgres ping failed 3x — pool may be wedged" (main.go), and the
# ledgerstream tier-read outcome "both_missing" (see
# ingestion.md#stellarindex_ledgerstream_tier_both_missing).

# 3. Is Galexie producing fresh ledger objects?
sudo journalctl -u galexie -n 50 --no-pager
mc ls --json --recursive local/galexie-live/ \
  | jq -r 'select(.key|test("\\.xdr\\.zst$")) | .lastModified' | sort -r | head -1
# newest ledger object; one lands every ~5 s (galexie_ledgers_per_file: 1).
# The mc alias on r1 is `local` (roles/archival-node/tasks/09-minio.yml);
# a non-recursive `mc ls` only lists partition prefixes, not freshness.
# OR if you suspect a network-state issue, query upstream directly
# (r1 has no local stellar-rpc; point at a public endpoint):
stellarindex-ops rpc-probe https://mainnet.sorobanrpc.com
# Expect: version info + latest ledger close time within 60s.

# 4. Is Timescale reachable + writable?
# Postgres is local to r1 (127.0.0.1); use peer auth as the postgres OS user.
sudo -u postgres psql -d stellarindex -c "INSERT INTO ingestion_cursors (source, sub_source, last_ledger) VALUES ('probe', 'healthcheck', 0) ON CONFLICT DO NOTHING;"
sudo -u postgres psql -d stellarindex -c "DELETE FROM ingestion_cursors WHERE source='probe';"  # remove the probe row (it otherwise shows in `stellarindex-ops list-cursors`)
```

Route by the result:

- Galexie isn't producing fresh objects in `galexie-live` → galexie's captive-core stalled or upstream network issue. Check `journalctl -u galexie`; if galexie itself is healthy, fall back to the public-rpc probe to confirm the network is closing ledgers.
- Galexie healthy + fresh objects in MinIO but indexer not reading → networking issue between indexer and MinIO, or indexer's MinIO credentials / endpoint config wrong. Check firewall, DNS, the `[storage]` section of `/etc/stellarindex.toml` (`s3_endpoint`, `s3_bucket_live`) and `STELLARINDEX_S3_ACCESS_KEY` / `STELLARINDEX_S3_SECRET_KEY` in `/etc/default/stellarindex`.
- psql INSERT fails → Timescale issue. Jump to [timescale-primary-down](postgres.md#stellarindex_timescale_primary_down).
- All probes pass but indexer produces no events → the indexer is alive but wedged. Likely deadlock or internal bug.

### Mitigation (≤ 15 min)

#### A. Galexie / MinIO is the upstream problem

- Confirm galexie itself is healthy: `systemctl status galexie`,
  `journalctl -u galexie --since="10 min ago"`. galexie embeds its
  own captive-core; recoverable hangs typically clear with a
  service restart. Check disk pressure on `/var/lib/galexie` (ZFS dataset): `df -h /var/lib/galexie`.
- Confirm fresh objects are landing in MinIO (same command as
  step 3 above; `mc ls --json --recursive local/galexie-live/ | jq …`).
  The newest object should be within ~1 minute. If MinIO itself is
  unhealthy: `systemctl status minio`; MinIO exporter-down →
  [exporter-down](meta.md#stellarindex_redis_exporter_down); MinIO scrape 403 →
  [minio-metrics-403](infra.md#minio-metrics-403). There is no
  minio-down runbook yet.
- Wider network problem? `stellarindex-ops rpc-probe https://mainnet.sorobanrpc.com`
  confirms ledgers are still closing on the network — if the
  network is fine but galexie has stalled, capture logs and
  restart galexie.
- *Future-tense:* if a deployment routes through stellar-rpc
  rather than direct-MinIO ingest, [rpc-lag](stellar-node.md#stellarindex_stellar_rpc_lag) covers
  that path. r1 does not run stellar-rpc as of 2026-04-23.

#### B. Timescale is the problem

- Proceed to [timescale-primary-down](postgres.md#stellarindex_timescale_primary_down).

#### C. Indexer itself is wedged

- Capture a goroutine dump before restarting:
  ```sh
  # NOTE: `pgrep stellarindex-indexer` (no -f) matches nothing — the
  # kernel comm is truncated to 15 chars. Signal via systemd instead.
  systemctl kill --signal=SIGQUIT stellarindex-indexer.service   # Go default SIGQUIT handler: goroutine dump to journal, then exit(2)
  sleep 3
  journalctl -u stellarindex-indexer -n 300 --no-pager > /tmp/indexer-dump-$(date +%s).log
  ```
  The unit has `Restart=on-failure` / `RestartSec=10s`, so it
  self-restarts ~10 s after the dump; the explicit restart below is
  only needed if it did not come back (`systemctl is-active
  stellarindex-indexer`).
- Restart the indexer:
  ```sh
  systemctl restart stellarindex-indexer
  ```
- Confirm recovery:
  - `stellarindex_source_events_total` rate > 0 within 60 s.
  - Alert clears within 3 min.

#### D. Recent deploy broke the indexer

- Check deploy history: last 4 h.
  ```sh
  # Tag history of releases — SemVer vX.Y.Z (e.g. v0.47.2)
  git tag --sort=-creatordate | head -10
  # Running versions on r1 are the on-host sidecars (source of record;
  # see docs/operations/deployed-versions.md):
  ssh root@r1 'for f in /var/lib/stellarindex/deployed-versions/stellarindex-*; do echo "$f: $(cat $f)"; done'
  ssh root@r1 'ls -lh /usr/local/bin/stellarindex-indexer.prev-*'
  ```
- **Revert** to the previous release per
  [`release-process.md`](../release-process.md) §"Rollback".
  The indexer ships as a systemd-managed binary, not a
  containerised service — so the revert is a single binary
  swap. F-1222 (codex audit-2026-05-12): the deploy task keeps
  the last 5 previous binaries as
  `/usr/local/bin/<binary>.prev-<previous-tag>` and records
  the running version under
  `/var/lib/stellarindex/deployed-versions/<binary>` (NOT under
  `/opt/stellarindex/release-<tag>/` — the `goreleaser`-style
  release archive doesn't exist on this host).

  The preferred path is to trigger the deploy workflow with
  the previous tag (`gh workflow run deploy.yml -f region=r1
  -f version=<previous> -f binaries=stellarindex-indexer`, or
  omit `binaries` to roll the whole managed set back); for the
  manual fallback:
  ```sh
  # r1 is a single host (inventory: one entry in the `archival_nodes` group).
  PREVIOUS=v0.47.1                         # whichever tag was healthy
  ssh root@r1 <<EOF
    set -euxo pipefail
    test -f /usr/local/bin/stellarindex-indexer.prev-${PREVIOUS}
    systemctl stop stellarindex-indexer
    install -m 0755 /usr/local/bin/stellarindex-indexer.prev-${PREVIOUS} /usr/local/bin/stellarindex-indexer
    echo ${PREVIOUS} > /var/lib/stellarindex/deployed-versions/stellarindex-indexer
    systemctl start stellarindex-indexer
    systemctl status stellarindex-indexer --no-pager
  EOF
  # If the wanted .prev-<tag> is older than 5 releases
  # back the operator rebuilds it from the tag on a build host
  # (`git checkout <tag> && make build`) — see
  # release-process.md §Rollback for the full recipe.
  ```
  Rolling back ONLY the indexer leaves the other managed binaries
  on the newer tag, which trips `stellarindex_binary_version_skew`
  (ticket, `for: 45m`) — expected; ack it or roll back the full set.
  See [binary-version-skew](binary-version-skew.md).

  Then file a SEV-2 minimum + a postmortem in
  `docs/operations/postmortems/` per release-process.md §Post-flight
  (postmortem requirement) / §Post-rollback.
- After revert, re-run diagnostics in step C.

### Root cause analysis

Gather:

- Goroutine dump from step C.
- Indexer logs `journalctl -u stellarindex-indexer --since "30 min ago"`.
- Grafana screenshots of `stellarindex_source_events_total` broken down by source — does it cliff-edge at a specific timestamp, or decay?
- Recent deploys — git log of `cmd/stellarindex-indexer/` in the last 72 h.
- Postgres `pg_stat_activity` during the window — were inserts blocked on locks?
- Galexie / MinIO state during the window: `journalctl -u galexie`, `journalctl -u minio`, and `stellarindex_ledgerstream_tier_read_total{outcome="both_missing"}`.

Patterns observed:

1. **Shared upstream down** — Galexie/MinIO live export stalled (captive-core hang, MinIO down, or bucket credentials). Mitigation: restart galexie / minio per §A; `stellarindex-ops rpc-probe` against a public endpoint tells you whether the network itself is closing ledgers.
2. **Shared storage backpressure** — Timescale insert latency spiked; the indexer's `events` channel filled and `ProcessLedger` blocked on the send. This backpressure is by design: the indexer's `ledgerstream` cursor advances once a ledger's events are enqueued (ADR-0041), so dropping queued on-chain events to keep moving would leave holes that the next restart resumes past (cursor+1) and that only the completeness verdict / gap detector would surface. Mitigation: fix the Postgres side (insert latency, lock waits in `pg_stat_activity`) per [trade-insert-backpressure](ingestion-sink.md#stellarindex_ingestion_trade_insert_backpressure). Only external CEX/FX trades (no cursor, vendor-refillable) drop-oldest, from a bounded retry buffer: watch `stellarindex_trade_insert_buffer_depth` and `stellarindex_source_insert_errors_total{kind="dropped"}` — that counter also counts on-chain rows isolated as permanent data faults and events abandoned at shutdown, so split it by `source` before reading a non-zero value as CEX/FX loss.
3. **Config-change caused source registry to be empty** — `ingestion.enabled_sources` accidentally set to `[]`. Note: `internal/config/validate.go` rejects empty *entries* in `enabled_sources` and unknown names, but an empty list `[]` is still accepted — check the effective config with `stellarindex-indexer -config /etc/stellarindex.toml` startup logs.
4. **Panic in a source** — bad decoder blows up one goroutine + the watch goroutine waits forever. Mitigation: defer-recover in the dispatcher / pipeline stages; the unit's `Restart=on-failure` covers a crash (the per-source orchestrator was retired 2026-04-23).

### Known false-positive patterns

- **Ledger range not yet in the live bucket** — galexie is behind (cold catchup after a restart takes ~9 min on mainnet) so the reader sees `both_missing` while it waits. The indexer correctly stops producing trade events until objects appear. Alert can fire spuriously. The staleness-age metrics `stellarindex_source_last_event_unix` / `stellarindex_source_last_insert_unix` (internal/obs/metrics.go) exist and already feed `stellarindex_ingestion_source_insert_stale`; this alert still uses the raw rate.
- **Midnight UTC continuous-aggregate refresh** — the aggregator's heavy CAGG refresh briefly blocks trade inserts. Indexer queues up, then drains. Alert might fire at the window if duration is short. Tune `for: 3m → for: 5m` if this recurs.

### Changelog

- 2026-04-22 — initial draft. the maintainer.
- 2026-04-30 — quick-diagnosis + Mitigation A rewritten around
  Galexie + MinIO (the actual r1 upstream); rpc-probe URL points
  at a public stellar-rpc since r1 doesn't run its own
  (removed 2026-04-23). Symptoms drop the retired
  `stellarindex_ingestion_lag_high` reference.
- 2026-08-28 — re-verified against HEAD: SIGQUIT via `systemctl kill`
  (bare `pgrep` never matches the truncated comm); psql via local
  peer auth (no `db-primary.internal`); `mc` alias `local` +
  recursive freshness listing; `[storage]` not `[ledgerstream]`;
  price_stale lag ~7 min; SemVer tags + deployed-versions sidecars
  replace CalVer / `r1-deployment-state.md`; single-host `root@r1`
  rollback writes the sidecar and expects `binary_version_skew`;
  orchestrator/stellar-rpc patterns rewritten for Galexie+MinIO.

## stellarindex_ingestion_source_stopped

**Runbook — the `stellarindex_ingestion_source_stopped*` family**

_Source page `ingestion.md#stellarindex_ingestion_source_stopped`: status draft, severity P2, last verified 2026-08-29._

### At a glance

| Field | Value |
| ----- | ----- |
| Alerts (four route here) | `stellarindex_ingestion_source_stopped` — 30 m rate / `for: 15m`, **high-volume allowlist only**<br>`stellarindex_ingestion_source_stopped_low_volume_dex` — 24 h rate / `for: 30m`<br>`stellarindex_ingestion_source_stopped_daily_publisher` — 30 h rate / `for: 1h` (`ecb`)<br>`stellarindex_ingestion_source_stopped_hourly_publisher` — 6 h rate / `for: 1h` (`band`) |
| Severity | P2 (`severity: ticket`) for all three |
| Detected by | `configs/prometheus/rules.r1/ingestion.yml` (the overlay r1 actually loads); multi-host template: `deploy/monitoring/rules/ingestion.yml`. Both trees carry the same exprs. |
| Typical MTTR | 15–60 min |
| Impact | One configured source has stopped producing events for longer than its own cadence budget. API clients querying that pair see price staleness creep up. If multiple sources stop, escalate to `ingestion.md#stellarindex_ingestion_all_sources_stopped` (P1). |

### Symptoms

**Read the alert NAME first — there is no single 30-minute window.**
F-1208 split the original universal-30m rule into three per-cadence
rules that share this runbook, because a rule tuned for Binance
false-positived every quiet afternoon on Phoenix and every day on
ECB. Each rule carries an explicit source allowlist; a source that
appears in none of the three is covered only by the fleet-level
`stellarindex_ingestion_all_sources_stopped`.

| Alert | Sources in its allowlist | Fires when | Sustain |
| ----- | ------------------------ | ---------- | ------- |
| `..._source_stopped` | `binance`, `bitstamp`, `coinbase`, `kraken`, `sdex`, `aquarius`, `reflector-dex`, `reflector-cex`, `reflector-fx`, `redstone`, `coingecko` | `rate(...[30m]) == 0` | 15 m |
| `..._source_stopped_low_volume_dex` | `comet`, `phoenix`, `soroswap`, `blend` | `rate(...[24h]) == 0` | 30 m |
| `..._source_stopped_daily_publisher` | `ecb` | `rate(...[30h]) == 0` | 1 h |
| `..._source_stopped_hourly_publisher` | `band` | `rate(...[6h]) == 0` | 1 h |

All four additionally require `stellarindex_source_enabled == 1`
joined `on (source)`, so a deliberately-disabled source stays quiet.

- Dashboard: *Ingestion → Events per source* panel shows a flat line for the offending source while other sources are still producing.

Total-outage coverage is the separate
`stellarindex_ingestion_all_sources_stopped` (5-minute rate,
`for: 3m`, P1/page) — that one stays tight because if no source at
all is emitting, something is broken across the whole
indexer/upstream surface.

### Quick diagnosis (≤ 5 min)

```sh
# Confirm which source: the alert label tells you, but dashboards
# sometimes drop the label on flat-line queries. :9464 is the
# indexer's metrics port and it is loopback-bound — query from r1.
ssh root@136.243.90.96 'curl -s http://localhost:9464/metrics | \
  grep -E "stellarindex_source_(events_total|enabled|last_event_unix)"'

# Health snapshot for every source's connection state:
stellarindex-ops list-cursors -config /etc/stellarindex.toml

# Is upstream the issue? r1 doesn't run its own stellar-rpc (removed
# 2026-04-23, see docs/operations/r1-deployment-state.md); point the
# probe at a public endpoint to confirm the network is closing
# ledgers and the source contract is still emitting events.
stellarindex-ops rpc-probe https://mainnet.sorobanrpc.com
```

Key signals:
- **Shared upstream failure**: on-chain and external sources both flatten at once. Jump to `ingestion.md#stellarindex_ingestion_all_sources_stopped`.
- **On-chain-only flattening**: inspect ledgerstream/indexer logs and current cursor movement for a dispatcher-path issue.
- **Per-source-only issue (others fine)**: the source's filter is rejecting everything, the source is legitimately idle, or a protocol change broke its decoder. Check `decode-errors` alert for correlation.

### Per-source cadence reference

Use this matrix BEFORE assuming a source is genuinely stopped —
several sources have natural cadences far longer than the
high-volume rule's 30-minute rate window, which is exactly why they
sit in the 24 h / 30 h tiers instead. The "expected idle cap" column
is the upper bound on a *normal* silent stretch (an operator
judgement, not the alert threshold); sustained idleness beyond it
warrants investigation, and the alert that actually fires is the one
whose allowlist names the source.

| Source | Expected cadence (active hours) | Expected idle cap (normal silence) | First-look-when-stopped |
| ------ | ------------------------------- | ---------------------------------- | ----------------------- |
| `sdex` (classic) | Continuous during US/EU trading; sparse off-hours | 30 min off-hours | Hubble cross-check via `stellarindex-ops hubble-check`; if Hubble shows trades we missed, decoder regression. |
| `soroswap` | Continuous during US/EU trading hours | 30 min off-hours | Soroban-RPC `getEvents` for the contract; if events flowing on-chain but not into us, decoder regression or cursor stuck. |
| `phoenix` | Low cadence; off-peak windows are common | 45 min off-peak | Same as soroswap. Phoenix's 8-event-per-swap shape (AGENTS.md surprise) means partial decode-error storms can mimic source-stopped. |
| `comet` | Pool-activity-driven; sparse — one curated pool | 45 min normal; alerted by the 24 h low-volume-DEX rule | Since 2026-07-08 comet is contract-identity **gated** to a curated allowlist (`comet.MainnetGatedSet()`, today exactly the Blend BLND/USDC backstop; ADR-0035/0040, CS-026), so unrelated Balancer-v1 deploys no longer leak in — and silence now genuinely means that one pool is quiet. If a *legitimate* new pool has appeared, admitting it is a code change to `comet.MainnetGatedSet()` plus a redeploy; `seed-protocol-contracts -source comet` only re-upserts the in-code set. |
| `aquarius` | Tied to AMM pool activity; sparse | 30 min | Soroban-RPC `getEvents`. |
| `blend` | Auction-driven; very sparse outside active markets | 90 min | Auctions don't run continuously — verify there's an active auction window before treating silence as a stop. |
| `band` | Relayer-push driven — relays **hourly** (divergence.md#stellarindex_oracle_stale) | ~1 h normal; alerted by the 6 h hourly-publisher rule | Band emits **zero events** (AGENTS.md surprise) — observed via `InvokeContract` op args through the dispatcher's `ContractCallDecoder`. Verify the `ContractCallDecoder` is wired and the contract is still being relayed-to upstream. |
| `redstone` | Batch pushes every ~1 min during active periods | 10 min active / 30 min off-peak | Redstone's adapter event topic is `"REDSTONE"`; the body has no `feed_id` (lives in OpArgs). Verify the OpArgs plumbing is intact. |
| `reflector` (×3 contracts: DEX/CEX/FX) | Continuous on the active feed | 15 min DEX/CEX, 60 min FX (FX feed is much slower) | Reflector is **three separate contracts** — confirm WHICH one is silent. The DEX/CEX contracts are the most-watched; the FX contract's slower cadence makes it falsely-page-prone. **Upstream-relayer-stuck check**: if the contract is emitting fresh events on-chain (check the ClickHouse `contract_events` lake — the projector's default read source per ADR-0034 — for recent topic_0=REFLECTOR rows; the Postgres `soroban_events` landing zone is the legacy fallback, decommission-pending) BUT every row in `oracle_updates` has the same stale `ts` value, the issue is Reflector's relayer pushing the same `last_update_timestamp` payload — our decoder is correct, the data is genuinely stale upstream. Confirmed pattern on 2026-05-29 (24+ hours stuck at one ts). Cannot mitigate from our side; raise upstream via Reflector ops + flag the staleness publicly. |
| `binance`, `kraken`, `bitstamp`, `coinbase` (CEX WS streamers) | Continuous (sub-second) when open | 60 s gap = anomalous; 5 min = certainly broken | These are WebSocket streamers, not pollers — silence usually means the WS connection dropped silently. Check streamer-error metrics + reconnect logs. |
| `coingecko` (poller) | Default 60s interval | 5 min (one missed cycle plus cooldown headroom) | CG-specific cooldown semantics — see [`external-pollers.md` § CoinGecko 429 pattern](external-pollers.md#coingecko-429-pattern). |
| `ecb` (FX dailies) | Once per business day ~16:00 CET | 24 hours weekdays / 72 hours weekends-and-holidays | ECB doesn't publish on EU bank holidays; cross-reference the silence date against the published TARGET2 closing days before treating as a stop. |

When the silent source is one of the off-peak-prone ones (Phoenix,
Comet, Aquarius, Blend, ECB) AND the silence is within the expected
idle cap, this is almost always a false positive — and the fix is
**not** to widen a window by hand. The three rules ARE the cadence
tiers: move the source between the allowlists in
`configs/prometheus/rules.r1/ingestion.yml` **and** the matching
`deploy/monitoring/rules/ingestion.yml` (the pair lint requires both
copies to stay in sync), then re-run `make monitoring-check`.
Aquarius is the one currently-awkward case — it sits in the
high-volume 30 m allowlist while its natural cadence is
pool-activity-driven; if it false-positives repeatedly, moving it to
the low-volume-DEX tier is the intended remedy, not a bespoke
fourth window.

### Mitigation

- [ ] Step 1 — restart the indexer if this is isolated to one or a few sources and the broader host/process is healthy. The indexer runs as `stellarindex-indexer.service`, templated by the `archival-node` ansible role (`templates/systemd/stellarindex-indexer.service.j2`); on r1 today there is exactly ONE such host. (The multi-host `indexer-0X` shape is ADR-0008's HA topology, which is not deployed.)
  ```sh
  ssh root@136.243.90.96 "systemctl restart stellarindex-indexer && \
    systemctl status stellarindex-indexer --no-pager | head -10"
  ```
- [ ] Step 2 — if events flow for 1-2 min post-restart then stop again: the source is probably legitimately idle, misconfigured, or affected by upstream schema drift. Compare its recent on-chain/off-chain activity to expectations before treating it as a dead connector.
- [ ] Step 3 — if decode-errors is also firing: the contract's event shape changed. Follow `ingestion.md#stellarindex_ingestion_decode_error` Step 3 (update decoder + backfill).
- [ ] Verification: `rate(stellarindex_source_events_total{source=...}[5m]) > 0` within 2 min of mitigation.

### Known false-positive patterns

- **Low-volume sources during quiet windows**. Phoenix, Blend, Comet, Band, and ECB are precisely why the rule was split: they are NOT covered by the 30-minute rule at all. Phoenix's measured 30-day maximum swap gap is 8 h 28 m and a lake-verified 12 h lull is on record, which is why the low-volume-DEX tier sits at 24 h. A firing from `..._low_volume_dex` or `..._daily_publisher` therefore already means "well past this venue's normal quiet stretch" — treat it as real until the cadence matrix says otherwise, and if the venue's cadence has genuinely changed, move it between allowlists (see above) rather than stretching one.
- **Immediately post-deploy**. A restart briefly shows zero events while the source boots. Every tier's sustain (15 m / 30 m / 1 h) gives ample headroom for normal restarts including stellar-core catchup.

### Changelog

- 2026-04-23 — initial draft.
- 2026-04-30 — rpc-probe URL points at a public stellar-rpc; r1
  doesn't run its own (removed 2026-04-23).
- 2026-05-12 — alert window widened to 30m rate × 15m sustain to
  suppress false positives on low-volume sources (blend, band,
  ecb, comet, phoenix). F-1212b.
- 2026-08-29 — re-verified against HEAD (runbook re-verification
  wave K). The runbook still described ONE 30 m × 15 m alert; the
  shipped shape is the F-1208 three-way split (high-volume allowlist
  30 m/15 m, low-volume DEX 24 h/30 m, daily publisher 30 h/1 h),
  all three sharing this `runbook_url`. At-a-glance, Symptoms and the
  false-positive guidance rewritten around the family, and "extend
  the window" replaced with "move the source between allowlists in
  BOTH rule trees". Band's cadence was documented as "every 5-15 min"
  (it is roughly daily); comet's row predated the ADR-0035/0040
  contract-identity gate; the reflector row pointed at Postgres
  `soroban_events` rather than the ADR-0034 ClickHouse
  `contract_events` lake. Config path → `/etc/stellarindex.toml`,
  host/port shapes → r1's IP + `:9464`.

## stellarindex_ingestion_decode_error

_Source page `ingestion.md#stellarindex_ingestion_decode_error`: status draft, severity P3, last verified 2026-08-29._

### At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_ingestion_decode_error` |
| Severity | P3 (informational) |
| Detected by | `configs/prometheus/rules.r1/ingestion.yml` (the overlay r1 actually loads); multi-host template: `deploy/monitoring/rules/ingestion.yml`. Both trees carry the same expr. |
| Typical MTTR | hours-to-days (investigation) |
| Impact | Per-event parse failures. One failure = one lost observation. At sustained >1/sec, a non-trivial fraction of the source's signal is being dropped. |

### Symptoms

- `rate(stellarindex_source_decode_errors_total{source=...}[5m]) > 1` sustained 5 min.
- Dashboard: *Ingestion → Decode errors* panel non-zero for the offending source.
- Decode-error rate sometimes tracks a specific asset or contract — check the indexer's debug logs for patterns in rejected events.

### Context — what counts as a decode error?

- The SCVal / XDR bytes didn't match the expected shape for the source's event schema.
- Amount values parsed as out-of-range (zero / negative) where the canonical.Trade invariants require positive.
- Asset codes or strkeys failed content validation (e.g. non-alphanumeric classic code, malformed issuer).

Distinct from `orphan-events` (events were well-formed but their correlation partner never arrived) and `insert-errors` (events decoded fine but persistence failed).

### Quick diagnosis (≤ 10 min)

```sh
# Which source is erroring? (alert label tells you this). :9464 is the
# indexer's metrics port and it is loopback-bound — query from the host.
ssh root@136.243.90.96 \
  'curl -s http://localhost:9464/metrics | grep stellarindex_source_decode_errors_total'

# Peek the indexer's stderr for the most recent rejection reasons.
# Source logs at debug when an event is dropped — enable temporarily
# if the default level is info.
ssh root@136.243.90.96 "journalctl -u stellarindex-indexer -n 500 --no-pager" \
  | grep -iE "decode|parse|malformed" | tail -30

# Cross-check: is the contract the source points at the right one?
# A protocol upgrade often changes event shape for a specific
# contract address — rpc-probe confirms the source contract still
# emits recent events, and what topic shape they have today.
# Note: r1 doesn't run its own stellar-rpc (removed 2026-04-23, see
# docs/operations/r1-deployment-state.md); point the probe at a
# public endpoint such as SDF's mainnet RPC.
stellarindex-ops rpc-probe https://mainnet.sorobanrpc.com
```

### Typical root causes

In decreasing order of frequency:

1. **Contract upgraded its event shape.** The most common trigger. A DEX redeploys a pair contract with tweaked event fields; our decoder's field arity check fails. Usually announced in the DEX's release notes; check there first.

2. **Stellar protocol version bump.** CAP-67 (P23) changed how classic asset events look — similar breaking changes happen at most protocol upgrades. `rpc-probe`'s `protocolVersion` line confirms whether the node's running a new protocol we haven't accounted for.

3. **Decoder regression in our repo.** After a deploy of the indexer, an ingest-path commit may have broken a specific event path. `git log --oneline internal/sources/<source>/` scoped to the post-deploy window identifies candidates. Revert is the fastest mitigation.

4. **The dispatcher hitting an off-schedule test/admin tx.** A contract's admin method (pause, upgrade) emits events that look like a swap but decode differently. These are rare and usually coincide with a DEX deploy. The fix is a decoder that ignores them; meanwhile, the error rate should revert to normal once the tx clears. (The ingest path is `internal/dispatcher`; the *orchestrator* is the aggregator's pricing loop, `internal/aggregate/orchestrator` — not involved here.)

### Per-source quick reference

If the alert label points at one of these sources, the listed
surprise is the single most common cause of decode regressions —
worth checking BEFORE deeper diagnosis. This table is the
operator-facing summary derived from the AGENTS.md "Things that
will surprise you" list.

| Source | Most common decode-regression cause | First place to look |
| ------ | ----------------------------------- | ------------------- |
| `soroswap` | `SwapEvent` has no post-state reserves; reserves come in the immediately-following `SyncEvent`. A missing/orphaned `SyncEvent` produces a partner-less swap that decodes but has no usable reserve context. | Check the `orphan-events` rate. |
| `phoenix` | Phoenix emits **8 events per swap** (one per field with a 2-tuple topic `("swap", "<field>")`). A swap reconstruction that's short of all 8 looks malformed. | Group by `(ledger, tx_hash, op_index)`. |
| `comet` | Comet uses a shared `("POOL", <event>)` topic across **every** Balancer-v1 deployment, so topic bytes alone cannot identify a pool. Since 2026-07-08 the decoder is contract-identity **GATED** (ADR-0035/0040, CS-026): a curated one-pool allowlist — `comet.MainnetGatedSet()`, today exactly the Blend BLND/USDC backstop `CAS3FL6T…` — is the trust root, and a copycat emitter now **fails closed** rather than landing rows. So a decode/recognition complaint on comet is more often "a real pool we haven't admitted" than "a schema change". | Confirm the emitting contract is in `comet.MainnetGatedSet()`; admitting a legitimate new pool is a code change: add it to `comet.MainnetGatedSet()` and redeploy (`seed-protocol-contracts -source comet` only re-upserts that in-code set; it cannot admit a pool). Foreign emitters surface via the completeness recognition audit. Rows written BEFORE the gate shipped came from the old topic-only decoder and were not retroactively re-verified. |
| `reflector` | Reflector is **three separate contracts** (DEX / CEX / FX) and has NO on-chain `twap` or `x_*` methods (we compute TWAP and cross-pair locally). | Check which Reflector contract is decoding. |
| `band` | Band's Soroban contract emits **zero events** — observed via `relay()` / `force_relay()` `InvokeContract` op args through the dispatcher's `ContractCallDecoder` interface. Pair rates are at E18 scale; relayed single-asset rates are at E9. | Verify the `ContractCallDecoder` is wired. |
| `redstone` | Adapter emits topic `"REDSTONE"` events but the body carries NO `feed_id`. Feed IDs live in the tx's `write_prices(updater, feed_ids, payload)` op args — plumbed through `events.Event.OpArgs`. A feed_ids/updated_feeds length mismatch is **no longer an immediate refusal**: `resolveFeedAttribution` (`decode.go`) first tries the accepted-feed subset derived from the op's contract-data write keys, then payload-median alignment (`payload.go`, verified byte-exact on ledger 59,258,375). Only a failed or ambiguous recovery refuses — `ErrFeedIDCountMismatch` / `ErrAmbiguousSubset` — and refusing the whole event is deliberate (honest-blind beats misattributed). | Check the OpArgs plumbing first; then read WHICH sentinel the log names — `ErrAmbiguousSubset` means recovery ran and could not attribute uniquely, `ErrStateWriteFeedMismatch` means the claimed feed names disagreed with what the contract actually stored. |
| `sdex` (classic) | Post-P23 (mainnet 2025-09-03) every classic asset movement emits a unified transfer/mint/burn event. The decoder handles both post-P23 event SHAPES — the 3-topic SEP-41 form and the 4-topic CAP-67 form (4th topic `sep0011_asset`) — but a protocol bump can introduce a third. There is **no operations+effects fallback**: pre-P23 classic movement is not reconstructed on this path at all (that era is rebuilt out-of-band from the ClickHouse lake, `internal/sources/classicmovements` per ADR-0047/0048), so "missing pre-P23 classic rows" is never a decode-error symptom. | Check `protocolVersion` from `rpc-probe`. |
| Any SEP-41 token | `transfer` event data can be EITHER a simple `i128` OR a map containing `amount` + `to_muxed_id`. Type-test before `MustI128()`. | Type-test before `MustI128()`. |
| `accounts`, `trustlines`, `claimable_balances`, `liquidity_pools`, `sac_balances` | These are `RegisterSupplyEntryDecoders` LedgerEntry-based supply observers (`internal/pipeline/dispatcher.go`), not soroban_events decoders — a decode error here means a malformed `AccountEntry`/`TrustLineEntry`/`ClaimableBalanceEntry`/`LiquidityPoolEntry`/`ContractData` change, not a contract-event schema shift. They are NOT on the `compute-completeness`/`verify-reconciliation` axis (`-source <name>` errors with an explicit "LedgerEntry-based, not reconcilable" message) — that tool only re-derives soroban_events. | Check the indexer's debug log for the LCM entry-change shape. There is no re-derive tool for this axis, and no ledger-gap target either: their `*_observations` tables only get a row when a watched entry changes, so absent ledgers are quiet, not lost (`excludedObservationTables` in `internal/storage/timescale/gap_targets_test.go` pins the decision). Only the account observer has a true processed watermark (`account_observer_watermark`, migration 0144). The other four have none: `MAX(ledger)` of their tables goes stale when a healthy observer is quiet, so a dead observer cannot be told from a quiet one. That is an open gap. |

When a Soroban DEX/oracle source decode-errors immediately after a
Stellar protocol bump or a known DEX redeploy: the source's WASM
likely changed event/topic shape. **Backfill is unsafe across the
upgrade boundary** until the WASM-hash audit re-runs (see
[`docs/operations/wasm-audits/`](../wasm-audits/) and
[`architecture/ingest-pipeline.md#contract-schema-evolution`](../../architecture/ingest-pipeline.md#contract-schema-evolution))
— flip `BackfillSafe = false` for that source until the audit log
shows the new WASM hash decodes cleanly.

### Mitigation

This alert is P3 because there's no emergency runtime response — we can't un-drop events after the fact. The mitigation ladder is:

- [ ] Step 1 — identify the root cause from the table above.
- [ ] Step 2 — if the cause is transient (option 4): wait. Rate should decline on its own.
- [ ] Step 3 — if the cause is a contract upgrade (option 1 or 2): update the decoder in `internal/sources/<source>/decode.go`. Typical iteration is one PR plus a golden-file fixture reproduction. Then re-derive the affected range — which path depends on the source (ADR-0032):
      - **Projected (Soroban-derived) sources** — rewind the projector, never a bespoke backfill:
        `stellarindex-ops projector-replay -config /etc/stellarindex.toml -source <name> -from <ledger> -write`.
        `-write` is not optional: `projector-replay` carries the shared
        `opsutil` write gate, so **dry run is the default** and the command
        without it reports what it would do and rewinds nothing (watch for the
        `═══ DRY RUN — no writes; pass -write to apply ═══` banner on stderr).
        Use `projected-rebuild` (same gate) for rewinds beyond roughly 1M ledgers.
      - **Non-projected sources** (`sdex`, external CEX/FX, `band`, `soroswap-router`) —
        `stellarindex-ops backfill -write -config /etc/stellarindex.toml -from N -to N -source <name>`,
        which **refuses** any source that is not `BackfillSafe` in
        `internal/sources/external/registry.go`.
      Either way, on r1 a re-derive is a heavy one-shot: run it under
      `/usr/local/sbin/run-heavy-job.sh`, one job at a time
      (docs/operations/maintainer-workflow.md, "Heavy one-shot jobs").
- [ ] Step 4 — if the cause is a regression (option 3): `git revert` the suspect commit and deploy. File an incident to retry the regressed change with a proper test.
- [ ] Verification: `rate(...decode_errors_total[5m])` drops back under the 1/sec threshold within 5 min of mitigation.

#### Comms note when `class_drop_spike` co-fires

If `stellarindex_aggregator_class_drop_spike` fires alongside this
alert, the affected source has dropped out of the VWAP for one or
more pairs. The remaining sources continue to serve prices, but
the smaller consensus may produce elevated
`flags.divergence_warning` on the affected pairs. **Surface this
in status comms** — it explains why a consumer might see a
warning flag without a corresponding price disruption. Template:
"Affected pairs may show elevated `flags.divergence_warning`; price
is still served correctly from remaining sources." See
[the 2026-04-30 SEV-2 drill](../drills/README.md#drill-log)
for the canonical exercise of this pattern.

### Changelog

- 2026-04-23 — initial draft alongside the SourceDecodeErrorsTotal wiring + orphan/decode split.
- 2026-04-30 — rpc-probe URL points at a public stellar-rpc; r1
  doesn't run its own (removed 2026-04-23).
- 2026-08-29 — re-verified against HEAD (runbook re-verification
  wave K). Four claims were stale or false: the sdex row asserted a
  pre-P23 operations+effects fallback that has never existed; the
  comet row still described the pre-ADR-0035/0040 topic-only match
  (the decoder has been contract-identity gated since 2026-07-08);
  the redstone row said a length mismatch refuses outright (payload-
  median / state-write recovery now runs first); and Step 3 told
  responders to "backfill from the cursor start via the indexer",
  which ADR-0032 replaced with projector-replay for projected sources.
  Host/port shapes → r1's IP + `:9464`; "Orchestrator" → dispatcher.

## stellarindex_decoder_panicked

_Source page `ingestion.md#stellarindex_decoder_panicked`: status active, severity P1, last verified 2026-09-02._

### At a glance

| | |
|---|---|
| **Alert** | `stellarindex_decoder_panicked` — `stellarindex_decoder_panics_total > 0` |
| **Severity** | page |
| **What it means** | A source decoder's `Matches` or `Decode` PANICKED on a ledger input. The dispatcher recovered it, skipped that one input, and ingest continued — the process is up and the cursor is advancing. A panic in `Decode` costs one event for one source; a panic in `Matches` aborts that whole dispatch chain, so other decoders that would have matched the SAME input are skipped too — one input, not one source. The projector counts a panic on a lake row here too (journal `decoder panicked; row SKIPPED`, attrs `source` / `ledger` / `tx`); it skips that row and advances its cursor. |
| **First action** | Read the `source` label, pull the `decoder panicked` journal line (it carries `ledger` / `tx_hash` / `op_index` / `stack`), and treat it as a decoder bug. |
| **Why page** | The decoder will keep silently dropping **every** event of that shape until a fixed binary ships. Nothing else fires: the ledger completes, the cursor advances, and only the ADR-0033 coverage verdict eventually notices. |
| **Data loss** | None permanent. The raw event is already in the ClickHouse lake — re-derive after the fix. |

### Why this exists

Before #371 F1 there was no `recover()` anywhere in `internal/dispatcher`.
A panic in a decoder unwound out of `ProcessLedger` and was caught only at
LEDGER granularity by `internal/pipeline/processor.go:41`, which returned
`dispatcher panic for ledger N`. That discarded the outputs of **every**
source for that ledger, refused the cursor advance, and the indexer's
`realMain` returned the error → process exit → `Restart=on-failure` →
re-read the same ledger from the same cursor → same panic. After
`StartLimitBurst` restarts systemd parked `stellarindex-indexer` in
`failed` and all ingest stopped until a human intervened. One decoder's
bug was a total, self-sustaining ingest outage.

The dispatch seams already had a policy for "this decoder cannot handle
this input": count it, skip that one input, let the ledger finish. A panic
now gets the same handling — so the blast radius is one input for one
source instead of every source forever — and this alert is what makes the
skip loud instead of silent.

### Symptoms

- `stellarindex_decoder_panicked{source="<name>"}` firing (fires on the
  first panic, and stays firing until the process restarts).
- Journal line `decoder panicked — input SKIPPED, ingest continues` on
  `stellarindex-indexer`, with `decoder`, `ledger`, `tx_hash`,
  `op_index`, `panic` and a `stack`.
- `stellarindex_source_decode_errors_total{source=…}` moved by the same
  amount — a recovered panic is counted as a decode error too, so the
  existing decode-error signal and `decoder_stats` stay consistent.
- Downstream, hours later: `/v1/coverage` reports `complete=false` for
  the affected window once `compute-completeness` re-derives it (below).

### Quick diagnosis (≤ 5 min)

```sh
# Which decoder, which ledger, what was the fault?
ssh root@136.243.90.96 'journalctl -u stellarindex-indexer --since "-2h" --no-pager | grep -A25 "decoder panicked"'

# How many, and is it still climbing? (One-offs and storms need
# different responses; a storm means an event SHAPE, not one bad event.)
ssh root@136.243.90.96 'curl -s "http://localhost:9090/api/v1/query?query=stellarindex_decoder_panics_total" | python3 -m json.tool | grep -E "source|value"'

# Sanity: ingest is genuinely still moving (it should be — the whole
# point of the guard). If it is NOT, this is a different incident.
ssh root@136.243.90.96 'curl -s "http://localhost:9090/api/v1/query?query=stellarindex_cursor_last_ledger" | python3 -m json.tool | grep -E "source|value"'
```

### How the skipped event stays visible (ADR-0033)

This is why "skip and continue" is not the same as "silently lose data".
The skip is recorded in three independent places, none of which depends
on the indexer process surviving:

1. **The raw event is in the ClickHouse lake already.** `dispatchOne`
   pushes to the raw-event sink BEFORE the decoder pass, and the lake
   extractor (`internal/storage/clickhouse` `ExtractLedger`, wired at
   `cmd/stellarindex-indexer/main.go`) is decoder-independent. So the
   substrate keeps its genesis-to-tip claim and `lake_complete` is
   unaffected — nothing is lost, it is only un-projected.
2. **The coverage VERDICT turns red.** `compute-completeness` re-derives
   each source's outputs from the lake through the SAME decoder, under
   `safeDecode` (`internal/completeness/reconcile.go`, the
   `decoder panicked on event shape` arm). A panicking decoder marks the
   ledger `blind.Undecodable` → `BlindSpots` → `projection_ok=false` →
   `/v1/coverage` reports `complete=false` for that window. The verdict
   tells the truth about the gap rather than papering over it.
3. **The decode-error delta is persisted.** `statsflush` writes the
   per-source decode-error count to `decoder_stats` every 5 minutes, and
   `ledger_ingest_log` (migration 0051) carries the decoder-independent
   census for the same ledger.

For classic op decoders (sdex) `stellarindex-ops verify-reconciliation
-source sdex` shows the delta (it re-derives from the lake through the
writer's filter; the raw `classic_trade_effect_count` census also counts
one-side-zero fills the trades table cannot hold); for entry-change
observers the per-class gap detector does.

### Mitigation (≤ 15 min)

There is no operational mitigation — a deterministic decoder panic needs
a code fix. What you can do now is bound it and prove it is bounded.

- [ ] **Do NOT restart the indexer to "clear" the alert.** The counter is
      process-lifetime; a restart resets it to absent and hides the bug.
      The guard means the unit is healthy — leave it running.
- [ ] Confirm ingest is unaffected: the cursor is advancing and
      `stellarindex_ingestion_all_sources_stopped` is not firing.
- [ ] Decide the blast radius from the panic RATE. A single increment is
      one poison event. A counter climbing every ledger means the decoder
      panics on a whole event SHAPE (a contract upgraded its event
      schema — see docs/architecture/ingest-pipeline.md#contract-schema-evolution), so
      that source is effectively dark from this ledger on.
- [ ] If the source must not run blind in the meantime, remove it from
      `ingestion.enabled_sources` in `/etc/stellarindex.toml` (via the
      ansible role — configuration is ansible-managed) and restart. This
      is a deliberate, recorded gap rather than a silent one.
- [ ] File the bug with the stack + `(ledger, tx_hash, op_index)` from
      the journal line.

### Recovery after the decoder is fixed

Re-derive from the lake — never a MinIO re-walk (invariant 8).

```sh
# Projected sources (soroswap, blend, phoenix, comet, defindex, sep41_*,
# reflector/redstone oracle_updates, …):
stellarindex-ops projector-replay -config /etc/stellarindex.toml \
  -source <name> -from <ledger> -write   # without -write this is a DRY RUN that exits 0

# Non-projected sources (sdex, band, supply observers):
stellarindex-ops ch-rebuild ...   # see docs/operations/backfill-procedure.md

# Then re-run the verdict for the window and confirm it goes green:
stellarindex-ops compute-completeness ...
```

### Root cause analysis

A decoder panic is always a code defect: an unchecked slice index, a
`MustI128()` on a map-shaped `transfer` body, a nil deref on an optional
field. The two recurring generators in this codebase are:

- **Contract schema evolution.** Soroswap / Phoenix / Aquarius /
  Reflector `update_contract` in place; a backfill replays every prior
  WASM version. Decode by Map-field-name, not position.
- **Adversary-shaped input.** Event bodies are attacker-influenced. A
  decoder is parsing untrusted bytes and must never assume arity.

Attach the stack, the `(ledger, tx_hash, op_index)`, and the raw event
pulled from the lake for that coordinate.

### Known false-positive patterns

None. A recovered decoder panic is never benign — it means events of that
shape are being dropped from the served tier right now.

Note the alert does NOT self-clear: `> 0` on a process-lifetime counter
keeps firing until the binary restarts. That is deliberate (the decoder
is still broken), and it is why the expression is not `increase(...)`: a
one-off panic creates a series that is born at 1 and never moves, and
`increase()` over a series that appears inside its own lookback window
evaluates to 0 — the alert would never fire for the single-poison-event
case it exists to catch.

### Changelog

- 2026-09-02 — created with the metric + guard (#371 F1).

## stellarindex_ingestion_lag_high

_Source page `ingestion.md#stellarindex_ingestion_lag_high`: status archived, severity P2, last verified 2026-08-29._

This alert is currently retired. The pre-dispatcher orchestrator emitted
a per-source lag gauge (`source_lag_ledgers`, deleted from internal/obs 2026-07-02 — zero production emitters ever); the current `ledgerstream -> dispatcher`
indexer does not. Keep this file only as historical operator context until a
replacement per-source lag signal lands.

### At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_ingestion_lag_high` |
| Severity | P2 (ticket) |
| Detected by | — (retired; no rule in either tree — neither `deploy/monitoring/rules/` nor `configs/prometheus/rules.r1/` defines `stellarindex_ingestion_lag_high`) |
| Typical MTTR | 15–60 min (backfill), longer if the bottleneck is write-side |
| Impact | A specific source is > 1000 ledgers behind the tip. At ~5 s per ledger that's ~1.4 h of freshness debt for the assets that source quotes. `/v1/price` for those assets will serve older `observed_at`s; aggregates including that source will under-weight its recent activity until it catches up. |

### Symptoms

- (historical) the per-source lag gauge exceeding 1000 for ≥ 10 min.
- `source_last_event_unix` still advancing (so the source isn't
  stopped — it's just slow).
- `api.md#stellarindex_api_price_stale` may also fire for assets that source quotes.

### Quick diagnosis (≤ 5 min)

```sh
# Who's behind and by how much?
# (historical — the gauge no longer exists; per-source freshness
# now comes from data-freshness.sh + source_coverage_snapshots)

# Is the source's processing rate > production rate?
#   Production: ~1 ledger/5 s = 0.2 ledger/s
#   If the source processes < 0.2 ledger/s it'll never catch up.
curl -s http://prometheus:9090/api/v1/query --data-urlencode \
  'query=rate(stellarindex_source_events_total{source="<X>"}[5m])'

# Is the RPC also lagging? If so, we can't catch up faster than it.
stellarindex-ops rpc-probe http://stellar-rpc:8000

# Is persistence the bottleneck (insert errors rising)?
curl -s http://indexer:9464/metrics | grep insert_errors_total
# (Hostnames above are historical — r1 today:
#  curl -s localhost:9464/metrics | grep insert_errors_total
#  on the box; the rpc-probe line targets a service r1 no longer runs.)
```

### Typical root causes

1. **We're behind because we just restarted.** The orchestrator
   resumes from `last_ledger` in `ingestion_cursors`; if that's
   hours behind (deployment took time, replay is slow), expected
   until catchup finishes.
   - Signal: lag shrinking over time — catchup in progress.
   - Mitigation: wait. Watch the rate of shrinkage to estimate ETA.

2. **The source is processing slower than the chain produces.**
   Heavy per-event work (decode + correlate + persist + metric
   emit) can fall behind when pair counts grow.
   - Signal: ledger-per-second rate on the source is flat at or
     below the production rate.
   - Mitigation: profile the decoder; parallelize persistence
     (batched `INSERT` vs per-row). Requires a code PR.

3. **Write-side bottleneck.** Timescale primary is slow (locks,
   WAL pressure, disk IO).
   - Signal: insert-rate drops but decode-rate stays normal;
     backend time in `pg_stat_activity` shows `IO:DataFileRead`
     or `Lock:tuple` waits.
   - Mitigation: `postgres.md#stellarindex_timescale_connections_saturated` / `postgres.md#stellarindex_timescale_replica_lag` /
     `postgres.md#stellarindex_timescale_disk_full` depending on the underlying storage issue.

4. **Upstream RPC is lagging.** We can process at any speed we
   like; if the RPC is 10 min behind wall-clock, we look 10 min
   lagged.
   - Signal: `rpc_latest_ledger_age_seconds` also elevated.
   - Mitigation: `stellar-node.md#stellarindex_stellar_rpc_lag`.

### Mitigation

- [ ] Step 1 — separate "behind and catching up" from "behind and
      staying behind" (plot lag over 15 min).
- [ ] Step 2 — if catching up: wait, estimate ETA from shrinkage.
- [ ] Step 3 — if stuck: find the bottleneck (RPC / persistence /
      decoder) and follow the linked runbook.
- [ ] Step 4 — if the gap is large and we want to skip ahead,
      run gap-detection then backfill the affected range:
      ```sh
      # 1. Identify the lagging cursor + the (from, to) range
      stellarindex-ops detect-gaps -config /etc/stellarindex.toml \
          -threshold 50
      # 2. Backfill the named range. -dry-run first to see scope.
      stellarindex-ops backfill -config /etc/stellarindex.toml \
          -from <FIRST_LEDGER> -to <LAST_LEDGER> \
          -source <SOURCE_NAME> -dry-run
      # 3. Drop -dry-run to commit.
      stellarindex-ops backfill -write -config /etc/stellarindex.toml \
          -from <FIRST_LEDGER> -to <LAST_LEDGER> \
          -source <SOURCE_NAME> -resume
      ```
      The two commands stack to a manual replay; an end-to-end
      "auto-detect-and-backfill" wrapper is post-launch scope —
      operators run the two-step procedure during incidents.

      **ADR-0032 split:** for **projected Soroban sources** (the
      `IsProjectedEvent` set — soroswap, blend, phoenix, comet,
      cctp, rozo, defindex, sep41, reflector/redstone, …) catch-up
      is `stellarindex-ops projector-replay -source <name>
      -from <ledger>`, NOT `backfill`; and `backfill` refuses any
      Soroban source whose `BackfillSafe` registry flag is false
      (unaudited WASM history). On r1 either command runs under
      the mandatory heavy-job wrapper:
      `/usr/local/sbin/run-heavy-job.sh <name> <cmd…>`.
- [ ] Verification: lag drops below 100 ledgers sustained 15 min.

### Root cause analysis

- When did lag start growing — correlate with deploys, RPC
  outages, schema migrations.
- Was only one source affected, or multiple? Multiple = shared
  upstream/storage problem. One = source-specific code/schema.
- For source-specific issues, inspect recent golden-file test
  fixtures — is the real event shape drifting from our decoder?

### Known false-positive patterns

- **Post-deploy catchup** triggers the alert if the deploy-gap >
  1000 ledgers. Silence during known-long deploys.
- **Paused sources** (operator-disabled via `enabled_sources`)
  don't update `source_enabled`, so the `and on(source) source_enabled == 1`
  qualifier keeps them out of this alert. If you see an alert for
  a source you thought was disabled, your gauge wiring is wrong
  — fix that first.

### Changelog

- 2026-04-23 — initial draft.
- 2026-08-29 — re-verified (still archived). Detected-by corrected
  to "retired; no rule in either tree" — the cited
  `deploy/monitoring/rules/ingestion.yml` no longer defines this
  alert, nor does `rules.r1/`. Dead link `pg_conns-saturated.md` →
  `postgres.md#stellarindex_timescale_connections_saturated`. Mitigation step 4 gained the ADR-0032
  split (projected sources catch up via `projector-replay`;
  `backfill` refuses non-`BackfillSafe` Soroban sources; both run
  under `/usr/local/sbin/run-heavy-job.sh` on r1). Diagnosis block
  annotated: `prometheus:9090` / `indexer:9464` / `stellar-rpc:8000`
  hostnames are historical — on r1 use
  `curl -s localhost:9464/metrics | grep insert_errors_total`.

## stellarindex_ledgerstream_tier_both_missing

_Source page `ingestion.md#stellarindex_ledgerstream_tier_both_missing`: status draft, severity P1, last verified 2026-08-29._

### At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_ledgerstream_tier_both_missing` |
| Severity | P1 (page) |
| Detected by | `configs/prometheus/rules.r1/ledgerstream-tier.yml` (the overlay r1 actually loads); multi-host template: `deploy/monitoring/rules/ledgerstream-tier.yml`. Both trees carry the same expr. |
| Typical MTTR | 10–30 min (rehydrate from AWS) up to a few hours (cross-region rebuild) |
| Impact | The indexer (or a backfill job) cannot read an LCM from EITHER tier — the affected cursor is stalled until the gap is filled. Customer-facing impact depends on which cursor: live-tip stall → API freshness lag growing; backfill stall → completing a historical range is blocked but live serving is unaffected. |

### Background — ADR-0027 in one paragraph

`internal/ledgerstream/tiered.go` reads each LCM from the local
`galexie-archive` MinIO bucket (hot) and, on a `NoSuchKey`, falls
back to the AWS public bucket
`s3://aws-public-blockchain/v1.1/stellar/ledgers/pubnet/` (cold).
`both_missing` is when neither tier has the object.

**Status on r1 (as of 2026-08-29): the cold tier is ENABLED and this
alert CAN fire.** Two dates matter when reading history:

- The tier was enabled in the r1 inventory on **2026-07-25**
  (`3fe41cc0`) — the same change that fixed the credential bug which
  had made every cold read fail with `InvalidAccessKeyId` since the
  feature landed. Before that, cold-tier init failed and ledgerstream
  degraded silently to hot-only by design.
- The `both_missing` counter was **nil in production** until
  **2026-08-01** (`f9f36e17`, W5-mon-3, "register the metric
  unconditionally"). Any incident review that predates 2026-08-01 must
  not read the absence of this page as evidence the condition never
  occurred — it could not have fired.

### Symptoms

- Prometheus counter
  `stellarindex_ledgerstream_tier_read_total{outcome="both_missing"}`
  has increased in the last 5 min.
- The affected binary's logs show `cold read: ... not found` (or
  the AWS-SDK equivalent) for one or more ledger sequences.
- If the affected cursor is live ingest,
  `stellarindex_cursor_last_ledger{source="ledgerstream"}` has stopped
  advancing (`"ledgerstream"` is the only `source` label this gauge
  ever carries — `cmd/stellarindex-indexer/main.go`), and the
  per-source `stellarindex_source_last_event_unix` gauges are ageing.
- If it's a backfill, the backfill range's progress has frozen —
  read it from `stellarindex-ops list-cursors -config /etc/stellarindex.toml`.

> Earlier revisions of this runbook cited an indexer ledger-lag gauge
> and a backfill-cursor gauge. **Neither has ever been registered** —
> don't go looking for them. The three signals above are the real
> ones.

### Quick diagnosis (≤ 5 min)

Identify the failing ledger(s), which tier is at fault, and whether
AWS is reachable at all.

```sh
# 1. Which ledgers are missing — the indexer log line names them.
ssh root@136.243.90.96 'journalctl -u stellarindex-indexer --since="10 min ago" | grep -iE "both.missing|tiered.*not.found" | tail -5'

# 2. Is the local hot tier intact for the affected range?
#    Replace SEQ with the ledger from step 1. Don't hand-compute the
#    partition prefix: partitions are named "%08X--<start>-<end>/"
#    where the hex is MaxUint32-start (so it DESCENDS as the ledger
#    ascends) and each partition spans files_per_partition = 64000
#    ledgers, one object per ledger. Let mc find the object instead.
SEQ=...   # e.g. 23000000
ssh root@136.243.90.96 "mc find local/galexie-archive --name '*--${SEQ}.xdr.*'"

# 3. Is the AWS public bucket reachable from r1?
#    (aws-public-blockchain is in us-east-2, NOT us-east-1.)
ssh root@136.243.90.96 'curl -sf -m 10 https://aws-public-blockchain.s3.us-east-2.amazonaws.com/v1.1/stellar/ledgers/pubnet/ -I | head -3'

# 4. Cold-read latency in the last 30 min (background context for
#    whether AWS is generally slow vs flat-out unavailable). :9464 is
#    the indexer's metrics port; :9100 is node_exporter.
ssh root@136.243.90.96 'curl -s localhost:9464/metrics | grep stellarindex_ledgerstream_cold_read_duration_seconds_count'
```

### Decision tree

#### A. AWS unreachable

Step 3 returns a non-2xx or times out. The AWS Open Data bucket is
either down or r1 has lost outbound HTTPS.

- **If r1 has lost outbound HTTPS**, page the on-call sysadmin to
  restore networking. Once restored, the indexer/backfill should
  retry automatically — the cursor isn't advanced past the failure.
- **If AWS Open Data is down**, this is rare but the ADR-0027
  "external dependency on AWS sponsorship" risk. Note that
  rehydration reads the SAME configured cold tier, so it cannot route
  around an AWS outage — wait it out (the cursor isn't advanced past
  the failure) and document the window in
  `docs/operations/incidents/`. Rehydration is the tool for the
  *other* two shapes: a hot range that was trimmed while cold was
  fine, and pre-warming hot before a planned backfill.
  ```sh
  # ⚠ READ THE FLAGS. The real signature is:
  #     rehydrate-galexie-archive -config PATH -from N -to N [-write] [-dry-run]
  # `-write` is real and it is the ONLY way this command copies a byte:
  # the shared opsutil write gate makes DRY RUN the default
  # (`DryRun() { return !*write }`, internal/ops/opsutil/opsutil.go), so
  # the posture is fail-CLOSED and `-dry-run` is a no-op alias kept for
  # callers that already pass it. What never existed is `--source` /
  # any peer selector: the tool always reads the CONFIGURED cold tier
  # (storage.s3_cold_*, which on r1 is aws-public-blockchain) and writes
  # into the local hot tier. It refuses outright if the cold tier isn't
  # configured, and it is idempotent (PutFileIfNotExists skips files
  # already hot). Every run announces its mode on stderr:
  #     ═══ DRY RUN — no writes; pass -write to apply ═══
  #     ═══ WRITING — applying changes ═══
  #
  # 1. Preview (this is also what you get if you forget -write):
  stellarindex-ops rehydrate-galexie-archive \
    -config /etc/stellarindex.toml -from <SEQ> -to <SEQ_END>
  #
  # ⚠ A dry run's `copied=N` is NOT a rehydration count and NOT a
  # cold-tier confirmation. In dry-run mode each not-in-hot path short-
  # circuits to the `copied` bucket without ever asking cold whether it
  # holds the object (rehydrateOneFile, rehydrate_galexie_archive.go),
  # so a preview over a range cold has LOST still logs
  #   "rehydrate complete" … copied=N missing_in_cold=0 errors=0
  # and exits 0. Read `copied` here as "files absent from hot". Only a
  # -write run's missing_in_cold / errors counts mean anything.
  #
  # 2. Commit — note the -write; without it you just re-run step 1 and
  #    get a success-shaped report having rehydrated nothing. Heavy
  #    one-shots on r1 go through the wrapper (AGENTS.md r1 rule):
  sudo /usr/local/sbin/run-heavy-job.sh rehydrate \
    /usr/local/bin/stellarindex-ops rehydrate-galexie-archive \
      -config /etc/stellarindex.toml -from <SEQ> -to <SEQ_END> -write
  ```

  If the configured cold tier itself must change (a different
  provider, a peer-region mirror per ADR-0016), that is an ansible
  change to `storage.s3_cold_*` — there is no in-tool peer selector.

#### B. AWS reachable but the specific partition is missing

Step 3 succeeds; step 1 lists ledger sequences; AWS GET on those
partitions returns NoSuchKey. Most likely a recent partition that
hasn't propagated to the public bucket yet (the bucket's freshness
SLA is "~30 min behind tip").

- Wait 30 min and check again — most cases self-resolve.
- If sustained beyond 1 h, escalate to AWS Open Data via the
  contact in the bucket's `README.md`.

#### C. The local hot tier was trimmed too aggressively

Step 2 confirms the local partition is gone; step 3 also fails.

- A trim job (`galexie-archive-trim`) deleted a range that wasn't
  yet safely in cold storage. This is the failure mode
  `--verify-upstream` is meant to prevent — check the trim log:
  ```sh
  ssh root@136.243.90.96 'journalctl -u galexie-archive-trim.service --since="24 h ago" | tail -50'
  ```
- Rehydrate the trimmed range from cold (command in option A above),
  then **disable the trim timer** until the trim operator is fixed:
  ```sh
  ssh root@136.243.90.96 'systemctl disable --now galexie-archive-trim.timer'
  ```
  File a ticket against stellarindex-ops to add the missing safety.

#### D. The indexer/backfill is mis-configured

Step 2 shows the partition is present locally and step 3 reaches
AWS, yet the metric increments. The binary is reading from the
wrong endpoint — its TOML points at a stale bucket or the AWS
config is missing.

- Check the binary's env / config for the right endpoints:
  ```sh
  ssh root@136.243.90.96 'grep -E "s3_bucket|s3_endpoint|s3_cold" /etc/stellarindex.toml'
  ssh root@136.243.90.96 'grep -E "AWS_|S3_" /etc/default/stellarindex'
  ```
  Note the 2026-07-25 lesson (`3fe41cc0`): r1's `AWS_ACCESS_KEY_ID` /
  `AWS_SECRET_ACCESS_KEY` are **MinIO's** credentials, for the HOT
  datastore. The cold client is built separately from
  `s3_cold_access_key_env` / `s3_cold_secret_key_env` — both empty
  means anonymous reads (correct for `aws-public-blockchain`); exactly
  one set is a config error.
- Restart the binary after fixing the config.

### Aftermath

Once the gap is filled, the counter stops incrementing and the
alert resolves on its own (no manual reset). Confirm the affected
cursor has resumed advancing:

```sh
ssh root@136.243.90.96 'curl -s localhost:9464/metrics | \
  grep -E "stellarindex_cursor_last_ledger|stellarindex_source_last_event_unix"'
stellarindex-ops list-cursors -config /etc/stellarindex.toml
```

### Changelog

- 2026-05-22 — initial draft alongside the page-grade
  `stellarindex_ledgerstream_tier_both_missing` alert.
- 2026-08-29 — re-verified against HEAD (runbook re-verification
  wave K). The rehydrate command invented a peer selector
  (`--source vultr`, "pull the missing range from R2 or R3's mirror")
  that has never existed — the tool only ever reads the configured
  cold tier — and the surrounding prose framed rehydration as an
  AWS-outage workaround, which it cannot be. The `-write` fail-closed
  note was correct and is kept, now with the dry-run's misleading
  `copied=N` count spelled out and `-write` restored to the commit
  command (lint-docs §11b now fails CI on a heavy-job command that
  omits it). Two cited gauges (an indexer ledger-lag and a backfill
  cursor) have no producer and never had one; replaced with
  `stellarindex_cursor_last_ledger` /
  `stellarindex_source_last_event_unix` / `list-cursors`, and
  lint-docs §11 widened so a future phantom in these namespaces fails
  CI. The hot-tier `mc ls` prefix arithmetic was wrong
  (partition span is 64000, and the hex prefix is MaxUint32-start) —
  replaced with `mc find`. Metrics port `:9100` → `:9464`; host shapes
  → r1's IP. The "cold tier disabled, alert cannot fire" background was
  stale: enabled 2026-07-25, and the counter was nil in production
  until 2026-08-01.

## stellarindex_ingestion_discovery_drops

_Source page `ingestion.md#stellarindex_ingestion_discovery_drops`: status draft, severity P3, last verified 2026-07-10._

### At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_ingestion_discovery_drops` |
| Severity | P3 (informational) |
| Detected by | `deploy/monitoring/rules/ingestion.yml` |
| Typical MTTR | minutes-to-hours |
| Impact | Discovery coverage is degrading — SEP-41 token sightings AND the oracle-suggestive event/call sightings added per docs/architecture/oracle-manipulation-defense.md §"Discovery pipeline" share one sink. The main ingest path keeps running, but some discovery hits are being dropped before they reach Postgres. |

### Symptoms

- `increase(stellarindex_discovery_dropped_hits_total[10m]) > 0` sustained 10 min.
- Indexer logs include `discovery: hits dropped` warnings.
- Persisted `discovered_assets` rows may lag the underlying event stream during the same window.

### Context

The discovery sink is intentionally non-blocking. When its buffered
channel fills, new discovery hits are dropped instead of stalling the
`ledgerstream -> dispatcher` hot path. This protects price ingestion,
but it means discovery completeness is best-effort under recorder
pressure.

### Quick diagnosis (≤ 10 min)

```sh
# Confirm drops are ongoing, not a one-off blip.
curl -s http://indexer:9464/metrics | grep stellarindex_discovery_dropped_hits_total

# Check whether Postgres/discovery writes are struggling.
ssh root@indexer-01 "journalctl -u stellarindex-indexer -n 200 --no-pager" \
  | grep "discovery:"

# Cross-check for broader storage pressure.
curl -s http://indexer:9464/metrics | grep stellarindex_source_insert_errors_total
```

### Typical root causes

1. Discovery recorder writes are slow because Postgres is degraded.
2. Discovery event volume spiked beyond the configured buffer.
3. A long-running recorder timeout is forcing repeated drop/retry cycles.

### Mitigation

- [ ] Check Timescale health first. If storage is degraded, restore it before tuning discovery.
- [ ] If storage is healthy, inspect whether recent discovery volume increased sharply.
- [ ] Increase the discovery buffer only after confirming this is sustained workload, not a transient outage.
- [ ] Verification: `increase(stellarindex_discovery_dropped_hits_total[10m])` returns to `0`.

## stellarindex_ingestion_dispatcher_tx_skips

_Source page `ingestion.md#stellarindex_ingestion_dispatcher_tx_skips`: status current, severity P3, last verified 2026-09-23._

### At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_ingestion_dispatcher_tx_skips` |
| Severity | P3 (`severity: ticket`) |
| Detected by | `deploy/monitoring/rules/ingestion.yml` and the R1 overlay `configs/prometheus/rules.r1/ingestion.yml` (group `stellarindex.ingestion`, `for: 0m`) |
| Typical MTTR | 15–60 min |
| Impact | The dispatcher (`internal/dispatcher/dispatcher.go`) skipped a whole transaction, or a whole ledger's state-archival evictions, rather than decoding it. Downstream this reads as an empty ledger, not a failure — the completeness reconcile would still pass. |

### What this fires on

Four process-wide counters, each incremented when `ProcessLedger` gives up
on a transaction (or, for the last, a ledger's eviction list) instead of
decoding it:

| Metric | Where it's incremented | Meaning |
| ------ | ----------------------- | ------- |
| `stellarindex_dispatcher_tx_read_errors_total` | `internal/dispatcher/dispatcher.go` (tx read) | The transaction itself was malformed and was skipped. |
| `stellarindex_dispatcher_tx_event_read_errors_total` | `internal/dispatcher/dispatcher.go` (`GetTransactionEvents`) | That transaction's Soroban events failed to read (G15-06) — every Soroban event in it is dropped. |
| `stellarindex_dispatcher_entry_meta_unsupported_total` | `internal/dispatcher/dispatcher.go` (apply-phase entry-change walk) | An unhandled `TransactionMeta` version stopped the entry-change walk for that tx — every classic balance/trustline/offer/LP change in it is skipped. |
| `stellarindex_dispatcher_evicted_keys_unreadable_total` | `internal/dispatcher/dispatcher.go` (`walkEvictedKeys`) | That ledger's evicted-key list failed to read — none of its state-archival evictions reached the entry decoders, so each evicted balance stays served as live. The dispatcher logs a WARN naming the ledger. |

`internal/dispatcher/statsflush/flusher.go` mirrors each counter's delta as
a WARN log on every 5-minute flush window (RLT-135); this alert is the
Prometheus-side signal so a sustained climb doesn't depend on someone
tailing logs.

### Quick diagnosis (≤ 5 min)

```sh
# Which of the four counters moved, and by how much?
ssh root@136.243.90.96 'curl -s localhost:9464/metrics | grep -E "stellarindex_dispatcher_(tx_read_errors|tx_event_read_errors|entry_meta_unsupported|evicted_keys_unreadable)_total"'

# The exact failing ledger/tx is in the indexer's logs — statsflush's WARN
# fires the same window this alert does.
journalctl -u stellarindex-indexer --since -2h | grep -E "dispatcher: (tx-read errors|tx-event read errors|unsupported TransactionMeta|evicted ledger keys unreadable)"
```

- `tx_read_errors` climbing → a malformed transaction is reaching the
  dispatcher. Check whether the upstream ledger source (captive core /
  RPC) is serving a corrupt or truncated ledger.
- `tx_event_read_errors` climbing → `GetTransactionEvents` is failing,
  most often a stellar-go XDR/SDK version behind a protocol upgrade that
  changed the Soroban events encoding. Check the deployed `stellar-go`
  version against the network's current protocol.
- `entry_meta_unsupported` climbing → a `TransactionMeta` version the
  apply-phase walk doesn't handle, almost always a protocol upgrade that
  shipped a new meta version ahead of a stellar-go/dispatcher bump.
- `evicted_keys_unreadable` climbing → `LedgerCloseMeta.EvictedLedgerKeys`
  is erroring, almost always an SDK behind a `LedgerCloseMeta` version
  bump. The per-ledger WARN (`ledger=`) names every affected ledger.

### Mitigation (≤ 15 min)

- [ ] Step 1 — confirm which counter(s) moved and correlate with any
      recent Stellar protocol upgrade or captive-core/RPC version change.
- [ ] Step 2 — if a protocol upgrade shipped a new `TransactionMeta`
      version or events encoding: bump the vendored `stellar-go` /
      protocol-support version and ship a dispatcher release that handles
      it. This is a code fix, not an operational mitigation — the skip is
      permanent until the dispatcher is patched.
- [ ] Step 3 — once the fix ships, re-derive the affected ledger range.
      The raw Soroban events are durable in the CH lake (ADR-0034); the
      apply-phase entry changes require a replay from the affected ledger
      (`stellarindex-ops projector-replay` per ADR-0032, or a full
      re-ingest of the range if the classic-side tables need it too).
      Skipped evictions need the live dispatcher re-run over the WARN's
      ledgers: the lake walker has no eviction phase, so a lake-sourced
      rebuild does not restore them.

### Root cause analysis

For the postmortem, gather:
- The exact ledger range the WARN logs cover (`ledger_from`/context in the
  surrounding dispatcher logs).
- The stellar-go / protocol-support version deployed at the time.
- Whether the network had a protocol upgrade in the affected window.

### Known false-positive patterns

- None yet. All four counters are zero in steady state; any nonzero
  increase is a dispatcher gap, not a rate to tolerate.

### Changelog

- 2026-09-23 — created (RLT-143 / #615: the counters were promoted to
  Prometheus by RLT-135 but had no alert or runbook).
- 2026-10-02 — added `evicted_keys_unreadable`.

## Related

**stellarindex_ingestion_all_sources_stopped**

- [rpc-lag](stellar-node.md#stellarindex_stellar_rpc_lag) — only for deployments that still route through stellar-rpc (not r1).
- [ledgerstream-tier-both-missing](ingestion.md#stellarindex_ledgerstream_tier_both_missing) — reader can find the ledger in neither MinIO tier.
- [exporter-down](meta.md#stellarindex_redis_exporter_down) / [minio-metrics-403](infra.md#minio-metrics-403) — MinIO monitoring.
- [binary-version-skew](binary-version-skew.md) — expected after a partial rollback.
- [timescale-primary-down](postgres.md#stellarindex_timescale_primary_down) — next step when DB is the root cause.
- [ingestion-lag](ingestion.md#stellarindex_ingestion_lag_high) — single-source-lag runbook.
- [cursor-stuck](ledger-ingest.md#stellarindex_ingestion_cursor_stuck) — cursor-specific diagnosis.
- Internal docs:
  - `internal/dispatcher/` + `internal/pipeline/` — dispatcher hot path and sinks (the orchestrator was retired 2026-04-23; see `internal/consumer/doc.go`).
  - `cmd/stellarindex-indexer/main.go` — wiring + shutdown.

**stellarindex_ingestion_source_stopped**

- `ingestion.md#stellarindex_ingestion_all_sources_stopped` — P1 escalation when multiple sources stop.
- `stellar-node.md#stellarindex_stellar_rpc_lag` — upstream root cause.
- `ingestion.md#stellarindex_ingestion_decode_error` — adjacent failure mode that can masquerade as source-stopped if every event is being rejected.
- `ledger-ingest.md#stellarindex_ingestion_cursor_stuck` — persistence-layer sibling (events flowing but cursor not advancing).

**stellarindex_ingestion_decode_error**

- `ingestion-events.md#stellarindex_ingestion_orphan_events` — adjacent failure mode (events well-formed but partnerless).
- `ingestion-sink.md#stellarindex_ingestion_insert_errors` — downstream failure mode (events decoded OK but write-path broke).
- `ingestion.md#stellarindex_ingestion_source_stopped` — when the rate hits 100% of pulled events, effectively stopping the source.
- `internal/sources/*/decode.go` — per-source decoder.

**stellarindex_decoder_panicked**

- #371 F1 (REL dependency-failure matrix) — the finding this closes.
- `internal/dispatcher/panic_guard.go` — the guard; `dispatcher.go`'s
  four dispatch seams are where it is installed.
- `internal/completeness/reconcile.go` — `safeDecode`, the ADR-0033 arm
  that turns the same panic into a visible blind spot.
- [worker-panicked](infra.md#stellarindex_worker_panicked) — the sibling alert for a
  panicking background WORKER (that one stops the worker; this one skips
  one input).
- [decode-errors](ingestion.md#stellarindex_ingestion_decode_error) — the per-source decode-error-rate
  alert; every panic also increments that counter.
- [cursor-stuck](ledger-ingest.md#stellarindex_ingestion_cursor_stuck) — use if the cursor
  is NOT advancing (a different incident).
- docs/architecture/ingest-pipeline.md#contract-schema-evolution; ADR-0033; ADR-0034.

**stellarindex_ingestion_lag_high**

- `ingestion.md#stellarindex_ingestion_source_stopped` — the "stopped" variant.
- `stellar-node.md#stellarindex_stellar_rpc_lag` — if the upstream is the bottleneck.
- `ingestion-sink.md#stellarindex_ingestion_insert_errors` — if persistence is failing (not just slow).
- `ledger-ingest.md#stellarindex_ingestion_cursor_stuck` — when the cursor doesn't advance at all.

**stellarindex_ledgerstream_tier_both_missing**

- ADR-0027 — LCM cache tiering, the rollout sequence (§Sequencing).
- ADR-0016 — per-region storage strategy (R2 reads AWS direct; R3
  has its own Vultr mirror). Either can become THE cold tier for a
  region, but only by repointing `storage.s3_cold_*` — the rehydrate
  tool has no peer selector.
- `feedback_cold_tier_premature_enable` — bare §3 (enable tiering)
  without §4 (bulk trim) introduces this failure mode without
  benefit; the rollout always lands them together.
- `docs/operations/lcm-cache-tiering.md` — the operator-facing
  tiering guide (bucket, region, credential shape).

**stellarindex_ingestion_discovery_drops**

- `ingestion-sink.md#stellarindex_ingestion_insert_errors` — storage write failures on the main ingest sink.
- `ingestion.md#stellarindex_ingestion_all_sources_stopped` — the severe case where ingestion itself stops.
- `internal/canonical/discovery/sink.go` — best-effort buffer/drop contract.

**stellarindex_ingestion_dispatcher_tx_skips**

- `ingestion-sink.md#stellarindex_ingestion_insert_errors` — storage-layer write failures; a different failure
  mode (the row decoded fine but couldn't persist).
- ADR-0029 (raw-event landing zone) / ADR-0032 (projector cursor replay) /
  ADR-0034 (CH lake re-derive).
