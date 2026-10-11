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
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// touchTimeout caps the async TouchSession write — this is a
// fire-and-forget background operation, so a stuck write
// shouldn't keep the goroutine alive.
const touchTimeout = 2 * time.Second

// sendTimeout caps an outbound mail send that runs after a durable
// write has already landed (e.g. the magic-link token). The request
// context is deliberately not used for cancellation there: a client
// disconnect must not read the same as a provider failure.
const sendTimeout = 10 * time.Second

// newTouchCtx returns a context for the async TouchSession goroutine. It keeps the request's values
// but drops its cancellation, since the handler has usually returned by then; a 2 s timeout caps a
// stuck write.
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

// Middleware resolves a valid session cookie and plants a SessionContext for SessionFromContext.
// Anonymous requests (no, invalid, revoked or expired cookie) pass through untouched; gating is
// RequireSession's job.
//
// TouchSession writes are debounced per minute so a single busy tab does not cause hot-row
// contention on its session.
func Middleware(cfg *Config) func(http.Handler) http.Handler {
	// The caller's Config may never have run validate(), and resolveSession calls cfg.Now() and
	// cfg.Logger.Warn(); a nil here would panic every authenticated request. Default them so the
	// middleware does not depend on how the Config was built.
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.SessionIdleTimeout == 0 {
		cfg.SessionIdleTimeout = defaultSessionIdleTimeout
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
func RequireSession() func(http.Handler) http.Handler {
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
	// The cookie carries the random token, not the DB primary key. Hash
	// it and look up by hash — the table stores only sha256(token), so a
	// read of the sessions table is not directly replayable. Any
	// non-matching / stale cookie simply misses.
	sess, err := cfg.Users.GetSessionByTokenHash(r.Context(), HashSessionToken(cookie.Value))
	if err != nil {
		// ErrNotFound covers absent, revoked and expired (the
		// store filters expires_at > now()). The user re-logs-in.
		if !errors.Is(err, platform.ErrNotFound) {
			// Log only `err`, never the cookie token: it is the bearer credential, and this branch
			// fires on any transient store error, so a database hiccup must not write one live
			// long-lived token per request into logs that ship to Loki.
			cfg.Logger.Warn("session lookup", "err", err)
		}
		return SessionContext{}, false
	}
	if !sess.ExpiresAt.After(cfg.Now()) {
		// The store filters on the database clock; this re-checks
		// on the API clock so skew between the two cannot extend a
		// session.
		return SessionContext{}, false
	}
	if sessionIdle(sess, cfg.Now(), cfg.SessionIdleTimeout) {
		// Revoke so a stolen-but-dormant cookie cannot be revived by
		// a later clock or config change.
		_ = cfg.Users.RevokeSession(r.Context(), sess.ID)
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
			// This runs in its own goroutine, so an unrecovered panic would terminate the whole
			// process; no upstream Recoverer wraps it. Recover so a panic only drops this touch.
			defer func() {
				if r := recover(); r != nil {
					// worker.Report moves stellarindex_worker_panics_total so the panic pages. No
					// session_id in the label or message: the worker label must stay bounded.
					worker.Report(cfg.Logger, "api-dashboard-session-touch", r)
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

// defaultSessionIdleTimeout bounds how long an unused session
// cookie stays valid inside its absolute SessionTTL.
const defaultSessionIdleTimeout = 7 * 24 * time.Hour

// sessionIdle reports whether sess has gone unused for longer than
// idle. last_seen_at is touched at most once a minute, so the bound
// is accurate to about that.
func sessionIdle(sess platform.Session, now time.Time, idle time.Duration) bool {
	last := sess.LastSeenAt
	if last.IsZero() {
		last = sess.CreatedAt
	}
	return idle > 0 && !last.IsZero() && now.Sub(last) > idle
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

func newTouchTracker(interval time.Duration) *touchTracker { //nolint:unparam // interval is a test seam; prod passes time.Minute
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

// sweepLocked evicts entries whose last touch has aged past interval. resolveSession has no
// session-ended signal, so without this the map grows by one entry per session forever; an aged
// entry no longer suppresses a touch anyway.
//
// It runs at most once per interval (see lastSweep) so a busy deployment does not pay an
// O(map size) scan on every touch.
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

// SessionAccountSubject scopes an Idempotency-Key on a dashboard create
// route to the caller's account, so two customers who pick the same
// literal key never share a cache entry. Empty when no session is
// attached, which middleware.Idempotency treats as "don't dedupe".
func SessionAccountSubject(r *http.Request) string {
	sc, ok := SessionFromContext(r.Context())
	if !ok {
		return ""
	}
	return sc.Account.ID.String()
}
