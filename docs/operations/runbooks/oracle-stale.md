---
title: Runbook — oracle-stale
last_verified: 2026-09-01
status: current
severity: P2
---

# Runbook — `stellarindex_oracle_stale`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_oracle_stale` |
| Severity | P2 (`severity: ticket`) |
| Detected by | `configs/prometheus/rules.r1/divergence.yml` (group `stellarindex.divergence`, `severity: ticket`, `for: 2m`) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/divergence.yml`. |
| Typical MTTR | 15–60 min |
| Impact | One (source, asset) pair stopped publishing for longer than its staleness budget — 10× the source's declared resolution by default (> 50 min for Reflector's 5-min cadence), or whatever `[[oracle.staleness_overrides]]` declares for that pair. `/v1/oracle/latest` responses for that asset become increasingly out of date; any downstream consumer using the oracle prices (triangulation, divergence) gets poisoned inputs. |

## Symptoms

- `(time() - stellarindex_oracle_last_update_unix) > stellarindex_oracle_staleness_budget_seconds` sustained 2 min. Both gauges are labelled `{source, asset}` and are emitted by the same call, so the comparison is per pair with no join.
- Alert label `source` names the specific variant — one of `reflector-dex`, `reflector-cex`, `reflector-fx`, `redstone`, `band`. (Chainlink-HTTP is a divergence reference in `internal/divergence/`, not an oracle source — it doesn't emit `stellarindex_oracle_*` metrics and won't appear here.)
- `stellarindex_source_events_total{source=reflector-...}` rate drops to zero at the same time (or has been zero throughout).

## Quick diagnosis (≤ 5 min)

```sh
# How long since last observation, and what is this pair allowed?
curl -s http://localhost:9464/metrics |
  grep -E "stellarindex_oracle_last_update_unix|stellarindex_oracle_staleness_budget_seconds|stellarindex_oracle_resolution_seconds"

# Is the CONTRACT itself still emitting? There is NO subscription
# to go stale: the dispatcher decodes reflector events (topic
# ["REFLECTOR","update"]) straight out of the Galexie ledger
# stream, so a stall on our side means the indexer/dispatcher
# path is stuck, not a dropped subscription.
# A Reflector contract goes 5 min between updates in normal ops;
# > 50 min is real stall. r1 doesn't run its own stellar-rpc
# (removed 2026-04-23, see docs/operations/r1-deployment-state.md);
# point the probe at a public endpoint to confirm the network is
# closing ledgers and the oracle contract has been invoked recently.
stellarindex-ops rpc-probe https://mainnet.sorobanrpc.com

# Check stellar.expert for the contract's recent tx activity:
#   https://stellar.expert/explorer/public/contract/<contract-id>
#
# Fresh updates on stellar.expert but zero in our metrics →
# our ingest path (indexer/dispatcher) is stuck or the decoder
# is rejecting the events. Zero updates on-chain → the
# relayer/publisher is down.
```

If `stellarindex_oracle_staleness_budget_seconds` is missing for every
asset while `stellarindex_oracle_last_update_unix` is present, the
running indexer predates the per-asset budget and this alert is blind
rather than clear — restart onto the current binary. A budget of `+Inf`
on one source means that source never declared its resolution.

Key signals:
- **On-chain activity continues, we see zero** → the indexer/dispatcher path is stalled on our side (there is no subscription to drop). Restart the indexer; if the issue persists, the contract's event shape may have changed.
- **On-chain activity paused** → the oracle's off-chain publisher (Reflector relayer, Redstone DataService, Band's chain-write bot) is down. Nothing we can do except switch providers or fail over.
- **We see SOME events but they're not decoding** → `stellarindex_source_decode_errors_total` for that source is also elevated. Jump to `decode-errors.md`.

## First, rule out a budget that never matched the asset (this alert's main false positive)

The threshold is the pair's `stellarindex_oracle_staleness_budget_seconds`,
which unless overridden is `10 × stellarindex_oracle_resolution_seconds` —
and that resolution comes from a **hard-coded constant per source**
(`DefaultResolutionSeconds` in each `internal/sources/<oracle>/events.go`).
Nothing reconciles it against how often the oracle really publishes. When
the budget is tighter than reality, this alert fires during entirely
normal operation and never clears.

There are two distinct shapes of that, and they have different fixes:

1. **The SOURCE's declared cadence is wrong** — every asset on it
   over-fires. Fix the constant.
2. **One ASSET publishes on its own rhythm** — its siblings are quiet
   and it alone over-fires. Fix that pair's budget; leave the source's
   cadence alone, because it is not wrong.

That is not hypothetical: `band` declared `60` — copied from a
poll-cadence *recommendation* rather than the relayer's publication
interval — while Band actually relays hourly. The alert fired for
**100% of samples over 7 days** on `crypto:USDC` and `crypto:XLM` before
it was corrected to `3600` (2026-09-01).

So before triaging a stall, check that the alert is measuring something
real:

```sh
# Measured cadence PER ASSET on the alerting source: how often did each
# pair's last-update gauge actually advance over a week? Per asset, not
# per source — a source average hides the one slow member.
curl -s --get http://localhost:9090/api/v1/query \
  --data-urlencode 'query=604800 / changes(stellarindex_oracle_last_update_unix{source="<source>"}[7d])'
# ...against what each pair is allowed:
curl -s --get http://localhost:9090/api/v1/query \
  --data-urlencode 'query=stellarindex_oracle_staleness_budget_seconds{source="<source>"}'
# ...and the source's declared cadence, which seeds the default budget:
curl -s --get http://localhost:9090/api/v1/query \
  --data-urlencode 'query=stellarindex_oracle_resolution_seconds{source="<source>"}'
