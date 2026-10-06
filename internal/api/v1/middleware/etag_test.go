package middleware

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const etagTestBody = `{"data":{"price":"0.1234"}}`

func expectedETag(body string) string {
	sum := sha256.Sum256([]byte(body))
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}

func jsonHandler(cacheControl string, status int, body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if cacheControl != "" {
			w.Header().Set("Cache-Control", cacheControl)
		}
		w.WriteHeader(status)
		// Two writes: the tag must cover the whole body, not the last chunk.
		_, _ = w.Write([]byte(body[:5]))
		_, _ = w.Write([]byte(body[5:]))
	})
}

func serveETag(h http.Handler, method, ifNoneMatch string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/v1/price", nil)
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	rec := httptest.NewRecorder()
	ETag(h).ServeHTTP(rec, req)
	return rec
}

func TestETag_TagsOKBodyAndAnswersMatchWith304(t *testing.T) {
	h := jsonHandler("public, max-age=30", http.StatusOK, etagTestBody)
	want := expectedETag(etagTestBody)

	first := serveETag(h, http.MethodGet, "")
	if first.Code != http.StatusOK || first.Body.String() != etagTestBody {
		t.Fatalf("first: status %d body %q", first.Code, first.Body.String())
	}
	if got := first.Header().Get("ETag"); got != want {
		t.Fatalf("ETag = %q, want %q", got, want)
	}
	if got := first.Header().Get("Content-Length"); got != "27" {
		t.Fatalf("Content-Length = %q, want 27", got)
	}

	for _, inm := range []string{want, "W/" + want, `"other", ` + want, "*"} {
		rec := serveETag(h, http.MethodGet, inm)
		if rec.Code != http.StatusNotModified {
			t.Fatalf("If-None-Match %s: status %d, want 304", inm, rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Fatalf("If-None-Match %s: 304 carried a body %q", inm, rec.Body.String())
		}
		if rec.Header().Get("ETag") != want || rec.Header().Get("Cache-Control") != "public, max-age=30" {
			t.Fatalf("If-None-Match %s: 304 dropped ETag/Cache-Control: %v", inm, rec.Header())
		}
		if rec.Header().Get("Content-Type") != "" {
			t.Fatalf("If-None-Match %s: 304 kept Content-Type", inm)
		}
	}

	stale := serveETag(h, http.MethodGet, `"0123"`)
	if stale.Code != http.StatusOK || stale.Body.String() != etagTestBody {
		t.Fatalf("non-matching If-None-Match: status %d body %q", stale.Code, stale.Body.String())
	}
}

func TestETag_HeadMatchesGet(t *testing.T) {
	h := jsonHandler("", http.StatusOK, etagTestBody)
	if got := serveETag(h, http.MethodHead, "").Header().Get("ETag"); got != expectedETag(etagTestBody) {
		t.Fatalf("HEAD ETag = %q", got)
	}
}

func TestETag_LeavesUntaggableResponsesAlone(t *testing.T) {
	cases := []struct {
		name   string
		method string
		h      http.Handler
	}{
		{"error status", http.MethodGet, jsonHandler("no-store", http.StatusNotFound, etagTestBody)},
		{"server error", http.MethodGet, jsonHandler("", http.StatusInternalServerError, etagTestBody)},
		{"no-store", http.MethodGet, jsonHandler("private, no-store", http.StatusOK, etagTestBody)},
		{"post", http.MethodPost, jsonHandler("", http.StatusOK, etagTestBody)},
		{"event stream", http.MethodGet, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(etagTestBody))
		})},
		{"oversize", http.MethodGet, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(etagTestBody))
			_, _ = w.Write(make([]byte, etagMaxBody))
		})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := serveETag(tc.h, tc.method, "*")
			if rec.Header().Get("ETag") != "" {
				t.Fatalf("ETag set: %q", rec.Header().Get("ETag"))
			}
			if rec.Code == http.StatusNotModified {
				t.Fatal("answered 304")
			}
			if !strings.HasPrefix(rec.Body.String(), etagTestBody) {
				t.Fatalf("body altered: %.40q", rec.Body.String())
			}
		})
	}
}

func TestETag_FlushStreamsUntagged(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("data: 1\n\n"))
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte("data: 2\n\n"))
	})
	rec := serveETag(h, http.MethodGet, "*")
	if !rec.Flushed {
		t.Fatal("Flush did not reach the underlying writer")
	}
	if rec.Code != http.StatusOK || rec.Header().Get("ETag") != "" {
		t.Fatalf("status %d ETag %q", rec.Code, rec.Header().Get("ETag"))
	}
	if rec.Body.String() != "data: 1\n\ndata: 2\n\n" {
		t.Fatalf("body %q", rec.Body.String())
	}
}

func TestETag_ResponseControllerReachesFlush(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("x"))
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("Flush via ResponseController: %v", err)
		}
	})
	if rec := serveETag(h, http.MethodGet, ""); !rec.Flushed || rec.Body.String() != "x" {
		t.Fatalf("flushed %v body %q", rec.Flushed, rec.Body.String())
	}
}
