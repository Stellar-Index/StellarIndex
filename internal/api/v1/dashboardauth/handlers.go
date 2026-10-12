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
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Stellar-Index/StellarIndex/internal/pii"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/httpx"
	"github.com/Stellar-Index/StellarIndex/internal/notify"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
)

// EmailLocker serialises first-login provisioning per email so concurrent callbacks cannot both create an Account; Release deletes only if the lock still carries the caller's token.
type EmailLocker interface {
	Acquire(ctx context.Context, emailHash string, ttl time.Duration) (bool, string, error)
	Release(ctx context.Context, emailHash, token string) error
}

// LoginThrottle bounds magic-link sends against inbox bombing, which the global anonymous rate-limit is too coarse to stop; nil disables the check.
type LoginThrottle interface {
	// Allow reports whether a magic-link send for (ip, email) is within quota. On false the handler
	// MUST skip the email yet still return the generic 200 {status:"sent"}, so no one learns a
	// throttle fired. An error means the store is unavailable and the handler falls OPEN
	// (availability over a brief abuse window). email is lowercased; implementations hash it.
	Allow(ctx context.Context, ip, email string) (bool, error)
}

// Config wires the auth handlers' dependencies; built once at boot.
type Config struct {
	Accounts  platform.AccountStore
	Users     platform.UserStore
	Tokens    platform.TokenStore
	Sender    notify.Sender
	Generator *Generator
	Logger    *slog.Logger
	Now       func() time.Time
	// EmailLocker (optional) serialises first-login provisioning per email; nil falls through to
	// the Suspend-on-conflict recovery path.
	EmailLocker EmailLocker
	// LoginThrottle (optional) caps magic-link sends per IP and per target email; nil leaves only
	// the global anon rate-limit.
	LoginThrottle LoginThrottle
	// Audit (optional) is the durable audit_log sink for staff look-ups, passkey changes, and
	// passkey sign-ins refused on a clone warning or ceremony replay. nil means log-only.
	Audit platform.AuditStore
	// Passkeys (optional) backs passkey sign-in; nil leaves /v1/auth/passkey/* unmounted while
	// email-code sign-in still works.
	Passkeys platform.WebAuthnCredentialStore
	// PasskeyCeremonyGuard (optional) makes each WebAuthn ceremony challenge single-use, so a
	// captured finish request cannot be replayed into a second session. Production wires Redis
	// SETNX (shared across instances); nil defaults to an in-process guard in validate().
	PasskeyCeremonyGuard PasskeyCeremonyGuard
	// DashboardBaseURL is the absolute URL of the explorer hosting the dashboard. Emailed links
	// are {DashboardBaseURL}/auth/callback?token=<plaintext>; post-login redirects land on
	// {DashboardBaseURL}{next}, with next defaulting to "/".
	DashboardBaseURL string
	// EmailFrom is the From: address, e.g. `Stellar Index <hello@stellarindex.io>`.
	EmailFrom string
	// MagicLinkTTL is link validity. Default 15 minutes.
	MagicLinkTTL time.Duration
	// SessionTTL is the fixed session lifetime from sign-in (default 30 days): TouchSession records
	// activity but never extends expires_at.
	SessionTTL time.Duration
	// SessionIdleTimeout revokes a session unused this long, before SessionTTL. Default
	// [defaultSessionIdleTimeout].
	SessionIdleTimeout time.Duration
	// CookieSecure sets Secure on the session-presence hint; credential cookies are always Secure
	// (see [credentialCookie]).
	CookieSecure bool
	// SessionHintDomain is the Domain of the session-presence hint only, so a sibling-host explorer
	// can read it; empty means host-only. Credential cookies never carry a Domain.
	SessionHintDomain string
	// AccountEraser backs DELETE /v1/dashboard/account and AccountExporter GET
	// /v1/dashboard/account/export; each is optional and nil leaves its route unmounted.
	AccountEraser   AccountEraser
	AccountExporter AccountExporter

	// accountActions caps erasure and export attempts per user; accountIdempotency replays a
	// retried erasure. Installed by validate().
	accountActions     *ratelimit.LocalFixedWindowCounter
	accountIdempotency *middleware.IdempotencyStore

	// The three limiters below are installed by validate() and never configurable off.
	// passkeyBeginLimiter caps anonymous begin-login ceremonies per client IP.
	passkeyBeginLimiter *ratelimit.LocalFixedWindowCounter
	// passkeySignupLimiter caps anonymous account-creating ceremonies per client IP.
	passkeySignupLimiter *ratelimit.LocalFixedWindowCounter
	// signedInBrowserSends caps the sends [Handlers.admitSignedInBrowser] lets past a full
	// LoginThrottle: one per address per link lifetime per instance.
	signedInBrowserSends *ratelimit.LocalFixedWindowCounter
}

