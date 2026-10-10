package v1_test

import (
	"context"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

// fakeSessionPeeker is the handler-level test double for
// [v1.SessionPeeker]. Returns a canned SessionInfo regardless of
// context, mirroring how dashboardauth's real implementation is
// wired via main.go's adapter.
type fakeSessionPeeker struct {
	info v1.SessionInfo
	ok   bool
}

func (f *fakeSessionPeeker) SessionFromContext(_ context.Context) (v1.SessionInfo, bool) {
	return f.info, f.ok
}
