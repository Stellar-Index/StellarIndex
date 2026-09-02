package dashboardauth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/pii"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/httpx"
	"github.com/Stellar-Index/StellarIndex/internal/notify"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// EmailLocker serialises first-login provisioning per email so
// concurrent /v1/auth/callback callers can't both create
// speculative Account rows before the email-unique-index Users
// insert decides a winner.
//
// F-1255 (codex audit-2026-05-12): without this seam, two valid
// magic links for the same just-verified email racing through
// the callback both pass `GetUserByEmail → ErrNotFound`, both
// `Accounts.Create` succeed (slug uniqueness gets resolved with
// the 4-hex retry), then only the first `Users.CreateUser` wins.
// The losing caller's Account row is then orphaned. A best-
// effort Suspend-mark on the orphan exists as defence-in-depth,
// but acquiring a per-email lock BEFORE the Account.Create
// removes the orphan creation entirely.
//
// Implementations:
//   - production: Redis SETNX with a 30s TTL (the
//     `dashboardauth.RedisEmailLocker` adapter).
//   - tests / Redis-less dev: nil — the Suspend-on-conflict
//     fallback handles the rare race without serialisation.
//
// `Acquire` returns (true, token, …) when the caller now holds
// the lock and must call `Release` with the SAME token. (false,
// "", …) means another caller holds it — the caller should poll
// `Users.GetUserByEmail` briefly to find the winner's user.
// Errors propagate; treat them as "lock not acquired, fall
// through to the legacy path".
//
// The `token` is a per-acquire fencing token (F-C, audit
// -2026-08-14): `Release` must delete the lock ONLY if it still
// carries this caller's token, so a holder that overran its TTL
// (a slow Account.Create + Users.CreateUser) cannot delete a
// successor's freshly-acquired lock. `Release` with an empty
// token is a no-op (nothing was held).
type EmailLocker interface {
	Acquire(ctx context.Context, emailHash string, ttl time.Duration) (bool, string, error)
	Release(ctx context.Context, emailHash, token string) error
}

// LoginThrottle, when set, bounds magic-link sends to prevent inbox
// email-bombing + sender-reputation / email-quota burn. The global
// anonymous rate-limit only caps per-IP REQUEST volume (60/min); a single
// IP under that ceiling can still bomb one victim inbox or spray many
// addresses, and each accepted request fires an outbound email. nil
// disables the check (legacy behaviour). audit-2026-06-14 A12.
type LoginThrottle interface {
	// Allow reports whether a magic-link send for (ip, email) is within
	// quota. On false the handler MUST skip the email yet still return the
	// generic 200 {status:"sent"} — neither the attacker nor the victim may
	// learn a throttle fired (the same anti-enumeration contract the
	// endpoint already keeps). A non-nil error means the backing store is
	// unavailable; the handler falls OPEN (sends) — availability over a
	// brief abuse window, mirroring the signup throttle's fail-open. email is
	// the lowercased address; implementations hash it before keying on it.
	Allow(ctx context.Context, ip, email string) (bool, error)
}

// Config wires the auth handlers' dependencies. Constructed
// once at server boot; mounted into the v1 mux.
type Config struct {
	Accounts  platform.AccountStore
	Users     platform.UserStore
	Tokens    platform.TokenStore
	Sender    notify.Sender
	Generator *Generator
	Logger    *slog.Logger
	Now       func() time.Time
	// EmailLocker (optional) serialises first-login provisioning
	// per email. nil = no locking; the handler falls through to
	// the legacy Suspend-on-conflict recovery path. F-1255.
	EmailLocker EmailLocker
	// LoginThrottle (optional) caps magic-link sends per IP + per
	// target email. nil = no throttle (only the global anon
	// rate-limit applies). audit-2026-06-14 A12.
	LoginThrottle LoginThrottle
	// Audit (optional) is the durable audit sink privileged staff
	// actions on this surface append to — currently the staff
	// customer look-up (C3-056, audit-2026-07-23), which reads another
	// customer's PII and must leave the same durable trail the sibling
	// admin surfaces do. nil degrades that handler to structured-log
	// -only audit (Redis-less / Postgres-less deployments).
	Audit platform.AuditStore
	// Passkeys (optional) is the WebAuthn credential store backing
	// passkey sign-in (migration 0140). nil leaves the
	// /v1/auth/passkey/* routes unmounted — email-code sign-in
	// still works.
	Passkeys platform.WebAuthnCredentialStore
	// PasskeyCeremonyGuard (optional) makes each WebAuthn ceremony
	// challenge single-use, so a captured finish-ceremony request
	// cannot be replayed into a second session (audit-2026-08-13).
	// Production wires the Redis-SETNX adapter so the spent-set is
	// shared across API instances; nil defaults to the in-process
	// guard installed by validate() below — never nil at runtime, so
	// the protection cannot be absent, only single-instance.
	PasskeyCeremonyGuard PasskeyCeremonyGuard
	// DashboardBaseURL is the absolute URL of the explorer hosting
	// the in-site dashboard (typically https://stellarindex.io).
	// The magic-link callback URL embedded in emails is
	// `{DashboardBaseURL}/auth/callback?token=<plaintext>` and the
	// post-login redirect lands on `{DashboardBaseURL}{next}` (next
	// defaults to "/"; the dashboard itself lives under /dashboard/*).
	DashboardBaseURL string
	// EmailFrom is the From: address (e.g.
	// `Stellar Index <hello@stellarindex.io>`).
	EmailFrom string
	// MagicLinkTTL — link validity. Default 15 minutes.
	MagicLinkTTL time.Duration
	// SessionTTL — cookie session lifetime. Default 30 days
	// rolling.
	SessionTTL time.Duration
	// CookieSecure — set Secure flag on the cookie. Production
	// = true; local dev = false (the dashboard runs over
	// http://localhost during dev).
	CookieSecure bool
	// CookieDomain — empty = host-only cookie scoped to the API
	// host. Set to ".stellarindex.io" (as prod does) so the apex
	// explorer and the api subdomain share the session cookie on
	// credentialed cross-origin requests.
	CookieDomain string
}

