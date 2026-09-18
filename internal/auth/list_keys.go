package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
)

// ─── The API-key lookup index ─────────────────────────────────────
//
// Four store methods answer a question the Redis key cannot: "which
// records does this owner hold" (ListKeysForIdentifier) and "which
// record carries this KeyID" (RevokeKeyByID, UpdateRateLimit,
// MarkEmailVerified). They used to answer it by walking `apikey:*`
// with one GET per credential in the deployment — on request paths
// any anonymously registered key can drive (F057 / K051).
//
// They now read [cachekeys.APIKeyIndex]: one HASH holding a pointer
// per KeyID, a record list per owner, and a `ready` marker. See the
// family comment in internal/cachekeys/keys.go for why it is ONE key
// (allkeys-lru eviction must take the marker with the entries).
//
// The index is only trusted while `ready` is readable. Anything else
// — the hash was evicted, a lockdown ACL does not admit the family
// yet (NOPERM), it was never built — means "walk the keyspace", which
// is always correct. A reader that finds the index absent tries to
// build it, so the walk is the transitional path, not the steady one.

const (
	keyIndexReadyField  = "ready"
	keyIndexKeyIDPrefix = "k:"
	keyIndexOwnerPrefix = "o:"

	// keyIndexBuildLockTTL bounds how long a crashed builder blocks the
	// next attempt. A FAILED build deliberately keeps the lock until it
	// expires, so a build that cannot succeed is retried once per TTL
	// and not once per request.
	keyIndexBuildLockTTL = 5 * time.Minute
	// keyIndexBuildTimeout caps one build. It runs on a context detached
	// from the triggering request so a client hanging up cannot abandon
	// a half-built index on every attempt.
	keyIndexBuildTimeout = 2 * time.Minute
	// keyIndexBuildBatch is how many index writes share one round trip.
	keyIndexBuildBatch = 500
)

// ErrKeyIndexBuildBusy is returned by [RedisAPIKeyStore.BuildKeyIndex]
// when another process holds the build lock.
var ErrKeyIndexBuildBusy = errors.New("auth: api-key index build already in progress")

// keyIndexAddLua adds one record to the index. Idempotent: re-adding
// a record rewrites the same pointer and leaves the owner list alone.
const keyIndexAddLua = `
local function index_add(idx, hash, kid, owner)
	redis.call('HSET', idx, 'k:' .. kid, hash)
	local field = 'o:' .. owner
	local cur = redis.call('HGET', idx, field)
	if not cur then
		redis.call('HSET', idx, field, hash)
	elseif not string.find(cur, hash, 1, true) then
		redis.call('HSET', idx, field, cur .. ' ' .. hash)
	end
end
`

// writeIndexedRecordScript writes a record AND its index entries as one
// atomic step, so no reader can observe a record the index does not
// know. KEYS: index, record. ARGV: body, ttl-ms (0 = none), hash,
// key_id, identifier.
//
// The index is touched FIRST on purpose. Redis does not roll a script
// back, so if the ACL denies the index family the script must die
// before the record exists — the caller then knows nothing was written
// and can fall back to the plain record write.
var writeIndexedRecordScript = redis.NewScript(keyIndexAddLua + `
index_add(KEYS[1], ARGV[3], ARGV[4], ARGV[5])
if ARGV[2] == '0' then
	redis.call('SET', KEYS[2], ARGV[1])
else
	redis.call('SET', KEYS[2], ARGV[1], 'PX', ARGV[2])
end
return 1
`)

// keyIndexAddSource is the build's per-record script. KEYS: index.
// ARGV: hash, key_id, identifier. Sent as source (EVAL), not by SHA:
// EVALSHA inside a pipeline has no NOSCRIPT fallback.
const keyIndexAddSource = keyIndexAddLua + `
index_add(KEYS[1], ARGV[1], ARGV[2], ARGV[3])
return 1
`

