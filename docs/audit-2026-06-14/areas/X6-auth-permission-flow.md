# X6 — Auth / Permission Flow (end-to-end) — security audit

**Date:** 2026-06-14 (written 2026-06-15)
**Scope:** the COMPLETE authentication + authorization chain, traced
end-to-end, hunting specifically for broken object-level authorization
(IDOR), missing authN on mutating routes, tier/privilege bypass,
revocation propagation, session fixation / cookie flags / replay, and
timing/enumeration leaks. Three credential families:

1. **API-key** — mint (`cmd/stellarindex-ops/mint_key.go` + `POST /v1/signup`
   + `POST /v1/account/keys` + dashboard mint) → hash/store
   (`internal/auth/store.go`, `internal/platform/postgresstore/apikey_store.go`)
   → validate (`internal/api/v1/middleware/auth.go` +
   `internal/auth/apikey_{redis,postgres}.go`) → per-tier rate-limit +
   key-policy (`internal/api/v1/middleware/{keypolicy,ratelimit}.go`) →
   usage metering (`internal/usage`, `middleware/usage.go`).
2. **Dashboard session** — magic-link login
   (`internal/api/v1/dashboardauth`) → session cookie → dashboard
   sub-handlers (`dashboardkeys`, `dashboardwebhooks`).
3. **SEP-10** — `internal/auth/sep10` + `internal/api/v1/auth_sep10.go`.

**Mode:** READ-ONLY. No source edited, no git run.
**Dimensions:** X6 (auth/permission flow, primary), D3 (security),
D4 (concurrency).

**Relationship to A12.** This pass deliberately re-walked the same
surface as `A12-auth-account.md` but with the lens narrowed to the
authorization FLOW (who-can-touch-whose-resource + route-gating +
revocation), not the broad crypto/abuse surface A12 covered. Where A12
already nailed a finding I cite it and do not re-litigate. **One A12
High (`/v1/auth/login` magic-link email-bomb) is now FIXED** —
`internal/auth/login_throttle.go` landed 2026-06-15 with per-IP +
per-target-email caps, wired in main.go:1286. The headline X6 finding
below (API-key store split-brain) is a *new* observation A12 flagged
only as a hypothetical ("depends on main.go wiring") — I traced the
wiring and confirmed it is real in the documented `auth_backend=postgres`
posture.

---

## Severity counts

| Severity | Count |
|----------|-------|
| Critical | 0 |
| High     | 1 |
| Medium   | 3 |
| Low      | 3 |
| Info / nit | 3 |

