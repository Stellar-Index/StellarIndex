package auth

import (
	"context"
	"errors"
	"testing"
)

// TestRedisAPIKeyStore_RevokeKeyByID_NothingRevokedIsAnError is the store
// contract behind the operator kill switch (GH-628): a revoke that matched
// no key must be distinguishable from one that killed a credential. A nil
// here let DELETE /v1/admin/keys/{id} answer 204 and write a key.revoke
// audit row for a typo'd identifier while the leaked key kept working.
func TestRedisAPIKeyStore_RevokeKeyByID_NothingRevokedIsAnError(t *testing.T) {
	s, _, _ := newTestStore(t)
	ctx := context.Background()
	rec, _, err := s.Create(ctx, CreateAPIKeyRequest{Identifier: "acct:owner", Label: "k"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cases := []struct{ name, identifier, keyID string }{
		{"unknown key id", "acct:owner", "kid_doesnotexist"},
		{"typo'd identifier", "acct:0wner", rec.KeyID},
	}
	for _, tc := range cases {
		if err := s.RevokeKeyByID(ctx, tc.identifier, tc.keyID); !errors.Is(err, ErrKeyNotFound) {
			t.Errorf("%s: RevokeKeyByID = %v, want ErrKeyNotFound", tc.name, err)
		}
	}

	if err := s.RevokeKeyByID(ctx, "acct:owner", rec.KeyID); err != nil {
		t.Fatalf("owner revoke: %v", err)
	}
	if err := s.RevokeKeyByID(ctx, "acct:owner", rec.KeyID); !errors.Is(err, ErrKeyNotFound) {
		t.Errorf("second revoke of the same key = %v, want ErrKeyNotFound (nothing left to revoke)", err)
	}
}