// validate fills in defaults and rejects unworkable configs.
func (c *Config) validate() error {
	if c.Accounts == nil || c.Users == nil || c.Tokens == nil {
		return errors.New("dashboardauth: stores are required")
	}
	if c.Sender == nil {
		return errors.New("dashboardauth: sender is required")
	}
	if c.Generator == nil {
		c.Generator = NewGenerator()
	}
	if len(c.Generator.Secret) == 0 {
		// The 6-digit code derivation must NEVER run unkeyed (that is
		// the vulnerability this secret exists to close — see
		// [Generator.CodeForHash]). No configured secret → random
		// per-process one: codes stay verifiable within this process's
		// lifetime, and a restart merely invalidates in-flight codes
		// (≤15 min; the magic link keeps working).
		secret := make([]byte, 32)
		if _, err := c.Generator.Read(secret); err != nil {
			return fmt.Errorf("dashboardauth: generate code secret: %w", err)
		}
		c.Generator.Secret = secret
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Now == nil {
		c.Now = func() time.Time { return time.Now().UTC() }
	}
	// Replay protection for passkey ceremonies is never optional —
	// only its blast radius is. Redis-less deployments get the
	// in-process spent-set (single-instance accounting), the same
	// posture the NTF-08 login-throttle fallback takes. Must come
	// after the Now default: the guard expires records on that clock.
	if c.PasskeyCeremonyGuard == nil {
		c.PasskeyCeremonyGuard = newInProcessPasskeyCeremonyGuard(c.Now)
	}
	if c.DashboardBaseURL == "" {
		return errors.New("dashboardauth: DashboardBaseURL is required")
	}
	if c.EmailFrom == "" {
		return errors.New("dashboardauth: EmailFrom is required")
	}
	if c.MagicLinkTTL == 0 {
		c.MagicLinkTTL = 15 * time.Minute
	}
	if c.SessionTTL == 0 {
		c.SessionTTL = 30 * 24 * time.Hour
	}
	return nil
}

// Handlers exposes the auth flow to be mounted in the v1 mux.
type Handlers struct{ cfg *Config }

// NewHandlers validates the config and returns a mount-ready
// Handlers.
func NewHandlers(cfg Config) (*Handlers, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Handlers{cfg: &cfg}, nil
}

// Mount installs the auth routes onto a mux:
//
//	POST /v1/auth/login        — request sign-in email (link + code)
//	GET  /v1/auth/callback     — consume magic-link token, mint session
//	POST /v1/auth/verify-code  — consume the 6-digit code, mint session
//	POST /v1/auth/logout       — revoke current session
//
// Plus, when Config.Passkeys is wired (see passkey.go):
//
//	POST   /v1/auth/passkey/begin-login       — WebAuthn assertion options
//	POST   /v1/auth/passkey/finish-login      — verify assertion, mint session
//	POST   /v1/auth/passkey/begin-register    — creation options (session-gated)
//	POST   /v1/auth/passkey/finish-register   — store credential (session-gated)
//	GET    /v1/auth/passkey/credentials       — list passkeys (session-gated)
//	DELETE /v1/auth/passkey/credentials/{id}  — remove one (session-gated)
//
// Caller wires the result into the regular middleware stack
// (RequestID + Logger + Recoverer + RateLimit + ...). These
// routes intentionally do NOT pass through the API-key auth
// middleware — they're the entry point for unauthenticated
// users.
//
// The three POSTs are wrapped in [middleware.RequireSameSiteWrite]
// (C3-031 / C3-057). They are state-changing and browser-driven:
// login MINTS a token (and the intent cookie that binds it), and
// verify-code MINTS a session — so a cross-site page that can
// drive either is a login-CSRF primitive even though neither
// consumes a session cookie. `GET /v1/auth/callback` is a
// top-level navigation from an email client and carries no usable
// Origin; it is bound instead by [LoginIntentCookieName] (C3-030).
func (h *Handlers) Mount(mux *http.ServeMux) {
	sameSite := middleware.RequireSameSiteWrite(h.cfg.Logger)
	mux.Handle("POST /v1/auth/login", sameSite(http.HandlerFunc(h.HandleLogin)))
	mux.HandleFunc("GET /v1/auth/callback", h.HandleCallback)
	mux.Handle("POST /v1/auth/verify-code", sameSite(http.HandlerFunc(h.HandleVerifyCode)))
	mux.Handle("POST /v1/auth/logout", sameSite(http.HandlerFunc(h.HandleLogout)))
	// Staff customer look-up — session-gated (RequireSession) AND staff-gated
	// (HandleAdminLookup checks IsStaff). Backs /dashboard/admin's first tool.
	mux.Handle("GET /v1/account/admin/lookup",
		RequireSession(h.cfg)(http.HandlerFunc(h.HandleAdminLookup)))

	// Passkey (WebAuthn) routes — only when a credential store is
	// wired. Registration + management are session-gated (adding a
	// passkey is a privilege of an existing session); the login pair
	// is the unauthenticated entry point, like /v1/auth/login. Every
	// state-changing route keeps the same-site write gate the other
	// auth POSTs carry — finish-login MINTS a session, so it is a
	// login-CSRF primitive exactly as verify-code is (C3-031/C3-057).
	if h.cfg.Passkeys != nil {
		requireSession := RequireSession(h.cfg)
		mux.Handle("POST /v1/auth/passkey/begin-login",
			sameSite(http.HandlerFunc(h.HandlePasskeyBeginLogin)))
		mux.Handle("POST /v1/auth/passkey/finish-login",
			sameSite(http.HandlerFunc(h.HandlePasskeyFinishLogin)))
		mux.Handle("POST /v1/auth/passkey/begin-register",
			requireSession(sameSite(http.HandlerFunc(h.HandlePasskeyBeginRegister))))
		mux.Handle("POST /v1/auth/passkey/finish-register",
			requireSession(sameSite(http.HandlerFunc(h.HandlePasskeyFinishRegister))))
		mux.Handle("GET /v1/auth/passkey/credentials",
			requireSession(http.HandlerFunc(h.HandlePasskeyList)))
		mux.Handle("DELETE /v1/auth/passkey/credentials/{id}",
			requireSession(sameSite(http.HandlerFunc(h.HandlePasskeyDelete))))
	}
}

// maxCodeAttempts caps wrong 6-digit code guesses against a single
// login token before it stops being a code candidate (the magic link
// still works — the cap gates code matching only). 5 keeps the
// brute-force success probability against the ~1e6 code space
// negligible while tolerating a fat-fingered user.
const maxCodeAttempts = 5

// Durable per-EMAIL code-failure policy (C3-032, audit-2026-07-23).
//
// maxCodeAttempts alone is a per-MINT cap: a new /v1/auth/login hands
// the guesser a fresh 5. The only thing bounding re-mints was
// [auth.RedisLoginThrottle] (5 sends/hour/target-email), which lives in
// Redis with an in-process fallback — a FLUSHALL, a fail-over or a
// restart clears it, and it is a fixed window that resets hourly
// regardless. Standing budget: ~25 guesses/hour/email, indefinitely
// ≈ 0.22 probability of hitting a code over a year of patient grinding
// on one targeted address.
//
// These numbers cut that to ~10 guesses/day ≈ 3.7e-3/year — two orders
// of magnitude down — at a usability cost that is close to zero,
// because the lockout gates the CODE path only: the magic link in the
// same email keeps working, exactly as maxCodeAttempts already does.
// A legitimate user would have to mistype ten times in a day to meet
// it, and their recovery is "click the link you were sent".
//
// Deliberately NOT tunable per deployment: an operator lowering the
// bound is the failure mode this finding is about.
const (
	// maxDurableCodeFailures is the per-email failure budget inside
	// durableCodeFailureWindow before the code path locks.
	maxDurableCodeFailures = 10
	// durableCodeFailureWindow is how long failures accumulate. A window
	// that fully elapses without reaching the cap starts fresh.
	durableCodeFailureWindow = 24 * time.Hour
	// durableCodeLockout is how long the code path stays shut once the
	// cap is met. Matched to the window so the sustained budget is
	// ~maxDurableCodeFailures per day rather than per window boundary.
	durableCodeLockout = 24 * time.Hour
)

// loginRequest is the JSON body POST /v1/auth/login accepts.
type loginRequest struct {
	Email string `json:"email"`
}

type loginResponse struct {
	Status string `json:"status"`
}

// HandleLogin issues a magic-link email. Always returns 200
// with `{status: "sent"}` regardless of whether the email
// matches an existing user — leaking that information would
// let attackers enumerate valid emails. Real signup happens
// on consumption: if the user doesn't exist when they click
// the link, the callback handler creates the account.
func (h *Handlers) HandleLogin(w http.ResponseWriter, r *http.Request) {
	// MaxBytesReader, not LimitReader: LimitReader returns (n, nil) at
	// its cap, so the read succeeds on a silently truncated body and
	// this branch was unreachable — an oversize body surfaced as
	// "malformed JSON". audit-2026-08-13.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<10))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "request body too large", "/v1/auth/login")
		return
	}
	var req loginRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "malformed JSON", "/v1/auth/login")
		return
	}
	email := strings.TrimSpace(strings.ToLower(req.Email))
	if !looksLikeEmail(email) {
		writeProblem(w, http.StatusBadRequest, "invalid email", "/v1/auth/login")
		return
	}

	// Magic-link abuse throttle (audit-2026-06-14 A12). Over quota → skip the
	// send but return the SAME generic 200 below, so neither an attacker nor
	// the victim's inbox learns a throttle fired. Redis blip → fall open
	// (the global anon rate-limit still bounds per-IP volume).
	if h.cfg.LoginThrottle != nil {
		ok, terr := h.cfg.LoginThrottle.Allow(r.Context(), clientIP(r).String(), email)
		switch {
		case terr != nil:
			h.cfg.Logger.Warn("login throttle unavailable; falling open", "err", terr)
		case !ok:
			h.cfg.Logger.Warn("magic-link login throttled", "ip", clientIP(r).String())
			// Emit a login-intent cookie here too, of a token that was
			// never persisted. Without it the throttled response would
			// be the only 200 that carries no `Set-Cookie:
			// stellarindex_login_intent` — a byte-visible oracle for
			// "a throttle fired for this address", which is exactly
			// what [LoginThrottle]'s contract forbids leaking. The
			// decoy digest witnesses a token no store ever saw, so it
			// can never redeem anything.
			if _, decoy, _, gerr := h.cfg.Generator.NewToken(); gerr == nil {
				h.setLoginIntentCookie(w, r, decoy)
			}
			_ = json.NewEncoder(w).Encode(loginResponse{Status: "sent"})
			return
		}
	}

	plaintext, hash, code, err := h.cfg.Generator.NewToken()
	if err != nil {
		h.cfg.Logger.Error("magic link token generation failed", "err", err)
		writeProblem(w, http.StatusInternalServerError, "internal error", "/v1/auth/login")
		return
	}

	if err := h.cfg.Tokens.CreateMagicLinkToken(r.Context(), platform.MagicLinkToken{
		TokenHash:   hash,
		Email:       email,
		Purpose:     platform.TokenPurposeLogin,
		ExpiresAt:   h.cfg.Now().Add(h.cfg.MagicLinkTTL),
		RequestedIP: clientIP(r),
	}); err != nil {
		h.cfg.Logger.Error("create magic link token", "err", err, "email", maskEmail(email))
		writeProblem(w, http.StatusInternalServerError, "internal error", "/v1/auth/login")
		return
	}

	// Bind the link to THIS browser (C3-030 login CSRF). Set before
	// the email goes out so a link can never be live without its
	// witness. See [LoginIntentCookieName].
	h.setLoginIntentCookie(w, r, hash)

	// Build callback URL: {dashboard}/auth/callback?token=<plaintext>
	cb, err := url.Parse(h.cfg.DashboardBaseURL)
	if err != nil {
		h.cfg.Logger.Error("invalid DashboardBaseURL", "err", err, "url", h.cfg.DashboardBaseURL)
		writeProblem(w, http.StatusInternalServerError, "internal error", "/v1/auth/login")
		return
	}
	cb.Path = strings.TrimRight(cb.Path, "/") + "/auth/callback"
	q := cb.Query()
	q.Set("token", plaintext)
	cb.RawQuery = q.Encode()

	msg, err := notify.MagicLinkMessage(h.cfg.EmailFrom, email, notify.MagicLinkInput{
		LinkURL:          cb.String(),
		Code:             code,
		ExpiresInMinutes: int(h.cfg.MagicLinkTTL / time.Minute),
		IPAddress:        clientIP(r).String(),
		UserAgent:        truncateUA(r.UserAgent()),
	})
	if err != nil {
		h.cfg.Logger.Error("render magic link template", "err", err)
		writeProblem(w, http.StatusInternalServerError, "internal error", "/v1/auth/login")
		return
	}
	if err := h.cfg.Sender.Send(r.Context(), msg); err != nil {
		// Instrument the mail outage (task #33 / W8 recon 9c): the send
		// failure is otherwise swallowed here (200 either way, see below),
		// so without this counter a Resend outage that silently kills login
		// is invisible until users complain.
		obs.NotifySendsTotal.WithLabelValues(obs.NotifyTemplateMagicLink, obs.NotifySendResultFailed).Inc()
		// Log + return 200 anyway — the user shouldn't see
		// "we tried to email you and failed" because that's a
		// signal an attacker can use to confirm an email
		// exists. Operator gets the alert via Loki / Sentry.
		h.cfg.Logger.Error("send magic link email", "err", err, "email", maskEmail(email))
	} else {
		obs.NotifySendsTotal.WithLabelValues(obs.NotifyTemplateMagicLink, obs.NotifySendResultSent).Inc()
	}

	_ = json.NewEncoder(w).Encode(loginResponse{Status: "sent"})
}

