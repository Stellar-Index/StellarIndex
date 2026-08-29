---
title: Runbook — decode-errors
last_verified: 2026-08-29
status: draft
severity: P3
---

# Runbook — `stellarindex_ingestion_decode_error`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_ingestion_decode_error` |
| Severity | P3 (informational) |
| Detected by | `configs/prometheus/rules.r1/ingestion.yml` (the overlay r1 actually loads); multi-host template: `deploy/monitoring/rules/ingestion.yml`. Both trees carry the same expr. |
| Typical MTTR | hours-to-days (investigation) |
| Impact | Per-event parse failures. One failure = one lost observation. At sustained >1/sec, a non-trivial fraction of the source's signal is being dropped. |

## Symptoms

- `rate(stellarindex_source_decode_errors_total{source=...}[5m]) > 1` sustained 5 min.
- Dashboard: *Ingestion → Decode errors* panel non-zero for the offending source.
- Decode-error rate sometimes tracks a specific asset or contract — check the indexer's debug logs for patterns in rejected events.

## Context — what counts as a decode error?

- The SCVal / XDR bytes didn't match the expected shape for the source's event schema.
- Amount values parsed as out-of-range (zero / negative) where the canonical.Trade invariants require positive.
- Asset codes or strkeys failed content validation (e.g. non-alphanumeric classic code, malformed issuer).

Distinct from `orphan-events` (events were well-formed but their correlation partner never arrived) and `insert-errors` (events decoded fine but persistence failed).

## Quick diagnosis (≤ 10 min)

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

## Typical root causes

In decreasing order of frequency:

1. **Contract upgraded its event shape.** The most common trigger. A DEX redeploys a pair contract with tweaked event fields; our decoder's field arity check fails. Usually announced in the DEX's release notes; check there first.

2. **Stellar protocol version bump.** CAP-67 (P23) changed how classic asset events look — similar breaking changes happen at most protocol upgrades. `rpc-probe`'s `protocolVersion` line confirms whether the node's running a new protocol we haven't accounted for.

3. **Decoder regression in our repo.** After a deploy of the indexer, an ingest-path commit may have broken a specific event path. `git log --oneline internal/sources/<source>/` scoped to the post-deploy window identifies candidates. Revert is the fastest mitigation.

4. **The dispatcher hitting an off-schedule test/admin tx.** A contract's admin method (pause, upgrade) emits events that look like a swap but decode differently. These are rare and usually coincide with a DEX deploy. The fix is a decoder that ignores them; meanwhile, the error rate should revert to normal once the tx clears. (The ingest path is `internal/dispatcher`; the *orchestrator* is the aggregator's pricing loop, `internal/aggregate/orchestrator` — not involved here.)

## Per-source quick reference

If the alert label points at one of these sources, the listed
surprise is the single most common cause of decode regressions —
worth checking BEFORE deeper diagnosis. This table is the
operator-facing summary derived from the CLAUDE.md "Things that
will surprise you" list.

