# W19 — Security, secrets, auth, billing

## Scope

Every authentication, authorization, and money path.

## Inputs

- `internal/auth/`
- `internal/auth/sep10/`
- `internal/platform/`: account, apikey, audit, billing, errors,
  token, usage, user, webhook
- `internal/platform/postgresstore/`
- `internal/usage/counter.go`
- `internal/notify/`
- `internal/customerwebhook/` (W32 cross-ref)
- `cmd/ratesengine-api/main.go` CORS / trusted-proxy
- `cmd/ratesengine-ops/mint_key.go`, `upgrade_key.go`
- `internal/api/v1/stripe_webhook.go` (W33 cross-ref)
- `.gitleaks.toml`
- `internal/config/`

## Checks

| # | Check | Method |
| --- | --- | --- |
| W19.1 | API key store: postgres + redis dual; hashed at rest | code |
| W19.2 | SEP-10 challenge/verify/JWT lifecycle | code + tests |
| W19.3 | JWT alg pinned (no algorithm-confusion) | code |
| W19.4 | rate-limit identity: API key > IP > anonymous; 127.0.0.1 exempt | middleware |
| W19.5 | CORS allowlist | main.go |
| W19.6 | Trusted-proxy list: Caddy IPs + Cloudflare IPs only | main.go + Caddy |
| W19.7 | `.gitleaks.toml` scans all tracked files (no exclude list) | scan |
| W19.8 | Secret management: env-only; no secret in code, no secret in docs | grep |
| W19.9 | mint_key.go: `--tier` required; no silent-default-to-high | flag handling |
| W19.10 | upgrade_key.go: respects audit log | flag handling + audit.go |
| W19.11 | dashboardauth admin gate: behind WG / VPN / IP allowlist (or strong auth) | config |
| W19.12 | Audit log integrity: append-only; no DELETE path | platform/audit.go |
| W19.13 | TLS auto-renew alerting: cert expiry monitored | r1 probe + metric |
| W19.14 | NEW: W32 webhook SSRF defence | cross-ref |
| W19.15 | NEW: W33 Stripe webhook signature + idempotency | cross-ref |
| W19.16 | NEW: customer URL pin: DNS-rebind defence | W32 cross-ref |
| W19.17 | Request signing on customer webhooks (HMAC) | W32 cross-ref |
| W19.18 | signup ip throttle (`internal/auth/signup_ip_throttle.go`) | code |
| W19.19 | signup email locker / verifier | code |
| W19.20 | apikey list + revoke flow | code |

## Closure criteria

Every check terminal. Findings on:
- ANY auth bypass (`critical`)
- ANY signing gap (`critical`/`high`)
- ANY money-touching path without idempotency
- ANY secret in tracked files
