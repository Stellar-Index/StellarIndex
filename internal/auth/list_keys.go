package auth

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
)

// The API-key lookup index. ListKeysForIdentifier (owner) and RevokeKeyByID, UpdateRateLimit, MarkEmailVerified (KeyID) cannot be answered from the Redis key, and walking `apikey:*` would put O(keyspace) cost on request paths any anonymously registered key can drive.
// They read cachekeys.APIKeyIndex: ONE HASH (a pointer per KeyID, a record list per owner, a `ready` marker; one key so allkeys-lru eviction takes the marker with the entries; see internal/cachekeys/keys.go).
// It is trusted only while `ready` is readable; otherwise readers walk the keyspace (always correct) and try to build it. A build marks `ready` only if its generation survived the walk and the API drops both at startup, so no trusted gap remains.

const (
	keyIndexReadyField  = "ready"
	keyIndexGenField    = "gen"
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

// ErrKeyIndexBuildBusy is returned by [RedisAPIKeyStore.BuildKeyIndex] when another process holds the build lock.
var ErrKeyIndexBuildBusy = errors.New("auth: api-key index build already in progress")

// errKeyIndexSuperseded means the build's generation vanished before
// `ready` was written: the hash was deleted, evicted or invalidated.
var errKeyIndexSuperseded = errors.New("api-key index changed under the build; not marked ready")

// markKeyIndexReadyScript sets `ready` only while the build's generation
// is still in the hash. KEYS: index. ARGV: generation.
var markKeyIndexReadyScript = redis.NewScript(`
if redis.call('HGET', KEYS[1], 'gen') ~= ARGV[1] then
	return 0
end
redis.call('HSET', KEYS[1], 'ready', '1')
return 1
`)

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

// writeIndexedRecordScript writes a record AND its index entries atomically so no reader sees a record the index lacks. KEYS: index, record. ARGV: body, ttl-ms (0 = none), hash, key_id, identifier.
// The index is touched FIRST: Redis does not roll back a script, so an ACL denial must kill it before the record exists, letting the caller fall back to the plain write.
var writeIndexedRecordScript = redis.NewScript(keyIndexAddLua + recordSetLua + `
index_add(KEYS[1], ARGV[3], ARGV[4], ARGV[5])
set_record(KEYS[2], ARGV[1], ARGV[2])
return 1
`)

const recordSetLua = `
local function set_record(key, body, ttl)
	if ttl == '0' then
		redis.call('SET', key, body)
	else
		redis.call('SET', key, body, 'PX', ttl)
	end
end
`

// writeDelegatedRecordScript is writeIndexedRecordScript for a delegated key: it writes nothing (returns 0) unless the
// record of every ancestor and of the minter still exists and no ancestor is being revoked.
// KEYS: index, record, the ARGV[6] lineage records (root first, minter last), revoke markers…
var writeDelegatedRecordScript = redis.NewScript(keyIndexAddLua + recordSetLua + `
local last = 2 + tonumber(ARGV[6])
for i = 3, last do
	if redis.call('EXISTS', KEYS[i]) == 0 then
		return 0
	end
end
for i = last + 1, #KEYS do
	if redis.call('EXISTS', KEYS[i]) == 1 then
		return 0
	end
end
index_add(KEYS[1], ARGV[3], ARGV[4], ARGV[5])
set_record(KEYS[2], ARGV[1], ARGV[2])
return 1
`)

// writeDelegatedUnindexedScript is the fallback when the ACL denies `apikey-index:*`. KEYS: record, lineage records…
// ARGV: body, ttl-ms.
var writeDelegatedUnindexedScript = redis.NewScript(recordSetLua + `
for i = 2, #KEYS do
	if redis.call('EXISTS', KEYS[i]) == 0 then
		return 0
	end
end
set_record(KEYS[1], ARGV[1], ARGV[2])
return 1
`)

// keyIndexAddSource is the build's per-record script. KEYS: index.
// ARGV: hash, key_id, identifier. Sent as source (EVAL), not by SHA:
// EVALSHA inside a pipeline has no NOSCRIPT fallback.
const keyIndexAddSource = keyIndexAddLua + `
index_add(KEYS[1], ARGV[1], ARGV[2], ARGV[3])
return 1
`

// removeIndexedRecordScript drops a record's index entries and, when ARGV[4] is '1', the record. KEYS: index, record. ARGV: hash, key_id, identifier (empty = unknown), delete-record.
// The KeyID pointer is removed only while it still names this hash, so pruning a stale entry cannot unhook a live record.
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

// writeRecord persists a credential record under its hash, indexed; shared by Create and CreateWithSecret so the index cannot drift. ttl 0 means no expiry.
// Outcomes: indexed write; or the ACL denied the index and the record was written alone (readers walk then); or an error with NOTHING written. A trusted index is never missing a record.
func (s *RedisAPIKeyStore) writeRecord(ctx context.Context, hash string, rec APIKeyRecord, body []byte, ttl time.Duration) error {
	recordKey := cachekeys.APIKey(hash).String()
	err := writeIndexedRecordScript.Run(ctx, s.rdb,
		[]string{cachekeys.APIKeyIndex().String(), recordKey},
		body, ttl.Milliseconds(), hash, rec.KeyID, rec.Identifier).Err()
	if isRedisNoPerm(err) {
		err = s.rdb.Set(ctx, recordKey, body, ttl).Err()
	}
	if err != nil {
		return err
	}
	writeMintedLineage(ctx, s.rdb, hash, rec)
	return nil
}

// writeDelegatedRecord is writeRecord for a key minted under the record at parentHash. The lineage check runs inside
// the atomic write, so a revoke that removed the minter or any ancestor, or marked an ancestor, first always wins.
func (s *RedisAPIKeyStore) writeDelegatedRecord(ctx context.Context, hash, parentHash string, rec APIKeyRecord, body []byte, ttl time.Duration) error {
	recordKey := cachekeys.APIKey(hash).String()
	lineage := make([]string, 0, len(rec.AncestorHashes)+1)
	for _, h := range append(slices.Clone(rec.AncestorHashes), parentHash) {
		lineage = append(lineage, cachekeys.APIKey(h).String())
	}
	keys := append([]string{cachekeys.APIKeyIndex().String(), recordKey}, lineage...)
	for _, id := range append(slices.Clone(rec.AncestorKeyIDs), rec.ParentKeyID) {
		keys = append(keys, cachekeys.APIKeyRevoking(rec.Identifier, id).String())
	}
	wrote, err := writeDelegatedRecordScript.Run(ctx, s.rdb, keys, body, ttl.Milliseconds(), hash, rec.KeyID, rec.Identifier, len(lineage)).Int()
	if isRedisNoPerm(err) {
		wrote, err = writeDelegatedUnindexedScript.Run(ctx, s.rdb, append([]string{recordKey}, lineage...), body, ttl.Milliseconds()).Int()
	}
	if err != nil {
		return err
	}
	if wrote == 0 {
		return fmt.Errorf("%w: the minting key %s or one of its ancestors was revoked before the new key was written", ErrMinterNotLive, rec.ParentKeyID)
	}
	writeMintedLineage(ctx, s.rdb, hash, rec)
	return nil
}

// walkAPIKeyRecords visits every decodable `apikey:*` record until visit returns stop: the ONE sanctioned keyspace walk (scripts/ci/lint-apikey-scan.sh bans others), used by the index build and by lookups while the index is unusable.
// Records that vanish or fail to decode are skipped, so one corrupt record cannot fail every lookup.
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

// BuildKeyIndex indexes every record then marks the index ready (only after a complete, fully acknowledged walk), returning the count, ErrKeyIndexBuildBusy, or the first Redis error; safe concurrently with issuance.
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
	n, err := s.buildKeyIndexLocked(bctx, token)
	if err != nil {
		// A superseded build can succeed on the next attempt; any other
		// failure keeps the lock (see keyIndexBuildLockTTL).
		if errors.Is(err, errKeyIndexSuperseded) {
			_ = releaseLockScript.Run(bctx, s.rdb, []string{lock}, token).Err()
		}
		return n, fmt.Errorf("auth: BuildKeyIndex: %w", err)
	}
	_ = releaseLockScript.Run(bctx, s.rdb, []string{lock}, token).Err()
	return n, nil
}

