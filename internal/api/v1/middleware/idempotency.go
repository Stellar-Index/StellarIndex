package middleware

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// IdempotencyKeyHeader is the header a client supplies to make a
// state-changing POST safe to retry without risking a duplicate
// mutation (double-submit, client timeout-and-retry). See
// [Idempotency].
const IdempotencyKeyHeader = "Idempotency-Key"

// MaxIdempotencyKeyLen caps the caller-supplied header so a hostile
// value can't grow the in-memory store unbounded. Mirrors
// MaxRateLimitKeyLen.
const MaxIdempotencyKeyLen = 256

// DefaultIdempotencyTTL is how long a captured response stays
// eligible for replay.
const DefaultIdempotencyTTL = 10 * time.Minute

// idempotencyRecord is a captured response, replayed verbatim on a
// repeat of the same key.
type idempotencyRecord struct {
	status    int
	header    http.Header
	body      []byte
	expiresAt time.Time
}

// IdempotencyStore is a process-local, TTL-bounded cache of recent
// idempotent responses. Safe for concurrent use.
//
// Process-local by design: a single API replica loses dedup across a
// restart or across sibling replicas behind a load balancer. That's
// an accepted trade for a mint-once endpoint (T284) — a retry that
// lands on a different replica within the TTL mints a second
// resource, exactly the pre-fix behaviour, never worse than it.
type IdempotencyStore struct {
	mu      sync.Mutex
	entries map[string]idempotencyRecord
	ttl     time.Duration
	now     func() time.Time
}

// NewIdempotencyStore builds a store with the given TTL (falls back
// to DefaultIdempotencyTTL when ttl <= 0).
func NewIdempotencyStore(ttl time.Duration) *IdempotencyStore {
	if ttl <= 0 {
		ttl = DefaultIdempotencyTTL
	}
	return &IdempotencyStore{entries: make(map[string]idempotencyRecord), ttl: ttl, now: time.Now}
}

func (s *IdempotencyStore) get(key string) (idempotencyRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.entries[key]
	if !ok {
		return idempotencyRecord{}, false
	}
	if s.now().After(rec.expiresAt) {
		delete(s.entries, key)
		return idempotencyRecord{}, false
	}
	return rec, true
}

// put stores a captured response unless a still-live entry already
// occupies key — first writer wins, so a burst of near-simultaneous
// duplicate submissions converges on the FIRST response rather than
// the last handler to finish clobbering it.
func (s *IdempotencyStore) put(key string, status int, header http.Header, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.entries[key]; ok && s.now().Before(existing.expiresAt) {
		return
	}
	s.entries[key] = idempotencyRecord{
		status:    status,
		header:    header,
		body:      body,
		expiresAt: s.now().Add(s.ttl),
	}
}

// idempotencyRecorder captures the handler's response so it can be
// stored alongside being written to the real client.
type idempotencyRecorder struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
}

func (rec *idempotencyRecorder) WriteHeader(status int) {
	rec.status = status
	rec.ResponseWriter.WriteHeader(status)
}

func (rec *idempotencyRecorder) Write(b []byte) (int, error) {
	rec.buf.Write(b)
	return rec.ResponseWriter.Write(b)
}

// Idempotency returns middleware that lets a client safely retry a
// state-changing POST. A caller supplying the Idempotency-Key header
// gets the ORIGINAL captured response replayed verbatim on any repeat
// within store's TTL, instead of the handler re-running — for a
// mint-once endpoint (API-key creation, price-alert creation) that
// otherwise silently mints a second resource the client has no way to
// reconcile against the first (T284).
//
// subjectKeyFn scopes the cache to the caller (account/session) so
// two different callers who happen to pick the same literal key
// string never collide. A nil subjectKeyFn, an empty subject, or a
// missing/blank header leaves the request to run normally — the
// header is opt-in, matching its semantics elsewhere (Stripe et al.).
//
// Only a 2xx response is cached: a validation failure or a transient
// 5xx must not be replayed, or a client who fixed the request (or
// retried past a blip) would keep getting the frozen error for the
// whole TTL.
func Idempotency(logger *slog.Logger, store *IdempotencyStore, subjectKeyFn func(*http.Request) string) Middleware {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rawKey := strings.TrimSpace(r.Header.Get(IdempotencyKeyHeader))
			if rawKey == "" || subjectKeyFn == nil || store == nil {
				next.ServeHTTP(w, r)
				return
			}
			if len(rawKey) > MaxIdempotencyKeyLen {
				writeIdempotencyKeyTooLong(w, r)
				return
			}
			subject := subjectKeyFn(r)
			if subject == "" {
				next.ServeHTTP(w, r)
				return
			}
			cacheKey := subject + ":" + rawKey

			if rec, ok := store.get(cacheKey); ok {
				replayIdempotentResponse(w, rec)
				return
			}

			rec := &idempotencyRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			if rec.status >= 200 && rec.status < 300 {
				store.put(cacheKey, rec.status, rec.Header().Clone(), append([]byte(nil), rec.buf.Bytes()...))
			}
		})
	}
}

func replayIdempotentResponse(w http.ResponseWriter, rec idempotencyRecord) {
	dst := w.Header()
	for k, vs := range rec.header {
		dst[k] = vs
	}
	dst.Set("Idempotency-Replayed", "true")
	w.WriteHeader(rec.status)
	_, _ = w.Write(rec.body)
}

func writeIdempotencyKeyTooLong(w http.ResponseWriter, r *http.Request) {
	p := rlProblem{
		Type:     "https://api.stellarindex.io/errors/idempotency-key-too-long",
		Title:    "Idempotency-Key too long",
		Status:   http.StatusBadRequest,
		Detail:   "Idempotency-Key must be at most 256 bytes",
		Instance: r.URL.RequestURI(),
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(p)
}