// sessionSameSite picks the session-cookie SameSite policy. The explorer at
// stellarindex.io calls the API at api.stellarindex.io — cross-*origin* but
// **same-site** (both are the `stellarindex.io` registrable domain). SameSite
// is a *site*-level control, so Lax is sent on those credentialed same-site
// requests while blocking genuine cross-*site* (e.g. evil.com) requests.
//
// CS-124: this previously returned SameSite=None (unnecessary — None is only
// needed for a different registrable domain), which let any site auto-submit a
// credentialed POST to the cookie-authed /v1/dashboard/* mutation handlers
// (CSRF — e.g. creating a webhook that exfiltrates the victim's payloads). Lax
// closes that with no impact on the legitimate same-site flow. If the dashboard
// is ever served from a truly different site, add a CSRF token — do NOT revert
// to None.
//
// C3-031 / C3-057 (audit-2026-07-23) closed the residual Lax leaves open:
// SameSite is a *site*-level control, so a sibling origin under the same
// registrable domain still counts as same-site and Lax still permits a
// top-level cross-site GET. Every state-changing dashboard + auth route is
// now additionally gated by [middleware.RequireSameSiteWrite] (Origin /
// Referer allow-list), and the magic-link callback — a GET, and therefore
// beyond any Origin check — is gated by [LoginIntentCookieName] (C3-030).
// This cookie's SameSite mode is defence in depth, no longer the only line.
func sessionSameSite() http.SameSite {
	return http.SameSiteLaxMode
}