// ensureCodeSecret installs the Generator and guarantees it is keyed.
func (c *Config) ensureCodeSecret() error {
	if c.Generator == nil {
		c.Generator = NewGenerator()
	}
	if len(c.Generator.Secret) == 0 {
		// A ceremony must finish on another instance or after a restart; a per-process key would fail
		// with a 400 indistinguishable from tampering, so passkeys require the secret.
		if c.Passkeys != nil {
			return errors.New("dashboardauth: passkeys are wired but the server secret is empty — set the api.dashboard.code_secret_env variable")
		}
		// The code derivation must NEVER run unkeyed (see [Generator.CodeForHash]). Fall back to
		// a random per-process secret: unforgeable, but in-flight codes, links and login-device
		// markers stop verifying on another instance or after a restart.
		secret := make([]byte, 32)
		if _, err := c.Generator.Read(secret); err != nil {
			return fmt.Errorf("dashboardauth: generate code secret: %w", err)
		}
		c.Generator.Secret = secret
	}
	for _, label := range []string{loginCodeDomain, passkeyCeremonyDomain, loginIntentDomain, loginDeviceDomain} {
		if _, err := purposeKey(c.Generator.Secret, label); err != nil {
			return fmt.Errorf("dashboardauth: derive %s key: %w", label, err)
		}
	}
	return nil
}

