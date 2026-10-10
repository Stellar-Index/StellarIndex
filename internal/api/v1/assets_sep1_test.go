package v1_test

import (
	"context"
	"database/sql"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// stubSep1Cache implements v1.Sep1CachedReader in-memory. Mirrors
// `Store.GetIssuerSep1Cached` semantics: returns the cached payload
// for known issuers, (nil, nil) when the row exists but has no
// payload yet, (nil, sql.ErrNoRows) for unknown issuers, or a
// configured error to exercise the error branch.
type stubSep1Cache struct {
	byIssuer map[string]*timescale.IssuerSep1Cached
	err      error
}

func (s *stubSep1Cache) GetIssuerSep1Cached(_ context.Context, gStrkey string) (*timescale.IssuerSep1Cached, error) {
	if s.err != nil {
		return nil, s.err
	}
	if payload, ok := s.byIssuer[gStrkey]; ok {
		return payload, nil
	}
	return nil, sql.ErrNoRows
}