// removeIndexedRecordScript drops a record's index entries and, when
// ARGV[4] is '1', the record itself. KEYS: index, record. ARGV: hash,
// key_id (empty = unknown), identifier (empty = unknown), delete-record.
// The KeyID pointer is only removed while it still names this hash, so
// pruning a stale entry can never unhook a live record.
var removeIndexedRecordScript = redis.NewScript(`
if ARGV[2] ~= '' and redis.call('HGET', KEYS[1], 'k:' .. ARGV[2]) == ARGV[1] then
	redis.call('HDEL', KEYS[1], 'k:' .. ARGV[2])
end
if ARGV[3] ~= '' then
	local field = 'o:' .. ARGV[3]
	local cur = redis.call('HGET', KEYS[1], field)
	if cur then
		local keep = {}
		for h in string.gmatch(cur, '%S+') do
			if h ~= ARGV[1] then keep[#keep + 1] = h end
		end
		if #keep == 0 then
			redis.call('HDEL', KEYS[1], field)
		else
			redis.call('HSET', KEYS[1], field, table.concat(keep, ' '))
		end
	end
end
if ARGV[4] == '1' then
	redis.call('DEL', KEYS[2])
end
return 1
`)

// isRedisNoPerm reports whether err is a Redis ACL denial. A lockdown
// deployment allow-lists key patterns, and the ACL is an ansible
// surface that does not ship with the binary — so "the index family is
// not admitted yet" is an expected state, not a fault.
func isRedisNoPerm(err error) bool {
	return err != nil && strings.Contains(err.Error(), "NOPERM")
}

// writeRecord persists a credential record under its hash, indexed.
// Shared by both issuance writers ([RedisAPIKeyStore.Create] and
// [RedisAPIKeyStore.CreateWithSecret]) so the index cannot drift from
// one of them. ttl 0 means no expiry.
//
// Outcomes: indexed write succeeded; or the ACL denied the index and
// the record was written alone (readers are walking in that state, so
// the record is still found); or an error with NOTHING written. There
// is no outcome in which a trusted index is missing a record.
func (s *RedisAPIKeyStore) writeRecord(ctx context.Context, hash string, rec APIKeyRecord, body []byte, ttl time.Duration) error {
	recordKey := cachekeys.APIKey(hash).String()
	err := writeIndexedRecordScript.Run(ctx, s.rdb,
		[]string{cachekeys.APIKeyIndex().String(), recordKey},
		body, ttl.Milliseconds(), hash, rec.KeyID, rec.Identifier).Err()
	if err == nil || !isRedisNoPerm(err) {
		return err
	}
	return s.rdb.Set(ctx, recordKey, body, ttl).Err()
}

// walkAPIKeyRecords visits every decodable `apikey:*` record until
// visit returns stop. It is the ONE sanctioned walk of the credential
// keyspace (scripts/ci/lint-apikey-scan.sh bans any other): the index
// build uses it, and so does a lookup while the index is unusable.
// Records that vanish between SCAN and GET, or fail to decode, are
// skipped — one corrupt record must not fail every lookup.
func (s *RedisAPIKeyStore) walkAPIKeyRecords(ctx context.Context, visit func(hash string, rec APIKeyRecord) (stop bool, err error)) error {
	prefix := cachekeys.APIKey("").String()
	// apikey-scan-ok: the one sanctioned walk — builds the lookup index
	// and answers lookups only while that index is unusable.
	iter := s.rdb.Scan(ctx, 0, cachekeys.APIKey("*").String(), 1000).Iterator()
	for iter.Next(ctx) {
		hash := strings.TrimPrefix(iter.Val(), prefix)
		rec, found, err := s.getRecord(ctx, hash)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		stop, err := visit(hash, rec)
		if err != nil || stop {
			return err
		}
	}
	if err := iter.Err(); err != nil {
		return fmt.Errorf("redis scan: %w", err)
	}
	return nil
}

// getRecord reads one record by hash. found=false covers both "no such
// key" and "undecodable" — neither is a record a lookup can return.
func (s *RedisAPIKeyStore) getRecord(ctx context.Context, hash string) (rec APIKeyRecord, found bool, err error) {
	k := cachekeys.APIKey(hash).String()
	raw, err := s.rdb.Get(ctx, k).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return APIKeyRecord{}, false, nil
		}
		return APIKeyRecord{}, false, fmt.Errorf("redis get %s: %w", k, err)
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		return APIKeyRecord{}, false, nil //nolint:nilerr // deliberate skip of a corrupt record
	}
	return rec, true, nil
}

