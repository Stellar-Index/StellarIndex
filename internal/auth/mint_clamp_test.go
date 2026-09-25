package auth

import (
	"context"
	"errors"
	"testing"
)

// GH-1147: the mint clamp bounded scopes but not rate_limit_per_min, so a
// scope-narrowed operator could mint 100,000/min keys. GH-1146: the clamp
// lived only in two HTTP handlers; the store now enforces it for every
// request that names its minter.
func TestClampToMinter_RateLimit(t *testing.T) {
	cases := []struct {
		name    string
		minter  Subject
		rate    int
		refused bool
	}{
		{"scoped minter on default cannot raise", Subject{Scopes: []string{"admin"}}, MaxKeyRateLimitPerMin, true},
		{"scoped minter on default may issue default", Subject{Scopes: []string{"admin"}}, 0, false},
		{"capped minter cannot exceed its own", Subject{RateLimitPerMin: 500}, 501, true},
		{"capped minter may match its own", Subject{Scopes: []string{"admin"}, RateLimitPerMin: 500}, 500, false},
		{"full-access minter on default delegates freely", Subject{}, MaxKeyRateLimitPerMin, false},
	}
	for _, tc := range cases {
		_, err := ClampToMinter(tc.minter, nil, tc.rate)
		if got := errors.Is(err, ErrMintExceedsCaller); got != tc.refused {
			t.Errorf("%s: refused = %v (err %v), want %v", tc.name, got, err, tc.refused)
		}
	}
}

func TestRedisAPIKeyStore_CreateEnforcesTheMinter(t *testing.T) {
	s, _, _ := newTestStore(t)
	ctx := context.Background()
	narrowed := Subject{Identifier: "operator:staff", Tier: TierOperator, KeyID: "kid_ops", Scopes: []string{"admin"}}

	_, _, err := s.Create(ctx, CreateAPIKeyRequest{
		Identifier: "acct:target", Tier: TierOperator, RateLimitPerMin: MaxKeyRateLimitPerMin, MintedBy: &narrowed,
	})
	if !errors.Is(err, ErrMintExceedsCaller) {
		t.Fatalf("scoped minter minting a %d/min key: err = %v, want ErrMintExceedsCaller", MaxKeyRateLimitPerMin, err)
	}

	rec, _, err := s.Create(ctx, CreateAPIKeyRequest{Identifier: "acct:target", MintedBy: &narrowed})
	if err != nil {
		t.Fatalf("in-bounds mint: %v", err)
	}
	if len(rec.Scopes) != 1 || rec.Scopes[0] != "admin" {
		t.Errorf("child of a scoped minter got scopes %v, want the minter's [admin], never full access", rec.Scopes)
	}
}
