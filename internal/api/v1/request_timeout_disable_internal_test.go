package v1

import (
	"testing"
	"time"
)

func stackHasRequestTimeout(s *Server) bool {
	for _, e := range s.middlewareStack() {
		if e.name == "RequestTimeout" {
			return true
		}
	}
	return false
}

// TestRequestTimeout_ExplicitDisableOmitsMiddleware: api.request_timeout = 0
// is documented to disable the blanket deadline, and config validation
// skips its ordering checks on that basis. New must not map every zero to
// the 15s default, or the configuration that was validated would never run.
func TestRequestTimeout_ExplicitDisableOmitsMiddleware(t *testing.T) {
	s := New(Options{DisableRequestTimeout: true, RequestTimeout: 20 * time.Second})
	if s.requestTimeout != 0 || stackHasRequestTimeout(s) {
		t.Errorf("DisableRequestTimeout: requestTimeout = %v, RequestTimeout middleware mounted = %v; want 0 and not mounted",
			s.requestTimeout, stackHasRequestTimeout(s))
	}
}

// TestRequestTimeout_UnsetKeepsDefault is the fail-safe half: a zero-value
// Options (every test server, any caller that forgets the field) must keep
// the default bound rather than silently run without one.
func TestRequestTimeout_UnsetKeepsDefault(t *testing.T) {
	s := New(Options{})
	if s.requestTimeout != defaultRequestTimeout || !stackHasRequestTimeout(s) {
		t.Errorf("zero Options: requestTimeout = %v, middleware mounted = %v; want %v and mounted",
			s.requestTimeout, stackHasRequestTimeout(s), defaultRequestTimeout)
	}
}
