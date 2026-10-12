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

// touchTimeout caps the fire-and-forget TouchSession write so a stuck one cannot pin its goroutine.
const touchTimeout = 2 * time.Second

// sendTimeout caps a mail send that runs after a durable write (e.g. the magic-link token). The
// request context is deliberately not used: a client disconnect must not read as a provider failure.
const sendTimeout = 10 * time.Second

// newTouchCtx returns a context for the async TouchSession goroutine: the request's values without its cancellation
// (the handler has usually returned), with a 2 s timeout capping a stuck write.
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

// Middleware resolves a valid session cookie into a SessionContext; anonymous requests (no, invalid, revoked or expired cookie) pass
// through untouched, gating is RequireSession's job.
func Middleware(cfg *Config) func(http.Handler) http.Handler {
	// The Config may never have run validate(); a nil Now or Logger would panic every authenticated
	// request, so default them here.
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

// RequireSession returns middleware that 401s requests without a valid session; wire it inside Middleware(...).
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

// resolveSession reads the cookie and loads the session from the DB, keeping Middleware small.
func resolveSession(r *http.Request, cfg *Config, tracker *touchTracker) (SessionContext, bool) {
	cookie, err := r.Cookie(SessionCookieName)
	if err != nil || cookie.Value == "" {
		return SessionContext{}, false
	}
	// The cookie carries the random token, not the primary key. The table stores only
	// sha256(token), so a read of it is not replayable.
	sess, err := cfg.Users.GetSessionByTokenHash(r.Context(), HashSessionToken(cookie.Value))
	if err != nil {
		// ErrNotFound covers absent, revoked and expired (the store filters expires_at > now()).
		if !errors.Is(err, platform.ErrNotFound) {
			// Log only `err`, never the cookie token: it is the bearer credential, and a database
			// hiccup must not write one live token per request into shipped logs.
			cfg.Logger.Warn("session lookup", "err", err)
		}
		return SessionContext{}, false
	}
	if !sess.ExpiresAt.After(cfg.Now()) {
		// The store filters on the database clock; re-check on the API clock so skew cannot extend
		// a session.
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
		// Suspended or closed account: deny and revoke the session.
		_ = cfg.Users.RevokeSession(r.Context(), sess.ID)
		return SessionContext{}, false
	}

	// Touch last-seen async (debounced); a failed write only leaves last_seen_at stale.
	if tracker.shouldTouch(sess.ID, cfg.Now()) {
		parent := r.Context()
		go func(parent context.Context, id uuid.UUID, ip string, ua string) {
			// No upstream Recoverer wraps this goroutine, so recover to keep a panic from killing the
			// process.
			defer func() {
				if r := recover(); r != nil {
					// worker.Report pages via stellarindex_worker_panics_total. No session_id in the
					// label: it must stay bounded.
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

// defaultSessionIdleTimeout bounds how long an unused session stays valid inside its SessionTTL.
const defaultSessionIdleTimeout = 7 * 24 * time.Hour

// sessionIdle reports whether sess has been unused longer than idle; last_seen_at moves at most
// once a minute, so the bound is that accurate.
func sessionIdle(sess platform.Session, now time.Time, idle time.Duration) bool {
	last := sess.LastSeenAt
	if last.IsZero() {
		last = sess.CreatedAt
	}
	return idle > 0 && !last.IsZero() && now.Sub(last) > idle
}

// touchTracker debounces TouchSession writes per session. It is in-memory; a restart costs at most
// one extra write per session.
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

// sweepLocked evicts entries aged past interval (an aged entry suppresses nothing; otherwise the map grows forever).
// At most once per interval to avoid an O(map) scan per touch. Caller must hold t.mu.
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

// SessionAccountSubject scopes an Idempotency-Key to the caller's account so two customers never share a cache entry; empty without a
// session, meaning "don't dedupe".
func SessionAccountSubject(r *http.Request) string {
	sc, ok := SessionFromContext(r.Context())
	if !ok {
		return ""
	}
	return sc.Account.ID.String()
}
