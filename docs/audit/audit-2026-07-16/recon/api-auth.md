# Recon: API / auth / ratelimit / input-validation (HEAD f84e2d0b)

## Auth core = strong (verified GOOD, re-verify in audit)
- No exploitable SSRF (server-side), no SQL/ClickHouse injection (every user value bind-parameterized; `sqlQuoteList` latent-only, trusted-pipeline-reachable). Errors never echo err.Error() to clients (logged raw, generic detail returned).
- Canonical parsing boundary strong: ParseAsset + SDK strkey.Decode (CRC-16), panic-free.
- Middleware chain: Logger→Recoverer→CORS→Auth→KeyPolicy→RequireEmailVerified→MonthlyQuota→UsageTracker→RateLimit (Auth before RateLimit so limits key off subject).
- SEP-1 outbound dialer hardened: Proxy:nil (F-1336 proxy-bypass block), redirect cap 5 + scheme-downgrade reject + cross-host reject, IP re-check post-DNS/pre-connect (anti-rebind), 1 MiB body cap. Background ingest only, NOT handler-reachable.
- Rate limit = Redis fixed-window (INCR+EXPIRE Lua), multi-instance-safe.

## WEAK POINTS (finder targets)
1. **Operational defaults** (see config-deploy.md): auth_mode=none + allowed_origins ["*"] + rate-limit uncapped-if-no-Redis, all warn-only not fatal.
2. **/metrics loopback gap**: api gates it; indexer/aggregator standalone /metrics listeners unwrapped (see entrypoints.md lead #1).
3. **Undocumented staff PII endpoint** /v1/account/admin/lookup (SESS+STAFF, CI-exempted from spec-drift).
4. **Fail-open abuse windows** (rate-limit fail-open if Redis down — prior CS said "fixed"; re-verify).

## NEW handler-level findings (all parameterized — DoS levers, not injection)
1. **MODERATE — missing per-handler query timeout on /v1/ohlc (ohlc.go:135), /v1/twap (twap.go:70), /v1/vwap (vwap.go:175)**: pass raw r.Context() with NO timeout. Siblings /v1/history (history.go:274) + /v1/observations (observations.go:128) wrap in 8s ctx. Server WriteTimeout:30s sets connection write deadline, does NOT cancel request context → a sparse/nonexistent-pair whole-index probe keeps running in Postgres after client disconnects. Clearest actionable finding.
2. **MEDIUM — handler validation gaps skipping strkey/asset check siblings enforce:**
   - /v1/assets/{asset_id}/holders (explorer/account_state.go:277): non-empty only, skips ParseAsset, feeds raw string into 2 CH FINAL scans.
   - /v1/contracts/{id}/interactions + /code-history (contracts_list.go:116,191): non-empty only (unlike ContractDetail's IsContractID); interactions drives 50k-row IN-subquery scan — cheap UNAUTH DoS lever.
   - /v1/pools base=/quote=/asset= (markets.go:212): unvalidated/un-canonicalized → accepted aliases (asset=XLM) return silent-empty instead of 400.
   - /v1/observations LatestTradePerSource: unbounded-window DISTINCT ON + per-pair SWR cache → rotating asset/quote forces cold multi-second full-chunk probes (amplification, capped 8s each).
3. **LOW — client-side SSRF via issuer-controlled logo URL**: SEP-1 image field returned to clients gated only by isSafeImageURL (assets.go:2210) = scheme-only http(s):// check. Hostile issuer sets image:http://169.254.169.254/ → browser <img src> fetches. Not server-side SSRF (never fetched by API), but should reject private/link-local for defense-in-depth.

## Mutating routes recap (authz targets — see entrypoints.md for full table)
account keys (KEY), admin keys/accounts + status-notices (OP, X-Reason audit), signup (OPEN throttled), stripe webhook (SIG, empty-secret→503), dashboard keys/webhooks/price-alerts (SESS). Prior audit refuted 8 IDOR candidates — re-verify ownership checks are still before every mutation, empty-subject fails closed.