No auth-bypass, no IDOR, no privilege-escalation, no tier-bypass, no
session-fixation, no SEP-10 replay or alg-confusion. Object-level
authorization is correctly enforced on EVERY dashboard + self-service
handler that takes an id (each scopes the lookup to the
caller's account/identifier and returns 404 on mismatch). The single
High is a key-management **split-brain** (mint/revoke surface and the
runtime validator can point at two different stores), which breaks
key issuance + revocation propagation in the cutover posture — a
correctness + security-operational defect, not a live bypass.

---

## Findings

| Severity | file:line | dim | issue | why it matters | fix | confidence |
|----------|-----------|-----|-------|----------------|-----|------------|
| High | `cmd/stellarindex-api/main.go:358-360` (`accountStore = auth.NewRedisAPIKeyStore(rdb)`) vs `:1296-1303` (`pgValidator`) + `internal/api/v1/account.go:337,399,472` | X6/D3 | The self-service key surface (`POST/GET/DELETE /v1/account/keys`) is wired UNCONDITIONALLY to the **Redis** `RedisAPIKeyStore` (`s.accounts`, main.go:358-360 — there is no postgres branch). But when `auth_backend=postgres` (the documented dashboard cutover posture, `buildAuthMiddleware`→`buildAPIKeyValidator` main.go:1136-1146), the runtime **validator** authenticates from **Postgres**. The two stores are disjoint. **Consequence A (mint):** a key minted via `POST /v1/account/keys` lands only in Redis → the Postgres validator's `GetByHash` (apikey_postgres.go:109) misses → `ErrUnauthorized` → the key never authenticates. **Consequence B (revoke):** `DELETE /v1/account/keys/{keyID}` hard-DELs from Redis (`RevokeKeyByID`, list_keys.go:74-108) → the still-valid Postgres row keeps authenticating → **a "revoked" key remains live**. Symmetrically, dashboard-minted (Postgres) keys are invisible to `/v1/account/keys` list/revoke. | Revocation that silently no-ops is a security defect: a customer who rotates away a leaked key via the self-service endpoint believes it's dead while it still authenticates for the full life of the Postgres row. Mint-that-doesn't-authenticate is a hard breakage. A12 flagged this as conditional ("worth a wiring assertion"); the wiring makes it unconditional under `auth_backend=postgres`. | Route the self-service `s.accounts` surface to the SAME backing store the active validator reads (Postgres when `auth_backend=postgres`), OR add a boot-time assertion that the AccountStore writer and the APIKeyValidator reader share a backend and fail loud otherwise. Best: have `/v1/account/keys` reuse the platform `APIKeyStore` + call `pgValidator.InvalidateCachedKey` on revoke exactly as `dashboardkeys.HandleRevoke` does. | High |
| Medium | `internal/api/v1/dashboardwebhooks/handlers.go:352-361` (`HandleListDeliveries`) | X6/D3 | `HandleListDeliveries` checks the session (`SessionFromContext`) and scopes to the account via `parseAndAuthorise` (correct IDOR defence), but — unlike `HandleCreate`/`HandleUpdate`/`HandleDelete` — it does NOT call `canManage(sc.User.Role)`. A `viewer`/`billing`-role member can read another webhook's full delivery history (event types, target-response statuses, last error strings) for any webhook **on their own account**. | Not cross-account (account scoping holds) and read-only, so this is an intra-account role-scope gap, not an IDOR. But it's an inconsistency: the same package gates the other four routes on role and this one doesn't — a `viewer` who is meant to be read-only-DASHBOARD may not be intended to see delivery diagnostics (which can leak partial customer-endpoint behaviour). The `HandleList` (webhook list) is also un-role-gated, same class. | Decide the intended read posture for `viewer`/`billing` and apply it consistently — either add `canManage`/a `canRead` gate to `HandleListDeliveries`+`HandleList`, or document that webhook reads are open to all account roles by design. | High |
| Medium | `internal/api/v1/dashboardauth/middleware.go:150-171` (`touchTracker.last`) | X6/D4 | (Confirms A12.) `touchTracker.last` is an unbounded in-memory `map[uuid.UUID]time.Time` keyed by session id; entries are inserted on first touch (`shouldTouch`, :169) and never evicted. Still unbounded as of this pass (grep for `delete(`/`evict`/`sweep` in the file: none). | Slow memory growth on a security-path structure: one entry per distinct session id for process lifetime. An attacker who can mint many sessions (each magic-link callback → new session id) accelerates it. Bounded only by restarts. | Evict entries older than `interval` on a periodic sweep, or use a TTL/LRU map. | High |
| Medium | `internal/api/v1/dashboardauth/handlers.go:341-342` (`GeoFirstSeen`/`GeoLastSeen` ← `CF-IPCountry`) | X6/D3 | (Confirms A12.) Session geo is taken verbatim from the client-supplied `CF-IPCountry` header with no trusted-proxy gate. If the deployment isn't actually fronted by Cloudflare (or the proxy doesn't strip the inbound header) a client forges it. The `clientIP` resolver (handlers.go:612-633) already gates X-Forwarded-For on the trusted-proxy CIDRs — geo does not get the same treatment. | Forged geo persisted as if trusted in the session audit trail; misleading "logged in from" UX and poisons any future geo-based security signal. Cosmetic today (geo gates nothing). | Honour `CF-IPCountry` only when the request arrived via a trusted proxy (same gate `middleware.RemoteIP` applies); else store "". | High |
| Low | `internal/api/v1/server.go:899-900` + `1207-1221` (Auth middleware vs cookie-only dashboard routes) | X6/D3 | When `auth_mode=apikey` (mandatory bearer), the global `Auth` middleware (stack pos server.go:899) runs BEFORE the mux and 401s any request lacking `Authorization`/`X-API-Key` (auth.go:113-116). The dashboard routes (`/v1/auth/login`, `/v1/auth/callback`, `/v1/dashboard/*`) are cookie-authenticated and carry no bearer header → they would be 401'd before the handler runs, making the entire dashboard unreachable. | Not a security hole (fails CLOSED — over-restrictive), and the deployed posture is `apikey_optional`/`none` where cookie routes pass through as Anonymous and the session middleware still resolves the cookie (server.go:949-950). But it's a latent foot-gun: an operator who flips to `auth_mode=apikey` silently bricks dashboard login with no startup warning. The doc comment at auth.go:36-39 claims endpoints "still 401 anonymous via their own Tier check" — true for `apikey_optional`, NOT for mandatory `apikey` where the middleware 401s first. | Either exempt the cookie-auth route prefixes (`/v1/auth/`, `/v1/dashboard/`) from the bearer Auth middleware, or fail-loud at boot when `auth_mode=apikey` AND the dashboard bundle is wired. | High |
| Low | `internal/auth/sep10/jwt.go:25,45` (`nbf` written, not enforced) | X6/D1 | (Confirms A12 Low.) `nbf` is written on issue (`= iat`, jwt.go:45) but `parseJWT` (jwt.go:71-112) enforces only the signature, `iss`, and `sub`; `exp` is checked at the call site (validator.go:311). `nbf` is never checked. | Harmless today (`nbf == iat` so a fresh token is always already valid). If `nbf` ever diverges from `iat` the gate silently won't honour it. | Enforce `now >= nbf` in `VerifyJWT`, or drop the claim. | High |
| Low | `internal/api/v1/middleware/keypolicy.go:144` (`permissionMatches` prefix) | X6/D3 | (Confirms A12 Low.) `EndpointPrefix` uses raw `strings.HasPrefix(r.URL.Path, e.EndpointPrefix)` with no path-segment boundary. A deny-prefix `/v1/price` also matches `/v1/price-internal`; an allow-prefix is similarly greedy. | Prefix rules can over/under-match adjacent routes; a future route name could silently widen or narrow a customer's deny rule. Today's route set makes collisions unlikely. | Match on a segment boundary: `prefix == path || strings.HasPrefix(path, prefix+"/")`. | High |
| Info | `internal/auth/sep10/jwt.go:31-33,77` (alg-confusion immunity) | X6/D3 | `parseJWT` hard-rejects any token whose first segment isn't byte-identical to the fixed `base64({"alg":"HS256","typ":"JWT"})` (jwt.go:77). `alg:none`, `alg:RS256`-key-confusion, and header-injection are all structurally impossible — the parser never reads `alg` from the token, it pins the entire header. Signature is `subtle.ConstantTimeCompare` (jwt.go:89). | Confirms the single most-exploited JWT bug class (algorithm confusion) is closed by construction, not by a denylist. | — | High |
| Info | SEP-10 challenge → token → JWT chain (`validator.go:227-298`) | X6/D3 | The token endpoint binds the issued JWT's `Sub` to the `clientAccountID` that `ReadChallengeTx` extracted from the SIGNED challenge (validator.go:232,293) — the caller cannot assert an arbitrary account; they must produce a challenge signed by that account's key. Replay guard hashes the full signed XDR and marks AFTER signature verification (validator.go:273-279), so a captured signed XDR is single-use within its TTL and bogus XDR never spends a dedup slot. Empty-signer set → `ErrUnauthorized` (validator.go:258-260). | Confirms no SEP-10 account-impersonation and no challenge replay. (A12's High on the token endpoint being unauthenticated-expensive-crypto stands as an abuse-surface note; not re-rated here — it is a DoS surface, not an authz flaw.) | — | High |
| Info | `cmd/stellarindex-ops/mint_key.go:47-73` (operator-tier provenance) | X6/D3 | `TierOperator` is settable ONLY via the operator CLI `-tier` flag (mint_key.go:47-68), never from any public HTTP path. `POST /v1/signup` forces `TierAPIKey` (signup.go:193) and ignores client-supplied `Tier`/`Identifier`/`RateLimit` (those `req.*` fields are never read in the mint). `POST /v1/account/keys` inherits the caller's own `subject.Tier`/`subject.Identifier` verbatim (account.go:337-344) — a Free key mints another Free key, cannot escalate. Dashboard mint forces `APIKeyTierAPIKey` + clamps `rate_limit_per_min` to the tier ceiling (`clampRateLimitToTier`, dashboardkeys.go:222,404-410). | Confirms the "Free key gets Pro limits / operator tier" privilege-escalation vector is closed at every mint surface — tier is always server-derived, never client-controllable. | — | High |

---

## CORRECT — verified, no issue

Checked specifically against the X6 brief (IDOR / missing-authN /
tier-bypass / revocation / session / replay):

1. **Object-level authorization (IDOR) — enforced on every id-taking
   handler.** Each looks the resource up by id, then compares its
   owner to the SESSION's account (never trusts the path param as the
   authority) and returns 404 (not 403) on mismatch to avoid an
   existence oracle:
   - `dashboardkeys.HandleRevoke` (handlers.go:328-343):
     `existing.AccountID != sc.Account.ID → 404`.
   - `dashboardwebhooks.parseAndAuthorise` (handlers.go:390-417):
     `current.AccountID != accountID → 404`; used by Update / Delete /
     ListDeliveries.
   - `dashboardkeys.HandleList` / `HandleCreate` / `dashboardwebhooks.
     HandleList` / `HandleCreate` scope writes + reads to
     `sc.Account.ID` (handlers.go:168,260,146,217) — no path-supplied
     account id is ever honoured.
   - `/v1/account/keys` (self-service) scopes by `subject.Identifier`:
     `RevokeKeyByID(identifier, keyID)` filters BOTH on identifier and
     keyID (list_keys.go:95) before DEL; `ListKeysForIdentifier`
     filters on identifier (list_keys.go:53). A caller cannot read or
     revoke another identifier's keys.
   - `/v1/account/usage` + `/v1/account/me` derive the lookup key from
     the CALLER's own `subject.KeyID`/`Identifier` (account.go:247-253,
     usage.go:52-60) — no cross-tenant usage read.