func (s *RedisAPIKeyStore) buildKeyIndexLocked(ctx context.Context, gen string) (int, error) {
	index := cachekeys.APIKeyIndex().String()
	if err := s.rdb.HSet(ctx, index, keyIndexGenField, gen).Err(); err != nil {
		return 0, err
	}
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
	marked, err := markKeyIndexReadyScript.Run(ctx, s.rdb, []string{index}, gen).Int()
	if err != nil {
		return indexed, err
	}
	if marked == 0 {
		return indexed, errKeyIndexSuperseded
	}
	return indexed, nil
}

// InvalidateKeyIndex deletes the whole index so the next lookup rebuilds it from the live records; NOPERM is not an error.
// Deleting, not just clearing `ready`, is what reclaims the pointers of records that aged out: the hash has no TTL, so
// nothing else ever frees them.
func (s *RedisAPIKeyStore) InvalidateKeyIndex(ctx context.Context) error {
	err := s.rdb.Unlink(ctx, cachekeys.APIKeyIndex().String()).Err()
	if err != nil && !isRedisNoPerm(err) {
		return fmt.Errorf("auth: InvalidateKeyIndex: %w", err)
	}
	return nil
}

// pruneIndex drops index entries pointing at a gone record (aged-out mirror, or a path bypassing this store). Best-effort: a dangling entry is skipped by readers, so a failed prune costs memory, not correctness. keyID or identifier may be "".
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

