# J40 — Attacker floods `/v1/signup` + `/v1/price/batch` during the F-0039 cascade

## Inputs

- **Attacker:** anonymous, no API key required for the targeted
  routes. Source IP is a single VPS (cheap to acquire, easy to
  rotate).
- **Goal:** create N free-tier accounts to mint N API keys
  (`/v1/account/keys` POST is auth'd but `/v1/signup` is open)
  AND exhaust the per-IP rate limit on `/v1/price/batch` to
  scrape `M` price quotes at zero cost.
- **System state at attack time:** F-0039 cascade active (Redis
  MISCONF), as observed on r1 throughout this audit.
- **Defences (designed):**
  - `internal/auth/signup_ip_throttle.go` — Redis-backed token
    bucket per source IP
  - `internal/ratelimit/bucket.go` — Redis-backed token bucket
    global per-key
  - SSRF guard on outgoing webhook fanout (F-0006 POSITIVE) —
    not relevant to inbound attack

## Hops

| # | Stage | File:line | Behaviour under F-0039 cascade |
| --- | --- | --- | --- |
| 1 | Attacker sends `POST /v1/signup` with random email + password | `internal/api/v1/server.go:POST /v1/signup` | hits the signup handler |
| 2 | Handler queries signup-IP-throttle Redis bucket | `internal/auth/signup_ip_throttle.go:75-79` | **Redis MISCONF → SET fails → fail-OPEN (F-0049)** |
| 3 | Throttle returns "ok, you may signup" despite no consumption | (fail-open path) | **NO rate limit applied** |
| 4 | Signup verifier creates the account (writes to Postgres `api_keys` table) | `internal/auth/signup_tracker.go` | succeeds — Postgres is healthy |
| 5 | Attacker repeats steps 1-4 from same IP | | EACH attempt succeeds; no throttle |
| 6 | Attacker now hits `POST /v1/account/keys` with the magic-link token | (assumes attacker can complete email verification with disposable mailbox) | mints API key |
| 7 | Attacker hits `GET /v1/price/batch?asset_ids=...&quote=USD` | `internal/api/v1/price.go::handlePriceBatch` | dispatches to per-asset price lookup |
| 8 | Per-asset price lookup consults global rate-limit bucket | `internal/ratelimit/bucket.go:138-190` | **Redis MISCONF → bucket consume fails → fail-OPEN (F-0050)** |
| 9 | Price batch handler proceeds without per-key throttling | | **NO global rate limit applied** |
| 10 | Handler reads from Redis VWAP cache OR falls back to Postgres via priceFallback | `internal/api/v1/price.go:283-308` | returns price (possibly stale; that's per ADR-0018) |
| 11 | Attacker repeats step 7 at maximum bandwidth | | per-request: 10s server timeout, but no THROTTLE → attacker can saturate request bandwidth indefinitely |

## What the attacker achieves

- **N free accounts** in the time it takes to register N emails
  (limited only by the upstream email-verification step, NOT by
  any of our rate limits which are all fail-open)
- **M price quotes scraped** at zero rate-limit cost; only
  bound by HTTP transport + the 10s per-request server ceiling
- **Postgres `api_keys` table bloat** — 32-byte random rows ×
  N = trivial storage but admin headache to clean up

## What stops them

- **HTTP transport** — Caddy will close connections after
  ~10k concurrent (default). Attacker uses multiple IPs (cheap)
- **The `api_error_rate_high` alert** (api.yml) WOULD fire at
  >1% 5xx rate — but cascade-affected routes return 5xx mostly
  legitimately under F-0039, so the alert is in
  "expected-fire" state and easy to miss as background noise
- **Eventually Postgres lock-table contention** from concurrent
  inserts (per the 2026-05-06 SEV-3 incident at F-0098)
- **Operator manually noticing weird traffic in `/v1/incidents`
  log or access logs** — but with F-0027 alert silence and
  F-0133 smoke green, there's nothing pinging them
- **Healthchecks.io heartbeat** (F-0106 POSITIVE) only fires
  on TOTAL system death, not abuse — so no signal from there
- **CDN Cloudflare** in front of api.ratesengine.net would
  rate-limit per-IP but cf-cache won't activate on
  /v1/signup (POST) or /v1/price/batch (per-key dynamic)

## Findings cross-references

- **F-0039** — root cascade enabling the attack
- **F-0049** — signup IP throttle fails OPEN
- **F-0050** — global rate-limit fails OPEN
- **F-0099** — 2026-05-10 SEV-2 follow-ups unchecked (this
  attack class was forseeable from that incident)
- **F-0133** — smoke green doesn't catch this either

## Adversarial verdict

Under the live cascade right now (~36h+ unfixed), this attack
would succeed. The cost to the attacker is:
- 1 VPS @ $5/mo
- N disposable email addresses (trivially scriptable)
- Bandwidth to repeat the attack at saturation

The cost to us is:
- N free accounts polluting `api_keys` (clean-up effort)
- M price-quote scrapes that should have been paid (revenue
  loss — small per query, accumulates)
- Postgres lock contention if the attacker pushes hard
- Reputational risk if discovered

**Fix dependency:** Wave 0 step 6 (flip rate-limit + signup-
throttle from fail-OPEN to fail-CLOSED + 503-with-Retry-After)
closes the attack vector. **Until that step lands, the
system is open to this class of abuse the moment the cascade
gets noticed by an attacker.**

## Tests covering this scenario

- `internal/auth/signup_ip_throttle_test.go` covers happy path;
  NO test asserts fail-CLOSED-on-Redis-error behaviour
- `internal/ratelimit/bucket_test.go` similar gap
- **Recommended adversarial test:** mock Redis to return
  MISCONF on every command; assert the surface returns HTTP
  503 (not 200 with fail-open). Add this to W15.

## Closure rule (audit)

J40 is closed when F-0049 + F-0050 + F-0039 are all closed.
Then re-walk this journey to confirm the surface refuses the
attack at step 3 (signup throttle 503).
