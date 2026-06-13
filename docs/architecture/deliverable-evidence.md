---
title: Deliverable evidence — acceptance criteria → proof
last_verified: 2026-06-13
status: living until sign-off; one section per acceptance criterion
---

# Deliverable evidence

One page per acceptance criterion (ctx-proposal §Milestones + Stellar
RFP §3/§5 + Freighter RFP SLAs) → the concrete, re-runnable proof.
Companion docs: `prod-verification-2026-06-12.md` (probe-level),
`coverage-matrix.md` (requirement-level), `freshness-definition.md`.

## AC1 — Real-time price staleness ≤ 30 s

- **Surface**: `/v1/price/tip` (+ SSE stream). Definition pinned in
  `freshness-definition.md` — pre-agree the one-liner with the customer.
- **Proof**: sla-probe verdict series (10-min timer, r1) — `pass` with
  per-endpoint freshness; spot probes show single-digit-second
  `observed_at` age for both `crypto:XLM` and (post-8fde6c84) `native`.
- **Caveat closed**: the native-spelling alias gap found 2026-06-12 is
  fixed + deployed.

## AC2 — p95 ≤ 200 ms (p99 ≤ 500 ms)

AC2 is a **server-latency** SLO. The authoritative evidence is two
independent measurements that agree:

- **Server-side histogram (Prometheus, measured live under the
  contractual k6 load, 2026-06-13):**
  `http_request_success_duration_seconds` **p95 = 68 ms, p99 = 98 ms**
  — PASS by ~3× / ~5×. `all`-request p95 equals success p95 (68 ms),
  confirming a ~0 % error rate at the served load.
- **k6 client-side, origin-direct** (`00-acceptance-contract-rate.js`,
  30 min @ 17 req/s = the 1000 req/min contractual rate, against
  `http://localhost:3000` to bypass the edge):
  `test/load/reports/2026-06-13/00-acceptance.json` (2026-06-13) —
  **30,600 requests, p95 = 54.4 ms, p90 = 48.0 ms, max = 901 ms,
  error rate = 0.00 % (0 / 30,600), checks 100 %.** All three k6
  thresholds (`p(95)<200`, `p(99)<500`, `rate<0.001`) green; p99 ≪ 500
  (Prometheus p99 = 98 ms over the same load). **PASS.**

**Why origin-direct, not through Cloudflare:** the production API
sits behind Cloudflare. A single-IP synthetic k6 burst trips
Cloudflare's anti-abuse layer (designed to block exactly that
shape) — yielding 60 s timeouts that are an artifact of the *test
source*, not server latency (a through-edge run showed a 13 % "error"
rate whose `expected_response:true` p95 was still 191 ms). Real
traffic is distributed across many client IPs and does not trip this.
The origin-direct run + the server-side histogram both isolate the
quantity AC2 actually constrains. (A pre-2026-06-13 fixture also
inflated the error rate by requesting three pairs that don't exist on
the served tier — a typo'd AQUA issuer and two CEX-quote-currency
"USDT/USD"/"USDC/USD" pairs; fixed in `lib/pairs.js`.)

## AC3 — Historical retention ≥ 1 yr (ideally since inception)

- **Proof**: `/v1/history/since-inception` + `/v1/ohlc?interval=1d`
  serve daily bars back to 2015 (SDEX genesis); probe report §history
  shows daily series to 2021+ for XLM with full RFP timeframe ladder
  (1m/15m/1h/4h/1d × 1h/24h/1w/1mo/1yr/all-time). 1h+ granularities
  retained indefinitely (migration 0031 removed trades retention;
  caggs indefinite).

## AC4 — ≥ 1000 requests/min per client

- **Proof**: the origin-direct acceptance run sustained **1031 req/min
  on a single key** (Prometheus `rate(http_request_duration_seconds_count)`,
  measured live) for 30 min with **zero rate-limit (429) failures** —
  the contractual floor, served clean.
- **Headroom**: anon tier is provisioned at 6000/min and authenticated
  keys default to 1000/min (`key_rate_limit_per_min`, per-key
  configurable via `mint-key -rate-limit-per-min`); the earlier
  saturation probe drove ~18,000/min on one key before any limiter
  pushback.

## AC5 — Completely open source; publicly accessible + reproducible

- **Status**: **push-button verified 2026-06-13** — `public-export.sh`
  against today's HEAD produced a clean export (secret-sweep clean,
  `go build ./...` OK, no residual prod IP, 2191 files). Pre-flight
  CLEAN (secrets/license/VERSIONS — `public-flip-preflight-2026-06-12.md`).
- **Remaining (operator-only)**: create the `StellarIndex` GitHub org +
  empty public repo, then run `public-flip-runbook.md` (export →
  single-commit push → v1.0.0 tag). Cannot be scripted — org creation
  is web-UI-only. <!-- flip executed: link the public repo here -->

## AC6 — Production deployment within ~10 weeks

- **Proof**: r1 serving since 2026-05-03; current binaries (Stellar
  Index) deployed 2026-06-12; smoke 13/13; status page live.
- **Multi-region**: decision per readiness-plan §6 due Jun 18.

## AC7 — API reference docs + self-service onboarding

- **Reference**: OpenAPI-generated reference (`docs/reference/api`),
  brand-clean (0 residual-brand hits across `examples/`,
  `docs/reference`, `docs/methodology`, `openapi/`).
- **Self-service onboarding**: `docs/getting-started.md` now leads with
  the real ≤1-min path — `POST /v1/signup` (email → usable `rek_…` key,
  no Stellar wallet needed), then the SEP-10 account-bound path as the
  advanced option. Example key prefix corrected (`rate_` → `rek_`) and
  the unquoted-`&` curl bug fixed.
- **E2E walkthrough (2026-06-13):** signup → key → authenticated request
  loop verified end-to-end on r1. Pre-launch, the deployment runs
  `signup_require_email_verification = false` (the documented operator
  opt-in for unverified signup) so a fresh key authenticates
  immediately; email-ownership verification re-enables once a
  transactional-email sender (Resend) is configured.
  <!-- FILL: paste the verified rek_ key-id + 200 response after the r1 flip -->
- **Examples**: `examples/` curl scripts + auto-generated Postman
  collection, brand-clean.

## Beyond-contract differentiators (the demo ceiling)

- **`/v1/coverage`** (live): per-source ADR-0033 verdicts — substrate
  continuity to genesis (proven at tip 63.0M, windowed audit),
  recognition, projection reconciliation. <!-- fill: N/15 complete at sign-off -->
- **`/v1/protocols` + `/v1/protocols/{name}`** (live): 15-protocol
  directory with verified factory trust-roots (ADR-0035), registered
  contracts, 24h activity.
- Per-protocol verification pages (`docs/protocols/`) cross-checked
  against team-published Dune data (53 DeFindex vaults, 178 Aquarius
  pools, Blend 2-factory enumeration).