// HandleCallback consumes a magic-link token, finds-or-creates
// the user (single-org v1: each email gets one account on first
// signup), and issues a session cookie.
//
// The link must be opened in the browser that requested it: the
// login-intent cookie is checked BEFORE the token is consumed
// (C3-030 — see [LoginIntentCookieName] for the login-CSRF this
// closes). Checking first, not after, means a user who merely
// opened their own link on a second device doesn't burn the token
// — they can still click it on the device that asked for it.
func (h *Handlers) HandleCallback(w http.ResponseWriter, r *http.Request) {
	plaintext := r.URL.Query().Get("token")
	if plaintext == "" {
		writeProblem(w, http.StatusBadRequest, "missing token", "/v1/auth/callback")
		return
	}
	tokenHash := HashMagicLinkPlaintext(plaintext)
	if !hasLoginIntent(r, tokenHash) {
		h.cfg.Logger.Warn("magic-link callback without a matching login-intent cookie",
			"ip", clientIP(r).String())
		writeProblem(w, http.StatusForbidden,
			"this sign-in link must be opened in the browser that requested it — "+
				"enter the 6-digit code from the email instead, or request a new link from this device",
			"/v1/auth/callback")
		return
	}
	tok, err := h.cfg.Tokens.ConsumeMagicLinkToken(r.Context(), tokenHash)
	if err != nil {
		switch {
		case errors.Is(err, platform.ErrTokenExpired):
			writeProblem(w, http.StatusGone, "this link has expired — request a fresh one", "/v1/auth/callback")
		case errors.Is(err, platform.ErrNotFound):
			writeProblem(w, http.StatusBadRequest, "invalid token", "/v1/auth/callback")
		default:
			h.cfg.Logger.Error("consume magic link", "err", err)
			writeProblem(w, http.StatusInternalServerError, "internal error", "/v1/auth/callback")
		}
		return
	}
	if tok.Purpose != platform.TokenPurposeLogin {
		// A token minted for invite-accept can't be used to
		// log in. Same-error-shape as not-found to avoid
		// leaking whether a token exists for a different purpose.
		writeProblem(w, http.StatusBadRequest, "invalid token", "/v1/auth/callback")
		return
	}

	if err := h.startSessionForEmail(w, r, tok.Email); err != nil {
		h.cfg.Logger.Error("start session (callback)", "err", err, "email", maskEmail(tok.Email))
		writeProblem(w, http.StatusInternalServerError, "internal error", "/v1/auth/callback")
		return
	}
	h.clearLoginIntentCookie(w)

	// Redirect into the dashboard. Caller-supplied `next` URL
	// is path-only (we never honour absolute URLs to avoid
	// open-redirect attacks); empty falls through to "/".
	next := r.URL.Query().Get("next")
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		next = "/"
	}
	dest := strings.TrimRight(h.cfg.DashboardBaseURL, "/") + next
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// verifyCodeRequest is the JSON body POST /v1/auth/verify-code accepts.
type verifyCodeRequest struct {
	Email string `json:"email"`
	Code  string `json:"code"`
}