2. **AccountID / identifier provenance is server-side, always.** The
   account/identifier a handler scopes to comes from the validated
   session (`sc.Account.ID`, planted by `dashboardauth.Middleware` from
   the cookie → DB lookup) or from the validated `auth.Subject`
   (`subject.Identifier`, set by the validator from the key record) —
   NEVER from a request body or path param. The `createKeyRequest` /
   webhook `createRequest` bodies carry no account/identifier field that
   the handler reads.

3. **No mutating dashboard route mounted outside the session
   middleware.** `dashboardauth.Middleware` (the session resolver) is
   in the GLOBAL stack (`s.sessionAuth`, server.go:949-950) wrapping the
   whole mux via `middleware.Chain`, so every `/v1/dashboard/*` request
   passes through it. Each handler independently re-checks
   `SessionFromContext` and 401s if absent (e.g. dashboardkeys.go:163-167)
   — defence-in-depth, not the sole gate. No dashboard/mutating route is
   reachable without a resolved session in the supported postures.

4. **Tier / privilege bypass — closed at every mint.** See Info finding
   on mint_key.go. Tier is server-derived everywhere; no client input
   path sets it. `KeyPolicy.checkPermissions` defaults CLOSED (no allow
   entries + `AllowAllPermissions=false` → 403, keypolicy.go:125-131)
   and ALWAYS consults the deny list even when `AllowAllPermissions=true`
   (keypolicy.go:119-124). Operator-tier bypasses per-endpoint
   permissions by design (keypolicy.go:66) but still gets IP/Referer
   enforcement; operator tier is unreachable from public mint.

