package keys

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/redisclient"
)

// LineageBackfill records each delegated API key's ancestor chain (KeyIDs and record hashes) on its Redis record,
// reporting keys whose chain cannot be resolved, creating missing lineage records and deleting orphaned ones.
// Prints counts and KeyIDs only. -write or -dry-run is required.
func LineageBackfill(args []string) error {
	cfgPath, gate, timeout, err := parseLineageBackfillFlags(args)
	if err != nil {
		return err
	}
	cfg, err := config.LoadWithEnv(cfgPath)
	if err != nil {
		return err
	}
	rdb := redisclient.Build(cfg.Storage)
	if rdb == nil {
		return errors.New("redis is not configured (storage.redis_addr / redis_sentinel_addrs both empty) — apikey-lineage-backfill requires Redis")
	}
	defer func() { _ = rdb.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("redis ping: %w", err)
	}
	write := gate.Banner()
	lineage, owners, err := lineageRecordHashes(ctx, rdb)
	if err != nil {
		return fmt.Errorf("list lineage records: %w", err)
	}
	rep, err := auth.NewRedisAPIKeyStore(rdb).BackfillAncestorHashes(ctx, write, lineage...)
	if err != nil {
		return err
	}
	if !rep.LineageDenied {
		// After the record sweep, so a set whose last records it just deleted goes in the same run.
		rep.LineageOwnerOrphans, rep.LineageOwnerOrphansDeleted, err = sweepOwnerSets(ctx, rdb, write, owners)
		if err != nil {
			return fmt.Errorf("orphaned lineage owner sets: %w", err)
		}
	}
	fmt.Print(lineageBackfillSummary(rep, write))
	if rep.LineageDenied {
		return errors.New("lineage records not checked: the Redis ACL does not admit ~apikey-lineage:* — apply the redis-sentinel role, then rerun")
	}
	return nil
}

// lineageRecordHashes lists the key hash of every `apikey-lineage:` record, and separately the key of every owner
// set, with a cursor SCAN, 1000 keys a call, so Redis is never blocked the way KEYS would block it. Offline: one
// pass per run.
func lineageRecordHashes(ctx context.Context, rdb redis.Cmdable) (hashes, owners []string, err error) {
	ownerPrefix := cachekeys.APIKeyLineage("owner:").String()
	iter := rdb.Scan(ctx, 0, cachekeys.APIKeyLineage("*").String(), 1000).Iterator()
	for iter.Next(ctx) {
		if strings.HasPrefix(iter.Val(), ownerPrefix) {
			owners = append(owners, iter.Val())
			continue
		}
		hashes = append(hashes, strings.TrimPrefix(iter.Val(), cachekeys.APIKeyLineage("").String()))
	}
	return hashes, owners, iter.Err()
}

// sweepOwnerSets counts the owner sets none of whose members has a lineage record or a credential and, with write,
// deletes each that is still so when the delete script re-checks it. A rerun finds nothing left to delete.
func sweepOwnerSets(ctx context.Context, rdb redis.Cmdable, write bool, owners []string) (orphans, deleted int, err error) {
	seen := map[string]bool{}
	for _, owner := range owners {
		if seen[owner] {
			continue
		}
		seen[owner] = true
		members, live, err := ownerSetMembers(ctx, rdb, owner)
		if err != nil {
			return orphans, deleted, err
		}
		if live || len(members) == 0 {
			continue
		}
		orphans++
		if !write {
			continue
		}
		ok, err := auth.DeleteOrphanOwnerSet(ctx, rdb, owner, members)
		if err != nil {
			return orphans, deleted, err
		}
		if ok {
			deleted++
		}
	}
	return orphans, deleted, nil
}

// ownerSetMembers SSCANs owner, 1000 members a call, and stops at the first page holding a member whose lineage
// record or credential exists (live).
func ownerSetMembers(ctx context.Context, rdb redis.Cmdable, owner string) (members []string, live bool, err error) {
	seen := map[string]bool{}
	var cursor uint64
	for {
		page, next, err := rdb.SScan(ctx, owner, cursor, "", 1000).Result()
		if err != nil {
			return nil, false, err
		}
		names := make([]string, 0, 2*len(page))
		for _, h := range page {
			names = append(names, cachekeys.APIKeyLineage(h).String(), cachekeys.APIKey(h).String())
			if !seen[h] {
				seen[h] = true
				members = append(members, h)
			}
		}
		if len(names) > 0 {
			n, err := rdb.Exists(ctx, names...).Result()
			if err != nil {
				return nil, false, err
			}
			if n > 0 {
				return nil, true, nil
			}
		}
		if next == 0 {
			return members, false, nil
		}
		cursor = next
	}
}

// parseLineageBackfillFlags parses argv without touching config or Redis.
func parseLineageBackfillFlags(args []string) (cfgPath string, gate *opsutil.WriteGate, timeout time.Duration, err error) {
	var fs *flag.FlagSet
	fs, gate = opsutil.NewMutatingFlagSet("apikey-lineage-backfill")
	cfg := fs.String("config", "", "Path to TOML config file (required)")
	to := fs.Duration("timeout", 10*time.Minute, "wall-clock budget for the keyspace walk and the rewrites")
	if err := fs.Parse(args); err != nil {
		return "", nil, 0, err
	}
	if *cfg == "" {
		return "", nil, 0, errors.New("-config is required")
	}
	if err := gate.RequireStatedMode(); err != nil {
		return "", nil, 0, err
	}
	return *cfg, gate, *to, nil
}

// lineageBackfillSummary renders rep with counts and KeyIDs only.
func lineageBackfillSummary(rep auth.LineageBackfillReport, write bool) string {
	verb := opsutil.WriteModeVerb(write, "recorded", "would record")
	out := fmt.Sprintf("scanned %d records: %d delegated, %d already complete, max depth %d\n",
		rep.Scanned, rep.Delegated, rep.Complete, rep.MaxDepth) +
		fmt.Sprintf("%s the chain of %d keys: %s\n", verb, len(rep.Resolved), strings.Join(rep.Resolved, " ")) +
		fmt.Sprintf("orphaned (an ancestor is missing, so lineage enforcement will refuse them) %d keys: %s\n",
			len(rep.Orphaned), strings.Join(rep.Orphaned, " "))
	if len(rep.Changed) > 0 {
		out += fmt.Sprintf("changed during the run, not written (rerun) %d keys: %s\n", len(rep.Changed), strings.Join(rep.Changed, " "))
	}
	if rep.LineageDenied {
		return out + "lineage records: not checked, the Redis ACL denies apikey-lineage:*\n"
	}
	orphans := fmt.Sprintf("would delete %d orphaned lineage records (no credential, no live descendant)\n", rep.LineageOrphans)
	if write {
		orphans = fmt.Sprintf("deleted %d of %d orphaned lineage records (no credential, no live descendant)\n",
			rep.LineageOrphansDeleted, rep.LineageOrphans)
	}
	owners := fmt.Sprintf("would delete %d orphaned lineage owner sets (no member has a lineage record or credential)\n",
		rep.LineageOwnerOrphans)
	if write {
		owners = fmt.Sprintf("deleted %d of %d orphaned lineage owner sets (no member has a lineage record or credential)\n",
			rep.LineageOwnerOrphansDeleted, rep.LineageOwnerOrphans)
	}
	return out + fmt.Sprintf("%s the lineage record of %d keys: %s\n",
		opsutil.WriteModeVerb(write, "created", "would create"), len(rep.LineageMissing), strings.Join(rep.LineageMissing, " ")) + orphans + owners
}
