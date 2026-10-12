package v1

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ReadyzHandler is the plain-text readiness probe for non-API daemons: 200 only when every critical check passes, else 503 naming the failures; non-critical checks never drain.
func ReadyzHandler(checks ...ReadyChecker) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		var failed []string
		for _, c := range checks {
			if !c.Critical() {
				continue
			}
			if err := c.Ping(ctx); err != nil {
				failed = append(failed, fmt.Sprintf("%s: %v", c.Name(), err))
			}
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if len(failed) > 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("not ready\n" + strings.Join(failed, "\n") + "\n"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
}
