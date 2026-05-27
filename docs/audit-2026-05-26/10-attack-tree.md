# Adversarial Attack Tree

This document enumerates the abuse vectors the auditor must test
against. Each leaf node is a concrete attack with:

- a target component
- a precondition
- the expected attacker outcome on success
- the existing defence (if any) and where to find it
- evidence / disposition after testing

For every leaf, after testing record:

- `tested:` yes/no
- `result:` defended / partial-defence / undefended
- `evidence:` `EV-####` ref
- `finding:` `F-####` ref if undefended or partial

## A. Data integrity attacks

### A1. Hostile XDR payloads

- **A1.1** SwapEvent with i128 amounts at `int64.Max + 1`.
  Defence: ADR-0003 (NUMERIC end-to-end). Test: malformed
  fixture in `test/fixtures/`.
- **A1.2** SyncEvent missing after SwapEvent (Soroswap).
  Defence: Soroswap decoder pairing logic. Test: drop fixture.
- **A1.3** Phoenix swap with 7 of 8 events. Defence: Phoenix
  grouping by `(ledger, tx_hash, op_index)`. Test:
  malformed-input test.
- **A1.4** Comet event with topic looking like Comet but emitted
  from a non-Blend pool contract. Defence: documented in
  CLAUDE.md; downstream filter on `Trade.Source = "comet"` +
  contract-address context.
- **A1.5** SEP-41 transfer body that's a map
  (`{amount, to_muxed_id}`) vs raw i128. Defence: type-test
  before MustI128.
- **A1.6** Reflector update event with malformed scvec.
  Defence: scval helper bounds checks.
- **A1.7** Redstone WritePrices where `len(updated_feeds) !=
  len(feed_ids)`. Defence: `ErrFeedIDCountMismatch` skips
  event.
- **A1.8** Band relay() args missing or shape-changed. Defence:
  ContractCallDecoder strict typing.
- **A1.9** WASM upgrade mid-backfill changes event schema
  (decoder reads field by name, then field is renamed).
  Defence: per-WASM-hash audit gates `BackfillSafe`.
- **A1.10** CAP-67 unified event with malformed `sep0011_asset`
  topic.
- **A1.11** Account entry mutation that flips trustline + adds
  claimable simultaneously across 3 ledgers (race in observers).
- **A1.12 (NEW)** CCTP deposit_for_burn with malformed
  `destination_token_messenger` BytesN<32>. Defence: scval
  AsBytes + length check.
- **A1.13 (NEW)** Rozo payment with empty `destination`.
  Defence: `InsertRozoEvent` rejects empty destination at
  storage layer (`internal/storage/timescale/rozo_events.go`).
- **A1.14 (NEW)** soroban_events row with topic_0_xdr that's
  not a valid SCVal. Defence: Capture validates before
  insert; corrupt row never enters the table.
- **A1.15 (NEW)** WASM upgrade between backfill chunks while
  `cctp-backfill` is mid-flight. Each chunk reads same
  contract; chunks 0-5 see old WASM, chunks 6-11 see new.
  Decoder fails on chunks 6-11. Defence: WASM audit (W24)
  must show ZERO upgrades over the entire backfill range
  before `BackfillSafe = true`.

### A2. Hostile vendor responses

- **A2.1** Binance returns wrong-pair price with right symbol
  (e.g. XLMUSDT response for ETHUSDT request). Defence: pair
  check in parse.go.
- **A2.2** Coinbase returns 200 with empty body. Defence:
  parse.go null check.
- **A2.3** Coingecko returns price with `last_updated`
  timestamp in the future (clock skew or attacker). Defence:
  clock-skew guard rail.
- **A2.4** ECB returns FX with valid-looking but wrong currency
  pair.
- **A2.5 (NEW)** Chainlink HTTP RPC returns valid signature on
  wrong-pair feed. Defence: pair binding in
  `internal/divergence/chainlink.go`.
- **A2.6 (NEW)** CMC paid tier returns rate-limit while we
  hold market-cap data customers depend on. Defence: cache
  + fallback.

### A3. Database/cache poisoning

- **A3.1** ON CONFLICT shape mismatch (rc.78→rc.79 repeat).
  Writer's ON CONFLICT column list doesn't match PK; every
  insert returns 42P10. Defence: per-migration audit loop §7
  check 9.
- **A3.2** Redis cache key collision via crafted asset slug.
  Defence: `internal/cachekeys.keys.go` round-trip stability.
- **A3.3** Prewarmer/handler arg drift produces stale cache
  reads. Defence: per-route audit loop §8 check 11.
- **A3.4 (NEW)** soroban_events PK accepts duplicate
  (ledger_close_time, ledger, tx_hash, op_index, event_index)
  via concurrent inserts. Defence: PK constraint + ON
  CONFLICT DO NOTHING.