| Source | Most common decode-regression cause | First place to look |
| ------ | ----------------------------------- | ------------------- |
| `soroswap` | `SwapEvent` has no post-state reserves; reserves come in the immediately-following `SyncEvent`. A missing/orphaned `SyncEvent` produces a partner-less swap that decodes but has no usable reserve context. | Check the `orphan-events` rate. |
| `phoenix` | Phoenix emits **8 events per swap** (one per field with a 2-tuple topic `("swap", "<field>")`). A swap reconstruction that's short of all 8 looks malformed. | Group by `(ledger, tx_hash, op_index)`. |
| `comet` | Comet uses a shared `("POOL", <event>)` topic across **every** Balancer-v1 deployment, so topic bytes alone cannot identify a pool. Since 2026-07-08 the decoder is contract-identity **GATED** (ADR-0035/0040, CS-026): a curated one-pool allowlist — `comet.MainnetGatedSet()`, today exactly the Blend BLND/USDC backstop `CAS3FL6T…` — is the trust root, and a copycat emitter now **fails closed** rather than landing rows. So a decode/recognition complaint on comet is more often "a real pool we haven't admitted" than "a schema change". | Confirm the emitting contract is in `comet.MainnetGatedSet()`; a legitimate new pool is admitted with `stellarindex-ops seed-protocol-contracts -config /etc/stellarindex.toml -source comet` (no redeploy). Foreign emitters surface via the completeness recognition audit. Rows written BEFORE the gate shipped came from the old topic-only decoder and were not retroactively re-verified. |
| `reflector` | Reflector is **three separate contracts** (DEX / CEX / FX) and has NO on-chain `twap` or `x_*` methods (we compute TWAP and cross-pair locally). | Check which Reflector contract is decoding. |
| `band` | Band's Soroban contract emits **zero events** — observed via `relay()` / `force_relay()` `InvokeContract` op args through the dispatcher's `ContractCallDecoder` interface (PR 168). Pair rates are at E18 scale; relayed single-asset rates are at E9. | Verify the `ContractCallDecoder` is wired. |
| `redstone` | Adapter emits topic `"REDSTONE"` events but the body carries NO `feed_id`. Feed IDs live in the tx's `write_prices(updater, feed_ids, payload)` op args — plumbed through `events.Event.OpArgs`. A feed_ids/updated_feeds length mismatch is **no longer an immediate refusal**: `resolveFeedAttribution` (`decode.go`) first tries the accepted-feed subset derived from the op's contract-data write keys, then payload-median alignment (`payload.go`, verified byte-exact on ledger 59,258,375). Only a failed or ambiguous recovery refuses — `ErrFeedIDCountMismatch` / `ErrAmbiguousSubset` — and refusing the whole event is deliberate (honest-blind beats misattributed). | Check the OpArgs plumbing first; then read WHICH sentinel the log names — `ErrAmbiguousSubset` means recovery ran and could not attribute uniquely, `ErrStateWriteFeedMismatch` means the claimed feed names disagreed with what the contract actually stored. |
| `sdex` (classic) | Post-P23 (mainnet 2025-09-03) every classic asset movement emits a unified transfer/mint/burn event. The decoder handles both post-P23 event SHAPES — the 3-topic SEP-41 form and the 4-topic CAP-67 form (4th topic `sep0011_asset`) — but a protocol bump can introduce a third. There is **no operations+effects fallback**: pre-P23 classic movement is not reconstructed on this path at all (that era is rebuilt out-of-band from the ClickHouse lake, `internal/sources/classicmovements` per ADR-0047/0048), so "missing pre-P23 classic rows" is never a decode-error symptom. | Check `protocolVersion` from `rpc-probe`. |
| Any SEP-41 token | `transfer` event data can be EITHER a simple `i128` OR a map containing `amount` + `to_muxed_id`. Type-test before `MustI128()`. | Type-test before `MustI128()`. |

When a Soroban DEX/oracle source decode-errors immediately after a
Stellar protocol bump or a known DEX redeploy: the source's WASM
likely changed event/topic shape. **Backfill is unsafe across the
upgrade boundary** until the WASM-hash audit re-runs (see
[`docs/operations/wasm-audits/`](../wasm-audits/) and
[`architecture/contract-schema-evolution.md`](../../architecture/contract-schema-evolution.md))
— flip `BackfillSafe = false` for that source until the audit log
shows the new WASM hash decodes cleanly.

## Mitigation

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
        `stellarindex-ops backfill -config /etc/stellarindex.toml -from N -to N -source <name>`,
        which **refuses** any source that is not `BackfillSafe` in
        `internal/sources/external/registry.go`.
      Either way, on r1 a re-derive is a heavy one-shot: run it under
      `/usr/local/sbin/run-heavy-job.sh`, one job at a time (CLAUDE.md
      "Heavy one-shot jobs on r1").
- [ ] Step 4 — if the cause is a regression (option 3): `git revert` the suspect commit and deploy. File an incident to retry the regressed change with a proper test.
- [ ] Verification: `rate(...decode_errors_total[5m])` drops back under the 1/sec threshold within 5 min of mitigation.

### Comms note when `class_drop_spike` co-fires

If `stellarindex_aggregator_class_drop_spike` fires alongside this
alert, the affected source has dropped out of the VWAP for one or
more pairs. The remaining sources continue to serve prices, but
the smaller consensus may produce elevated
`flags.divergence_warning` on the affected pairs. **Surface this
in status comms** — it explains why a consumer might see a
warning flag without a corresponding price disruption. Template:
"Affected pairs may show elevated `flags.divergence_warning`; price
is still served correctly from remaining sources." See
[drills/2026-04-sev2-soroswap-decode-regression.md](../drills/2026-04-sev2-soroswap-decode-regression.md)
for the canonical exercise of this pattern.

## Related

- `orphan-events.md` — adjacent failure mode (events well-formed but partnerless).
- `insert-errors.md` — downstream failure mode (events decoded OK but write-path broke).
- `source-stopped.md` — when the rate hits 100% of pulled events, effectively stopping the source.
- `internal/sources/*/decode.go` — per-source decoder.

## Changelog

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
