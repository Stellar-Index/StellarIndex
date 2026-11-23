// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
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

// dropGoneLineageLua: for a hash whose credential is absent, delete its lineage record if the record names the
// identifier and drop the hash from the owner set; a key whose credential exists keeps both.
// ARGV: identifier, credential prefix, lineage prefix, ...
const dropGoneLineageLua = `
local function drop(owner, h)
	if redis.call('EXISTS', ARGV[2] .. h) == 1 then
		return 0
	end
	local body = redis.call('GET', ARGV[3] .. h)
	if body then
		local ok, rec = pcall(cjson.decode, body)
		if ok and type(rec) == 'table' and rec.identifier == ARGV[1] then
			redis.call('DEL', ARGV[3] .. h)
		end
	end
	return redis.call('SREM', owner, h)
end
`

// deleteGoneLineageScript runs drop over the hashes in ARGV[4..]. KEYS: owner set.
var deleteGoneLineageScript = redis.NewScript(dropGoneLineageLua + `
for i = 4, #ARGV do
	drop(KEYS[1], ARGV[i])
end
return 0
`)

// deleteOwnerPageScript runs drop over one SSCAN page of the owner set and returns the next cursor and how many
// members it dropped, so the page read and its deletes are one atomic step. KEYS: owner set. ARGV[4]: cursor,
// ARGV[5]: page size.
var deleteOwnerPageScript = redis.NewScript(dropGoneLineageLua + `
local page = redis.call('SSCAN', KEYS[1], ARGV[4], 'COUNT', ARGV[5])
local dropped = 0
for _, h in ipairs(page[2]) do
	dropped = dropped + drop(KEYS[1], h)
end
return {page[1], dropped}
`)

// lineageDeleteBatch bounds the hashes one lineage-delete script call handles.
const lineageDeleteBatch = 100

func goneLineageArgs(identifier string) []any {
	return []any{identifier, cachekeys.APIKey("").String(), cachekeys.APIKeyLineage("").String()}
}

// deleteGoneLineage deletes the lineage records naming identifier at hashes whose credentials are gone, in
// batches and best-effort (counted, never returned). A record is never deleted while its credential exists,
// so a revoke that fails part-way leaves every still-live key's record standing.
func deleteGoneLineage(ctx context.Context, rdb redis.Cmdable, identifier string, hashes ...string) bool {
	owner := cachekeys.APIKeyLineageOwner(identifier).String()
	for batch := range slices.Chunk(hashes, lineageDeleteBatch) {
		args := goneLineageArgs(identifier)
		for _, h := range batch {
			args = append(args, h)
		}
		if err := deleteGoneLineageScript.Run(ctx, rdb, []string{owner}, args...).Err(); err != nil {
			obs.APIKeyLineageWriteFailuresTotal.WithLabelValues(obs.APIKeyLineageOpDelete).Inc()
			return false
		}
	}
	return true
}

// deleteAccountLineage deletes, as [deleteGoneLineage] does, every gone key's lineage record identifier's owner
// set lists, a bounded page per call; the set disappears with its last member. It reaches the records of keys
// whose credentials have already idled out. A pass that dropped members is repeated: SSCAN promises only the
// members present throughout, and the drops change the set under the cursor.
func deleteAccountLineage(ctx context.Context, rdb redis.Cmdable, identifier string) bool {
	owner := cachekeys.APIKeyLineageOwner(identifier).String()
	for dropped := int64(1); dropped > 0; {
		dropped = 0
		for cursor := "0"; ; {
			page, err := deleteOwnerPageScript.Run(ctx, rdb, []string{owner},
				append(goneLineageArgs(identifier), cursor, lineageDeleteBatch)...).Slice()
			next, ok := pageCursor(page)
			if err != nil || !ok {
				obs.APIKeyLineageWriteFailuresTotal.WithLabelValues(obs.APIKeyLineageOpDelete).Inc()
				return false
			}
			dropped += page[1].(int64)
			if cursor = next; cursor == "0" {
				break
			}
		}
	}
	return true
}

// pageCursor reads deleteOwnerPageScript's {cursor, dropped} reply.
func pageCursor(page []any) (string, bool) {
	if len(page) != 2 {
		return "", false
	}
	cursor, ok := page[0].(string)
	_, isInt := page[1].(int64)
	return cursor, ok && isInt
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