// indexLookup reads one index field. usable=false means the index must
// not be trusted for this call and the caller walks instead. When the
// index is merely absent it is built first, so the walk is paid once.
func (s *RedisAPIKeyStore) indexLookup(ctx context.Context, field string) (value string, found, usable bool) {
	for attempt := 0; attempt < 2; attempt++ {
		vals, err := s.rdb.HMGet(ctx, cachekeys.APIKeyIndex().String(), keyIndexReadyField, field).Result()
		if err != nil || len(vals) != 2 {
			return "", false, false
		}
		if vals[0] != nil {
			value, found = vals[1].(string)
			return value, found, true
		}
		if attempt > 0 {
			break
		}
		if _, err := s.BuildKeyIndex(ctx); err != nil {
			break
		}
	}
	return "", false, false
}

// BuildKeyIndex indexes every record that exists and then marks the
// index ready. It is the backfill for records that predate the index
// and the repair after an eviction; lookups call it themselves when
// they find the index absent, and an operator may call it directly.
//
// Safe to run at any time and concurrently with issuance: every write
// is idempotent, issuance indexes its own records atomically, and an
// entry left behind for a record deleted mid-build is a dangling
// pointer that readers skip and prune. `ready` is written only after
// a walk that completed with every index write acknowledged.
//
// Returns the number of records indexed, [ErrKeyIndexBuildBusy] when
// another process is building, or the first Redis error — including
// NOPERM while the ACL does not admit the family.
func (s *RedisAPIKeyStore) BuildKeyIndex(ctx context.Context) (int, error) {
	token, err := newLockToken()
	if err != nil {
		return 0, fmt.Errorf("auth: BuildKeyIndex: %w", err)
	}
	lock := cachekeys.APIKeyIndexBuildLock().String()
	won, err := s.rdb.SetNX(ctx, lock, token, keyIndexBuildLockTTL).Result()
	if err != nil {
		return 0, fmt.Errorf("auth: BuildKeyIndex: acquire lock: %w", err)
	}
	if !won {
		return 0, ErrKeyIndexBuildBusy
	}

	bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), keyIndexBuildTimeout)
	defer cancel()
	n, err := s.buildKeyIndexLocked(bctx)
	if err != nil {
		// Keep the lock: see keyIndexBuildLockTTL.
		return n, fmt.Errorf("auth: BuildKeyIndex: %w", err)
	}
	_ = releaseLockScript.Run(bctx, s.rdb, []string{lock}, token).Err()
	return n, nil
}

func (s *RedisAPIKeyStore) buildKeyIndexLocked(ctx context.Context) (int, error) {
	index := cachekeys.APIKeyIndex().String()
	pipe := s.rdb.Pipeline()
	indexed := 0
	err := s.walkAPIKeyRecords(ctx, func(hash string, rec APIKeyRecord) (bool, error) {
		if rec.KeyID == "" || rec.Identifier == "" {
			return false, nil // no lookup can ask for it
		}
		pipe.Eval(ctx, keyIndexAddSource, []string{index}, hash, rec.KeyID, rec.Identifier)
		indexed++
		if pipe.Len() < keyIndexBuildBatch {
			return false, nil
		}
		_, err := pipe.Exec(ctx)
		return false, err
	})
	if err != nil {
		return indexed, err
	}
	if pipe.Len() > 0 {
		if _, err := pipe.Exec(ctx); err != nil {
			return indexed, err
		}
	}
	if err := s.rdb.HSet(ctx, index, keyIndexReadyField, "1").Err(); err != nil {
		return indexed, err
	}
	return indexed, nil
}

// pruneIndex drops index entries that point at a record that is gone —
// an idle mirrored key that aged out, or a record deleted by a path
// that bypasses this store. Best-effort: a dangling entry is skipped by
// every reader, so failing to prune costs memory, never correctness.
// keyID or identifier may be "" when the reader does not know it.
func (s *RedisAPIKeyStore) pruneIndex(ctx context.Context, hash, keyID, identifier string) {
	_ = removeIndexedRecordScript.Run(ctx, s.rdb,
		[]string{cachekeys.APIKeyIndex().String(), cachekeys.APIKey(hash).String()},
		hash, keyID, identifier, "0").Err()
}

