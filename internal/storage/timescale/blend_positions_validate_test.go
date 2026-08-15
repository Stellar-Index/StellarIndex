package timescale

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/domain"
)

// validBlendPositionEvent returns an otherwise-well-formed event so a
// test can null/negate a single amount field in isolation.
func validBlendPositionEvent() domain.BlendPositionEvent {
	return domain.BlendPositionEvent{
		Pool:        "CPOOL",
		Kind:        domain.BlendEventSupply,
		Asset:       "CASSET",
		User:        "GUSER",
		TokenAmount: big.NewInt(1),
		BOrDAmount:  big.NewInt(1),
		Ledger:      100,
		TxHash:      "abc",
		OpIndex:     0,
		EventIndex:  0,
		Timestamp:   time.Unix(1, 0).UTC(),
	}
}

// TestInsertBlendPositionEvent_RejectsBadAmount is the BLEND-1
// regression: token_amount / b_or_d_amount had no SQL-boundary
// magnitude validator, so a nil amount was silently coerced to "0"
// (bigIntToNumericString) and a negative one written verbatim. The
// guard must reject both BEFORE touching the DB.
//
// s.db is nil here, so any code path that reaches the ExecContext
// call panics — reaching (and returning from) the guard first is what
// proves the ordering. Without the fix these cases fall through to the
// nil-db Exec and the test fails.
func TestInsertBlendPositionEvent_RejectsBadAmount(t *testing.T) {
	s := &Store{}
	ctx := context.Background()

	cases := []struct {
		name    string
		mutate  func(*domain.BlendPositionEvent)
		wantSub string
	}{
		{"nil TokenAmount", func(e *domain.BlendPositionEvent) { e.TokenAmount = nil }, "TokenAmount is nil"},
		{"negative TokenAmount", func(e *domain.BlendPositionEvent) { e.TokenAmount = big.NewInt(-1) }, "TokenAmount must be >= 0"},
		{"nil BOrDAmount", func(e *domain.BlendPositionEvent) { e.BOrDAmount = nil }, "BOrDAmount is nil"},
		{"negative BOrDAmount", func(e *domain.BlendPositionEvent) { e.BOrDAmount = big.NewInt(-1) }, "BOrDAmount must be >= 0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := validBlendPositionEvent()
			tc.mutate(&e)
			err := s.InsertBlendPositionEvent(ctx, e)
			if err == nil {
				t.Fatalf("InsertBlendPositionEvent(%s) = nil error, want rejection", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}