## B. Identity/Auth/Money attacks

### B1. API key

- **B1.1** Key in URL → server logs. Defence: keys via
  Authorization header only; no URL surface.
- **B1.2** Key replay across tiers. Defence: tier carried in
  hashed key store, not user-presented.
- **B1.3** Key enumeration via timing. Defence: constant-time
  compare in `internal/auth/apikey.go`.
- **B1.4** Key string in error response. Defence: error
  envelope never includes the key.
- **B1.5 (NEW)** Operator-minted key (via
  `cmd/ratesengine-ops/mint_key.go`) lacks billing tier;
  defaults to highest tier silently. Defence: explicit
  `--tier` flag required.

### B2. SEP-10 auth

- **B2.1** Server challenge stolen via MitM → replayed.
  Defence: TLS + signed-challenge with timestamp.
- **B2.2** JWT extended via algorithm-confusion (HS256 vs
  RS256). Defence: strict alg verification.
- **B2.3** JWT replay after rotation. Defence: kid in JWT;
  retired kid keys still accept old JWTs until expiry.

### B3. Stripe webhook (NEW)

- **B3.1** Unsigned webhook accepted. Defence:
  `Stripe-Signature` HMAC verification.
- **B3.2** Replay valid webhook. Defence: idempotency key
  (Stripe Event.id) tracked per-webhook.
- **B3.3** Crafted event with valid signature from a different
  endpoint's secret. Defence: per-endpoint secret separation.
- **B3.4** Refund-bypass via webhook race: customer triggers
  refund, then immediately new charge before our DB
  reconciles. Defence: state machine in `stripe_webhook.go`.

### B4. Rate-limit bypass

- **B4.1** Forged X-Real-IP. Defence: Caddy trusted-proxy list
  (ADR-0025); only Cloudflare IPs accepted.
- **B4.2** Spinning up new anonymous IPs from same residential
  range. Defence: per-IP-net rate limit.
- **B4.3** Signing up cheap tier then aggregating. Defence:
  signup IP throttle (`internal/auth/signup_ip_throttle.go`).

### B5. Webhook SSRF (NEW)

- **B5.1** Customer registers `http://localhost:5432` as their
  webhook URL → fanout calls localhost → discovers internal
  postgres. Defence: `internal/customerwebhook/ssrf.go`.
- **B5.2** Same with 169.254.169.254 (AWS metadata). Defence:
  ssrf.go private-IP guard.
- **B5.3** DNS-rebinding: customer URL resolves to public IP
  on first check, private IP on second. Defence: pin
  resolution before delivery.

## C. Availability attacks

### C1. Resource exhaustion

- **C1.1** `/v1/markets` scanning 40M+ trades. Defence: query
  limits + indexes.
- **C1.2** Large date range on `/v1/history`. Defence:
  pagination + max window.
- **C1.3** SSE subscribers that don't drain. Defence: slow-
  consumer disconnect; ring buffer overflow drop.
- **C1.4 (NEW)** Customer registers 1000 webhooks → fanout
  storm. Defence: per-customer subscription cap.
