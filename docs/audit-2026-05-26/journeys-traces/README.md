# Journey Traces

One trace file per journey from [03-journeys.md](../03-journeys.md).
Use the template at [_template.md](_template.md).

File naming: `J<NN>-<short-description>.md`.

Each trace is one of:

- **Data-plane** (J01..J20): user-visible data paths
- **Operator-plane** (J21..J32): operator workflows
- **Adversarial** (J33..J40): hostile inputs / abuse vectors

A trace is `done` when:
1. all hops are filled with file:line refs
2. all failure modes are documented
3. tests that exercise it are listed (or absence documented as a
   finding under W15)
4. at least one live R1 trace excerpt is included where the
   journey runs in production

## Status

| Journey | Status | Trace file |
| --- | --- | --- |
| J01 Soroswap trade ingest | `todo` | — |
| J02 Phoenix 8-event trade ingest | `todo` | — |
| J03 SDEX classic trade ingest | `todo` | — |
| J04 Oracle ingest (Reflector/Redstone/Band) | `todo` | — |
| J05 Closed-bucket price path | `todo` | — |
| J06 Triangulation | `todo` | — |
| J07 Stablecoin fiat-proxy | `todo` | — |
| J08 CCTP bridge-out | `todo` | — |
| J09 Rozo intent-bridge | `todo` | — |
| J10 soroban_events capture (ADR-0029) | `todo` | — |
| J11 soroban_events SQL backfill (×6) | `todo` | — |
| J12 Live FX ingest | `todo` | — |
| J13 SEP-1 resolution | `todo` | — |
| J14 Stripe webhook | `todo` | — |
| J15 Customer webhook fanout | `todo` | — |
| J16 SEP-10 authentication | `todo` | — |
| J17 Supply snapshot | `todo` | — |
| J18 Verify-archive bootstrap + resume | `todo` | — |
| J19 Divergence check (Chainlink) | `todo` | — |
| J20 Anomaly freeze | `todo` | — |
| J21 Backfill — Soroban-era | `todo` | — |
| J22 soroban-events fill walk | `todo` | — |
| J23 Cold-tier read | `todo` | — |
| J24 WASM history audit | `todo` | — |
| J25 WASM audit documentation | `todo` | — |
| J26 SLA probe | `todo` | — |
| J27 Release cut | `todo` | — |
| J28 Deploy to R1 | `todo` | — |
| J29 Cursor-stuck diagnosis | `todo` | — |
| J30 r1-smoke health check | `todo` | — |
| J31 Verify decoders | `todo` | — |
| J32 Scan soroban events diagnostic | `todo` | — |
| J33 Decoder DoS via malformed XDR | `todo` | — |
| J34 Aggregator poison via hostile source | `todo` | — |
| J35 Rate-limit bypass via forged X-Real-IP | `todo` | — |
| J36 API key exfiltration | `todo` | — |
| J37 Webhook SSRF | `todo` | — |
| J38 Hostile ledger stream cancel | `todo` | — |
| J39 SQL injection via asset slug | `todo` | — |
| J40 Stripe webhook replay | `todo` | — |