// ListKeysForIdentifier returns every APIKeyRecord whose Identifier matches, re-checking each record against identifier so a stale index entry leaks nothing; nil, nil means no matches.
func (s *RedisAPIKeyStore) ListKeysForIdentifier(ctx context.Context, identifier string) ([]APIKeyRecord, error) {
	if identifier == "" {
		return nil, errors.New("auth: ListKeysForIdentifier: identifier is required")
	}
	owned, err := s.recordsForIdentifier(ctx, identifier)
	if err != nil {
		return nil, fmt.Errorf("auth: ListKeysForIdentifier: %w", err)
	}
	var out []APIKeyRecord
	for _, o := range owned {
		out = append(out, o.rec)
	}
	return out, nil
}

// ownedRecord is one record an identifier holds, with the hash that keys it.
type ownedRecord struct {
	hash string
	rec  APIKeyRecord
}

// lineage is the chain a key minted under o carries: o's ancestors then o; nil for the zero value.
func (o ownedRecord) lineage() []string {
	if o.hash == "" {
		return nil
	}
	return append(slices.Clone(o.rec.AncestorKeyIDs), o.rec.KeyID)
}

// lineageHashes is lineage's record hashes, index for index.
func (o ownedRecord) lineageHashes() []string {
	if o.hash == "" {
		return nil
	}
	return append(slices.Clone(o.rec.AncestorHashes), o.hash)
}

// recordsForIdentifier resolves identifier's records through the index when
// it is usable and by walking otherwise; see [RedisAPIKeyStore.ListKeysForIdentifier].
func (s *RedisAPIKeyStore) recordsForIdentifier(ctx context.Context, identifier string) ([]ownedRecord, error) {
	var out []ownedRecord
	if owned, _, usable := s.indexLookup(ctx, keyIndexOwnerPrefix+identifier); usable {
		for _, hash := range strings.Fields(owned) {
			rec, found, err := s.getRecord(ctx, hash)
			if err != nil {
				return nil, err
			}
			if !found {
				s.pruneIndex(ctx, hash, "", identifier)
				continue
			}
			if rec.Identifier == identifier {
				out = append(out, ownedRecord{hash: hash, rec: rec})
			}
		}
		return out, nil
	}
	err := s.walkAPIKeyRecords(ctx, func(hash string, rec APIKeyRecord) (bool, error) {
		if rec.Identifier == identifier {
			out = append(out, ownedRecord{hash: hash, rec: rec})
		}
		return false, nil
	})
	return out, err
}