type verifyCodeResponse struct {
	Status string `json:"status"`
}

// HandleVerifyCode consumes the 6-digit email code — the paste-friendly
// alternative to clicking the magic link — and mints the same session
// cookie HandleCallback does. Unlike the callback it returns JSON
// (`{status:"ok"}`) rather than a 303, because the SPA calls it via a
// credentialed fetch: the Set-Cookie rides the response and the SPA
// navigates itself. Same find-or-create-on-first-login semantics.
//
// The code is matched only against the email's in-flight login tokens
// and each wrong guess burns an attempt (see [maxCodeAttempts]); all
// failure modes return one generic error so a caller can't tell "no
// token" from "wrong code" from "too many attempts".
//
// Non-matching includes CORRECT-BUT-STALE. A code whose token has
// expired, been consumed, or already burned [maxCodeAttempts] is not a
// candidate, so submitting it registers a durable per-email failure
// (C3-032) exactly as a wrong guess does. That is deliberate — the
// server cannot distinguish "the owner pasted yesterday's code" from
// "an attacker guessed a value that happens to match a dead token"
// without leaking which — and it is affordable because the lockout
// gates this door only: the magic LINK in the same email keeps working,
// and any successful sign-in clears the counter. A user who exhausts
// the budget on stale codes clicks the link instead.
func (h *Handlers) HandleVerifyCode(w http.ResponseWriter, r *http.Request) {
	// MaxBytesReader, not LimitReader — see HandleLogin.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<10))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "request body too large", "/v1/auth/verify-code")
		return
	}
	var req verifyCodeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "malformed JSON", "/v1/auth/verify-code")
		return
	}
	email := strings.TrimSpace(strings.ToLower(req.Email))
	code := strings.TrimSpace(req.Code)
	if !looksLikeEmail(email) || !looksLikeCode(code) {
		writeProblem(w, http.StatusBadRequest, "invalid email or code", "/v1/auth/verify-code")
		return
	}

	// C3-032: durable per-email lockout, checked BEFORE any candidate is
	// compared so a locked address burns no code space. Same generic 400
	// as every other failure — a distinguishable "you are locked out"
	// would be an enumeration oracle, and the anti-enumeration contract
	// on this endpoint is absolute.
	if h.loginCodeLocked(r, email) {
		writeProblem(w, http.StatusBadRequest, "invalid or expired code — request a new one", "/v1/auth/verify-code")
		return
	}

	cands, err := h.cfg.Tokens.ConsumableLoginCandidates(r.Context(), email, maxCodeAttempts)
	if err != nil {
		h.cfg.Logger.Error("consumable login candidates", "err", err, "email", maskEmail(email))
		writeProblem(w, http.StatusInternalServerError, "internal error", "/v1/auth/verify-code")
		return
	}

	var matchedHash []byte
	for i := range cands {
		expected := h.cfg.Generator.CodeForHash(cands[i].TokenHash)
		if subtle.ConstantTimeCompare([]byte(expected), []byte(code)) == 1 {
			matchedHash = cands[i].TokenHash
			break
		}
	}
	if matchedHash == nil {
		// Wrong (or no) code. Burn an attempt against the email's
		// in-flight tokens so the small code space can't be ground
		// down, then return the generic error.
		if incErr := h.cfg.Tokens.IncrementLoginCodeAttempts(r.Context(), email); incErr != nil {
			h.cfg.Logger.Warn("increment login code attempts", "err", incErr, "email", maskEmail(email))
		}
		// C3-032: and burn one against the EMAIL, which a re-mint does
		// not reset. This is the counter that actually bounds a patient
		// grinder; the per-token one above only bounds a burst.
		h.registerFailedLoginCode(r, email)
		writeProblem(w, http.StatusBadRequest, "invalid or expired code — request a new one", "/v1/auth/verify-code")
		return
	}

	// Atomically consume the matched token so the code can't be
	// replayed and so it races safely against a magic-link click or a
	// concurrent verify. The expected terminal states (lost race /
	// expired in the gap) map to the same generic 400, not a 500.
	if _, err := h.cfg.Tokens.ConsumeMagicLinkToken(r.Context(), matchedHash); err != nil {
		if errors.Is(err, platform.ErrNotFound) || errors.Is(err, platform.ErrTokenExpired) {
			writeProblem(w, http.StatusBadRequest, "invalid or expired code — request a new one", "/v1/auth/verify-code")
			return
		}
		h.cfg.Logger.Error("consume code token", "err", err, "email", maskEmail(email))
		writeProblem(w, http.StatusInternalServerError, "internal error", "/v1/auth/verify-code")
		return
	}

	if err := h.startSessionForEmail(w, r, email); err != nil {
		h.cfg.Logger.Error("start session (verify-code)", "err", err, "email", maskEmail(email))
		writeProblem(w, http.StatusInternalServerError, "internal error", "/v1/auth/verify-code")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(verifyCodeResponse{Status: "ok"})
}

