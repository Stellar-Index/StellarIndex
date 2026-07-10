// Package discovery auto-detects new SEP-41 Soroban tokens AND
// oracle-suggestive contracts (event-emitting or event-less) from
// the ingest stream so the engine can flag them for operator review
// without a code change.
//
// Background — SEP-41 (the original capability): every SEP-41 token
// contract on pubnet emits events matching a small set of topic
// shapes (`transfer`, `mint`, `burn`, `clawback` per the SEP-41
// spec). When the dispatcher observes one of these events from a
// contract id we don't yet know about, this package records it in
// `discovered_assets` so operators can:
//
//   - audit which contracts have been seen recently (rate-of-arrival
//     of new tokens informs whether the network is busy or
//     suspicious activity is happening);
//   - bootstrap downstream wiring (supply tracking, decoder
//     registration, asset-detail metadata fetch) without manual
//     contract-id curation.
//
// Broadened 2026-07-10 (docs/architecture/generic-oracle-sep-onboarding.md
// §3(b)) with two oracle-suggestive sniffers, same table, same
// sighting-only discipline:
//
//   - [SniffOracleEvent] — a wider topic[0] symbol set drawn from
//     the investigation's ClickHouse census (`price`, `prices`,
//     `lastprice`, `oracle`/`Oracle`/`ORACLE`, `REDSTONE`,
//     `StandardReference`, …; see [oracleEventSymbols] for the full
//     list). Flags a NEW oracle-shaped contract on the event path
//     the same way [Sniff] flags a new SEP-41 token.
//   - [SniffOracleCall] — the event-less-oracle half. Band's Soroban
//     StandardReference proves a real Stellar oracle can update
//     storage via `relay()`/`force_relay()` without publishing any
//     event, which made it structurally invisible to a topic-only
//     sniffer. SniffOracleCall matches InvokeContract calls by
//     function name instead (`relay`, `force_relay`, `write_prices`,
//     `lastprice`, `price`, `prices`, `x_last_price`; see
//     [oracleCallFunctions]), so a future Band-alike under a
//     different contract id still gets sighted.
//
// ALL THREE sniffers are pure observation: they record a sighting in
// `discovered_assets` (distinguished by the [Kind] column) and
// NEVER decode, attribute, or feed price/trade data — the same
// "sighting-only, fail-closed, operator-triage" discipline the
// SEP-41 sniffer has had in production since its original ship date
// (ADR-0035 doctrine: discovery is not attribution). A discovery hit
// becoming a real source still goes through the normal
// docs/contributing/add-onchain-source.md recipe — contract-identity
// gating, WASM audit, every-event completeness — same as reflector /
// redstone / band. False positives are expected and are the operator
// review step's job to catch (the investigation's own census found
// several against this exact symbol set: a beef-traceability anchor
// on `update`, dead RedStone test deployments, tutorial contracts).
//
// Discovery is read-only on the input side: the sniffers inspect an
// [events.Event] (or, for the call path, the invoked contract id +
// function name) and report whether it matches a watched shape
// without modifying anything. The [Recorder] interface persists new
// contract ids; production wiring is a Postgres-backed adapter
// against the discovered_assets hypertable.
//
// Package surface (current):
//
//   - [Sniff] — pure function that classifies an event's SEP-41
//     event-type based on topic[0]. Returns (hit, ok) where ok=false
//     for non-SEP-41 events. Kind == [KindSEP41].
//   - [SniffOracleEvent] — pure function, same topic[0] shape, wider
//     oracle-suggestive symbol set. Kind == [KindOracleEvent].
//   - [SniffOracleCall] — pure function over (contract_id,
//     function_name) from the ContractCallContext path. Kind ==
//     [KindOracleCall].
//   - [Kind] — discriminates which sniffer produced a [Hit].
//   - [Recorder] — write-side interface for persistence.
//   - [SEP41EventType] — enum identifying which SEP-41 event was
//     observed (KindSEP41 hits only).
//
// Wired today:
//
//   - Dispatcher integration:
//     [internal/dispatcher/dispatcher.go]'s `dispatchOne` calls both
//     [discovery.Sniff] and [discovery.SniffOracleEvent] on every
//     event flowing through, before decoder dispatch;
//     `dispatchContractCall` calls [discovery.SniffOracleCall] on
//     every InvokeContract call (top-level and nested sub-calls) the
//     dispatcher observes, independent of whether any
//     ContractCallDecoder is registered.
//   - Postgres-backed Recorder:
//     [internal/storage/timescale/discovery.go] implements
//     [Recorder] against the `discovered_assets` hypertable (with a
//     `discovery_kind` column since migration 0103);
//     `cmd/stellarindex-indexer/main.go` adapts the Store to the
//     interface. One shared [AsyncSink] instance carries all three
//     sniffers' hits — no parallel storage/reporting surface.
//   - Ops command: `stellarindex-ops discovery` (see
//     `internal/ops/discovery/discovery.go`) — list, recent-window,
//     per-source counts, now including the discovery_kind column.
//   - Alert metric: `stellarindex_ingestion_discovery_drops` per
//     `deploy/monitoring/rules/ingestion.yml` — fires when
//     discovery writes start failing, for any of the three sniffers
//     (the underlying counters are unlabeled and shared).
package discovery
