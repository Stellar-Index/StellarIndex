package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

// HeaderRequestID is the canonical header name; client-supplied values are accepted so traces correlate across services, else a fresh one is minted.
const HeaderRequestID = "X-Request-ID"

// RequestID ensures every request has an X-Request-ID; an incoming one is kept only if trace-safe (alphanum, `-`, `_`, `.`, up to 128 bytes), else a random 16-byte hex ID replaces it.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(HeaderRequestID)
		if !isSafeRequestID(id) {
			// Either empty, too long, or contained CR/LF/control
			// bytes that would otherwise leak into logs or the
			// response header. Fall back to a minted one — a client
			// that sent garbage gets a server-chosen trace ID.
			id = generateRequestID()
		}

		w.Header().Set(HeaderRequestID, id)
		ctx := withString(r.Context(), ctxKeyRequestID, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// isSafeRequestID accepts non-empty strings up to 128 bytes of alphanum, `-`, `_` and `.`.
// Anything else (CR/LF, spaces, quotes, unicode) is rejected so headers stay valid and logs
// are free of injection.
func isSafeRequestID(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '-', c == '_', c == '.':
			// ok
		default:
			return false
		}
	}
	return true
}

// generateRequestID produces a 32-char hex identifier from 16 bytes
// of crypto/rand. Panics only if the OS entropy source fails —
// same bar as any crypto code.
func generateRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is a panic-worthy condition — every
		// other middleware assumes ids exist.
		panic("middleware: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
