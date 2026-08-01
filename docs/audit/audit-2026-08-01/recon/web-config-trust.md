# Web / config / trust boundaries — recon (audit-2026-08-01, HEAD f8c099ee)

Cold, read-only. (Full detail in tasks/a1f601d74b7fc4365.output.)

## Top trust-boundary risks (→ skeptic verification)

| # | Sev | Who | Risk |
|---|---|---|---|
| R1 | HIGH | unauth | Production anon rate limit is **6000/min, not 60** (toml.j2:238 vs config.go default 60). Every DoS estimate in the recipe is sized 100× wrong. |
| R2 | HIGH | unauth | The refresh gate bounds the DETACHED tier only. FIVE unauth account routes (/accounts/{g}/transactions,operations,movements,trades,positions) read the same 8-conn ClickHouse pool INLINE, keyed on attacker-mintable G-addresses, no cross-key bound. Gate reserving 4 of 8 leaves inline headroom of 4. |
| R3 | HIGH | unauth | Gate saturation on /accounts/{g} returns HTTP 500 + Error-level log per request (the exact endpoint the gate protects) — errAccountStateRefreshFailed (pkg clickhouse) vs errRefreshSaturated (pkg explorer) can't errors.Is across packages, so the 503 mapping siblings use is unreachable. → false 5xx SLO burn + log-volume amplification on a disk-full-incident host. |
| R4 | HIGH | unauth | UA `stellarindex-smoke/1|-probe/|-prewarm/` erases the request from ALL HTTP metrics + latency/error SLO (http_middleware.go:98, bare prefix test, no loopback condition). Detection evasion (throttle still applies). |
| R5 | HIGH | insider/regression | **46 vitest files in web/explorer NEVER execute** — no pnpm test in CI, no web-test Make target. Dead gates include og.test.js (OG-SSRF), safe-domain.test.ts (XSS/phishing), no-duplicated-price-formatters.test.ts. |
| R6 | HIGH | malicious issuer | No gate enforces the serializeJsonLd chokepoint (14 dangerouslySetInnerHTML sites comply by CONVENTION); CSP is script-src 'self' 'unsafe-inline' so no backstop. |
| R7 | HIGH | host-adjacent | Redis has NO AUTH on r1 AND is the canonical tier-bearing API-key store under the default auth_backend=redis. Any loopback/file-write/SSRF-to-loopback → mint an operator-tier key, no 2FA. Highest single-step escalation on the host. |
| R8 | MED | insider | DEPLOY_APPROVAL_RELAXED=true still armed → deploys land with no human approval (honest/fail-closed-on-other-values, but live). |
| R9 | MED | correctness | Frontend ADR-0003 residuals (markets/[pair]/page.tsx:472 Number()+/1e7 on quote_volume; :548,551; issuers:509) — lint-i128.sh is Go-only, nothing gates web. |
| R10 | HIGH | unauth | /accounts/{g}/positions runs SIX sequential uncached Postgres folds per request (positions.go:229-234) against the 25-conn pool; at 6000/min that's ~600 PG queries/s from one IP. |

## Verified-strong (do NOT re-flag; codify as positive patterns)
- Ingress identity: XFF honoured only from trusted peer, walked right-to-left; IPv6 /64 aggregation; anon bucket keys on resolved IP NOT Subject.Identifier (UA-rotation bypass closed); key length capped 256B; two-stage fail (open on transient, closed on sustained). A refactor back to Identifier re-opens total bypass.
- Order-book quarantine = the repo's clearest "model compromised upstream as adversary"; contractsWindowLadder quantiser (365→5 window sizes, rounds up = superset) = the bound template.
- OG edge SSRF fully closed (Map not object, single decode, HTML-escape before satori, private-IP block incl 169.254.169.254, kill switch). Residual = amplification (attacker-selectable cache keys → render + origin fetch), not SSRF.
- safe-domain.ts (isSafeHomeDomain/isSafePublicImageUrl) strong; all consumers guarded + rel=noreferrer. Convention only (R6 class).
- Migration rollback-safety gate (rule 9, lint-migration-compat.sh) = strongest gate in the tree; runs --staged inside deploy.yml as the last gate before migrate up. Self-bypass fix (CID-03) restores base-ref for lint-migrations + baseline-growth (20 other gates PR-head-judged = accepted residual).
- Staff lookup double-gated (RequireSession + IsStaff→403) + durable audit row; residual: throttled at anon 6000/min tier (SessionAuth inside RateLimit) → stolen session enumerates customer base at 100/s; audit fails open.

## Config prod-vs-default table (the load-bearing facts)
anon_rate_limit 60→6000 · key_rate_limit →6000 · auth_mode none→apikey_optional ·
signup_require_email_verification →FALSE · auth_backend →redis (unset) · Redis no AUTH.

## Recipe deltas
"anon 60/min"→6000. "refresh-gate bounds this"→detached only, 5 inline routes unbounded +
saturation 500s. "serializeJsonLd rule"→convention, NO rule (same safe-domain). Frontend
ADR-0003 has NO gate. 46 explorer tests never run. Add adversary: synthetic-UA metric erasure.
Redis-no-AUTH is the canonical tier-bearing key store, not just a cache.
