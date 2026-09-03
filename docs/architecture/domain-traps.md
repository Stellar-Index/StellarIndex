---
title: Domain traps — counter-intuitive facts about Stellar and our dependencies
last_verified: 2026-09-03
status: living doc
---

# Domain traps

Traps / counter-intuitive facts about Stellar and our dependencies.
If you're about to do something that touches any of these, the
linked design doc has the full detail.

- **Soroswap's `SwapEvent` has no post-state reserves.** Reserves
  come in the immediately-following `SyncEvent`. Correlate by
  `(ledger, tx_hash, op_index)`.
- **Phoenix emits 8 events per swap** (one per field with a 2-tuple
  topic `("swap", "<field>")`). A single swap reconstruction
  requires grouping all 8.
- **Comet uses a shared `("POOL", <event>)` topic across every pool
  contract**, not a per-protocol namespace. Any pubnet contract that
  deploys Balancer-v1 Comet code will look identical on the wire.
  **ADR-0035 contract-identity gating is COMPLETE (2026-07-08).**
  `soroswap` (pair/factory registry), `blend` (childgate), `phoenix`
  (curated set, 2026-07-02), `aquarius` (router-anchored: the router's
  `add_pool` events announce exactly the registry-API pool set, 2026-07-05),
  `defindex` (the factory `create` event does NOT carry the VAULT'S OWN
  address, so new vaults fail-close until operator-seeded — curated
  evidence-verified set, 2026-07-05. Its body DOES carry each asset's
  assigned BlendStrategy address, and strategies were self-registered
  from it between 2026-07-10 and 2026-08-25 — that was REMOVED (76418937,
  W8 6c) because the factory is public and the field is caller-supplied,
  so anyone could name an arbitrary address and poison the registry.
  Strategies are now admitted exactly like vaults: curated seed or an
  operator `protocol_contracts` row. The extraction survives only as a
  one-off audit tool. See docs/protocols/defindex.md and
  TestDecode_factoryCreate_doesNotSeedFromBody) and `comet` (curated one-pool allowlist —
  no KNOWN DEPLOYED mainnet factory; upstream ships a factory contract
  emitting `("LOG","NEW_POOL")`, so factory anchoring becomes available
  if one ever deploys; the only mainnet pool is Blend's BLND/USDC
  backstop, seeded in-code via `comet.MainnetGatedSet`; 2026-07-08, closes
  CS-026) all gate `Matches()` on contract identity. A future genuine
  comet pool must be operator-admitted (`seed-protocol-contracts -source
  comet` / a `protocol_contracts` row) before its events attribute —
  fail-closed, visible as an ADR-0033 recognition gap. →
  [docs/adr/0035-factory-anchored-contract-gating.md](../../docs/adr/0035-factory-anchored-contract-gating.md),
  [docs/adr/0040-completing-contract-gating.md](../../docs/adr/0040-completing-contract-gating.md)
- **Reflector v3 has no on-chain `twap` or `x_*` methods.** Some
  upstream docs imply it does; it doesn't. We compute TWAP and
  cross-pair locally.
- **Reflector is three separate contracts** (DEX / CEX / FX), not
  one.
- **Band stores pair rates at E18 scale**. Relayed single-asset
  rates are at E9.
- **Band's Soroban contract emits zero events.** A conventional
  topic-match Decoder never fires on Band. We observe the
  `relay()` / `force_relay()` InvokeContract call instead via
  the dispatcher's `ContractCallDecoder` interface (PR 168). Any
  future Soroban source that updates storage without publishing
  events plugs into the same hook — match by (contract_id,
  function_name), decode from op args.
- **Off-chain sources (CEX/FX) live in `internal/sources/external/`,
  not `internal/sources/<venue>/`.** They run their own goroutines
  speaking HTTPS / WebSocket to vendor APIs — parallel to the
  Galexie → dispatcher path, not under it. Source-class metadata
  (`exchange`/`aggregator`/`oracle`/`authority_sanity`) lives in
  `external.Registry` — a single Go map the aggregator queries at
  VWAP compute time to decide which sources contribute. Only
  `ClassExchange` contributes by default; aggregators and oracles
  are reported alongside but excluded (mixing them double-counts
  upstream markets or imposes their methodology on our output).
