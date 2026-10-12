package middleware

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

// IdempotencyKeyHeader is the header a client supplies to make a state-changing POST safe to retry; see [Idempotency].
const IdempotencyKeyHeader = "Idempotency-Key"

// MaxIdempotencyKeyLen caps the caller-supplied header so a hostile value can't grow the in-memory store unbounded; mirrors MaxRateLimitKeyLen.
const MaxIdempotencyKeyLen = 256

// DefaultIdempotencyTTL is how long a captured response stays eligible for replay.
const DefaultIdempotencyTTL = 10 * time.Minute

// idempotencyRecord is a captured response, replayed verbatim on a
// repeat of the same key.
type idempotencyRecord struct {
	status    int
	header    http.Header
	body      []byte
	expiresAt time.Time
}

// IdempotencyStore is a process-local, TTL-bounded cache of recent idempotent responses, safe for concurrent use; dedup is lost across restarts and replicas.
type IdempotencyStore struct {
	mu      sync.Mutex
	entries map[string]idempotencyRecord
	// inFlight holds keys whose first request is still running. A
	// timed-out client's retry typically lands while the original is
	// still committing; without this both run and mint twice.
	inFlight map[string]struct{}
	ttl      time.Duration
	now      func() time.Time
}

// NewIdempotencyStore builds a store with the given TTL, falling back to DefaultIdempotencyTTL when ttl <= 0.
func NewIdempotencyStore(ttl time.Duration) *IdempotencyStore {
	if ttl <= 0 {
		ttl = DefaultIdempotencyTTL
	}
	return &IdempotencyStore{
		entries:  make(map[string]idempotencyRecord),
		inFlight: make(map[string]struct{}),
		ttl:      ttl,
		now:      time.Now,
	}
}

// idempotencyClaim is begin's verdict for one request.
type idempotencyClaim int

const (
	claimProceed  idempotencyClaim = iota // first request for this key: run the handler
	claimReplay                           // a captured response exists: replay it
	claimInFlight                         // the first request is still running: 409
)

// begin atomically resolves key to a replay, an in-flight conflict, or
// a fresh claim. A proceed verdict marks key in flight; the caller must
// call finish exactly once.
func (s *IdempotencyStore) begin(key string) (idempotencyRecord, idempotencyClaim) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := s.entries[key]; ok {
		if !s.now().After(rec.expiresAt) {
			return rec, claimReplay
		}
		delete(s.entries, key)
	}
	if _, running := s.inFlight[key]; running {
		return idempotencyRecord{}, claimInFlight
	}
	s.inFlight[key] = struct{}{}
	return idempotencyRecord{}, claimProceed
}

// finish releases key's in-flight claim.
func (s *IdempotencyStore) finish(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inFlight, key)
}

// put stores a captured response unless a still-live entry already occupies key. First writer
// wins, so near-simultaneous duplicates converge on the FIRST response.
//
// Every call also sweeps expired entries: a key used exactly once is never replayed, so
// begin()'s eviction never reaches it, and the map would otherwise grow for the process
// lifetime. This bounds it to about one TTL of traffic with no extra goroutine.
func (s *IdempotencyStore) put(key string, status int, header http.Header, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if existing, ok := s.entries[key]; ok && now.Before(existing.expiresAt) {
		return
	}
	s.sweepExpiredLocked(now)
	s.entries[key] = idempotencyRecord{
		status:    status,
		header:    header,
		body:      body,
		expiresAt: now.Add(s.ttl),
	}
}

// sweepExpiredLocked removes every entry whose TTL has passed as of
// now. Callers must hold s.mu.
func (s *IdempotencyStore) sweepExpiredLocked(now time.Time) {
	for k, rec := range s.entries {
		if now.After(rec.expiresAt) {
			delete(s.entries, k)
		}
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

// Idempotency replays the ORIGINAL 2xx response for a repeated Idempotency-Key within the store's TTL; scoped per subject, method and path, and only 2xx is cached so fixed errors are not frozen.
func Idempotency(store *IdempotencyStore, subjectKeyFn func(*http.Request) string) Middleware {
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
			// Route-scoped so one key reused across endpoints (or across
			// {id}s) never replays another route's response.
			cacheKey := subject + ":" + r.Method + " " + r.URL.Path + ":" + rawKey

			prior, claim := store.begin(cacheKey)
			switch claim {
			case claimReplay:
				replayIdempotentResponse(w, prior)
				return
			case claimInFlight:
				writeIdempotencyKeyInFlight(w, r)
				return
			case claimProceed:
			}
			// Deferred so the claim is released after put below (a retry
			// never sees "not in flight, nothing captured" for a request
			// that succeeded) and also when the handler panics.
			defer store.finish(cacheKey)
			rec := &idempotencyRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			if rec.status >= 200 && rec.status < 300 {
				store.put(cacheKey, rec.status, stripCORSHeaders(rec.Header().Clone()), append([]byte(nil), rec.buf.Bytes()...))
			}
		})
	}
}

// stripCORSHeaders removes the CORS-owned headers (and the Origin token from Vary) so a replay
// never carries the first request's origin decision. It runs on both store and replay.
func stripCORSHeaders(h http.Header) http.Header {
	for k := range h {
		if strings.HasPrefix(k, "Access-Control-") {
			delete(h, k)
		}
	}
	var kept []string
	for _, v := range h.Values("Vary") {
		for _, tok := range strings.Split(v, ",") {
			if tok = strings.TrimSpace(tok); tok != "" && !strings.EqualFold(tok, "Origin") {
				kept = append(kept, tok)
			}
		}
	}
	if len(kept) == 0 {
		h.Del("Vary")
	} else {
		h.Set("Vary", strings.Join(kept, ", "))
	}
	return h
}

func replayIdempotentResponse(w http.ResponseWriter, rec idempotencyRecord) {
	dst := w.Header()
	for k, vs := range stripCORSHeaders(rec.header.Clone()) {
		if k == "Vary" {
			for _, v := range vs {
				dst.Add(k, v)
			}
			continue
		}
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

func writeIdempotencyKeyInFlight(w http.ResponseWriter, r *http.Request) {
	p := rlProblem{
		Type:     "https://api.stellarindex.io/errors/idempotency-key-in-flight",
		Title:    "Request with this Idempotency-Key still in progress",
		Status:   http.StatusConflict,
		Detail:   "the original request carrying this Idempotency-Key has not finished; retry shortly to receive its response",
		Instance: r.URL.RequestURI(),
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Retry-After", "1")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(p)
}