// validate fills in defaults and rejects unworkable configs.
func (c *Config) validate() error {
	if c.Accounts == nil || c.Users == nil || c.Tokens == nil {
		return errors.New("dashboardauth: stores are required")
	}
	if c.Sender == nil {
		return errors.New("dashboardauth: sender is required")
	}
	if err := c.ensureCodeSecret(); err != nil {
		return err
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Now == nil {
		c.Now = func() time.Time { return time.Now().UTC() }
	}
	// Replay protection is never optional, only its scope: Redis-less deployments get a
	// single-instance spent-set. Must follow the Now default, since the guard expires records
	// on that clock.
	if c.PasskeyCeremonyGuard == nil {
		c.PasskeyCeremonyGuard = newInProcessPasskeyCeremonyGuard(c.Now)
	}
	if c.passkeyBeginLimiter == nil {
		c.passkeyBeginLimiter = ratelimit.NewLocalFixedWindowCounter(passkeyBeginLoginWindow, c.Now)
	}
	if c.passkeySignupLimiter == nil {
		c.passkeySignupLimiter = ratelimit.NewLocalFixedWindowCounter(passkeySignupWindow, c.Now)
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
	if c.signedInBrowserSends == nil {
		c.signedInBrowserSends = ratelimit.NewLocalFixedWindowCounter(max(c.MagicLinkTTL, time.Second), c.Now)
	}
	if c.SessionTTL == 0 {
		c.SessionTTL = 30 * 24 * time.Hour
	}
	if c.SessionIdleTimeout == 0 {
		c.SessionIdleTimeout = defaultSessionIdleTimeout
	}
	if c.accountActions == nil {
		c.accountActions = ratelimit.NewLocalFixedWindowCounter(accountActionWindow, c.Now)
	}
	if c.accountIdempotency == nil {
		c.accountIdempotency = middleware.NewIdempotencyStore(0)
	}
	return nil
}

// Handlers exposes the auth flow to be mounted in the v1 mux.
type Handlers struct{ cfg *Config }

// NewHandlers validates cfg IN PLACE and returns Handlers backed by that pointer, because validate() installs defaults that [Middleware] and [RequireSession] must also see.
func NewHandlers(cfg *Config) (*Handlers, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Handlers{cfg: cfg}, nil
}

// Mount installs the /v1/auth routes; the POSTs sit behind [middleware.RequireSameSiteWrite] against login-CSRF, and the GET callback is bound by [LoginIntentCookieName] instead.
func (h *Handlers) Mount(mux *http.ServeMux, public *middleware.PublicRoutes) {
	sameSite := middleware.RequireSameSiteWrite(h.cfg.Logger)
	public.Handle(mux, "POST /v1/auth/login", sameSite(http.HandlerFunc(h.HandleLogin)))
	public.Handle(mux, "GET /v1/auth/callback", http.HandlerFunc(h.HandleCallback))
	public.Handle(mux, "POST /v1/auth/verify-code", sameSite(http.HandlerFunc(h.HandleVerifyCode)))
	public.Handle(mux, "POST /v1/auth/logout", sameSite(http.HandlerFunc(h.HandleLogout)))
	// Staff customer look-up: session- and staff-gated. POST although read-only, because the email
	// term must not travel in a URL (see [adminLookupRequest]); the same-site gate stops a
	// cross-site page driving a staff session's PII reads.
	mux.Handle("POST /v1/account/admin/lookup",
		RequireSession()(sameSite(http.HandlerFunc(h.HandleAdminLookup))))

	// Passkey routes, only when a credential store is wired. Registration and management are
	// session-gated; login and signup are unauthenticated. Every state-changing route keeps the
	// same-site gate: finish-login mints a session, a login-CSRF primitive like verify-code.
	if h.cfg.Passkeys != nil {
		requireSession := RequireSession()
		public.Handle(mux, "POST /v1/auth/passkey/begin-login",
			sameSite(http.HandlerFunc(h.HandlePasskeyBeginLogin)))
		public.Handle(mux, "POST /v1/auth/passkey/finish-login",
			sameSite(http.HandlerFunc(h.HandlePasskeyFinishLogin)))
		public.Handle(mux, "POST /v1/auth/passkey/begin-signup",
			sameSite(http.HandlerFunc(h.HandlePasskeyBeginSignup)))
		public.Handle(mux, "POST /v1/auth/passkey/finish-signup",
			sameSite(http.HandlerFunc(h.HandlePasskeyFinishSignup)))
		mux.Handle("POST /v1/auth/passkey/begin-register",
			requireSession(sameSite(http.HandlerFunc(h.HandlePasskeyBeginRegister))))
		mux.Handle("POST /v1/auth/passkey/finish-register",
			requireSession(sameSite(http.HandlerFunc(h.HandlePasskeyFinishRegister))))
		mux.Handle("GET /v1/auth/passkey/credentials",
			requireSession(http.HandlerFunc(h.HandlePasskeyList)))
		mux.Handle("DELETE /v1/auth/passkey/credentials/{id}",
			requireSession(sameSite(http.HandlerFunc(h.HandlePasskeyDelete))))
	}

	// Erasure and export are session-only: an API key must not destroy or download its own account.
	if h.cfg.AccountEraser != nil {
		idem := middleware.Idempotency(h.cfg.accountIdempotency, SessionAccountSubject)
		mux.Handle("DELETE /v1/dashboard/account",
			RequireSession()(sameSite(idem(http.HandlerFunc(h.HandleAccountDelete)))))
	}
	if h.cfg.AccountExporter != nil {
		mux.Handle("GET /v1/dashboard/account/export", RequireSession()(http.HandlerFunc(h.HandleAccountExport)))
	}
}

// maxCodeAttempts caps wrong 6-digit guesses against one login token; the magic link still
// works, only code matching stops. 5 keeps brute force over the ~1e6 space negligible.
const maxCodeAttempts = 5

// Durable per-email code-failure policy. maxCodeAttempts alone is per mint: each new login hands
// the guesser a fresh 5, and the send throttle bounding re-mints is a volatile fixed window (~25
// guesses/hour/email, ~0.22 chance of hitting a code over a year). This cuts it to ~10 guesses/day
// (~3.7e-3/year), but only because each guess is compared against ONE code (the newest mint's);
// matching every live mint would multiply it by the send throttle's 5. The lockout gates the code
// path only, so the magic link keeps working. Deliberately not tunable per deployment.
const (
	// maxDurableCodeFailures is the per-email failure budget inside durableCodeFailureWindow.
	maxDurableCodeFailures = 10
	// durableCodeFailureWindow is how long failures accumulate; an elapsed window starts fresh.
	durableCodeFailureWindow = 24 * time.Hour
	// durableCodeLockout matches the window so the sustained budget is ~maxDurableCodeFailures per
	// day, not per window boundary.
	durableCodeLockout = 24 * time.Hour
)

// loginRequest is the JSON body POST /v1/auth/login accepts.
type loginRequest struct {
	Email string `json:"email"`
}

type loginResponse struct {
	Status string `json:"status"`
}

// refuseLoginWithoutMail answers 503 and reports true when the mail transport holds no provider
// credential ([notify.IsUnconfigured]), instead of a 200 "sent" for mail that never left.
//
// It runs before the throttle and any side effect, so no link row or login-intent cookie is minted
// for undeliverable mail, and every well-formed request gets the same 503, so the refusal is
// neither an enumeration nor a throttle oracle. It counts as a failed send for the notify
// failure-ratio alert.
func (h *Handlers) refuseLoginWithoutMail(w http.ResponseWriter) bool {
	if !notify.IsUnconfigured(h.cfg.Sender) {
		return false
	}
	obs.NotifySendsTotal.WithLabelValues(obs.NotifyTemplateMagicLink, obs.NotifySendResultFailed).Inc()
	h.cfg.Logger.Error("magic-link login refused: mail transport is not configured",
		"err", notify.ErrNotConfigured)
	writeProblem(w, http.StatusServiceUnavailable,
		"email sign-in is not available on this deployment", "/v1/auth/login")
	return true
}

// HandleLogin issues a magic-link email and returns 200 {status:"sent"} whether or not the email matches a user, so addresses cannot be enumerated (503 only when mail is unconfigured).
func (h *Handlers) HandleLogin(w http.ResponseWriter, r *http.Request) {
	// MaxBytesReader, not LimitReader: LimitReader returns (n, nil) at its cap, so an oversize body
	// would be silently truncated and surface as "malformed JSON".
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
	email, err := notify.CanonicalRecipient(req.Email)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid email", "/v1/auth/login")
		return
	}

	if h.refuseLoginWithoutMail(w) {
		return
	}

	if h.loginThrottled(w, r, email) {
		return
	}

	// Bind the link to THIS browser (login CSRF): the token carries a MAC of this browser's
	// login-intent id. See [LoginIntentCookieName].
	browser := h.setLoginIntentCookie(w, r)
	plaintext, hash, code, err := h.cfg.Generator.newBoundToken(browser)
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
		UserAgent:        notify.ClientFromUserAgent(r.UserAgent()),
	})
	if err != nil {
		h.cfg.Logger.Error("render magic link template", "err", err)
		writeProblem(w, http.StatusInternalServerError, "internal error", "/v1/auth/login")
		return
	}
	// The token is already written; a client disconnect must not cancel the send, or it would
	// look like a provider failure.
	sendCtx, cancelSend := context.WithTimeout(context.WithoutCancel(r.Context()), sendTimeout)
	defer cancelSend()
	if err := h.cfg.Sender.Send(sendCtx, msg); errors.Is(err, notify.ErrSuppressed) {
		obs.NotifySendsTotal.WithLabelValues(obs.NotifyTemplateMagicLink, obs.NotifySendResultSuppressed).Inc()
	} else if err != nil {
		// The failure is swallowed below (200 either way), so this counter is the only signal of
		// a mail outage.
		obs.NotifySendsTotal.WithLabelValues(obs.NotifyTemplateMagicLink, obs.NotifySendResultFailed).Inc()
		// Return 200 anyway: a visible send failure would let an attacker confirm an address
		// exists. The operator alert is the notify failure-ratio Prometheus rule on this counter.
		h.cfg.Logger.Error("send magic link email", "err", err, "email", maskEmail(email))
	} else {
		obs.NotifySendsTotal.WithLabelValues(obs.NotifyTemplateMagicLink, obs.NotifySendResultSent).Inc()
	}

	_ = json.NewEncoder(w).Encode(loginResponse{Status: "sent"})
}