5. **Key revocation propagation (dashboard path) — immediate.**
   `dashboardkeys.HandleRevoke` does Postgres soft-delete (`Revoke`,
   handlers.go:345) THEN `CacheInvalidator.InvalidateCachedKey`
   (handlers.go:354-360) which DELs the Redis read-through cache row
   (apikey_postgres.go:366-371). The Postgres validator re-checks
   `RevokedAt` on both the cache-hit path (apikey_postgres.go:190-192)
   and the canonical path (apikey_postgres.go:116-118), and the cache
   TTL (1h, apikey_postgres.go:77-79) bounds the worst case if the DEL
   fails. Revoke takes effect on the next request. (The self-service
   Redis-store revoke is immediate within its own store too — it hard-DELs
   — the problem is only that it's the WRONG store under
   `auth_backend=postgres`; see the High.)

6. **Session lifecycle — revocation propagates, no fixation.**
   `resolveSession` (middleware.go:86-144) loads the session row by
   cookie-uuid EVERY request and re-validates: `ErrNotFound`/revoked →
   anonymous (middleware.go:96-106), `expires_at` past → anonymous
   (middleware.go:107-111), and a suspended/closed account → session
   REVOKED server-side + denied (middleware.go:123-128). So a
   server-side `RevokeSession` (logout, handlers.go:378; or
   account-suspend) takes effect on the very next request — no cache
   window. The session id is minted fresh on every callback
   (`CreateSession`, handlers.go:335) from a crypto/rand UUIDv4
   (auth.go:121-138) — there is no pre-auth session to fixate, and the
   cookie value is replaced on login, so session fixation is not
   possible.

7. **Cookie flags — correct.** Session cookie is `HttpOnly`,
   `SameSite=Lax`, `Secure` (gated on `CookieSecure` config),
   host-only by default (`CookieDomain` empty), `Path:/`
   (handlers.go:350-359). Logout clears it (`MaxAge:-1`) even for an
   invalid cookie and revokes server-side (handlers.go:375-395). Value
   is a v4 UUID from crypto/rand (256→128-bit, auth.go:121-138).