// loginCodeLocked reports whether the durable per-email failure counter
// currently bars the code path (C3-032).
//
// Fails OPEN on a store error, loudly. Fail-closed was considered and
// rejected: the realistic way this query fails while the rest of the
// handler works is migration 0122 not being applied yet on a node, and
// refusing there would take dashboard code sign-in offline fleet-wide
// for a defence-in-depth control. It is not a bypass an attacker can
// reach either — the candidate lookup two lines down uses the same
// Postgres pool, so a DB an attacker could break takes the whole
// verify path with it, and the per-token maxCodeAttempts cap still
// stands regardless.
func (h *Handlers) loginCodeLocked(r *http.Request, email string) bool {
	state, err := h.cfg.Tokens.LoginCodeLockoutStatus(r.Context(), email)
	if err != nil {
		// The control is now OFF for this request and the HTTP response
		// is indistinguishable from the healthy path, so the counter is
		// the only evidence it happened. A fail-open control without one
		// is the defect class this audit wave keeps finding.
		obs.LoginCodeLockoutErrorsTotal.WithLabelValues(obs.LoginCodeLockoutOpStatusCheck).Inc()
		h.cfg.Logger.Error("login code lockout status unavailable; allowing the attempt",
			"err", err, "email", maskEmail(email))
		return false
	}
	if !state.Locked(h.cfg.Now()) {
		return false
	}
	h.cfg.Logger.Warn("login code verification refused: email is locked out",
		"email", maskEmail(email), "failed_count", state.FailedCount,
		"locked_until", state.LockedUntil, "ip", clientIP(r).String())
	return true
}

// registerFailedLoginCode records one wrong-code attempt against the
// email. Best-effort like [platform.TokenStore.IncrementLoginCodeAttempts]
// — the response is the same generic error either way — but the
// transition INTO a lockout is logged at WARN, because a grinder
// crossing the cap is the signal an operator wants.
func (h *Handlers) registerFailedLoginCode(r *http.Request, email string) {
	state, err := h.cfg.Tokens.RegisterFailedLoginCode(
		r.Context(), email, maxDurableCodeFailures, durableCodeFailureWindow, durableCodeLockout)
	if err != nil {
		// The guess was free: nothing durable recorded it. Same silence
		// as the status-check path, same counter.
		obs.LoginCodeLockoutErrorsTotal.WithLabelValues(obs.LoginCodeLockoutOpRegister).Inc()
		h.cfg.Logger.Warn("register failed login code", "err", err, "email", maskEmail(email))
		return
	}
	if state.Locked(h.cfg.Now()) {
		h.cfg.Logger.Warn("email locked out of code sign-in after repeated failures",
			"email", maskEmail(email), "failed_count", state.FailedCount,
			"locked_until", state.LockedUntil, "ip", clientIP(r).String())
	}
}

// startSessionForEmail finds-or-creates the user for a just-verified
// email, marks it verified + bumps last_login, mints a session, and
// writes the session cookie. Shared by HandleCallback (magic link) and
// HandleVerifyCode (email code): both arrive here once a login token
// has been validated + atomically consumed. On success it writes only
// the Set-Cookie header — the caller owns the response body (303
// redirect vs JSON) so it can shape errors appropriately.
func (h *Handlers) startSessionForEmail(w http.ResponseWriter, r *http.Request, email string) error {
	user, err := h.cfg.Users.GetUserByEmail(r.Context(), email)
	if err != nil {
		if !errors.Is(err, platform.ErrNotFound) {
			return fmt.Errorf("get user by email: %w", err)
		}
		user, err = h.signupNewUser(r.Context(), email)
		if err != nil {
			return fmt.Errorf("signup new user: %w", err)
		}
	}

	// C3-032: a successful sign-in through EITHER door (code or magic
	// link) retires the durable failure counter. Possession of a live
	// token proves the address's owner is present, and without this a
	// legitimate user's typos would accumulate across months until they
	// locked themselves out of the code path for nothing. Best-effort:
	// a failure here must never turn a valid login into an error.
	if err := h.cfg.Tokens.ClearLoginCodeLockout(r.Context(), email); err != nil {
		h.cfg.Logger.Warn("clear login code lockout", "err", err, "email", maskEmail(email))
	}

	// Mark email verified — a consumed login token proves control of
	// the address. Passkey logins don't re-prove it, so the flag is
	// set here, not in mintSession.
	user.EmailVerifiedAt = h.cfg.Now()

	return h.mintSession(w, r, user)
}

// mintSession bumps last_login, creates the DB session row, and
// writes the session cookie for an ALREADY-AUTHENTICATED user. It is
// the single session-issuance path — magic link, email code, and
// passkey login all end here, so every door issues the identical
// credential with identical attributes.
func (h *Handlers) mintSession(w http.ResponseWriter, r *http.Request, user platform.User) error {
	// last_login (+ any caller-staged field like EmailVerifiedAt) is
	// non-fatal on error — the session can still issue; we just won't
	// have a fresh timestamp on the next /me lookup.
	now := h.cfg.Now()
	user.LastLoginAt = now
	if err := h.cfg.Users.UpdateUser(r.Context(), user); err != nil {
		h.cfg.Logger.Warn("update user post-login", "err", err, "user_id", user.ID)
	}

	// The cookie carries a high-entropy random token; the row stores
	// only sha256(token). A leak of the sessions table is therefore not
	// directly replayable — the same hashed-at-rest posture as api_keys
	// and magic_link_tokens (W1-auth-passkey-2). We hold the plaintext
	// just long enough to set the cookie, then drop it.
	token, tokenHash, err := h.cfg.Generator.NewSessionToken()
	if err != nil {
		return fmt.Errorf("mint session token: %w", err)
	}

	sess, err := h.cfg.Users.CreateSession(r.Context(), platform.Session{
		UserID:       user.ID,
		TokenHash:    tokenHash,
		ExpiresAt:    now.Add(h.cfg.SessionTTL),
		IPFirstSeen:  clientIP(r),
		IPLastSeen:   clientIP(r),
		UserAgent:    truncateUA(r.UserAgent()),
		GeoFirstSeen: r.Header.Get("CF-IPCountry"), // safe to leave empty when CF isn't fronting
		GeoLastSeen:  r.Header.Get("CF-IPCountry"),
	})
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}

	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		Domain:   h.cfg.CookieDomain,
		Expires:  sess.ExpiresAt,
		HttpOnly: true,
		Secure:   h.cfg.CookieSecure,
		SameSite: sessionSameSite(),
	})
	return nil
}