// loginThrottled applies the magic-link throttle (per IP and per target email). Over quota it skips
// the send, writes the same generic 200 as the send path and reports true, so no one learns a
// throttle fired. A throttle error falls open; auth.RedisLoginThrottle only errors after its
// in-process fallback admitted the send under the same caps.
func (h *Handlers) loginThrottled(w http.ResponseWriter, r *http.Request, email string) bool {
	if h.cfg.LoginThrottle == nil {
		return false
	}
	ok, terr := h.cfg.LoginThrottle.Allow(r.Context(), clientIP(r).String(), email)
	switch {
	case terr != nil:
		h.cfg.Logger.Warn("login throttle unavailable; falling open", "err", terr)
		return false
	case ok:
		return false
	case h.admitSignedInBrowser(r, email):
		h.cfg.Logger.Info("magic-link throttle full; sending to a browser previously signed in to this address",
			"ip", clientIP(r).String())
		return false
	}
	h.cfg.Logger.Warn("magic-link login throttled", "ip", clientIP(r).String())
	// Stamp the login-intent cookie exactly as a real send does; its absence would be a
	// byte-visible oracle for "a throttle fired". The cookie carries no per-link state.
	h.setLoginIntentCookie(w, r)
	_ = json.NewEncoder(w).Encode(loginResponse{Status: "sent"})
	return true
}

// admitSignedInBrowser lets a send past a full LoginThrottle when the request carries this
// address's [LoginDeviceCookieName] proof; the per-address cap is shared by everyone who knows the
// address, so the owner could otherwise be crowded out. The bypass is one send per address per
// link lifetime per instance, so no inbox flood. The proof is checked first so a caller without
// one never adds a key.
func (h *Handlers) admitSignedInBrowser(r *http.Request, email string) bool {
	return h.hasLoginDeviceProof(r, email) &&
		h.cfg.signedInBrowserSends.Allow(hashEmailForLocker(email), 1)
}

// sessionSameSite picks the session-cookie SameSite policy. The explorer and the API share a
// registrable domain (cross-origin but same-site), so Lax is sent on its credentialed requests
// while genuine cross-site ones are blocked. Not None, which would let any site auto-submit a
// credentialed POST to the cookie-authed /v1/dashboard/* mutations (CSRF); if the dashboard moves
// to another site, add a CSRF token instead.
//
// Lax leaves a gap: a sibling origin is same-site and Lax permits a top-level cross-site GET. So
// state-changing routes are also gated by [middleware.RequireSameSiteWrite] and the magic-link
// callback by [LoginIntentCookieName]; SameSite is defence in depth only.
func sessionSameSite() http.SameSite {
	return http.SameSiteLaxMode
}

// credentialCookie is the only constructor for cookies that carry or bind a credential. It takes
// no Domain on purpose: such cookies are __Host- named, which browsers store only when Secure,
// Path=/ and host-only, so no sibling host can read, overwrite or plant them.
func credentialCookie(name, value string) *http.Cookie {
	return &http.Cookie{ //nolint:gosec // G124: Secure and HttpOnly are literal true; sessionSameSite() is Lax
		Name:     name,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: sessionSameSite(),
	}
}

// HandleCallback consumes a magic-link token, finds-or-creates the user and issues a session cookie; the [LoginIntentCookieName] binding is checked before the token is consumed.
func (h *Handlers) HandleCallback(w http.ResponseWriter, r *http.Request) {
	// The URL carries the magic-link plaintext, so set this here rather than rely on outer middleware.
	w.Header().Set("Referrer-Policy", "no-referrer")
	plaintext := r.URL.Query().Get("token")
	if plaintext == "" {
		writeProblem(w, http.StatusBadRequest, "missing token", "/v1/auth/callback")
		return
	}
	tokenHash := HashMagicLinkPlaintext(plaintext)
	if !h.hasLoginIntent(r, plaintext) {
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
		// A token minted for another purpose cannot log in; same error shape as not-found so it
		// does not leak that such a token exists.
		writeProblem(w, http.StatusBadRequest, "invalid token", "/v1/auth/callback")
		return
	}

	if err := h.startSessionForEmail(w, r, tok.Email); err != nil {
		h.cfg.Logger.Error("start session (callback)", "err", err, "email", maskEmail(tok.Email))
		writeProblem(w, http.StatusInternalServerError, "internal error", "/v1/auth/callback")
		return
	}
	h.clearLoginIntentCookie(w)

	// `next` is path-only: absolute URLs are never honoured (open redirect); empty means "/".
	next := r.URL.Query().Get("next")
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		next = "/"
	}
	dest := strings.TrimRight(h.cfg.DashboardBaseURL, "/") + next
	http.Redirect(w, r, dest, http.StatusSeeOther) //nolint:gosec // G710: next is a single-slash path appended to the configured DashboardBaseURL, so the host is fixed
}

