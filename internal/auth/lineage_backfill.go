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
	return rep, nil
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