// HandleLogout revokes the current session and clears the
// cookie. Idempotent — calling without a session cookie is a
// 200, not a 401.
func (h *Handlers) HandleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(SessionCookieName); err == nil && c.Value != "" {
		// The cookie is the random token, not the PK — hash it, resolve
		// the row, and revoke by internal ID. Best-effort: a missing /
		// already-revoked session is not an error at logout.
		if sess, err := h.cfg.Users.GetSessionByTokenHash(r.Context(), HashSessionToken(c.Value)); err == nil {
			if err := h.cfg.Users.RevokeSession(r.Context(), sess.ID); err != nil {
				h.cfg.Logger.Warn("revoke session at logout", "err", err)
			}
		}
	}
	// Clear the cookie regardless — even an invalid one, so
	// the browser stops sending it.
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		Domain:   h.cfg.CookieDomain,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   h.cfg.CookieSecure,
		SameSite: sessionSameSite(),
	})
	w.WriteHeader(http.StatusOK)
}

// signupNewUser creates the account + first user from a
// just-verified email. Single-org v1: every new email gets
// its own account with the user as owner.
//
// F-1255 (codex audit-2026-05-12): concurrent /v1/auth/callback
// callbacks for the same just-verified email can race — both pass
// the GetUserByEmail check, both create an account, only the
// first user-insert wins on `users_email_idx`. The full fix is
// the optional per-email EmailLocker (Redis SETNX): the loser
// short-circuits before Account.Create, polls briefly for the
// winner's user, and never inserts a speculative-account row.
//
// When no EmailLocker is configured (tests, Redis-less dev) the
// legacy Suspend-on-conflict path stays as defence-in-depth:
// catch ErrConflict on CreateUser, mark the speculative-account
// row Suspended with reason `signup-race:` so the operator
// reaper has an unambiguous signal, then reload the winner.
func (h *Handlers) signupNewUser(ctx context.Context, email string) (platform.User, error) {
	winner, gotWinner, release, err := h.acquireSignupLock(ctx, email)
	if err != nil {
		return platform.User{}, err
	}
	if gotWinner {
		return winner, nil
	}
	if release != nil {
		defer release()
	}

	slug := slugFromEmail(email)
	acct, err := h.cfg.Accounts.Create(ctx, platform.Account{
		Name:         email, // operator can rename later
		Slug:         slug,
		BillingEmail: email,
		Tier:         platform.TierFree,
		Status:       platform.AccountActive,
	})
	if err != nil {
		// Slug collision retry — append a 4-hex suffix and try
		// once. v1 single-org keeps this rare; v2 will use a
		// more robust handle generator.
		if errors.Is(err, platform.ErrConflict) {
			acct, err = h.cfg.Accounts.Create(ctx, platform.Account{
				Name:         email,
				Slug:         slug + "-" + uuid.New().String()[:4],
				BillingEmail: email,
				Tier:         platform.TierFree,
				Status:       platform.AccountActive,
			})
		}
		if err != nil {
			return platform.User{}, fmt.Errorf("create account: %w", err)
		}
	}
	user, err := h.cfg.Users.CreateUser(ctx, platform.User{
		AccountID: acct.ID,
		Email:     email,
		Role:      platform.RoleOwner,
	})
	if err != nil {
		if errors.Is(err, platform.ErrConflict) {
			// Race lost on the email unique index. The OTHER
			// concurrent callback won, its CreateUser
			// succeeded, and its account is the canonical one.
			// Re-fetch and use that user.
			h.cfg.Logger.Warn("signup race: rolling back to winning user",
				"email", maskEmail(email), "speculative_account_id", acct.ID)
			winner, getErr := h.cfg.Users.GetUserByEmail(ctx, email)
			if getErr != nil {
				return platform.User{}, fmt.Errorf("create user conflict + reload: %w", getErr)
			}
			// F-1255 follow-up (codex audit-2026-05-12): mark the
			// speculative-account row as Suspended with a reason
			// the operator reaper can match. Without this the
			// orphan accumulates as a never-recovered Active row;
			// the reaper has to fuzz-match against "accounts with
			// no users" to find them. With this, the reaper just
			// scans for `Suspended` + reason starting with
			// "signup-race:". Best-effort — Suspend errors log
			// and drop because the load-bearing operation (login
			// for `winner`) has already succeeded.
			if suspErr := h.cfg.Accounts.Suspend(ctx, acct.ID, "signup-race: orphan speculative account "+email); suspErr != nil {
				h.cfg.Logger.Warn("signup race: failed to mark orphan account",
					"err", suspErr, "speculative_account_id", acct.ID)
			}
			return winner, nil
		}
		return platform.User{}, fmt.Errorf("create user: %w", err)
	}
	return user, nil
}

// acquireSignupLock attempts to serialise first-login provisioning
// for `email` through the configured EmailLocker. Returns:
//
//	(winner, true, nil, nil)  — lock lost AND the winner's user row
//	                            became visible while we polled.
//	                            Caller returns the winner directly.
//	(_, false, release, nil)  — either no locker is configured, the
//	                            lock acquire failed (treat as "fall
//	                            through to legacy path"), or we won
//	                            the lock. `release` is non-nil only
//	                            when we hold the lock; caller must
//	                            defer it.
//	(_, false, nil, err)      — re-check under the lock surfaced a
//	                            non-NotFound store error.
//
// Extracted from [signupNewUser] to keep that function's cognitive
// complexity under the linter's gocognit cap.
func (h *Handlers) acquireSignupLock(ctx context.Context, email string) (platform.User, bool, func(), error) {
	if h.cfg.EmailLocker == nil {
		return platform.User{}, false, nil, nil
	}
	emailHash := hashEmailForLocker(email)
	acquired, token, lockErr := h.cfg.EmailLocker.Acquire(ctx, emailHash, 30*time.Second)
	if lockErr != nil {
		h.cfg.Logger.Warn("signup email-lock acquire failed; falling through to non-locked path",
			"err", lockErr, "email", maskEmail(email))
		return platform.User{}, false, nil, nil
	}
	if !acquired {
		winner, pollErr := h.waitForWinnerUser(ctx, email)
		if pollErr != nil {
			return platform.User{}, false, nil, fmt.Errorf("signup race: poll for winner: %w", pollErr)
		}
		if winner.ID != uuid.Nil {
			return winner, true, nil, nil
		}
		h.cfg.Logger.Warn("signup email-lock held by another caller but winner did not materialise in poll window; attempting provisioning ourselves",
			"email", maskEmail(email))
		return platform.User{}, false, nil, nil
	}
	release := func() {
		if relErr := h.cfg.EmailLocker.Release(ctx, emailHash, token); relErr != nil {
			h.cfg.Logger.Warn("signup email-lock release failed",
				"err", relErr, "email", maskEmail(email))
		}
	}
	// Inside the lock, a concurrent winner may have already
	// committed before we acquired (the lock TTL elapsed between
	// their Release and our Acquire). Re-check user-by-email
	// before the speculative Account.Create.
	if existing, getErr := h.cfg.Users.GetUserByEmail(ctx, email); getErr == nil {
		release()
		return existing, true, nil, nil
	} else if !errors.Is(getErr, platform.ErrNotFound) {
		release()
		return platform.User{}, false, nil, fmt.Errorf("recheck user under lock: %w", getErr)
	}
	return platform.User{}, false, release, nil
}