// verifyCodeRequest is the JSON body POST /v1/auth/verify-code accepts.
type verifyCodeRequest struct {
	Email string `json:"email"`
	Code  string `json:"code"`
}

type verifyCodeResponse struct {
	Status string `json:"status"`
}

// HandleVerifyCode consumes the 6-digit email code against only the newest in-flight token, charging each attempt (per token and per email) BEFORE comparing; all failures return one generic error.
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
	email, emailErr := notify.CanonicalRecipient(req.Email)
	code := strings.TrimSpace(req.Code)
	if emailErr != nil || !looksLikeCode(code) {
		writeProblem(w, http.StatusBadRequest, "invalid email or code", "/v1/auth/verify-code")
		return
	}

	// A browser that has signed in to this address before is charged against its own budget:
	// the email's budget is spendable by anyone who knows the address, so sharing it would let a
	// stranger's guesses lock the owner out. The proof holder stays bounded too.
	lockKey := email
	if h.hasLoginDeviceProof(r, email) {
		lockKey = platform.LoginCodeDeviceKey(email)
	}

	// Durable lockout, checked BEFORE any candidate is compared so a locked address burns no
	// code space. Same generic 400 as every failure: a distinct "locked out" would be an
	// enumeration oracle.
	if h.loginCodeLocked(r, lockKey) {
		writeProblem(w, http.StatusBadRequest, "invalid or expired code — request a new one", "/v1/auth/verify-code")
		return
	}

	// With nothing to compare against the answer is already "no"; charging would spend the owner's
	// budget on a request that could not match.
	if !h.loginCodeLive(r, email) {
		writeProblem(w, http.StatusBadRequest, "invalid or expired code — request a new one", "/v1/auth/verify-code")
		return
	}

	// The lockout check is a cheap early refusal that a concurrent burst can all pass; the charge
	// is what bounds the burst, so it must precede the comparison.
	if !h.chargeLoginCodeAttempt(r, lockKey) {
		writeProblem(w, http.StatusBadRequest, "invalid or expired code — request a new one", "/v1/auth/verify-code")
		return
	}

	cand, err := h.cfg.Tokens.ReserveLoginCode(r.Context(), email, maxCodeAttempts)
	if err != nil && !errors.Is(err, platform.ErrNotFound) {
		h.cfg.Logger.Error("reserve login code", "err", err, "email", maskEmail(email))
		writeProblem(w, http.StatusInternalServerError, "internal error", "/v1/auth/verify-code")
		return
	}
	if err != nil || subtle.ConstantTimeCompare([]byte(h.cfg.Generator.CodeForHash(cand.TokenHash)), []byte(code)) != 1 {
		// Wrong (or no) code. Both counters were already charged above.
		writeProblem(w, http.StatusBadRequest, "invalid or expired code — request a new one", "/v1/auth/verify-code")
		return
	}
	matchedHash := cand.TokenHash

	// Consume atomically so the code cannot be replayed and races safely against a magic-link click
	// or concurrent verify; a lost race or expiry in the gap maps to the generic 400, not a 500.
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