8. **Magic-link single-use + replay.** `ConsumeMagicLinkToken`
   (called handlers.go:287) is the atomic single-use consume; the token
   is looked up by SHA-256 hash (`HashMagicLinkPlaintext`, auth.go:97-100),
   never plaintext. Purpose is re-checked after consume
   (`tok.Purpose != TokenPurposeLogin → 400`, handlers.go:300-306) with
   the same error shape as not-found (no cross-purpose oracle).
   Expired/absent are distinct only as 410/400, neither reveals
   existence of a token for a different email.

9. **SEP-10 — account binding, replay, alg-confusion.** See the two
   SEP-10 Info findings: JWT `Sub` is bound to the signed challenge's
   client account; replay guard is full-XDR + mark-after-verify;
   alg-confusion impossible by header-pinning; `iss` checked; constant-
   time sig compare.

10. **Magic-link email-bomb (A12 High) — FIXED.**
    `internal/auth/login_throttle.go` (landed 2026-06-15) adds per-IP
    (10/h) + per-target-email (5/h) sliding-window caps; wired in
    main.go:1286 and enforced in `HandleLogin` (handlers.go:212-222)
    with the correct anti-enumeration contract (throttled → same generic
    200 `{status:"sent"}`, no signal to attacker or victim) and
    fail-OPEN on Redis error. Email is hashed before keying
    (login_throttle.go:131-134) so addresses never land in Redis.

11. **Anti-enumeration — login + revoke + IDOR all use the same
    no-oracle contract.** `/v1/auth/login` returns 200 regardless of
    whether the email exists (handlers.go:185-190, 267-275 logs+200 on
    send failure). Cross-account key/webhook access returns 404 ==
    not-found (no "exists on another account" leak). `RevokeKeyByID`
    returns nil for both not-found and not-owned (list_keys.go:106-108).
    Signup duplicate returns 409 with the EXISTING key_id only to the
    owner of that email (gated by the SETNX reservation), not a bare
    "email taken" oracle.

12. **Open-redirect — callback `next` is path-only.**
    `next` is rejected unless it starts with `/` and not `//`
    (handlers.go:364-367), defeating `//evil.com` and absolute-URL
    open redirects; empty falls through to `/`.

---

## Notes on the brief's specific concerns

- **Broken object-level authz (IDOR):** Not found. Every id-taking
  handler scopes to the session/subject's account, not the path param.
  See CORRECT #1, #2.
- **Missing authN on a mutating route:** Not found — all dashboard +
  account routes are inside the global session/auth middleware and
  re-check the principal. The closest thing is the `apikey`-mode
  interaction (Low) where the dashboard becomes *over*-restricted
  (401), not under.
- **Privilege / tier bypass:** Not found. Tier is server-derived at
  every mint (Info, CORRECT #4).
- **Revocation propagation:** Dashboard path is immediate (CORRECT #5);
  session revocation is immediate (CORRECT #6). **The split-brain High
  is the one place revocation can silently no-op** (self-service Redis
  revoke against a Postgres validator).
- **Session fixation / cookie flags / token entropy / replay:** All
  sound — see CORRECT #6, #7, #8, #9. The biased 6-digit paste code
  (A12 Low, auth.go:86-92) is not a credential; consumption requires
  the full plaintext token — not re-rated.
- **Timing / enumeration in auth comparisons:** JWT sig compare is
  constant-time (jwt.go:89); API-key + magic-link lookups are by
  SHA-256 hash equality (Redis GET / `WHERE key_hash=$1`), no plaintext
  compare; anti-enumeration contracts hold (CORRECT #11).
- **Intra-account role scoping:** the one gap is webhook reads
  (`HandleListDeliveries` / `HandleList`) not gating on role (Medium).

---

## Files read (count: 17)

`internal/api/v1/dashboardauth/`: handlers.go, middleware.go, auth.go.
`internal/api/v1/dashboardkeys/`: handlers.go.
`internal/api/v1/dashboardwebhooks/`: handlers.go.
`internal/api/v1/middleware/`: auth.go, keypolicy.go.
`internal/api/v1/`: account.go, signup.go, auth_sep10.go,
server.go (Handler() stack + route mounting).
`internal/auth/`: store.go, list_keys.go, apikey_postgres.go,
login_throttle.go.
`internal/auth/sep10/`: validator.go, jwt.go.
`cmd/stellarindex-api/main.go` (auth + dashboard wiring),
`cmd/stellarindex-ops/mint_key.go` (operator mint, grepped).
`internal/config/config.go` + `validate.go` (auth_mode default + set),
`configs/example.toml` (deployed posture), grepped.

(Sibling `A12-auth-account.md` read first for format + to avoid
re-litigating its findings; test files enumerated but not line-audited.)
