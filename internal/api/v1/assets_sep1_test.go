package v1_test

import (
	"context"
	"database/sql"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// stubSep1Cache implements v1.Sep1CachedReader in memory, mirroring Store.GetIssuerSep1Cached: payload for known issuers, (nil,
// nil) for a row with no payload yet, (nil, sql.ErrNoRows) for unknown issuers, or a configured error.
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