// loginCodeLocked reports whether the durable per-email failure counter currently bars the code
// path. It fails OPEN on a store error, loudly: failing closed would take code sign-in offline for
// a defence-in-depth control (e.g. a node missing the lockout migration). That is no attacker
// bypass, since the candidate lookup uses the same Postgres pool and maxCodeAttempts still applies.
func (h *Handlers) loginCodeLocked(r *http.Request, email string) bool {
	state, err := h.cfg.Tokens.LoginCodeLockoutStatus(r.Context(), email)
	if err != nil {
		// The control is OFF for this request and the response looks healthy, so the counter
		// is the only evidence.
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

// clearLoginCodeLockouts retires both durable code budgets for email (shared and signed-in
// browsers'). Best-effort: a failure must never turn a valid login into an error.
func (h *Handlers) clearLoginCodeLockouts(r *http.Request, email string) {
	for _, key := range []string{email, platform.LoginCodeDeviceKey(email)} {
		if err := h.cfg.Tokens.ClearLoginCodeLockout(r.Context(), key); err != nil {
			h.cfg.Logger.Warn("clear login code lockout", "err", err, "email", maskEmail(email))
		}
	}
}

// loginCodeLive reports whether a code candidate exists for email. A store error answers true, so
// the attempt is charged rather than waved through.
func (h *Handlers) loginCodeLive(r *http.Request, email string) bool {
	live, err := h.cfg.Tokens.HasLiveLoginCode(r.Context(), email, maxCodeAttempts)
	if err != nil {
		h.cfg.Logger.Warn("login code liveness check failed; charging the attempt",
			"err", err, "email", maskEmail(email))
		return true
	}
	return live
}

// chargeLoginCodeAttempt records this attempt against the email's durable failure budget BEFORE any
// code is compared, and reports whether it is within budget. It is charged as a failure up front; a
// successful sign-in clears it ([Handlers.startSessionForEmail]). The store's upsert serialises on
// the email's row, so concurrent attempts get distinct counts and at most [maxDurableCodeFailures]
// per window are admitted, including the one reaching the cap (hence <=). Fails OPEN on a store
// error, as [Handlers.loginCodeLocked] does.
func (h *Handlers) chargeLoginCodeAttempt(r *http.Request, email string) bool {
	state, err := h.cfg.Tokens.RegisterFailedLoginCode(
		r.Context(), email, maxDurableCodeFailures, durableCodeFailureWindow, durableCodeLockout)
	if err != nil {
		// Fails open: nothing durable recorded the attempt; same counter as the status-check path.
		obs.LoginCodeLockoutErrorsTotal.WithLabelValues(obs.LoginCodeLockoutOpRegister).Inc()
		h.cfg.Logger.Warn("register failed login code", "err", err, "email", maskEmail(email))
		return true
	}
	if state.Locked(h.cfg.Now()) {
		h.cfg.Logger.Warn("email locked out of code sign-in after repeated failures",
			"email", maskEmail(email), "failed_count", state.FailedCount,
			"locked_until", state.LockedUntil, "ip", clientIP(r).String())
	}
	return state.FailedCount <= maxDurableCodeFailures
}

// startSessionForEmail finds-or-creates the user for a just-verified email, marks it verified,
// mints a session and writes the cookie. HandleCallback and HandleVerifyCode both arrive here
// once a login token is consumed. It writes only Set-Cookie; the caller owns the response body.
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

	// A successful sign-in through either door retires the durable failure counter: a live token
	// proves the owner is present, and otherwise typos would accumulate until they locked the
	// owner out. Best-effort.
	h.clearLoginCodeLockouts(r, email)

	// A consumed login token proves control of the address. Passkey logins do not, so the flag is
	// set here, not in mintSession.
	user.EmailVerifiedAt = h.cfg.Now()

	return h.mintSession(w, r, user)
}

// maxLiveSessionsPerUser is how many signed-in devices a user keeps;
// minting one more revokes the oldest.
const maxLiveSessionsPerUser = 10

// mintSession bumps last_login, creates the session row and writes the cookie for an
// ALREADY-AUTHENTICATED user. It is the single issuance path (link, code and passkey), so every
// door issues an identical credential.
func (h *Handlers) mintSession(w http.ResponseWriter, r *http.Request, user platform.User) error {
	// A failed last_login update (or caller-staged field like EmailVerifiedAt) is non-fatal: the
	// session still issues.
	now := h.cfg.Now()
	user.LastLoginAt = now
	if err := h.cfg.Users.UpdateUser(r.Context(), user); err != nil {
		h.cfg.Logger.Warn("update user post-login", "err", err, "user_id", user.ID)
	}

	// The row stores only sha256(token), so a leaked sessions table is not replayable (same
	// hashed-at-rest posture as api_keys and magic_link_tokens).
	token, tokenHash, err := h.cfg.Generator.NewSessionToken()
	if err != nil {
		return fmt.Errorf("mint session token: %w", err)
	}
	h.revokePresentedSession(r)

	// CF-IPCountry is a client-reachable header like the UA, so sanitise it before it reaches
	// Postgres. Empty is expected when Cloudflare is not fronting the request.
	const maxGeoLen = 8
	geo := safeText(r.Header.Get("CF-IPCountry"), maxGeoLen)

	sess, err := h.cfg.Users.CreateSession(r.Context(), platform.Session{
		UserID:       user.ID,
		TokenHash:    tokenHash,
		ExpiresAt:    now.Add(h.cfg.SessionTTL),
		IPFirstSeen:  clientIP(r),
		IPLastSeen:   clientIP(r),
		UserAgent:    truncateUA(r.UserAgent()),
		GeoFirstSeen: geo,
		GeoLastSeen:  geo,
	})
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	// Bounds live sessions per user however fast a door mints; best-effort.
	if err := h.cfg.Users.CapUserSessions(r.Context(), user.ID, sess.ID, maxLiveSessionsPerUser); err != nil {
		h.cfg.Logger.Warn("cap live sessions at login", "err", err, "user_id", user.ID)
	}

	sc := credentialCookie(SessionCookieName, token) //nolint:gosec // G124: credentialCookie sets Secure, HttpOnly and SameSite=Lax
	sc.Expires = sess.ExpiresAt
	http.SetCookie(w, sc)
	// The JS-readable shadow of the cookie above, written in the same response so the two never
	// disagree. See [SessionHintCookieName].
	h.setSessionHintCookie(w, sess.ExpiresAt)
	if email, err := notify.CanonicalRecipient(user.Email); err == nil && !platform.IsPlaceholderEmail(email) {
		h.setLoginDeviceCookie(w, email)
	}
	return nil
}

// revokePresentedSession revokes the session the browser is replacing, so a stolen copy of the old
// cookie dies on re-login. Best-effort: a failed revoke must not block login.
func (h *Handlers) revokePresentedSession(r *http.Request) {
	c, err := r.Cookie(SessionCookieName)
	if err != nil || c.Value == "" {
		return
	}
	sess, err := h.cfg.Users.GetSessionByTokenHash(r.Context(), HashSessionToken(c.Value))
	if err != nil {
		return
	}
	if err := h.cfg.Users.RevokeSession(r.Context(), sess.ID); err != nil {
		h.cfg.Logger.Warn("revoke replaced session at login", "err", err, "session_id", sess.ID)
	}
}

// HandleLogout revokes the current session and clears the cookie (?all=true revokes every session of the user); idempotent, no cookie is a 200.
func (h *Handlers) HandleLogout(w http.ResponseWriter, r *http.Request) {
	all := false
	if v := r.URL.Query().Get("all"); v != "" {
		parsed, err := strconv.ParseBool(v)
		if err != nil {
			// Refuse rather than log out narrower than asked.
			writeProblem(w, http.StatusBadRequest, "all must be true or false", r.URL.Path)
			return
		}
		all = parsed
	}
	if c, err := r.Cookie(SessionCookieName); err == nil && c.Value != "" {
		// The cookie is the random token, not the PK: hash it, resolve the row, revoke by ID.
		// A missing or already-revoked session is not an error at logout.
		sess, err := h.cfg.Users.GetSessionByTokenHash(r.Context(), HashSessionToken(c.Value))
		switch {
		case err == nil && all:
			// Not best-effort: a 200 tells the user every other session is dead.
			if err := h.cfg.Users.RevokeAllUserSessions(r.Context(), sess.UserID); err != nil {
				h.cfg.Logger.Error("revoke all sessions at logout", "err", err, "user_id", sess.UserID)
				writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
				return
			}
		case err == nil:
			if err := h.cfg.Users.RevokeSession(r.Context(), sess.ID); err != nil {
				h.cfg.Logger.Warn("revoke session at logout", "err", err)
			}
		case all && !errors.Is(err, platform.ErrNotFound):
			// Keep the cookie so the caller can retry once the store is back.
			h.cfg.Logger.Error("resolve session at logout", "err", err)
			writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
			return
		}
	}
	// Clear regardless, even an invalid cookie, so the browser stops sending it.
	h.clearSessionCookies(w)
	w.WriteHeader(http.StatusOK)
}

// clearSessionCookies expires the session cookie and, in the same response, its presence hint;
// a leftover hint would cost the explorer a 401 per page load until it expired.
func (h *Handlers) clearSessionCookies(w http.ResponseWriter) {
	cleared := credentialCookie(SessionCookieName, "") //nolint:gosec // G124: credentialCookie sets Secure, HttpOnly and SameSite=Lax
	cleared.MaxAge = -1
	http.SetCookie(w, cleared)
	h.clearSessionHintCookie(w)
}

// signupNewUser creates the account and first user (as owner) from a just-verified email.
//
// Concurrent callbacks for one email can both pass GetUserByEmail and create an account; only the
// first user insert wins on users_email_idx. EmailLocker prevents the speculative account. Without
// one, ErrConflict on CreateUser marks it Suspended with a "signup-race:" reason (an unambiguous
// reaper signal) and reloads the winner.
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

	acct, err := h.createSignupAccount(ctx, email)
	if err != nil {
		return platform.User{}, err
	}
	user, err := h.cfg.Users.CreateUser(ctx, platform.User{
		AccountID: acct.ID,
		Email:     email,
		Role:      platform.RoleOwner,
	})
	if err != nil {
		if errors.Is(err, platform.ErrConflict) {
			// Race lost on the email unique index: the other callback's account is canonical, so
			// re-fetch and use its user.
			h.cfg.Logger.Warn("signup race: rolling back to winning user",
				"email", maskEmail(email), "speculative_account_id", acct.ID)
			winner, getErr := h.cfg.Users.GetUserByEmail(ctx, email)
			if getErr != nil {
				return platform.User{}, fmt.Errorf("create user conflict + reload: %w", getErr)
			}
			// Best-effort: the winner's login has already succeeded. The reason carries no address
			// because it is copied into audit metadata.
			if suspErr := h.cfg.Accounts.Suspend(ctx, acct.ID, "signup-race: orphan speculative account"); suspErr != nil {
				h.cfg.Logger.Warn("signup race: failed to mark orphan account",
					"err", suspErr, "speculative_account_id", acct.ID)
			}
			return winner, nil
		}
		return platform.User{}, fmt.Errorf("create user: %w", err)
	}
	return user, nil
}