// hashEmailForLocker produces a stable hex digest for the per-
// email signup lock. The plaintext email is never the cache
// key so a Redis dump doesn't leak addresses; the digest is
// stable across processes so two API workers serialise on the
// same key.
func hashEmailForLocker(email string) string {
	h := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	return hex.EncodeToString(h[:])
}

// waitForWinnerUser polls Users.GetUserByEmail until the
// winner's row materialises, the context expires, or the local
// budget (1500ms across ~10 attempts) is exhausted. Returns the
// zero-value [platform.User] (with `ID == uuid.Nil`) if we time
// out without seeing a winner — the caller falls through and
// tries to provision themselves.
//
// Tight budget on purpose: the user is waiting on the callback
// redirect; a long poll would surface as a hung browser tab.
// 1.5s is generous enough to bridge a slow first INSERT but
// short enough to fail open if the lock-holder crashed.
func (h *Handlers) waitForWinnerUser(ctx context.Context, email string) (platform.User, error) {
	const (
		maxAttempts = 10
		gap         = 150 * time.Millisecond
	)
	for i := 0; i < maxAttempts; i++ {
		if ctx.Err() != nil {
			return platform.User{}, ctx.Err()
		}
		u, err := h.cfg.Users.GetUserByEmail(ctx, email)
		if err == nil {
			return u, nil
		}
		if !errors.Is(err, platform.ErrNotFound) {
			return platform.User{}, err
		}
		select {
		case <-ctx.Done():
			return platform.User{}, ctx.Err()
		case <-time.After(gap):
		}
	}
	return platform.User{}, nil
}

// ─── Helpers ─────────────────────────────────────────────────────

func looksLikeEmail(s string) bool {
	if len(s) < 3 || len(s) > 254 {
		return false
	}
	at := strings.IndexByte(s, '@')
	if at <= 0 || at >= len(s)-1 {
		return false
	}
	if strings.IndexByte(s[at+1:], '.') < 0 {
		return false
	}
	return true
}

// looksLikeCode reports whether s is a well-formed 6-digit numeric
// code. Cheap pre-filter so a malformed body never reaches the token
// store; the real check is the constant-time match in HandleVerifyCode.
func looksLikeCode(s string) bool {
	if len(s) != 6 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func clientIP(r *http.Request) net.IP {
	// F-1224 (codex audit-2026-05-12): use the trusted-proxy-
	// resolved IP from `middleware.RemoteIP` rather than parsing
	// r.RemoteAddr directly. Behind Caddy / Cloudflare, the
	// socket peer is the local proxy; the real client IP is in
	// X-Forwarded-For, and the middleware decides whether to
	// honour it based on the `trusted_proxy_cidrs` config.
	//
	// Falls back to the socket peer when middleware.RemoteIP
	// returns empty (well-formed requests always carry one;
	// the empty case is left to upstream policy).
	if resolved := middleware.RemoteIP(r); resolved != "" {
		if ip := net.ParseIP(resolved); ip != nil {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return nil
	}
	return net.ParseIP(host)
}

func truncateUA(ua string) string {
	const maxUALen = 256
	// CS-071: the UA is rendered verbatim into the plaintext magic-link
	// email body, so a client-supplied CR/LF (or other control char) could
	// inject arbitrary lines ("URGENT: account compromised — call …") into a
	// trusted, DKIM-signed, branded email. Strip control characters before
	// it reaches the template. (The HTML variant is html/template-escaped;
	// this closes the plaintext gap.)
	ua = strings.Map(func(rr rune) rune {
		if rr == '\t' {
			return ' '
		}
		if rr < 0x20 || rr == 0x7f {
			return -1 // drop CR, LF, and other control chars
		}
		return rr
	}, ua)
	if len(ua) <= maxUALen {
		return ua
	}
	return ua[:maxUALen]
}

// slugFromEmail derives an account slug from the local part of
// an email. Lowercase, hyphenated, ASCII-only.
func slugFromEmail(email string) string {
	at := strings.IndexByte(email, '@')
	if at <= 0 {
		return "user"
	}
	local := email[:at]
	var b strings.Builder
	b.Grow(len(local))
	for _, r := range local {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		case r == '-' || r == '_' || r == '.':
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "user"
	}
	return out
}

// writeProblem emits a problem+json error body matching the
// rest of the v1 surface, via the shared httpx helper. Kept as a
// local one-liner only to pin this surface's type URL —
// dashboardauth must not depend on internal/api/v1's enveloped
// writeProblem.
func writeProblem(w http.ResponseWriter, status int, detail, instance string) {
	httpx.WriteProblem(w, "https://api.stellarindex.io/errors/auth", status, detail, instance)
}

// maskEmail redacts a customer email for logging (audit PRV1): keep the first
// local-part character + the full domain, hide the rest —
// "alice@example.com" -> "a***@example.com". Enough to correlate a log line to
// a domain / support ticket without persisting the full PII in application logs.
func maskEmail(email string) string { return pii.MaskEmail(email) }
