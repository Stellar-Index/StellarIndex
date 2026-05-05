package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/redis/go-redis/v9"

	"github.com/RatesEngine/rates-engine/internal/cachekeys"
)

// ListKeysForIdentifier returns every [APIKeyRecord] whose
// Identifier matches. Used by:
//
//   - The Stripe webhook handler, when a payment lands and we
//     need to lift every key that customer holds into the paid
//     tier (rather than asking them to rotate).
//   - The future /v1/account/keys (GET) endpoint that lists a
//     caller's keys.
//
// Implementation: SCANs `apikey:*`, JSON-decodes each, filters
// by Identifier. O(N) on key count — acceptable at v1's scale;
// if/when the total key count crosses ~10⁵, swap the SCAN for a
// `signup:identifier:<id>` Redis SET written at Create time.
//
// Returns nil + nil for "no matches" (the operator-facing path
// distinguishes "Stripe sent us a webhook for an identifier we
// don't know" from a Redis I/O failure).
func (s *RedisAPIKeyStore) ListKeysForIdentifier(ctx context.Context, identifier string) ([]APIKeyRecord, error) {
	if identifier == "" {
		return nil, errors.New("auth: ListKeysForIdentifier: identifier is required")
	}

	var out []APIKeyRecord
	iter := s.rdb.Scan(ctx, 0, cachekeys.APIKey("*"), 1000).Iterator()
	for iter.Next(ctx) {
		k := iter.Val()
		raw, err := s.rdb.Get(ctx, k).Bytes()
		if err != nil {
			if errors.Is(err, redis.Nil) {
				continue // raced with delete
			}
			return nil, fmt.Errorf("auth: ListKeysForIdentifier: redis get %s: %w", k, err)
		}
		var rec APIKeyRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			// Malformed record — skip + continue (don't fail the
			// whole call because some other key is corrupt).
			continue
		}
		if rec.Identifier == identifier {
			out = append(out, rec)
		}
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("auth: ListKeysForIdentifier: redis scan: %w", err)
	}
	return out, nil
}