// createSignupAccount inserts the account for a first-login email. Name and slug derive from the
// address and are clamped to the accounts CHECK bounds; the full address lives in billing_email.
func (h *Handlers) createSignupAccount(ctx context.Context, email string) (platform.Account, error) {
	slug := slugFromEmail(email)
	newAccount := func(slug string) platform.Account {
		return platform.Account{
			Name:         accountNameFromEmail(email), // operator can rename later
			Slug:         slug,
			BillingEmail: email,
			Tier:         platform.TierFree,
			Status:       platform.AccountActive,
		}
	}
	acct, err := h.cfg.Accounts.Create(ctx, newAccount(slug))
	if errors.Is(err, platform.ErrConflict) {
		// Slug collision: retry once with a 4-hex suffix.
		acct, err = h.cfg.Accounts.Create(ctx, newAccount(suffixSlug(slug, uuid.New().String()[:4])))
	}
	if err != nil {
		return platform.Account{}, fmt.Errorf("create account: %w", err)
	}
	return acct, nil
}

// acquireSignupLock serialises first-login provisioning for email through the EmailLocker. It
// returns (winner, true, nil, nil) when the lock was lost and the winner's user became visible;
// (_, false, release, nil) when there is no locker, acquire failed (fall through unlocked) or we
// hold the lock, with release non-nil only when held and to be deferred; and (_, false, nil, err)
// when the re-check under the lock hit a non-NotFound store error.
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
	// A winner may have committed before we acquired (their lock TTL elapsed); re-check before
	// the speculative Account.Create.
	if existing, getErr := h.cfg.Users.GetUserByEmail(ctx, email); getErr == nil {
		release()
		return existing, true, nil, nil
	} else if !errors.Is(getErr, platform.ErrNotFound) {
		release()
		return platform.User{}, false, nil, fmt.Errorf("recheck user under lock: %w", getErr)
	}
	return platform.User{}, false, release, nil
}