```

Read the per-asset numbers side by side:

- **Every asset's measured interval exceeds its budget** → the source's
  declared resolution is wrong. Fix the constant, don't chase an outage.
  `internal/sources/band/resolution_test.go` pins that relationship for
  band; a source without such a test can drift silently.
- **One asset's does, its siblings' don't** → that asset publishes on
  its own rhythm and needs its own budget (below). This is the shape a
  peg asset makes: it is republished only when the price moves, so the
  oracle is healthy and quiet at the same time.

Two tells that distinguish either from a real stall: the alert has been
firing intermittently for a long time rather than starting at a point in
time, and `/v1/oracle/streams` shows recent observations for the same
source.

### Widening one pair's budget

`[[oracle.staleness_overrides]]` in `/etc/stellarindex.toml` (codified in
`configs/ansible/roles/archival-node/templates/stellarindex.toml.j2`)
replaces the default budget for exactly one (source, asset):

```toml
[[oracle.staleness_overrides]]
source         = "reflector-cex"
asset          = "crypto:DAI"     # canonical form, NOT "DAI"
budget_seconds = 32400
reason         = "peg asset — republished only on movement; measured gaps to 7h"
```

- `asset` must be the exact identifier the metric's `asset` label
  carries. The indexer refuses to start on a bare or aliased spelling
  rather than accepting an override that matches nothing.
- Size it from the measured worst gap plus headroom, and put that
  observation in `reason` — the next responder needs to re-test the
  claim, not re-derive it.
- An override is **not** a mute: past the wider bound the pair tickets
  normally, so a genuine outage on it still reaches you.
- It takes effect at the next indexer restart, and only reaches the
  gauge on that pair's next publication — for a slow asset, that wait is
  the behaviour being tolerated.

The shipped example is `reflector-cex` / `crypto:DAI` at 9 h: measured
gaps reached 7 h against the 50-minute source default, breaching it on
11.6% of evaluations with the feed healthy (#478).

## Mitigation

- [ ] Step 1 — identify whether the stall is upstream (publisher / contract) or downstream (our ingestion) via the probes above.
- [ ] Step 2 — if our-side: `ssh root@136.243.90.96 systemctl restart stellarindex-indexer` — it resumes from the persisted cursor, so nothing is skipped. If events are ARRIVING but not landing, that's a decode-side stall: check `stellarindex_source_decode_errors_total` and `stellarindex_source_unknown_symbols_total` — the oracle decoders emit `raw:` rows for unrecognised symbols rather than dropping them (see [oracle-unknown-symbols.md](oracle-unknown-symbols.md)).
- [ ] Step 3 — if publisher-side: check the provider's status page (Reflector: app.reflector.world, Redstone: app.redstone.finance, Band: data.bandprotocol.com). Open an incident tracking the upstream ETA. Our API will flag affected asset prices with `stale=true` in the response envelope — communicate that SLA departure to consumers.
- [ ] Step 4 — if a specific asset stops but others from the same source keep flowing: either that asset publishes on its own rhythm (see the budget section above — check its measured cadence before assuming an outage) or the contract de-listed it. There is no "fallback aggregation config" to update — oracles NEVER contribute to VWAP (class policy: `internal/sources/external/registry.go` includes only `ClassExchange`), so impact is bounded to `/v1/oracle/*` responses and divergence references. Note the de-listing in the incident and move on.
- [ ] Verification: `stellarindex_oracle_last_update_unix` for the affected source starts incrementing again.

## Severity note

Originally flagged P2 (ticket) because the impact is bounded — the API keeps serving stale oracle prices instead of failing. If multiple oracle sources go stale at once, escalate to P1: every triangulation path that relies on an oracle is now broken. A single oracle going stale is usually handled by falling back to the other two Reflector variants.

## Related

- `decode-errors.md` — when we're receiving events but failing to parse them.
- `source-stopped.md` — same root-cause class, different alert (trade sources vs oracle sources).
- `oracle-unknown-symbols.md` — decode-side degradation (`raw:` rows) that precedes a full stall.
- Oracle contract IDs are CONFIG, not code: `[oracle.reflector]` in `/etc/stellarindex.toml` (`ReflectorOracleConfig`, `internal/config/config.go`).
- Per-asset staleness budgets are CONFIG too: `[[oracle.staleness_overrides]]` (`OracleStalenessOverrideConfig`, same file); the default and the gauge live in `internal/obs/oracle_staleness.go`.

## Changelog

- 2026-09-08 — the staleness budget went per-(source, asset)
  (`stellarindex_oracle_staleness_budget_seconds`, #478); the alert is
  now a bare comparison rather than a join against the per-source
  resolution. Added the two-shapes triage split and the
  `[[oracle.staleness_overrides]]` procedure. Declared resolution is
  unchanged and still per-source.
- 2026-09-01 — added the mis-declared-resolution section. `band` declared a
  60s resolution against an hourly relay cadence, so this alert fired 100%
  of the time for both its assets; corrected to 3600 and pinned by a test.
- 2026-04-23 — initial draft. Replaces the 404 in the divergence alert rules' runbook_url.
- 2026-04-30 — rpc-probe URL points at a public stellar-rpc; r1
  doesn't run its own (removed 2026-04-23).
- 2026-08-29 — re-verified against HEAD: subscription model removed
  (the dispatcher decodes reflector events from the Galexie ledger
  stream — there is no subscription to go stale), restart command
  in r1 shape, the fictional "fallback aggregation config" replaced
  with the real class policy (oracles never in VWAP), contract-ID
  location corrected to `[oracle.reflector]` config, dual-tree
  Detected-by. Status draft → current.
