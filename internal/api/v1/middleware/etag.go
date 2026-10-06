package middleware

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
)

// etagMaxBody bounds how much of a response ETag buffers to hash; a
// larger body streams through without a validator.
const etagMaxBody = 8 << 20

// ETag sets a strong ETag over the exact bytes of a 200 GET/HEAD response
// and answers a matching If-None-Match with 304 Not Modified (RFC 9110
// §8.8.3, §13.1.2). It buffers the body to hash it, so anything that must
// not be buffered passes through untouched and untagged: a non-200 status,
// a text/event-stream or a handler that flushes, a Cache-Control: no-store
// response, a handler-set ETag, and a body over etagMaxBody.
//
// It must sit inside every middleware that flushes after the handler
// returns ([AfterResponse]); an outer flush would arrive after the body is
// already written, but an inner one would spill the buffer untagged.
func ETag(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		ew := &etagWriter{ResponseWriter: w}
		next.ServeHTTP(ew, r)
		ew.finish(r)
	})
}

type etagWriter struct {
	http.ResponseWriter
	status      int // final status once known; 0 before
	passthrough bool
	buf         bytes.Buffer
}

func (e *etagWriter) WriteHeader(status int) {
	if status < http.StatusOK {
		e.ResponseWriter.WriteHeader(status)
		return
	}
	if e.status != 0 {
		if e.passthrough {
			e.ResponseWriter.WriteHeader(status)
		}
		return
	}
	e.status = status
	if !e.taggable() {
		e.passthrough = true
		e.ResponseWriter.WriteHeader(status)
	}
}

func (e *etagWriter) taggable() bool {
	h := e.Header()
	if e.status != http.StatusOK || h.Get("ETag") != "" || h.Get("Content-Encoding") != "" {
		return false
	}
	if strings.HasPrefix(strings.ToLower(h.Get("Content-Type")), "text/event-stream") {
		return false
	}
	for _, v := range h.Values("Cache-Control") {
		for _, d := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(d), "no-store") {
				return false
			}
		}
	}
	return true
}

func (e *etagWriter) Write(p []byte) (int, error) {
	if e.status == 0 {
		e.WriteHeader(http.StatusOK)
	}
	if e.passthrough {
		return e.ResponseWriter.Write(p)
	}
	if e.buf.Len()+len(p) > etagMaxBody {
		if err := e.spill(); err != nil {
			return 0, err
		}
		return e.ResponseWriter.Write(p)
	}
	return e.buf.Write(p)
}

// spill abandons tagging: it commits the held status and buffered bytes.
func (e *etagWriter) spill() error {
	e.passthrough = true
	e.ResponseWriter.WriteHeader(e.status)
	if e.buf.Len() == 0 {
		return nil
	}
	_, err := e.ResponseWriter.Write(e.buf.Bytes())
	e.buf.Reset()
	return err
}

// Flush means the handler is streaming, so the response goes out untagged.
func (e *etagWriter) Flush() {
	if e.status == 0 {
		e.WriteHeader(http.StatusOK)
	}
	if !e.passthrough {
		_ = e.spill()
	}
	if f, ok := e.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.NewResponseController reach SetWriteDeadline et al.
func (e *etagWriter) Unwrap() http.ResponseWriter {
	return e.ResponseWriter
}

func (e *etagWriter) finish(r *http.Request) {
	if e.passthrough || e.status == 0 {
		return
	}
	sum := sha256.Sum256(e.buf.Bytes())
	tag := `"` + hex.EncodeToString(sum[:16]) + `"`
	h := e.Header()
	h.Set("ETag", tag)
	if ifNoneMatch(r.Header.Values("If-None-Match"), tag) {
		h.Del("Content-Type")
		h.Del("Content-Length")
		e.ResponseWriter.WriteHeader(http.StatusNotModified)
		return
	}
	if h.Get("Content-Length") == "" {
		h.Set("Content-Length", strconv.Itoa(e.buf.Len()))
	}
	e.ResponseWriter.WriteHeader(e.status)
	_, _ = e.ResponseWriter.Write(e.buf.Bytes())
}

// ifNoneMatch applies RFC 9110 §13.1.2's weak comparison of tag against
// every entity-tag in the If-None-Match field lines.
func ifNoneMatch(values []string, tag string) bool {
	for _, v := range values {
		for _, c := range strings.Split(v, ",") {
			c = strings.TrimSpace(c)
			if c == "*" || strings.TrimPrefix(c, "W/") == tag {
				return true
			}
		}
	}
	return false
}
