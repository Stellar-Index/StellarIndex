package v1

import (
	"slices"
	"sort"
	"testing"
)

// TestPublicRoutes_MatchOpenAPISecurityNone keeps handlePublic and the spec's `security: []` in lockstep both ways: a
// spec-public route mounted gated 401s under apikey/sep10; a server-public route the spec doesn't declare is an
// undocumented hole.
func TestPublicRoutes_MatchOpenAPISecurityNone(t *testing.T) {
	// Credential-BLIND infra probes: exempted by
	// middleware.isUnauthenticatedInfraPath, not by handlePublic.
	infra := []string{"GET /v1/healthz", "GET /v1/readyz", "GET /v1/livez/lake", "GET /v1/version"}
	// Public but outside the /v1 spec by nature (RFC 9116 metadata).
	offSpec := []string{"GET /.well-known/security.txt"}

	var want []string
	for _, op := range specSecurityNoneOps(t) {
		if isDashboardLoginOp(op) || slices.Contains(infra, op) {
			continue
		}
		want = append(want, op)
	}
	want = append(want, offSpec...)
	sort.Strings(want)

	got := New(Options{}).publicRoutes.Patterns()
	sort.Strings(got)
	if !slices.Equal(got, want) {
		t.Errorf("handlePublic routes != spec security:[] ops\n got: %v\nwant: %v", got, want)
	}
}
