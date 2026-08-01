# Recon: entry-point extraction (HEAD f8c099ee, 2026-08-01)

Extraction-derived, not read-derived. Every row came from parsing the
registration machinery (`ServeMux` patterns, the `subcommands` map,
systemd units, `go func` sites, the CF Pages `functions/` tree), then
diffed against the same extraction at `f84e2d0b` (the 2026-07-16 recipe's HEAD).

## Extraction method (reproducible)

```
grep -rn --include='*.go' -E '(s\.)?mux\.(HandleFunc|Handle)\("' internal/api/ | grep -v '_test\.go'
sed -n '/^var subcommands = map/,/^}/p' cmd/stellarindex-ops/main.go
grep -nE '^\s*go func|^\s*go [a-zA-Z]' cmd/stellarindex-*/main.go
find deploy configs -name '*.timer' -o -name '*.timer.j2'; grep -rn 'cron:' .github/workflows/
git ls-tree -r --name-only HEAD -- web/explorer/functions/
```

## Headline counts vs the 2026-07-16 recipe

| Surface | 2026-07-16 | 2026-08-01 | Δ |
|---|---|---|---|
| HTTP method+path patterns | 120 | 127 | +7, 0 removed |
| OpenAPI operations | 115 | 120 (1:1) | +5 |
| Undocumented (CI-exempt) | 1 staff + 4 utility | 1 staff + 6 utility | +2 |
| ops subcommands (top level) | 57 (recipe said 55) | 60 | +3 |
| API background goroutines | 13 | 19 | +6 |
| aggregator / indexer goroutines | 15 / 11 | 15 / 11 | 0 |
| CF Pages Functions | 6 (recipe listed 1) | 7 | +1 file, +5 unlisted |
| ansible-rendered timers | 6 | 11 | +5 (under-counted) |
| GH cron workflows | 4 | 8 | +4 (under-counted) |

Spec parity holds exactly: 120 OpenAPI operations ↔ 120 documented mux
patterns, zero spec paths without a handler, 7 intentional exemptions.

## NEW SINCE THE 2026-07-16 RECIPE (the audit-relevant deltas)

**Routes (+7):** `GET /v1/sdex/orderbook` (server.go:1471) · `GET
/v1/divergence/series` (:1666) · `GET /v1/accounts/{g}/trades` (:1501) ·
`GET /v1/accounts/{g}/activity` (:1502) · `DELETE /v1/admin/keys/{keyID}`
(:1711, OP mutating, leaked-key kill switch) · `GET /errors/{slug}` (:1807) ·
`GET /errors/{$}` (:1808).

**Route-shaped changes a table-diff misses:**
- `/v1/protocols/{name}` is a **registry(16) × window(1/7/30/90) × category(5)
  cross-product = 64 addressable analytics cells**, driven by
  protocols_registry.go:66-216 × protocols.go:179 whitelist × protocol_bespoke.go:83-95
  dispatch (different packages). 2 bridge sources cctp/rozo; hourly grain at days==1.
- `/v1/protocols` per-protocol DEX TVL snapshot.
- `/v1/anomalies?include=daily` → FreezeDailyReasonCounts.
- `/v1/network/throughput` gained fee_pool/total_coins/protocol_version/partial
  (explorer/operations.go:361-369, "chain economics").
- `/v1/assets/{id}/holders` native-XLM code path (account-balance board;
  folds native/crypto:XLM/XLM alias, account_state.go:350-356).
- `/v1/accounts`, `/v1/contracts`, holders, op-type-stats → stale-while-revalidate
  + `flags.stale` instead of 503.

**Ops subcommands (+3):** freeze-unfreeze, usage-rollup-backfill, verify-usd-volume.
Total 60 top-level + 7 supply modes + 1 discovery.

**Destructive-path NEW invariant (migration 0125):** `projector-replay`
(ops/ingest/projector.go:111) and `projected-rebuild -write`
(ops/chops/projected_rebuild.go:226) now WRITE `projection_dirty_windows`
BEFORE mutating and REFUSE to run if that write fails; compute-completeness
extends its reconcile floor over those windows (compute_completeness.go:276,914)
and clears only on a clean verdict (:507-511). This changes the risk posture
of every re-derive path the last audit reviewed.

**API background workers (+6):** stripe-deadletter-gauge · dex-tvl-cache ·
sdex-orderbook-cache (Load/Advance/VerifyPending) · prewarm-supply-wealth
(+op-mix) · prewarm-protocol-details (64-cell sweep) · login-code-lockout-reaper.
Plus a `recoverBackgroundWorker` panic wrapper across all.

**New infra primitives:** clickhouse/refresh_gate.go (detached-refresh
semaphore) · api/streaming/periplimit.go (per-IP SSE cap) ·
middleware/csrf_origin.go (RequireSameSiteWrite) · middleware/request_timeout.go
(moved OUTSIDE the credential/quota block, C3-102) · api/v1/logredact.go ·
dashboardauth/login_intent.go.

**Edge (+1):** web/explorer/functions/markets/[[path]].js.

## Recipe corrections (existed at f84e2d0b but were under-counted/mislabelled)

- CF Pages Functions: recipe named 1, there were 6 (now 7) — an entire
  request-time exec environment was 5/6 uninventoried.
- Ansible timers: recipe named 6, there are 11 (missing ch-schema-snapshot,
  ch-schema-drift, restore-drill + 2 inline).
- GH crons: recipe named 4, there are 8 (missing ci-health every-2h,
  deploy-protection, reconciliation-health, launch-readiness).
- `/v1/oracle/streams` is plain JSON, NOT SSE (oracle.go:209). SSE set =
  {price/stream, price/tip/stream, observations/stream, ledger/stream}.
- 19 workflows all carry workflow_dispatch = 19 manual entry points incl.
  deploy.yml (r1 rollout + migrations), release.yml.

## Mutating surface (34 routes)

All non-GET + `GET /v1/signup/verify` + `GET /v1/auth/callback` +
side-effecting `GET /v1/account/admin/lookup` (writes staff.customer.lookup
audit row, handlers_admin.go:157). Auth spread: OP=7, SESS/SESS+STAFF=17,
KEY=5, SIG=1, OPEN-by-design=6, LOOPBACK=1.

Scope enforcement is PREFIX-based (auth/scopes.go:28-37): /v1/admin/*→admin,
/v1/account/*→account, /v1/dashboard/*→dashboard, else→read. **A new
management endpoint OUTSIDE those three prefixes silently gets `read` scope**
with zero wiring — a systematic-coverage risk for any future mutating route.

## Destructive ops subcommands (13)

trim-galexie-archive (S3 DeleteObject, monthly timer, --commit required, dry-run
default) · projector-replay (0125-guarded) · projected-rebuild -write (0125-guarded) ·
ch-rebuild -write (-sep41 TRUNCATE procedure) · classic-movements-backfill -write
(writes OUTSIDE HandleEvent/lockstep/ADR-0033 catalogue — audit lead) · state-snapshot
-write · ch-reproject/ch-backfill/census-backfill/ch-txindex/ch-participant (PK-dedup
convention) · reconcile-balances · freeze-unfreeze · mint-key/upgrade-key ·
usage-rollup-backfill · emit-incident (customer webhook fanout) · supply seed-*.

## Standalone /metrics listeners (4 of 5 have NO loopback guard)

api /metrics (server.go:1535, LOOPBACK-guarded) · indexer (main.go:1795, NO) ·
aggregator (main.go:1604, NO) · ops cross-region-monitor (cross_region_monitor.go:139,
NO) · ops verify-archive -metrics-listen (NO). Carried lead — still true.

## SSE / webhooks / edge

4 SSE (3 with no first-party consumer; ledger/stream feeds status page).
Admission: streaming.TryAcquireStreamSlot + periplimit.go per-IP cap; SSE exempt
from RequestTimeout; warnCollapsedStreamCap at main.go:3671.
5 outbound webhook types (anomaly.freeze, divergence.firing, incident.sev1/resolved,
price.alert) → customerwebhook.Fanout, SSRF-guarded dialer (ssrf.go:28).
1 inbound signed webhook POST /v1/webhooks/stripe (empty secret → hard 503).
7 CF Pages Functions (og/ does request-time fetch to api.stellarindex.io — SSRF/edge surface).

## The three things a reading would most likely miss (coverage-critical)

1. `/v1/protocols/{name}` is a 64-cell cross-product, not one endpoint.
2. The 2026-07-16 recon listed 1 CF Pages Function when 6 were live.
3. projector-replay / projected-rebuild -write are now fail-closed against the
   completeness verifier (migration 0125) — changes every re-derive path's risk posture.

(Full 127-row route table + worker tables preserved in the agent transcript
tasks/a3976e51ce7e8bbad.output; the deltas above are what the recipe needs.)