- **External-source amount scaling is NOT uniform.** On-chain
  sources stamp `canonical.Trade.BaseAmount` / `QuoteAmount` at
  per-asset decimals (XLM=7, Soroban tokens vary). Off-chain CEX +
  reference-aggregator sources normalise to a fixed **10^8** integer
  scale (`binance.externalAmountDecimals`), but **FX sources** use
  **10^6** (`DefaultDecimals = 6` / `AmountDecimals: 6`). Always
  read the per-source `Decimals` field; don't assume 10^8.
  Aggregator looks up `external.Lookup(trade.Source).Class` to know
  which side of the boundary a trade came from.
- **`massive` is the ACTIVE fiat-FX feed** (massive.com = Polygon's
  backend). It runs as the `internal/sources/external/forex` worker
  in the API binary and writes hourly fiat rates to `fx_quotes` —
  the USD-anchor behind per-trade usd_volume and the USD-anchored
  local-currency derivation (ADR-0051). There is **no `/v1/currencies`
  route** — it was removed for want of consumers; the FX snapshot
  survives only as the in-process `CurrenciesReader` seam, so don't go
  looking for an HTTP surface.
  `polygon-forex` / `exchangeratesapi` are same-role trades-path
  connectors, currently disabled; `ecb` is `ClassAuthoritySanity`
  (standby cross-check, NOT a VWAP contributor). Full detail in the
  registry comment at `internal/sources/external/registry.go`.
- **Stablecoin fiat-proxy is aggregator policy, not decoder
  policy.** Ingest stores the real pair (`XLM/USDT`, `XLM/USDC`).
  The aggregator maps `USDT→USD`, `USDC→USD`, `DAI→USD`,
  `PYUSD→USD`, `USDP→USD`, `EURC→EUR`, `EUROC→EUR`, `EUROB→EUR`,
  `MXNe→MXN` at VWAP compute time (full map: `internal/aggregate/stablecoin.go`).
  Eager normalisation at ingest would hide a depeg event; late
  binding keeps data honest.
- **Redstone Adapter DOES emit events** (topic `"REDSTONE"`) — one
  per batch push containing all updated feeds. Subscribe rather
  than poll all 19 per-feed contracts.