// DeleteKeysForIdentifier deletes every record identifier owns with its index entries and returns the deleted KeyIDs; a leftover record would authenticate as the next holder of the identifier.
func (s *RedisAPIKeyStore) DeleteKeysForIdentifier(ctx context.Context, identifier string) ([]string, error) {
	if identifier == "" {
		return nil, errors.New("auth: DeleteKeysForIdentifier: identifier is required")
	}
	owned, err := s.recordsForIdentifier(ctx, identifier)
	if err != nil {
		return nil, fmt.Errorf("auth: DeleteKeysForIdentifier: %w", err)
	}
	deleted := make([]string, 0, len(owned))
	for _, o := range owned {
		if err := s.removeRecord(ctx, o.hash, o.rec.KeyID, identifier); err != nil {
			return deleted, fmt.Errorf("auth: DeleteKeysForIdentifier: %w", err)
		}
		deleted = append(deleted, o.rec.KeyID)
	}
	return deleted, nil
}

// RevokeKeyByID deletes the key keyID constrained to identifier (ownership checked against the RECORD); ErrKeyNotFound covers missing and foreign keys alike (no existence oracle) and is never nil on a miss.
func (s *RedisAPIKeyStore) RevokeKeyByID(ctx context.Context, identifier, keyID string) error {
	_, err := s.RevokeKeyTree(ctx, identifier, keyID)
	return err
}

// RevokeKeyTree is [RedisAPIKeyStore.RevokeKeyByID] that also returns the KeyIDs of the delegated
// keys the cascade removed (keyID excluded), including those removed before a failure.
func (s *RedisAPIKeyStore) RevokeKeyTree(ctx context.Context, identifier, keyID string) ([]string, error) {
	if identifier == "" {
		return nil, errors.New("auth: RevokeKeyByID: identifier is required")
	}
	if keyID == "" {
		return nil, errors.New("auth: RevokeKeyByID: keyID is required")
	}
	hash, rec, found, err := s.findRecordByKeyID(ctx, keyID)
	if err != nil {
		return nil, fmt.Errorf("auth: RevokeKeyByID: %w", err)
	}
	owned := found && rec.Identifier == identifier
	if !owned {
		// Nothing is written for a key identifier does not hold. With no root record no new key can
		// join the tree (a mint needs its minter's record), so an empty listing here stays empty.
		records, err := s.recordsForIdentifier(ctx, identifier)
		if err != nil {
			return nil, fmt.Errorf("auth: RevokeKeyByID: list keys delegated from %s: %w", keyID, err)
		}
		if len(descendantsOf(records, keyID)) == 0 {
			return nil, ErrKeyNotFound
		}
	}
	// Marked before the sweep lists, so every delegated mint that writes after the listing is refused.
	if err := s.markRevoking(ctx, identifier, keyID); err != nil {
		return nil, fmt.Errorf("auth: RevokeKeyByID: %w", err)
	}
	// The sweep runs even without the root's own record (a Postgres-backed key with no mirror here),
	// so its delegated keys still die with it.
	removed, err := s.revokeDescendants(ctx, identifier, keyID)
	if err != nil {
		return removed, fmt.Errorf("auth: RevokeKeyByID: %w", err)
	}
	// The root goes last: a sweep that fails part-way leaves it live, so a key revoking itself can retry.
	if owned {
		if err := s.removeRecord(ctx, hash, keyID, identifier); err != nil {
			return removed, fmt.Errorf("auth: RevokeKeyByID: %w", err)
		}
		// An ACL that denies the marker lets a mint under the live root land during the first sweep;
		// with the root's record gone no further mint can, so this pass catches every such child.
		late, err := s.revokeDescendants(ctx, identifier, keyID)
		removed = append(removed, late...)
		if err != nil {
			return removed, fmt.Errorf("auth: RevokeKeyByID: %w", err)
		}
	}
	if !owned && len(removed) == 0 {
		return nil, ErrKeyNotFound
	}
	return removed, nil
}