// findRecordByKeyID resolves a public KeyID to its record: through the
// index when it is usable, by walking otherwise.
func (s *RedisAPIKeyStore) findRecordByKeyID(ctx context.Context, keyID string) (hash string, rec APIKeyRecord, found bool, err error) {
	if ptr, ok, usable := s.indexLookup(ctx, keyIndexKeyIDPrefix+keyID); usable {
		if !ok {
			return "", APIKeyRecord{}, false, nil
		}
		rec, found, err = s.getRecord(ctx, ptr)
		if err != nil {
			return "", APIKeyRecord{}, false, err
		}
		if !found || rec.KeyID != keyID {
			s.pruneIndex(ctx, ptr, keyID, "")
			return "", APIKeyRecord{}, false, nil
		}
		return ptr, rec, true, nil
	}
	err = s.walkAPIKeyRecords(ctx, func(h string, r APIKeyRecord) (bool, error) {
		if r.KeyID != keyID {
			return false, nil
		}
		hash, rec, found = h, r, true
		return true, nil
	})
	return hash, rec, found, err
}

// ListKeysForIdentifier returns every [APIKeyRecord] whose
// Identifier matches. Used by:
//
//   - The admin tier-clamp path, which lowers every key an
//     account holds when its tier ceiling drops.
//   - The /v1/account/keys (GET) endpoint that lists a
//     caller's keys, and its POST quota check.
//
// Cost is proportional to the keys the identifier OWNS: one index read
// plus one GET per owned record. Only while the index is unusable does
// it walk the keyspace (see the index comment at the top of this file).
// Every returned record is re-checked against `identifier`, so a stale
// index entry can hide nothing and leak nothing.
//
// Returns nil + nil for "no matches" (the operator-facing path
// distinguishes "an identifier we don't know" from a Redis I/O
// failure).
func (s *RedisAPIKeyStore) ListKeysForIdentifier(ctx context.Context, identifier string) ([]APIKeyRecord, error) {
	if identifier == "" {
		return nil, errors.New("auth: ListKeysForIdentifier: identifier is required")
	}

	var out []APIKeyRecord
	if owned, _, usable := s.indexLookup(ctx, keyIndexOwnerPrefix+identifier); usable {
		for _, hash := range strings.Fields(owned) {
			rec, found, err := s.getRecord(ctx, hash)
			if err != nil {
				return nil, fmt.Errorf("auth: ListKeysForIdentifier: %w", err)
			}
			if !found {
				s.pruneIndex(ctx, hash, "", identifier)
				continue
			}
			if rec.Identifier == identifier {
				out = append(out, rec)
			}
		}
		return out, nil
	}

	err := s.walkAPIKeyRecords(ctx, func(_ string, rec APIKeyRecord) (bool, error) {
		if rec.Identifier == identifier {
			out = append(out, rec)
		}
		return false, nil
	})
	if err != nil {
		return nil, fmt.Errorf("auth: ListKeysForIdentifier: %w", err)
	}
	return out, nil
}

// RevokeKeyByID deletes the API key whose KeyID matches `keyID`,
// constrained to the supplied `identifier` so a caller can only
// revoke keys they own. Returns nil + nil for "not found / not
// yours" — distinguishing those would let an attacker probe key-
// id existence cross-account, and the v1 handler treats both as
// 404 anyway.
//
// The ownership check is made against the RECORD, never against the
// index: the index only says where to look.
func (s *RedisAPIKeyStore) RevokeKeyByID(ctx context.Context, identifier, keyID string) error {
	if identifier == "" {
		return errors.New("auth: RevokeKeyByID: identifier is required")
	}
	if keyID == "" {
		return errors.New("auth: RevokeKeyByID: keyID is required")
	}
	hash, rec, found, err := s.findRecordByKeyID(ctx, keyID)
	if err != nil {
		return fmt.Errorf("auth: RevokeKeyByID: %w", err)
	}
	if !found || rec.Identifier != identifier {
		// Not found / not owned. Silent — the handler renders 404 either
		// way and conflating the two prevents enumeration probes.
		return nil
	}
	recordKey := cachekeys.APIKey(hash).String()
	err = removeIndexedRecordScript.Run(ctx, s.rdb,
		[]string{cachekeys.APIKeyIndex().String(), recordKey},
		hash, keyID, identifier, "1").Err()
	if isRedisNoPerm(err) {
		// The ACL does not admit the index, so there is no entry to
		// remove — but the credential must still die.
		err = s.rdb.Del(ctx, recordKey).Err()
	}
	if err != nil {
		return fmt.Errorf("auth: RevokeKeyByID: redis del %s: %w", recordKey, err)
	}
	return nil
}