- **C1.5 (NEW)** soroban_events table grows unbounded (the
  Soroban era is hundreds of millions of events). Defence:
  retention policy (verify it's set); cold-tier offload (W30).

### C2. Validator-network abuse

- **C2.1** Hostile peer in core quorum. Defence: ADR-0012
  (quorum set composition).
- **C2.2** Withholding ledger close → all regions stall.
  Defence: multi-source (R2 reads from aws-public-blockchain
  S3 direct; R3 hybrid).
- **C2.3 (NEW)** Trailing-edge missing-file race (galexie
  hasn't uploaded yet). Defence: rc.81
  `TolerateTrailingMissing` for bounded walks; live tail uses
  RetryWait.

### C3. Aggregator class drop

- **C3.1** Single source goes silent → VWAP weight redistributes
  → silent skew. Defence: class-drop alert.
- **C3.2** All exchange-class sources drop → aggregator-class
  fallback. Defence: alert + degraded-mode flag in response.
- **C3.3** FX feed lags → triangulated assets silently use
  stale FX. Defence: FX snap fallback + stale-FX flag.

### C4. Storage exhaustion (NEW)

- **C4.1** Root partition (`/`) fills via journal/log growth.
  **OBSERVED 2026-05-26 23:14 UTC**: `/dev/md1` 49G is 100%
  full. Defence: log rotation + journal vacuum + root-disk
  alert.
- **C4.2** ZFS pool exhaustion (2026-05-17 incident). Defence:
  per-pool free-space alert.
- **C4.3** MinIO bucket growth → eviction lifecycle missing.
  Defence: MinIO retention policy.

## D. Operability/recoverability attacks

### D1. Deploy + rollback

- **D1.1** Failed deploy doesn't roll back. Defence:
  deploy.yml playbook explicit rollback path.
- **D1.2** Backup retention exceeded; last good is gone.
  Defence: 5-most-recent retention in deploy playbook.
- **D1.3** Tag pushed without rebuild. Defence: cut-release.sh
  refuses dirty tree / out-of-sync main.

### D2. Migration disasters

- **D2.1** Up migration applies, down migration broken. Defence:
  W09 audit: every up has a working down.
- **D2.2** Migration runner skips by mistake.
- **D2.3 (NEW)** Per `feedback_migrations_not_auto_deployed`:
  deploy.yml does NOT auto-apply migrations; operator must
  scp + run `ratesengine-migrate up`. Risk: operator forgets;
  binary starts with mismatched schema. Defence: indexer
  startup health check vs `schema_migrations`.

### D3. Cursor lag

- **D3.1** Ingest cursor stuck. Defence: `cursor-stuck`
  runbook; first hypothesis = galexie tip lag (mc stat).
- **D3.2 (NEW)** AsyncSink shutdown drops cursor advance race.
  Defence: rc.80 back-pressure semantics + ctx-cancel
  watchdog.

### D4. Verify-archive disabled

- **D4.1** Trailing-edge bug stops timer. Defence: rc.81
  TolerateTrailingMissing flag.
- **D4.2 (NEW)** Per-chunk state file corrupted.
- **D4.3 (NEW)** State file references retired prior `to`
  that's now below current tip; resume can't proceed.

## E. Observability attacks (silent-failure surface)

### E1. Alert dead-letter

- **E1.1** Alertmanager config rejects new rule but doesn't
  emit warning.
- **E1.2** Rule references non-existent metric (rename drift).
  Defence: per-alert audit loop §10 check 1.
- **E1.3** Runbook missing for active alert. Defence: `lint-docs.sh`
  monitoring-rules check.

### E2. Metric drift

- **E2.1** Operator drops a job (e.g. job 1000 trades CAGG
  compression) in postgres and forgets to re-enable; CAGG
  compression silently stops; trades hypertable grows. Defence:
  `compression-lag` alert.
- **E2.2** Prometheus retention silently truncates data.
  Defence: retention alert (NOT YET VERIFIED — note).

### E3. Logs

- **E3.1** Loki retention exceeds disk → falls back to silent
  drop.
- **E3.2** Structured-log field drift between code emit and
  Grafana query.

## F. Multi-region attacks (NEW — relative to baseline)

### F1. Region divergence

- **F1.1** R1 vs R2 vs R3 serve different `/v1/price` for same
  asset/quote. Defence: closed-bucket invariant (ADR-0015) +
  cross-region check.
- **F1.2** R2 reads from aws-public-blockchain S3 falling
  behind. Defence: tiered datastore freshness check.

### F2. Cold-tier read attacks (NEW)

- **F2.1** Operator enables ADR-0027 §3 without §4 →
  galexie-archive grows unbounded (no trim) → disk fills.
  Defence: `feedback_cold_tier_premature_enable` rule;
  operator runbook discipline.
- **F2.2** Cold-tier endpoint compromised → hostile XDR
  returned. Defence: hash-chain verification at every chunk
  (verify-archive).

## G. Granular-coverage attacks (NEW — W35)

### G1. Partial-event decoder

- **G1.1** Source X claims contract C, decodes events
  A/B/C/D, silently drops E. API consumers querying for E
  see empty. Defence: W35 register; per-decoder coverage
  audit; ratesengine-ops verify-decoders subcommand (if it
  exists — see W13 audit).
- **G1.2** Soroban contract upgrade adds new event symbol the
  decoder doesn't know about. Defence: WASM audit (W24).

## H. Repository / supply-chain attacks

### H1. Dependency

- **H1.1** A direct dep gets back-doored upstream. Defence:
  pinned versions in `VERSIONS.md`; `go mod verify` in CI.
- **H1.2** A GitHub Action gets back-doored (e.g.
  actions/setup-go SHA-pin compromise). Defence:
  `lint-actions-pinning.sh` enforcement.
- **H1.3 (NEW — 2026-05-26 incident)** GH Actions CDN dropped
  setup-go@<sha> for 30+ minutes; release.yml failed three
  times in a row. Defence: nothing automatic; operator
  retries. Audit: should we add a workflow-side retry?

### H2. Secrets leak

- **H2.1** Secret in a commit. Defence: gitleaks pre-commit +
  CI scan.
- **H2.2** Secret in a doc. Defence: gitleaks scan covers all
  tracked files.
- **H2.3** Secret in a journal (operator typed `psql` with
  password). Defence: systemd journal access ACL.
