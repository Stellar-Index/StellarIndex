package dashboardauth

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// touchTimeout caps the async TouchSession write — this is a
// fire-and-forget background operation, so a stuck write
// shouldn't keep the goroutine alive.
const touchTimeout = 2 * time.Second

// newTouchCtx returns a fresh context for the async TouchSession
// goroutine. It uses context.WithoutCancel to inherit values
// (logger / tracing / etc) from the request context but
// deliberately drop its cancellation — by the time the goroutine
// runs, the request handler will likely have already returned and
// cancelled its context, but we still want the touch write to
// land. A 2 s timeout caps a stuck write.
func newTouchCtx(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), touchTimeout)
}

// parseIP is a defensive net.ParseIP wrapper. Empty string returns
// nil (TouchSession tolerates a nil IP — leaves the column unchanged).
func parseIP(s string) net.IP {
	if s == "" {
		return nil
	}
	return net.ParseIP(s)
}

// Middleware returns an HTTP middleware that resolves the
// session cookie (if present + valid) and plants a
// SessionContext on the request context. The wrapped handler
// can call SessionFromContext to read it.
//
// Anonymous requests (no cookie / invalid cookie / revoked
// session / expired session) pass through untouched —
// downstream RequireSession (separate middleware) is what
// gates routes that require authentication.
//
// touchEvery debounces TouchSession writes: the first request
// in a minute updates last_seen_at; subsequent requests inside
// that minute skip the DB write. Hot-row contention on a
// single session would otherwise dominate at high request
// rates from a single tab.
func Middleware(cfg *Config) func(http.Handler) http.Handler {
	// Defensive defaults. The auth Handlers get a validated Config
	// copy via NewHandlers (validate() fills these in), but the
	// resolver Middleware is constructed straight from the caller's
	// Config, which may never have run validate(). resolveSession
	// calls cfg.Now() and cfg.Logger.Warn() — a nil here panics every
	// authenticated request, which is exactly how /v1/account/me
	// started 500-ing after a successful magic-link login. Default the
	// fields resolveSession depends on so the middleware is
	// self-sufficient regardless of how the Config was built.
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	tracker := newTouchTracker(time.Minute)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sc, ok := resolveSession(r, cfg, tracker)
			if ok {
				r = r.WithContext(WithSession(r.Context(), sc))
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireSession returns middleware that 401s requests
// without a valid session. Wire it inside Middleware(...) for
// dashboard routes that require login.
func RequireSession(cfg *Config) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, ok := SessionFromContext(r.Context()); !ok {
				writeProblem(w, http.StatusUnauthorized, "authentication required", r.URL.Path)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// resolveSession is the inner read-the-cookie + load-from-DB
// path. Pulls into a helper so Middleware stays small.
func resolveSession(r *http.Request, cfg *Config, tracker *touchTracker) (SessionContext, bool) {
	cookie, err := r.Cookie(SessionCookieName)
	if err != nil || cookie.Value == "" {
		return SessionContext{}, false
	}
	id, err := uuid.Parse(cookie.Value)
	if err != nil {
		return SessionContext{}, false
	}

	sess, err := cfg.Users.GetSession(r.Context(), id)
	if err != nil {
		// ErrNotFound covers expired (already filtered server-
		// side via the WHERE revoked_at IS NULL pred + the
		// caller's session-scan), revoked, and absent. The
		// user re-logs-in.
		if !errors.Is(err, platform.ErrNotFound) {
			cfg.Logger.Warn("session lookup", "err", err, "session_id", id)
		}
		return SessionContext{}, false
	}
	if !sess.ExpiresAt.After(cfg.Now()) {
		// Belt-and-suspenders: row was returned (revoked_at
		// NULL) but expires_at has passed.
		return SessionContext{}, false
	}

	user, err := cfg.Users.GetUserByID(r.Context(), sess.UserID)
	if err != nil {
		cfg.Logger.Warn("session user lookup", "err", err, "user_id", sess.UserID)
		return SessionContext{}, false
	}
	acct, err := cfg.Accounts.Get(r.Context(), user.AccountID)
	if err != nil {
		cfg.Logger.Warn("session account lookup", "err", err, "account_id", user.AccountID)
		return SessionContext{}, false
	}
	if acct.Status != platform.AccountActive {
		// Suspended / closed account → session denied.
		// Revoke the session so the user's browser drops it.
		_ = cfg.Users.RevokeSession(r.Context(), sess.ID)
		return SessionContext{}, false
	}

	// Touch last-seen async (debounced). Don't block the
	// request on the write — if it fails the dashboard still
	// works; we just won't have a fresh last_seen_at.
	if tracker.shouldTouch(sess.ID, cfg.Now()) {
		parent := r.Context()
		go func(parent context.Context, id uuid.UUID, ip string, ua string) {
			// AGT-12: this fire-and-forget write runs in its OWN
			// goroutine, so an unrecovered panic here terminates the
			// WHOLE process — nothing upstream (middleware.Recoverer
			// et al) wraps a bare `go func(){}()`. Recover so a panic
			// only drops this one touch-write instead of taking down
			// every in-flight request.
			defer func() {
				if r := recover(); r != nil {
					cfg.Logger.Error("touch session panic", "err", r, "session_id", id)
				}
			}()
			ctx, cancel := newTouchCtx(parent)
			defer cancel()
			ipParsed := parseIP(ip)
			_ = cfg.Users.TouchSession(ctx, id, ipParsed, ua)
		}(parent, sess.ID, clientIP(r).String(), truncateUA(r.UserAgent()))
	}

	return SessionContext{Session: sess, User: user, Account: acct}, true
}

// touchTracker debounces TouchSession DB writes per session.
// Map of session_id → last-touched timestamp. In-memory only;
// a process restart resets it (worst case: one extra write per
// session post-restart, which is fine).
type touchTracker struct {
	mu        sync.Mutex
	last      map[uuid.UUID]time.Time
	interval  time.Duration
	lastSweep time.Time
}

func newTouchTracker(interval time.Duration) *touchTracker {
	return &touchTracker{
		last:     make(map[uuid.UUID]time.Time),
		interval: interval,
	}
}

func (t *touchTracker) shouldTouch(id uuid.UUID, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if last, ok := t.last[id]; ok && now.Sub(last) < t.interval {
		return false
	}
	t.last[id] = now
	t.sweepLocked(now)
	return true
}

// sweepLocked opportunistically evicts entries whose last-touch has
// aged past interval (REL-05). resolveSession has no "session ended"
// signal to delete on, so without this every distinct session ID ever
// seen stays in the map for the lifetime of the process — one
// permanent entry per session, unbounded growth. An entry this old can
// no longer suppress a touch anyway (shouldTouch's own staleness check
// already treats it as due again), so it's pure dead weight; evicting
// it bounds the map to roughly "sessions active within the last
// interval" instead.
//
// Rate-limited to once per interval (checked against lastSweep) so a
// request-heavy deployment doesn't pay an O(map size) scan on every
// single touch — only on the (at most) one touch per interval that
// already does a map write.
//
// Caller must hold t.mu.
func (t *touchTracker) sweepLocked(now time.Time) {
	if now.Sub(t.lastSweep) < t.interval {
		return
	}
	t.lastSweep = now
	for id, last := range t.last {
		if now.Sub(last) >= t.interval {
			delete(t.last, id)
		}
	}
}
