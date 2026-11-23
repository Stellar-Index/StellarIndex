// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"encoding/json"
	"errors"
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

// lineageSlideRetry is how soon a failed slide may be tried again: one attempt per key per minute per
// process while Redis refuses, never a retry per request.
const lineageSlideRetry = time.Minute

// LineageTTLMargin is how far a lineage record outlives the idle window it is slid to; it covers the
// slide throttle's lag behind the credential's per-request refresh.
const LineageTTLMargin = 2 * lineageSlideInterval

// writeLineageScript creates a lineage record only while its credential exists, expiring LineageTTLMargin
// after the credential (persistent when the credential is); an existing record is left alone. Either way
// the hash joins its owner's set, which lives at least as long as the record.
// KEYS: credential, lineage, owner set. ARGV: body, margin in ms, hash. Returns 1 when it wrote.
var writeLineageScript = redis.NewScript(`
local ttl = redis.call('PTTL', KEYS[1])
if ttl == -2 then
	return 0
end
local wrote
if ttl == -1 then
	wrote = redis.call('SET', KEYS[2], ARGV[1], 'NX')
else
	wrote = redis.call('SET', KEYS[2], ARGV[1], 'NX', 'PX', ttl + tonumber(ARGV[2]))
end
local lt = redis.call('PTTL', KEYS[2])
local ot = redis.call('PTTL', KEYS[3])
redis.call('SADD', KEYS[3], ARGV[3])
if lt == -1 then
	redis.call('PERSIST', KEYS[3])
elseif lt > 0 and (ot == -2 or (ot >= 0 and ot < lt)) then
	redis.call('PEXPIRE', KEYS[3], lt)
end
return wrote and 1 or 0
`)

// createLineage writes hash's lineage record if its credential exists and it has none.
func createLineage(ctx context.Context, rdb redis.Cmdable, hash string, rec APIKeyRecord) error {
	body, err := json.Marshal(lineageRecord{KeyID: rec.KeyID, Identifier: rec.Identifier})
	if err != nil {
		return err
	}
	return writeLineageScript.Run(ctx, rdb,
		[]string{cachekeys.APIKey(hash).String(), cachekeys.APIKeyLineage(hash).String(), cachekeys.APIKeyLineageOwner(rec.Identifier).String()},
		body, LineageTTLMargin.Milliseconds(), hash).Err()
}

// writeMintedLineage records a just-written credential's lineage; a failure leaves the mint standing
// and apikey-lineage-backfill writes the record later.
func writeMintedLineage(ctx context.Context, rdb redis.Cmdable, hash string, rec APIKeyRecord) {
	if err := createLineage(ctx, rdb, hash, rec); err != nil {
		obs.APIKeyLineageWriteFailuresTotal.WithLabelValues(obs.APIKeyLineageOpMint).Inc()
	}
}

// DeleteLineageRecords deletes the lineage records of identifier's keys at hashes, in one round trip and
// best-effort: a failure is counted, never returned, so it cannot change a revoke's or an erasure's outcome.
// A record left behind expires on its own TTL or is removed by apikey-lineage-backfill -write.
func DeleteLineageRecords(ctx context.Context, rdb redis.Cmdable, identifier string, hashes ...string) bool {
	if len(hashes) == 0 {
		return true
	}
	names := make([]string, 0, len(hashes))
	members := make([]any, 0, len(hashes))
	for _, h := range hashes {
		names = append(names, cachekeys.APIKeyLineage(h).String())
		members = append(members, h)
	}
	_, err := rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		p.Del(ctx, names...)
		p.SRem(ctx, cachekeys.APIKeyLineageOwner(identifier).String(), members...)
		return nil
	})
	if err != nil {
		obs.APIKeyLineageWriteFailuresTotal.WithLabelValues(obs.APIKeyLineageOpDelete).Inc()
		return false
	}
	return true
}

// deleteOwnedLineage is [DeleteLineageRecords] for only those of hashes whose lineage record names identifier,
// so a chain read from one account's records never deletes another account's record.
func deleteOwnedLineage(ctx context.Context, rdb redis.Cmdable, identifier string, hashes ...string) bool {
	if len(hashes) == 0 {
		return true
	}
	gets := make([]*redis.StringCmd, len(hashes))
	_, _ = rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		for i, h := range hashes {
			gets[i] = p.Get(ctx, cachekeys.APIKeyLineage(h).String())
		}
		return nil
	})
	var owned []string
	for i, h := range hashes {
		if err := gets[i].Err(); err != nil && !errors.Is(err, redis.Nil) {
			obs.APIKeyLineageWriteFailuresTotal.WithLabelValues(obs.APIKeyLineageOpDelete).Inc()
			return false
		}
		var rec lineageRecord
		if gets[i].Err() == nil && json.Unmarshal([]byte(gets[i].Val()), &rec) == nil && rec.Identifier == identifier {
			owned = append(owned, h)
		}
	}
	return DeleteLineageRecords(ctx, rdb, identifier, owned...)
}

// deleteAccountLineage deletes every lineage record identifier's owner set lists, then the set, best-effort.
// It reaches the records of keys whose credentials have already idled out.
func deleteAccountLineage(ctx context.Context, rdb redis.Cmdable, identifier string) {
	owner := cachekeys.APIKeyLineageOwner(identifier).String()
	hashes, err := rdb.SMembers(ctx, owner).Result()
	if err != nil {
		obs.APIKeyLineageWriteFailuresTotal.WithLabelValues(obs.APIKeyLineageOpDelete).Inc()
		return
	}
	if !deleteOwnedLineage(ctx, rdb, identifier, hashes...) {
		return
	}
	if err := rdb.Del(ctx, owner).Err(); err != nil {
		obs.APIKeyLineageWriteFailuresTotal.WithLabelValues(obs.APIKeyLineageOpDelete).Inc()
	}
}

// lineageSlider throttles lineage slides to one per authenticating key per lineageSlideInterval in this
// process: N API instances add at most N one-round-trip pipelines per key per interval. Zero value ready;
// a nil slider slides nothing.
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

// failed makes hash's slide due again lineageSlideRetry after the attempt at now failed.
func (l *lineageSlider) failed(hash string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if at, ok := l.last[hash]; ok && at.Equal(now) {
		l.last[hash] = now.Add(lineageSlideRetry - lineageSlideInterval)
	}
}

// slide extends the lineage records of the key at hash and of every ancestor to idle+LineageTTLMargin, in one
// pipeline, after rec authenticated. EXPIRE ... XX cannot create a record (a revoked ancestor stays gone) and
// leaves a persistent one persistent; no credential record is touched, and a failure is only counted.
func (l *lineageSlider) slide(ctx context.Context, rdb redis.Cmdable, hash string, rec APIKeyRecord, idle time.Duration, now time.Time) {
	if l == nil || idle <= 0 || !l.due(hash, now) {
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
		// A delegated chain stays on one identifier, so one owner set covers it; GT keeps a persistent set persistent.
		p.ExpireGT(ctx, cachekeys.APIKeyLineageOwner(rec.Identifier).String(), ttl)
		return nil
	})
	if err != nil {
		obs.APIKeyLineageWriteFailuresTotal.WithLabelValues(obs.APIKeyLineageOpSlide).Inc()
		l.failed(hash, now)
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