// keyRevokeMarkerTTL outlasts any revoke sweep. After a failed sweep the root is still live, and
// once the marker lapses it can mint again until a retried revoke removes it.
const keyRevokeMarkerTTL = time.Hour

// keyRevokedTombstoneTTL covers a client retrying a revoke whose 204 it never received.
const keyRevokedTombstoneTTL = 24 * time.Hour

// RecordRevoked notes that revokers held authority over keyID, now revoked, so their retry still
// qualifies under [MayRevoke] once keyID's record is gone. Key ids are random and never reissued.
// An ACL that denies the note's key family writes nothing and is not an error.
func (s *RedisAPIKeyStore) RecordRevoked(ctx context.Context, identifier, keyID string, revokers []string) error {
	if len(revokers) == 0 {
		return nil
	}
	err := s.rdb.Set(ctx, cachekeys.APIKeyRevoked(identifier, keyID).String(),
		strings.Join(revokers, ","), keyRevokedTombstoneTTL).Err()
	if isRedisNoPerm(err) {
		return nil
	}
	return err
}

// RevokedBy returns the revokers [RedisAPIKeyStore.RecordRevoked] noted for keyID; nil when none
// were, the note lapsed, or the ACL denies the note's key family (no note could have been written).
func (s *RedisAPIKeyStore) RevokedBy(ctx context.Context, identifier, keyID string) ([]string, error) {
	v, err := s.rdb.Get(ctx, cachekeys.APIKeyRevoked(identifier, keyID).String()).Result()
	if errors.Is(err, redis.Nil) || isRedisNoPerm(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return strings.Split(v, ","), nil
}

// RevokersOf is who may revoke keyID by [MayRevoke]'s delegation rule, as recorded for a retry:
// actor and keyID's minters at every depth.
func RevokersOf(actor RevokeActor, keyID string, owned []APIKeyRecord) []string {
	if actor.KeyID == "" || actor.KeyID == keyID {
		return nil // an account-wide actor needs no note; a key that revoked itself cannot retry
	}
	byID := make(map[string]APIKeyRecord, len(owned))
	for _, rec := range owned {
		byID[rec.KeyID] = rec
	}
	revokers := []string{actor.KeyID}
	seen := map[string]bool{actor.KeyID: true, keyID: true}
	add := func(id string) bool {
		if id == "" || seen[id] {
			return false
		}
		seen[id] = true
		revokers = append(revokers, id)
		return true
	}
	rec, ok := byID[keyID]
	for _, id := range rec.AncestorKeyIDs {
		add(id)
	}
	for ok && add(rec.ParentKeyID) {
		rec, ok = byID[rec.ParentKeyID]
	}
	return revokers
}

// markRevoking records that keyID's tree on identifier is being revoked. An ACL that denies the
// marker also denies the delegated write script, whose fallback still checks the minter's record.
func (s *RedisAPIKeyStore) markRevoking(ctx context.Context, identifier, keyID string) error {
	err := s.rdb.Set(ctx, cachekeys.APIKeyRevoking(identifier, keyID).String(), "1", keyRevokeMarkerTTL).Err()
	if err != nil && !isRedisNoPerm(err) {
		return fmt.Errorf("mark %s revoking: %w", keyID, err)
	}
	return nil
}

// KeyRevoker removes one credential record by key id, scoped to its owner.
type KeyRevoker interface {
	RevokeKeyByID(ctx context.Context, identifier, keyID string) error
}

// RevokeKeyTree revokes keyID through r and returns the delegated KeyIDs its cascade removed, for
// the revoke's audit row; nil when r does not report them.
func RevokeKeyTree(ctx context.Context, r KeyRevoker, identifier, keyID string) ([]string, error) {
	if t, ok := r.(interface {
		RevokeKeyTree(ctx context.Context, identifier, keyID string) ([]string, error)
	}); ok {
		return t.RevokeKeyTree(ctx, identifier, keyID)
	}
	return nil, r.RevokeKeyByID(ctx, identifier, keyID)
}

// revokeDescendants deletes every key on identifier delegated from keyID at any depth, deepest
// first, and returns their KeyIDs. A failure leaves every not-yet-deleted descendant findable by
// its stored chain, so a retry finishes the job.
func (s *RedisAPIKeyStore) revokeDescendants(ctx context.Context, identifier, keyID string) ([]string, error) {
	owned, err := s.recordsForIdentifier(ctx, identifier)
	if err != nil {
		return nil, fmt.Errorf("list keys delegated from %s: %w", keyID, err)
	}
	doomed := descendantsOf(owned, keyID)
	removed := make([]string, 0, len(doomed))
	for _, o := range doomed {
		if err := s.removeRecord(ctx, o.hash, o.rec.KeyID, identifier); err != nil {
			return removed, err
		}
		removed = append(removed, o.rec.KeyID)
	}
	return removed, nil
}

// descendantsOf returns the records in owned delegated from keyID (by AncestorKeyIDs, or the
// ParentKeyID chain for records minted before the chain was stored), deepest first, keyID excluded.
func descendantsOf(owned []ownedRecord, keyID string) []ownedRecord {
	doomed := map[string]bool{keyID: true}
	inDoomed := func(id string) bool { return doomed[id] }
	for grew := true; grew; {
		grew = false
		for _, o := range owned {
			if !doomed[o.rec.KeyID] && (doomed[o.rec.ParentKeyID] || slices.ContainsFunc(o.rec.AncestorKeyIDs, inDoomed)) {
				doomed[o.rec.KeyID] = true
				grew = true
			}
		}
	}
	var out []ownedRecord
	for _, o := range owned {
		if o.rec.KeyID != keyID && doomed[o.rec.KeyID] {
			out = append(out, o)
		}
	}
	depth := delegationDepths(owned)
	slices.SortStableFunc(out, func(a, b ownedRecord) int { return cmp.Compare(depth[b.rec.KeyID], depth[a.rec.KeyID]) })
	return out
}

// delegationDepths maps each KeyID to its depth below its root. A legacy record's depth comes from
// its ParentKeyID chain, so a child is always deleted before the parent that links it to the root.
func delegationDepths(owned []ownedRecord) map[string]int {
	parent := make(map[string]string, len(owned))
	depth := make(map[string]int, len(owned))
	for _, o := range owned {
		parent[o.rec.KeyID] = o.rec.ParentKeyID
		depth[o.rec.KeyID] = len(o.rec.AncestorKeyIDs)
	}
	// Relaxation bounded by the record count, so a corrupt parent cycle cannot loop forever.
	for range owned {
		for id, p := range parent {
			if d, ok := depth[p]; ok && d+1 > depth[id] {
				depth[id] = d + 1
			}
		}
	}
	return depth
}

// removeRecord deletes one record, its index entries and its lineage record.
func (s *RedisAPIKeyStore) removeRecord(ctx context.Context, hash, keyID, identifier string) error {
	// Lineage goes first so a failure leaves the credential for a retry to find, and again after,
	// for a backfill that created it while the credential still existed.
	if err := DeleteLineageRecords(ctx, s.rdb, hash); err != nil {
		return err
	}
	if err := s.removeCredential(ctx, hash, keyID, identifier); err != nil {
		return err
	}
	return DeleteLineageRecords(ctx, s.rdb, hash)
}

// removeCredential deletes one credential record and its index entries.
func (s *RedisAPIKeyStore) removeCredential(ctx context.Context, hash, keyID, identifier string) error {
	recordKey := cachekeys.APIKey(hash).String()
	err := removeIndexedRecordScript.Run(ctx, s.rdb,
		[]string{cachekeys.APIKeyIndex().String(), recordKey},
		hash, keyID, identifier, "1").Err()
	if isRedisNoPerm(err) {
		// The ACL does not admit the index, so there is no entry to
		// remove — but the credential must still die.
		err = s.rdb.Del(ctx, recordKey).Err()
	}
	if err != nil {
		return fmt.Errorf("redis del %s: %w", recordKey, err)
	}
	return nil
}