// hashEmailForLocker returns a stable hex digest for the signup lock key, so a Redis dump leaks
// no addresses and all API workers serialise on the same key.
func hashEmailForLocker(email string) string {
	h := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	return hex.EncodeToString(h[:])
}

// waitForWinnerUser polls GetUserByEmail until the winner's row appears, the context expires or the
// budget runs out, returning a zero User (ID == uuid.Nil) on timeout so the caller provisions
// itself. The ~1.5s budget is tight because the user is waiting on the callback redirect, yet it
// bridges a slow first INSERT and fails open if the lock-holder crashed.
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

// looksLikeCode is a cheap 6-digit pre-filter so a malformed body never reaches the token store;
// the real check is the constant-time match in HandleVerifyCode.
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
	// Behind the proxy the socket peer is the proxy itself; the middleware honours X-Forwarded-For
	// only per trusted_proxy_cidrs. Falls back to the socket peer when it returns empty.
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

// safeText sanitises a client-supplied string before it reaches a Postgres text column. It strips
// control characters (CR/LF could inject lines into the plaintext login email) and truncates by
// runes, so a multi-byte codepoint is never cut into invalid UTF-8.
func safeText(s string, maxRunes int) string {
	s = strings.Map(func(rr rune) rune {
		if rr == '\t' {
			return ' '
		}
		if rr < 0x20 || rr == 0x7f {
			return -1 // drop CR, LF, and other control chars
		}
		return rr
	}, s)
	if utf8.RuneCountInString(s) > maxRunes {
		s = string([]rune(s)[:maxRunes])
	}
	return s
}

// uaDangerousChars strips delimiter punctuation and bidi, zero-width and line-separator code
// points (which safeText's <0x20 check misses) from the stored UA. Outbound email never renders
// the raw UA; it uses notify.ClientFromUserAgent's closed vocabulary.
var uaDangerousChars = regexp.MustCompile(
	"[()\"'`:!?<>\\[\\]{}\u200b\u200e\u200f\u2028\u2029\u0085\u202a-\u202e\u2066-\u2069]")

func truncateUA(ua string) string {
	const maxUALen = 256
	return uaDangerousChars.ReplaceAllString(safeText(ua, maxUALen), "")
}

// slugFromEmail derives a lowercase, hyphenated, ASCII slug of at most platform.MaxAccountSlugLen
// bytes from the email's local part.
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
	return clampSlug(b.String(), platform.MaxAccountSlugLen)
}

// clampSlug trims edge hyphens and caps an ASCII slug at maxLen bytes, falling back to "user".
func clampSlug(s string, maxLen int) string {
	s = strings.Trim(s, "-")
	if len(s) > maxLen {
		s = strings.TrimRight(s[:maxLen], "-")
	}
	if s == "" {
		return "user"
	}
	return s
}

// suffixSlug appends "-<suffix>", shortening slug first to fit platform.MaxAccountSlugLen.
func suffixSlug(slug, suffix string) string {
	return clampSlug(slug, platform.MaxAccountSlugLen-len(suffix)-1) + "-" + suffix
}

// accountNameFromEmail cuts email to platform.MaxAccountNameLen runes, as Postgres length() counts.
func accountNameFromEmail(email string) string {
	n := 0
	for i := range email {
		if n == platform.MaxAccountNameLen {
			return email[:i]
		}
		n++
	}
	return email
}

// writeProblem emits a problem+json body via httpx, pinning this surface's type URL;
// dashboardauth must not depend on internal/api/v1's enveloped writeProblem.
func writeProblem(w http.ResponseWriter, status int, detail, instance string) {
	httpx.WriteProblem(w, "https://api.stellarindex.io/errors/auth", status, detail, instance)
}

// maskEmail redacts an email for logging ("alice@example.com" -> "a***@example.com"): enough to
// correlate a log line without persisting full PII.
func maskEmail(email string) string { return pii.MaskEmail(email) }
