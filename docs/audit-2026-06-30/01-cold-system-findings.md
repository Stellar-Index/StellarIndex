---
title: Audit 1 — Cold system findings register (CS-###)
status: in progress (pre-execution recon findings banked; systematic execution pending plan ratification)
---

# Cold system findings — register

Severity per [README](README.md). Each finding names a concrete failing
scenario or it isn't Critical/High. The **Cleared** section records flags that
were investigated and found to be non-issues — recording these prevents
re-litigating them and documents the adversarial-verification discipline (the
surface mapper's concrete flags had a ~50% false-positive rate).

## Pre-execution recon findings

### CS-001 — Live GCP service-account key in the working tree — **Medium**
- **Location:** `./rates-engine-data-validation-0603331b2417.json` (repo root).
- **Verified:** Real SA key (`type: service_account`, contains `private_key`,
  `client_email` @ `rates-engine-data-validation.iam.gserviceaccount.com`). It is
  **gitignored and NOT git-tracked** — so it is *not* in repo history and gitleaks
  (history scan) correctly never saw it. (Private-key material was NOT inspected
  or printed.)
- **Why it matters:** a live credential sitting inside the repo working tree is a
  footgun — one `.gitignore` edit, `git add -f`, stray `tar`/backup, or editor
  plugin away from exposure. The project name (`rates-engine-…`) is the *old*
  brand, suggesting a possibly-stale data-validation SA.
- **Failing scenario:** if `.gitignore` is reorganized (common during refactors)
  the key gets committed; or a "zip the repo to share" leaks it.
- **Fix:** move the key OUT of the repo directory (e.g. `~/.config/…`); confirm
  whether the SA is still used at all; **rotate the key** if there's any chance it
  was ever shared/synced; add a CI check that no `*.json` with a `private_key`
  field exists in the tree.

### CS-003 — `internal/platform/postgresstore` near-zero test coverage — **Medium (confirm in execution)**
- **Location:** `internal/platform` (+`postgresstore`): mapper reports ~2489 src
  LOC vs ~54 test LOC — the customer-account / API-key / webhook persistence store.
- **Why it matters:** this is the store behind paid accounts, key issuance, and
  webhook config — exactly where IDOR / wrong-tenant / nil-time bugs (cf. the
  rek_/sip_ prefix + 2055-last-used + magic-link nil-Now bugs already seen) live.
- **Next:** A18 execution — confirm the ratio, then read the store for tenant-
  scoping on every query + the zero-time guard class.

### CS-005 — CLAUDE.md repo-map under-counts `internal/` ~3× — **Low (doc drift)**
- **Location:** `CLAUDE.md` "Repo map" (~30 packages) vs actual ~95
  packages/subpackages (`internal/api/*`, `aggregate/*`, `sources/*`, `storage/*`
  subpkgs, `xdrjson`, `platform/postgresstore`, `dispatcher/statsflush` undocumented).
- **Why it matters:** CLAUDE.md is the AI-agent entry point + freshness-checked in
  CI, yet materially incomplete — agents (and auditors) under-scope.
- **Fix:** regenerate the map or note it's a curated subset; ideally a CI check
  that every `internal/*` package appears.

### CS-007 — ADR-0003 i128 enforcement TOOLING is claimed but absent — **Low**
- **Location:** `docs/adr/0003-i128-no-truncation.md:128` claims "Lint rule in
  `.golangci.yml` (via a small custom analyzer) flags [int64 truncation]"; the
  migration-status BIGINT refusal at 0003:116 is likewise claimed. **Verified:**
  no such analyzer in `.golangci.yml`, no `tools/`/analyzer package, no migration
  type-lint in `scripts/ci/`. Reconfirms prior `D2-10` (audit-2026-06-11).
- **Mitigation that holds:** the *runtime* discipline is real — a tree-wide scan
  for `int64(…Lo/…Hi/Int128Parts)` truncation sites found **zero**, and
  `ErrI128Overflow` guards the parse path. So this is a missing *guard-rail*, not
  an active bug: a future truncation would not be caught at CI time.
- **Fix:** build the analyzer + migration lint (launch-todo P4-6), or downgrade
  the ADR's claim to match reality. (Doc-vs-code drift, CC-7.)

### CS-006 — `internal/hashdb` shipped with zero production callers — **Info**
- **Location:** `internal/hashdb` (226 src LOC). CLAUDE.md itself admits "LIBRARY
  ONLY — currently has zero production callers; the ADR-0033 'feeder' role is
  aspirational." So this is *acknowledged*, but it's dead weight that implies a
  completeness guarantee (drift-detection-vs-upstream-rewrites) the system does
  not actually have. **Fix:** wire it or delete it; don't let an unwired library
  imply a guarantee.

## Cleared (investigated → not a finding)

- **[CLEARED] Migrations 0031/0040 down re-introduce forbidden retention.** The
  surface mapper flagged the `.down.sql` files as re-adding `add_retention_policy`
  on `trades`/`oracle_updates`. **False:** both downs are explicit NO-OPs with a
  banner comment ("intentionally a NO-OP. DO NOT re-add retention… the precise
  loss ADR-0034 [forbids]"). This is *good* defensive engineering, not a bug.
- **[CLEARED] `r1.secrets.yml` plaintext secrets in inventory.** **False:** the
  file is `$ANSIBLE_VAULT;1.1;AES256`-encrypted AND not git-tracked.
- **[CLEARED→re-scoped] GCP key "checked into repo root."** Re-scoped to CS-001:
  it's gitignored/untracked (working-tree footgun, Medium), not a committed leak.
- **[VERIFIED GOOD] A3 one-writer invariant (ADR-0031/0032).** The projector
  registry's `buildSource` source set and `pipeline/sink.go::IsProjectedEvent`'s
  event-type arms are **in sync** — every projected source (soroswap, aquarius,
  phoenix, comet, reflector, redstone, blend, blend_backstop, cctp, rozo,
  defindex, sep41_supply, sep41_transfers) appears in both (reflector via the
  oracle-config path, sep41_* via the `watchedSEP41` path). No double-write /
  silent-drop drift. (Recorded to avoid re-litigation; a deeper A3 pass can still
  fuzz the event-type↔table mapping.)

## Systematic findings (from area execution)

### A18 — Multi-tenant IDOR — **surface is clean** (1 Low)
Full end-to-end trace of every dashboard/account/admin handler → store query.
**No Critical/High cross-tenant read/write survived** (8 candidates C1-C8
adversarially refuted: key revoke, webhook PATCH/DELETE/deliveries, Redis key
path, empty-subject fail-OPEN→actually fail-closed, staff-lookup spoof, mass-
assignment, Stripe/billing, usage/audit/invites). Confirms prior audits' "auth
primitives sound."

- **CS-008 — Tenant isolation enforced in the handler layer, not the SQL — Low
  (defense-in-depth).** `postgresstore` by-id methods act by PK alone
  (`apikey_store.go:291` `Get`; `webhook_store.go` `GetWebhook`/`UpdateWebhook`/
  `DeleteWebhook`/`ListDeliveries` — all `WHERE id = $1`, no `AND account_id`).
  Isolation depends on every handler remembering to compare `AccountID` before
  acting (they all do today). **Failing scenario:** the *next* handler added on
  top of these store methods that forgets the compare = instant Critical IDOR, and
  the store's thin tests (only `apikey_store_test.go`; webhook/account/audit/token/
  billing stores untested — ties CS-003) won't catch it. **Fix:** push the owner
  predicate into the query (`WHERE id=$1 AND account_id=$2` → `ErrNotFound`), and
  add store-level "A's id + B's owner → ErrNotFound" tests. Re-run this review when
  team/invite, audit-trail, and `/v1/account/subscription` HTTP surfaces ship (the
  account-scoped store methods exist but aren't yet wired to handlers).

### A26/A30 — CF Pages Edge Functions (never previously audited)
- **CS-009 — OG image edge function: unauthenticated blind SSRF + no-timeout DoS
  via double-decoded, unescaped path segment — High** (rubric: SSRF-to-arbitrary-
  host = High; held at the boundary because it's blind + edge-sandboxed — see
  note). `web/explorer/functions/og/[[path]].js` reads the URL path, **decodes it
  up to twice** (defeating the upstream `ogImageFor` `encodeURIComponent`), passes
  it through `prettyLabel`/`code` (no HTML-escape), and string-interpolates it
  **unescaped** into HTML handed to `new ImageResponse(html)` (`workers-og`/satori).
  satori's `<img>` handler does `fetch(src)` for any `http…` src with **no allow-
  list, no SSRF guard, no timeout**. **Exploit:** `GET /og/ledgers/%3Cimg%20src=
  http://attacker%3E` → the CF edge fetches the attacker URL on render; the 24-char
  label cap is bypassed via a short redirector domain (satori follows redirects).
  Unauthenticated, attacker-chosen outbound GET from the edge + a no-timeout fetch
  = cost/resource DoS (unique URLs defeat the 60s cache). *Refuted down from "stored
  XSS":* output is `image/png` (rasterized, never browser-interpreted) — the
  same-origin OG-card content-spoof is Low. *Note:* reviewer rated **Medium** as
  blind + no internal target demonstrated; I keep it **High** for the audit because
  an unauthenticated SSRF primitive on the public edge is a real, fixable exposure
  and the no-timeout fetch is a cheap DoS. **Fix:** HTML-escape `label`/`sub`; stop
  the double-decode; ideally allow-list satori's image fetch host. (CC-6 trust
  boundary; the never-audited surface the gap analysis ranked #1.)

### A6/A8/A34 — Data-correctness at scale (the "code-correct ≠ data-correct" seam)
- **CS-010 — XLM circulating supply served == total == max (~50.0B); `xlm_sdf_
  reserve_exclusion` basis is a no-op → market cap overstated ~+58% — High.**
  Live `/v1/assets/native`: `circulating_supply = total_supply = max_supply =
  500018068120000000` stroops (= **50.0B XLM**), `supply_basis:
  xlm_sdf_reserve_exclusion`, `market_cap_usd = $9.20B`. **Airtight internal
  contradiction:** a basis that excludes SDF reserves MUST make circulating <
  total; circulating == total proves the exclusion nets to **zero** (the SDF-held
  ~18-19B is not subtracted). Every public source (CoinGecko/Stellar Expert) puts
  XLM circulating ≈31B → served circulating + market cap are overstated **~+58%**
  on the **flagship asset**. **Failing scenario:** any consumer reading XLM market
  cap / circulating supply from the API (or the explorer headline) gets a number
  ~1.58× reality. Confirms + worsens prior `06-14 Q4` (+48% one-sample). **Fix:**
  make the SDF-reserve exclusion actually subtract SDF-held balances, or stop
  labeling the basis as an exclusion; reconcile against Stellar Expert in CI.
  → **systematic reconciliation needed** (this was one sample; sweep per-asset
  supply, 24h volume, OHLC vs ground truth — Wave 2).
- **CS-011 — `/v1/assets/xlm` (slug/GlobalAssetView shape) omits supply + market
  cap entirely** (only `ticker`+`price_usd`), while `/v1/assets/native`
  (AssetDetail shape) carries them — the dual-shape split (LC-040) means the
  headline slug a CG-style user hits (`/assets/xlm`) silently lacks market cap. Low
  (coherence/completeness; cross-refs LC-040). 

### A21 — SSE / streaming (prior audits: "hub has no functional/chaos test" — confirmed exploitable)
- **CS-012 — send-on-closed-channel panic race in `Hub.Publish` → whole-process
  crash — High.** `streaming/hub.go:92` releases `t.mu` then sends off-lock
  (`hub.go:96`); `dropSubscriber` closes `sub.ch` under lock (`hub.go:171`). A
  `select` send on a *closed* channel is "ready" (chosen over `default`) → panics
  `send on closed channel`. The panic is in the publisher goroutine
  (`streampublish.pollLoop`/`redispub.Subscriber.Run`) which has **no `recover()`**
  (`middleware.Recoverer` only wraps HTTP) → **entire API process crashes.**
  **Scenario:** `/v1/price/stream` client disconnects (handler `defer cancel()` →
  `dropSubscriber` → `close`) in the µs window between Publish's snapshot-unlock
  and its send → aggregator publishes a closed bucket → panic. `closeOnce` guards
  double-*close*, not concurrent *send*. Untested (the two Hub tests avoid the
  publish-vs-disconnect race). **Fix:** make the send panic-safe (guard a per-sub
  "closed" flag read under `t.mu`, or recover in the publisher goroutines).
- **CS-013 — cleared write deadline + teardown only on `ctx.Done()` → stalled
  client leaks goroutine+conn+FD forever; no connection cap → DoS — High.**
  `handler.go:88` does `SetWriteDeadline(time.Time{})` (clears it, no rolling
  replacement); the loop exits only on `ctx.Done()`, which fires on client *close*,
  not *stall*. A non-reading / zero-window client blocks `Flush()` indefinitely; ctx
  never cancels. **No max-conn cap, no idle-stream timeout.** With anon stream
  establishment allowed (`anon_rate_limit_per_min=6000`), thousands of non-reading
  connections → FD/goroutine/memory exhaustion (health checks refused). `IdleTimeout`
  /`ReadTimeout` don't cover an in-flight SSE handler. **Fix:** rolling per-write
  deadline + per-subject/global concurrent-stream cap (`netutil.LimitListener`).
- **CS-014 — client-controlled sub-second tick × unbounded connections = DB-load
  amplification — Medium.** Per-connection producers re-run a backend query each
  tick; tick is client-set to `[1,60]s`. `observations` drives a `LatestTradePerSource`
  hypertable scan (8s-bounded per tick, but unbounded in *count*). N streams at
  `interval_seconds=1` → N scans/s on Postgres. **Fix:** floor the tick + cap streams.
- **CS-015 — rate limit gates establishment *rate*, not *concurrency* — Medium.**
  Streams hit the same per-minute bucket (good) but nothing caps how many you hold
  open; `main.go:1058` `ListenAndServe` has no `LimitListener`. Structural enabler
  for CS-013/014.
- **CS-016 — `redispub` subscriber has no restart; silent degrade to heartbeats-
  only — Low/Med.** On `Run` error (`subscriber.go:88`) `main.go:1028` only logs +
  never resubscribes; `/v1/price/stream` then serves heartbeats with no
  `price_update`s, still 200 (invisible). Mostly fires on shutdown (go-redis absorbs
  transient blips), so Low/Med. **Fix:** supervise/restart + health-surface.
- *Scope correction:* `/v1/oracle/streams` is NOT SSE (plain 8s-bounded JSON) — only
  4 real SSE endpoints. Several leak/poison candidates were cleared (normal-disconnect
  cleanup is correct; pub/sub routing is per-topic, no cross-subscriber leakage).

### Data-correctness blast-radius (CS-010 follow-up)
- USDC: `circulating == total == 39.0M` with `issuer_exclusion` basis — for USDC
  circulating==total is **plausibly legitimate** (issuer holds ~0), so not flagged.
  But the `circulating == total` pattern recurring across assets means **every
  `*_exclusion` basis needs the Wave-2 reconciliation** to confirm it actually
  subtracts (XLM's provably doesn't — CS-010). AQUA returned no supply data
  (separate completeness gap to check).

### A33 — Pricing read-paths (prior perf-incident fix re-verified intact)
- **CS-017 — dormant pairs return months-old VWAP with `stale=false` — Medium.**
  `storePriceReader.LatestPrice` (`cmd/stellarindex-api/main.go:2252`) hardcodes
  `stale=false` on any `prices_1m` hit; `LatestClosedVWAP1mForPair` resolves the
  latest closed bucket within a **400-day** window. **Scenario:** `/v1/price` for a
  direct-quoted pair whose last trade was 200 days ago → returns the 200-day-old
  bucket with `flags.stale=false` and a 200-day-old `observed_at`. Same "degraded/old
  served as fresh" pattern as the prior Redis-BGSAVE SEV; the PriceReader "stale when
  older than freshness target" contract isn't enforced on the VWAP branch (only the
  last-trade branch sets stale). Bites the ~250k dormant/delisted long-tail (not the
  XLM/USD common case, which uses priceFallback). **Fix:** flip `stale=true` when
  `now − bucket_end` exceeds the freshness target. (CC-1.)
- **CS-018 — self-peg hides a depeg on the SEP-40 wire shape — Low (by-design).**
  `/v1/oracle/lastprice|x_last_price` reach `tryStablecoinFiatProxy` via
  `priceFallback` and emit `SEP40Price` with no divergence field, so a SEP-40
  consumer (lending protocol) reads `1.0 @ now` during a USDC depeg with only
  `stale` as signal. **NOTE — the P2-4b self-peg arm itself was reviewed CORRECT:**
  it's only reached after the primary read returns ErrPriceNotFound, never overrides
  a real bucket (no exchange emits crypto:USDC/fiat:USD), and crypto:USDC/fiat:EUR
  correctly doesn't trip it. **Fix:** add a peg/divergence flag to the SEP-40 shape.
- **CS-019 — two non-sargable plan-time predicates — Low.** `RecentClosedVWAP1mForPair`
  (`aggregates.go:241`, backs /v1/oracle/prices) and `ClosedVWAP1mAtOrBefore`
  (`aggregates.go:289`, backs /v1/assets change_24h) use `bucket + INTERVAL <= …`.
  `ORDER BY bucket DESC LIMIT` rescues *execution*, but the first lacks a literal
  lower-bound → plan-time chunk enumeration (the layer-2 cost the prior fix
  neutralised elsewhere); the second back-scans ~1440 buckets. Low (not full-scan).
  **Fix:** give them the same literal-cutoff bound / sargable rewrite.
- **CS-020 — batch identity-id aborts the whole batch — Low.** One `asset==quote`
  id in `/v1/price/batch` 400s the entire request (should skip that id); per-row
  stale collapsed to an envelope OR (over-marks safe, but no per-row signal).
- *Re-verified good:* the exact prior incident bug (`LatestClosedVWAP1mForPair`
  `max()` over non-sargable `bucket+INTERVAL<=now()`) is **fixed + sargable**
  (`aggregates.go:393`); stale propagation through the fallback chain is fixed;
  price strings are big.Int/big.Rat end-to-end (only FX cross-rate is float — fine).

### A11 — ClickHouse raw lake (prior audit: "design correct, ZERO integration coverage" — confirmed)
- **CS-021 — `ledger_entries_current` versioned only by `ledger_seq` → intra-ledger
  ordering lost → stale/resurrected current state — Medium.** `tier1_schema.sql:205`
  `ReplacingMergeTree(ledger_seq)` ORDER BY `(entry_type,key_xdr)`. An entry mutated
  ≥2× in its newest ledger shares sort key AND version → FINAL keeps an
  implementation-defined row. **Scenario:** offer partially filled (`updated`) tx5
  then fully consumed (`removed`) tx9 of ledger N → FINAL may keep `updated` →
  `accountOffers`/`AssetHolders` **resurrect a deleted offer**; account balance =
  intermediate not closing. No stored total intra-ledger order (`change_index`
  resets per-tx). Manifests under cross-part interleaving (live vs backfill/heal),
  where CH's equal-version tiebreak is unspecified → untested + unprovable-safe.
  **Fix:** version by `(ledger_seq, tx_index, change_index)` or a global ordinal.
- **CS-022 — CH write buffer cap is in ledger-units, tuned for empty `Changes`;
  Phase-C entry-capture inflated the real byte ceiling — Medium.** `live_sink.go`
  caps `MaxBufferLedgers=4096`; `sink.go:437` comment still claims `Extract.Changes`
  "always empty (G12-03)," but `extract.go:108` now extracts entry-changes — the
  **highest-volume table** (~1.7B rows, base64 `entry_xdr`/`key_xdr` blobs). During
  a CH outage the buffer holds 4096 ledgers × all entry-changes = a far larger heap
  than the G12-01 cap (protecting the shared r1/Postgres host) was tuned for.
  Bounded (F-1349 genuinely fixed) but mis-stated/un-retuned/untested. **Fix:**
  byte-based cap or re-tune; fix the stale comment.
- **CS-023 — non-FINAL aggregate reads double-count un-merged parts — Low/Med.**
  `explorer_reader.go:227,266` (`OperationTypeStats`, `NetworkThroughput`) sum
  `tx_count`/`op_count` without FINAL → healed/backfilled ledgers double-count until
  merges settle (throughput/op-type charts inflate). Counting-critical paths (gate,
  completeness, supply) correctly use FINAL — these two charts are the inconsistency.
- **CS-024 — UInt32 underflow in window predicate + unescaped IN-list concat — Low.**
  `explorer_reader.go:233,277` `ledger_seq > (max)-?` wraps to ~4.29B on a small/fresh
  lake → silently empty (never on the 62M prod lake). `event_reader.go:41`
  `sqlQuoteList` concatenates IN-lists unescaped — docstring claims compile-time
  constants but `contractIDs` come from runtime config (`projector.go:330`); not
  exploitable today (strkeys can't contain quotes) but an injection footgun.
- **CS-025 — the load-bearing lake hole-heal script `ch-live-catchup.sh` is NOT in
  the repo — Medium (verifiability/supply-chain).** The `LiveSink` is best-effort
  (drops on outage) and explicitly relies on `ch-live-catchup.sh` gap-scanning
  below CH_max to refill holes (`live_sink.go:49`); the projector watermark + the
  "100% coverage" claim depend on it. Only `deploy/systemd/ch-live-catchup.service`
  exists — it calls `/usr/local/bin/ch-live-catchup.sh`, which is **not in the
  repo** (and `find-data-gaps` scans Postgres, not the CH lake). So the correctness
  of the backstop the whole tiered-completeness story rests on is **unverifiable
  from the codebase**. **Fix:** vendor the script into the repo + under test.
- *Re-verified GOOD:* core-table dedup sound (deterministic ORDER BY from LCM,
  `event_index` preserved across re-runs; counting reads use FINAL); **contiguity +
  hash-chain is actually CHECKED** (`completeness.go::SubstrateProblem` gap-scan +
  `prev_hash` chain to genesis), not assumed; F-1349 unbounded buffer genuinely
  fixed (bounded channel + commit-marker-last durability ordering);
  `explorer_reader` keyset pagination + `>2^53`-as-string + no-i128-truncation all
  correct; injection clean except CS-024's latent footgun.

> _Remaining: a11y reviewer in flight; then final synthesis._
