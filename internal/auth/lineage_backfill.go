// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
)

// LineageBackfillReport is what [RedisAPIKeyStore.BackfillAncestorHashes] found; it names keys by KeyID only.
type LineageBackfillReport struct {
	Scanned   int      // decodable records walked
	Delegated int      // records with a ParentKeyID
	Complete  int      // delegated records already carrying their full chain of hashes
	Resolved  []string // KeyIDs whose chain was resolved (and, with write, recorded)
	Orphaned  []string // KeyIDs with a missing, foreign, malformed or over-deep ancestor chain
	Changed   []string // KeyIDs rewritten by another writer between the read and the write; rerun to cover them
	MaxDepth  int      // most ancestors any non-orphaned delegated record has

	LineageMissing []string // KeyIDs of standing keys with no `apikey-lineage:` record (with write, created)
	LineageDenied  bool     // the Redis ACL does not admit `apikey-lineage:*`, so no record was checked
}

// backfillLineageScript rewrites a record only while it still holds the body the backfill read.
// KEYS: record. ARGV: body read, body to write.
var backfillLineageScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) ~= ARGV[1] then
	return 0
end
redis.call('SET', KEYS[1], ARGV[2], 'KEEPTTL')
return 1
`)

// BackfillAncestorHashes records AncestorKeyIDs and AncestorHashes on every delegated record lacking them, resolving
// each chain from one keyspace walk (not the index). Without write it only reports. A record whose chain cannot be
// resolved is reported orphaned and left as it is.
func (s *RedisAPIKeyStore) BackfillAncestorHashes(ctx context.Context, write bool) (LineageBackfillReport, error) {
	var rep LineageBackfillReport
	byID := map[string]ownedRecord{}
	var delegated []ownedRecord
	err := s.walkAPIKeyRecords(ctx, func(hash string, rec APIKeyRecord) (bool, error) {
		rep.Scanned++
		if rec.KeyID != "" {
			byID[rec.KeyID] = ownedRecord{hash: hash, rec: rec}
		}
		if rec.ParentKeyID != "" {
			delegated = append(delegated, ownedRecord{hash: hash, rec: rec})
		}
		return false, nil
	})
	if err != nil {
		return rep, fmt.Errorf("auth: BackfillAncestorHashes: %w", err)
	}
	rep.Delegated = len(delegated)
	slices.SortFunc(delegated, func(a, b ownedRecord) int { return strings.Compare(a.rec.KeyID, b.rec.KeyID) })
	lookup := func(_ context.Context, keyID string) (string, APIKeyRecord, bool, error) {
		o, ok := byID[keyID]
		return o.hash, o.rec, ok, nil
	}

	for _, o := range delegated {
		if err := s.backfillOne(ctx, o, byID, lookup, write, &rep); err != nil {
			return rep, fmt.Errorf("auth: BackfillAncestorHashes: %s: %w", o.rec.KeyID, err)
		}
	}
	err = s.backfillLineageRecords(ctx, byID, write, &rep)
	if isRedisNoPerm(err) {
		rep.LineageMissing, rep.LineageDenied = nil, true
		return rep, nil
	}
	if err != nil {
		return rep, fmt.Errorf("auth: BackfillAncestorHashes: lineage records: %w", err)
	}
	return rep, nil
}

// lineageBackfillBatch bounds one EXISTS pipeline of the lineage-record check.
const lineageBackfillBatch = 500

// backfillLineageRecords reports every standing key with no lineage record and, with write, creates it
// (expiring LineageTTLMargin after its credential, or persistent with it). Revoked or expired keys get none.
func (s *RedisAPIKeyStore) backfillLineageRecords(ctx context.Context, byID map[string]ownedRecord, write bool, rep *LineageBackfillReport) error {
	now := s.now()
	var ids []string
	for id, o := range byID {
		if o.rec.RevokedAt.IsZero() && (o.rec.ExpiresAt.IsZero() || now.Before(o.rec.ExpiresAt)) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	for start := 0; start < len(ids); start += lineageBackfillBatch {
		missing, err := s.lineageMissing(ctx, byID, ids[start:min(start+lineageBackfillBatch, len(ids))])
		if err != nil {
			return err
		}
		rep.LineageMissing = append(rep.LineageMissing, missing...)
		for _, id := range missing {
			if !write {
				break
			}
			if err := createLineage(ctx, s.rdb, byID[id].hash, byID[id].rec); err != nil {
				return err
			}
		}
	}
	return nil
}

// lineageMissing returns the ids among chunk whose key has no lineage record, in one pipeline.
func (s *RedisAPIKeyStore) lineageMissing(ctx context.Context, byID map[string]ownedRecord, chunk []string) ([]string, error) {
	exists := make([]*redis.IntCmd, len(chunk))
	if _, err := s.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		for i, id := range chunk {
			exists[i] = p.Exists(ctx, cachekeys.APIKeyLineage(byID[id].hash).String())
		}
		return nil
	}); err != nil {
		return nil, err
	}
	var missing []string
	for i, id := range chunk {
		if exists[i].Val() == 0 {
			missing = append(missing, id)
		}
	}
	return missing, nil
}

// backfillOne resolves one delegated record's chain into rep and, with write, records it.
func (s *RedisAPIKeyStore) backfillOne(ctx context.Context, o ownedRecord, byID map[string]ownedRecord, lookup recordLookup, write bool, rep *LineageBackfillReport) error {
	ids, hashes, err := resolveLineage(ctx, o.rec, lookup)
	if errors.Is(err, ErrMinterNotLive) || errors.Is(err, ErrMintExceedsCaller) || (err == nil && !wellFormedChain(o.rec, ids, hashes, byID)) {
		rep.Orphaned = append(rep.Orphaned, o.rec.KeyID)
		return nil
	}
	if err != nil {
		return err
	}
	rep.MaxDepth = max(rep.MaxDepth, len(ids))
	if slices.Equal(ids, o.rec.AncestorKeyIDs) && slices.Equal(hashes, o.rec.AncestorHashes) {
		rep.Complete++
		return nil
	}
	rep.Resolved = append(rep.Resolved, o.rec.KeyID)
	if !write {
		return nil
	}
	wrote, err := s.recordLineage(ctx, o, ids, hashes)
	if err != nil {
		return err
	}
	if !wrote {
		rep.Changed = append(rep.Changed, o.rec.KeyID)
	}
	return nil
}

// wellFormedChain reports whether a resolved chain is one a lineage check can accept: within the depth cap,
// ending at the record's parent, and with each hash keying the live record of its ancestor id.
func wellFormedChain(rec APIKeyRecord, ids, hashes []string, byID map[string]ownedRecord) bool {
	if len(ids) == 0 || len(ids) > MaxDelegationDepth || len(ids) != len(hashes) || ids[len(ids)-1] != rec.ParentKeyID {
		return false
	}
	for i, id := range ids {
		if o, ok := byID[id]; !ok || o.hash != hashes[i] || o.rec.Identifier != rec.Identifier {
			return false
		}
	}
	return true
}

// recordLineage rewrites o's record with the resolved chain, keeping its TTL; false means the record changed
// (or vanished) since the walk and nothing was written.
func (s *RedisAPIKeyStore) recordLineage(ctx context.Context, o ownedRecord, ids, hashes []string) (bool, error) {
	key := cachekeys.APIKey(o.hash).String()
	raw, err := s.rdb.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var cur APIKeyRecord
	if err := json.Unmarshal([]byte(raw), &cur); err != nil || cur.KeyID != o.rec.KeyID || cur.ParentKeyID != o.rec.ParentKeyID {
		return false, nil //nolint:nilerr // a record that no longer decodes as the one resolved is reported changed
	}
	cur.AncestorKeyIDs, cur.AncestorHashes = ids, hashes
	body, err := json.Marshal(cur)
	if err != nil {
		return false, err
	}
	n, err := backfillLineageScript.Run(ctx, s.rdb, []string{key}, raw, body).Int()
	return n == 1, err
}
