# Mandatory Journeys

Each journey must produce:

- evidence entries
- cross-file interaction entries
- any findings triggered by the walk

## J1. On-Chain Trade Ingest

Path:

1. Galexie/MinIO object layout
2. `internal/ledgerstream`
3. `internal/dispatcher`
4. venue decoder
5. `internal/pipeline.PersistEvents`
6. `internal/storage/timescale.InsertTrade`
7. aggregator read path
8. API read path

Use for:

- Soroswap
- Aquarius
- Phoenix
- Comet
- SDEX

## J2. Band Event-Less Oracle Path

Path:

1. ledger operation
2. dispatcher contract-call path
3. Band decoder
4. oracle update persistence
5. API oracle read path

## J3. Redstone Event Plus Op-Args Path

Path:

1. event extraction
2. `events.Event.OpArgs`
3. Redstone decoder zip logic
4. oracle update persistence
5. downstream API surfaces

## J4. External Venue Trade Path

Path:

1. external runner
2. venue streamer/poller
3. emitted trade shape
4. shared sink channel
5. Timescale trade insert
6. class-filter and stablecoin-proxy behavior
7. API exposure

## J5. Closed-Bucket Price Path

Path:

1. raw trades
2. CAGG or latest closed-bucket read
3. freeze/confidence/divergence enrichment
4. `/v1/price`
5. `/v1/price/stream`

## J6. Rolling Tip Path

Path:

1. latest-trade or short-window source
2. rolling-window compute
3. stale/fallback semantics
4. `/v1/price/tip`
5. `/v1/price/tip/stream`

## J7. Raw Observation Path

Path:

1. latest trade per source
2. source filtering
3. `/v1/observations`
4. `/v1/observations/stream`

## J8. Asset Metadata Path

Path:

1. asset identity
2. storage asset lookup
3. issuer home-domain overlay
4. SEP-1 fetch/cache/validation
5. `/v1/assets/{id}`
6. `/v1/assets/{id}/metadata`

## J9. API Key Auth Path

Path:

1. auth middleware extraction
2. Redis validator lookup
3. subject propagation
4. account routes
5. rate-limit interaction

## J10. SEP-10 Auth Path

Path:

1. config/env bootstrap
2. challenge issue
3. signed challenge verify
4. JWT issue
5. JWT verify on request path
6. account route behavior under SEP-10 subject

## J11. Supply Path

Path:

1. supply derivation logic
2. storage snapshot persistence
3. latest/history read path
4. asset-detail F2 fields
5. operator supply audit CLI

## J12. Backfill Path

Path:

1. CLI flag parse
2. ledger range selection
3. dispatcher reuse
4. shared sink path
5. storage writes
6. effects on aggregates and cursors

## J13. Archive Completeness Path

Path:

1. archive scan
2. gap report
3. fix workflow
4. verify workflow
5. systemd/timer/monitoring wiring

## J14. Cross-Region Determinism Path

Path:

1. region API query
2. same-window request generation
3. byte-equality comparison
4. exported monitor metrics
5. alert/runbook mapping

## J15. Hostile Paths

Minimum hostile cases:

- malformed request input
- invalid asset/pair parsing
- empty Redis / Redis outage
- unavailable Postgres
- no trades / truncated trade windows
- all trades filtered as outliers
- stale or missing archive segments
- invalid auth credentials
- expired SEP-10 JWT
- duplicate insert and replay paths
