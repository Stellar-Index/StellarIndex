// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// An `apikey-lineage:<hash>` record says the key at hash still stands, and outlives its credential
// while descendants are in use. No authentication decision reads it, and every write to it is
// best-effort: a failure is counted, never returned to a request, and never touches a credential.

// lineageRecord is the value of an `apikey-lineage:` record: who the key is, never what it is.
type lineageRecord struct {
	KeyID      string `json:"key_id"`
	Identifier string `json:"identifier"`
}

// lineageSlideInterval is the most often one process slides the lineage records of one authenticating key.
const lineageSlideInterval = time.Hour

// LineageTTLMargin is how far a lineage record outlives the idle window it is slid to; it covers the
// slide throttle's lag behind the credential's per-request refresh.
const LineageTTLMargin = 2 * lineageSlideInterval

// writeLineageScript creates a lineage record only while its credential exists, expiring LineageTTLMargin
// after the credential (persistent when the credential is); an existing record is left alone.
// KEYS: credential, lineage. ARGV: body, margin in ms. Returns 1 when it wrote.
var writeLineageScript = redis.NewScript(`
local ttl = redis.call('PTTL', KEYS[1])
if ttl == -2 then
	return 0
end
if ttl == -1 then
	return redis.call('SET', KEYS[2], ARGV[1], 'NX') and 1 or 0
end
return redis.call('SET', KEYS[2], ARGV[1], 'NX', 'PX', ttl + tonumber(ARGV[2])) and 1 or 0
`)

// createLineage writes hash's lineage record if its credential exists and it has none.
func createLineage(ctx context.Context, rdb redis.Cmdable, hash string, rec APIKeyRecord) error {
	body, err := json.Marshal(lineageRecord{KeyID: rec.KeyID, Identifier: rec.Identifier})
	if err != nil {
		return err
	}
	return writeLineageScript.Run(ctx, rdb,
		[]string{cachekeys.APIKey(hash).String(), cachekeys.APIKeyLineage(hash).String()},
		body, LineageTTLMargin.Milliseconds()).Err()
}

// writeMintedLineage records a just-written credential's lineage; a failure leaves the mint standing
// and apikey-lineage-backfill writes the record later.
func writeMintedLineage(ctx context.Context, rdb redis.Cmdable, hash string, rec APIKeyRecord) {
	if err := createLineage(ctx, rdb, hash, rec); err != nil {
		obs.APIKeyLineageWriteFailuresTotal.WithLabelValues(obs.APIKeyLineageOpMint).Inc()
	}
}

// DeleteLineageRecords deletes the lineage records of hashes. An ACL denying the family is not an error
// (no record could have been written); any other failure is, so a revoke fails rather than leave one.
func DeleteLineageRecords(ctx context.Context, rdb redis.Cmdable, hashes ...string) error {
	if len(hashes) == 0 {
		return nil
	}
	names := make([]string, 0, len(hashes))
	for _, h := range hashes {
		names = append(names, cachekeys.APIKeyLineage(h).String())
	}
	err := rdb.Del(ctx, names...).Err()
	if isRedisNoPerm(err) {
		obs.APIKeyLineageWriteFailuresTotal.WithLabelValues(obs.APIKeyLineageOpDelete).Inc()
		return nil
	}
	if err != nil {
		return fmt.Errorf("redis del lineage records: %w", err)
	}
	return nil
}

// lineageSlider throttles lineage slides to one per authenticating key per lineageSlideInterval in this
// process: N API instances add at most N one-round-trip pipelines per key per interval. Zero value ready.
type lineageSlider struct {
	mu    sync.Mutex
	last  map[string]time.Time
	swept time.Time
}

// due reports whether hash's lineage should be slid now, and if so marks it slid.
func (l *lineageSlider) due(hash string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if last, ok := l.last[hash]; ok && now.Sub(last) < lineageSlideInterval {
		return false
	}
	if l.last == nil {
		l.last = map[string]time.Time{}
	}
	if now.Sub(l.swept) >= lineageSlideInterval {
		for h, at := range l.last {
			if now.Sub(at) >= lineageSlideInterval {
				delete(l.last, h)
			}
		}
		l.swept = now
	}
	l.last[hash] = now
	return true
}

// slide extends the lineage records of the key at hash and of every ancestor to idle+LineageTTLMargin, in one
// pipeline, after rec authenticated. EXPIRE ... XX cannot create a record (a revoked ancestor stays gone) and
// leaves a persistent one persistent; no credential record is touched, and a failure is only counted.
func (l *lineageSlider) slide(ctx context.Context, rdb redis.Cmdable, hash string, rec APIKeyRecord, idle time.Duration, now time.Time) {
	if idle <= 0 || !l.due(hash, now) {
		return
	}
	ttl := idle + LineageTTLMargin
	chain := rec.AncestorHashes
	if len(chain) > MaxDelegationDepth {
		chain = chain[:MaxDelegationDepth]
	}
	_, err := rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		p.ExpireXX(ctx, cachekeys.APIKeyLineage(hash).String(), ttl)
		for _, h := range chain {
			p.ExpireXX(ctx, cachekeys.APIKeyLineage(h).String(), ttl)
		}
		return nil
	})
	if err != nil {
		obs.APIKeyLineageWriteFailuresTotal.WithLabelValues(obs.APIKeyLineageOpSlide).Inc()
	}
}

// AncestorStands reports whether the key at hash still stands: its lineage record or its credential
// record exists. Not called by Lookup until lineage enforcement ships; an ACL denial is an error, never true.
func (v *RedisAPIKeyValidator) AncestorStands(ctx context.Context, hash string) (bool, error) {
	n, err := v.rdb.Exists(ctx, cachekeys.APIKeyLineage(hash).String(), cachekeys.APIKey(hash).String()).Result()
	if err != nil {
		return false, fmt.Errorf("auth: apikey lineage exists: %w", err)
	}
	return n > 0, nil
}