- **Redstone's event body carries no feed_id.** `WritePrices
  { updater, updated_feeds: Vec<PriceData> }` gives prices +
  timestamps, not which feed each entry is. Feed IDs live in the
  tx's `write_prices(updater, feed_ids, payload)` InvokeContract
  op args — plumbed through `events.Event.OpArgs` (PR 166). The
  adapter's freshness verifier can filter `updated_feeds` to a
  subset of `feed_ids`; zip positionally when lengths match. When
  they DON'T match (a real, ongoing class — 1,626 events on the
  2026-07-29 full verify), the signed payload in args[2] recovers
  the mapping: the adapter stores each accepted feed's signer-value
  MEDIAN, so each surviving price must equal exactly one candidate
  feed's payload median at its package_timestamp
  (`internal/sources/redstone/payload.go`; verified byte-exact on
  ledger 59,258,375). Non-unique attribution refuses the whole
  event (`ErrAmbiguousSubset`) — honest-blind beats misattributed.
  Any new decoder that needs tx args follows the same
  `events.Event.OpArgs` pattern.
- **Oracle decoders never DROP an unmapped symbol.** Since the
  capture-totality change (PR-2, #247) reflector / redstone / band
  record a symbol or feed_id that maps to no canonical asset
  VERBATIM as a `raw:<symbol>` row (`canonical.AssetOracleRaw`,
  `internal/canonical/asset_raw.go`) instead of skipping the slot —
  which is also what keeps the synthetic `op_index` stable, since it
  is derived from the vector POSITION. Raw rows are RECORD-layer
  only: `Pair.Validate` refuses one as a pair leg, `supply.AssetKey`
  refuses one as a supply key, and nothing in the interpretation
  layer (VWAP, divergence) may read one — scans over
  `oracle_updates` that don't key by canonical asset must filter on
  `Asset.IsMapped()` / `asset NOT LIKE 'raw:%'`.
  `/v1/oracle/streams` omits them unless `include_unmapped=true`
  (every reading carries a `mapped` discriminator; `/v1/oracle/latest`
  returns one only when asked for by its exact `raw:` key).
  `stellarindex_source_unknown_symbols_total` still counts them —
  it now means "recorded as raw", a mapping gap for the allow-list
  owner, and `stellarindex_ingestion_oracle_unknown_symbols` tickets
  on it. Widening the allow-list re-derives the same PK and promotes
  `raw:X` → `crypto:X` in place on replay, so no capture is lost.
  Design: docs/design/oracle-capture-totality-design.md.
- **Post-P23 (Whisk, mainnet 2025-09-03) every classic asset
  movement emits a unified transfer/mint/burn event with a 4th
  `sep0011_asset` topic.** Our decoder handles both event SHAPES —
  the 3-topic SEP-41 form and the 4-topic CAP-67 form (the 4th topic
  is `sep0011_asset`). It does NOT, however, parse pre-P23 classic
  movements: there is no operations+effects fallback for the era
  before unified events existed, so historical classic-asset movement
  before P23 is not reconstructed from this path.
- **SEP-41 `transfer` data can be EITHER a simple `i128` OR a map**
  containing `amount` + `to_muxed_id`. Type-test before
  `MustI128()`.
- **stellar/go monorepo was archived 2025-12-16.** The new Go SDK
  lives at `github.com/stellar/go-stellar-sdk`. Horizon, Galexie,
  stellar-rpc, stellar-archivist are each in their own repos now.
- **`withObsrvr/cdp-pipeline-workflow` has verified correctness
  bugs** in its i128 decoding and SDEX trade extraction. We do
  **not** inherit from it.
- **stellar-rpc is NOT in our production ingest path.** The
  standalone `stellar-rpc` and `stellar-core` watcher services were
  removed from r1 on 2026-04-23, along with the core prometheus
  exporter. `stellar-core` itself still runs on r1 — but only as a
  *captive-core* subprocess spawned by Galexie
  (`/usr/bin/stellar-core … --metadata-output-stream fd:3`) — which
  is the supported Galexie deployment shape per ADR-0002. The
  indexer reads Galexie's MinIO output directly via
  `go-stellar-sdk/ingest.ApplyLedgerMetadata`. If you catch yourself
  writing `rpc.GetEvents` for ingest, stop and read
  [docs/architecture/ingest-pipeline.md](../../docs/architecture/ingest-pipeline.md). →
  [docs/operations/r1-deployment-state.md](../../docs/operations/r1-deployment-state.md)
- **Soroban DeFi contracts upgrade in place.** Soroswap / Phoenix /
  Aquarius / Reflector can each `update_contract` without changing
  their contract address. Event body schemas (field names, types,
  arity) and topic shapes can change across an upgrade. Live ingest
  only sees current WASM; **backfill sees every prior version** that
  ran for the replayed range. Decode by Map-field-name not position,
  dispatch on topic[0] symbol not contract address, and gate
  backfill behind a per-WASM-hash decoder audit. →
  [docs/architecture/contract-schema-evolution.md](../../docs/architecture/contract-schema-evolution.md)
- **`/v1/assets/{slug}` returns two different wire shapes.**
  When `{slug}` is a verified-currency catalogue slug (`usdc`,
  `eurc`, `aqua`, …) the handler returns `GlobalAssetView`
  (Stellar-asset identity + headline USD price + Stellar issuance);
  when it's a canonical asset_id (`USDC-G…`, `native`, `C…`,
  `fiat:USD`) it returns `AssetDetail` (per-Stellar-asset detail).
  Same route, two shapes — Go's mux dispatches on the catalogue
  lookup before parsing as canonical. Clients distinguish via
  wire-shape discriminators (`ticker` + `price_usd` vs `asset_id`
  + `type`). The old cross-chain `networks[]` array + the
  `/v1/assets/{slug}/{network}` drill-down were removed in the
  Stellar-focus refactor (docs/architecture/stellar-focus-refactor-plan.md).
- **`internal/currency` is the verified-currency trust surface.**
  Hand-curated YAML at `internal/currency/data/seed.yaml`, embedded
  in the binary via `//go:embed`. Adding a verified currency means
  a code change + redeploy. The catalogue feeds the CG poller's
  ticker map, the indexer's aggregator pair set, the
  unverified-collision warning on `/v1/assets/{id}`, the
  `/v1/assets/verified` listing endpoint, and the explorer's
  verified-badge UI. Do NOT auto-populate from CG / CMC — the
  whole point is that it's hand-vetted.

---

---

The one-line rule form of each of these is in `AGENTS.md`. This page is
the evidence and the reasoning behind the rules; read it when you need to
know *why*, or when the rule does not obviously cover your case.
