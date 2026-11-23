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
	lineage, err := lineageRecordHashes(ctx, rdb)
	if err != nil {
		return fmt.Errorf("list lineage records: %w", err)
	}
	rep, err := auth.NewRedisAPIKeyStore(rdb).BackfillAncestorHashes(ctx, write, lineage...)
	if err != nil {
		return err
	}
	fmt.Print(lineageBackfillSummary(rep, write))
	if rep.LineageDenied {
		return errors.New("lineage records not checked: the Redis ACL does not admit ~apikey-lineage:* — apply the redis-sentinel role, then rerun")
	}
	return nil
}

// lineageRecordHashes lists the key hash of every `apikey-lineage:` record (owner sets skipped) with a cursor
// SCAN, 1000 keys a call, so Redis is never blocked the way KEYS would block it. Offline: one pass per run.
func lineageRecordHashes(ctx context.Context, rdb redis.Cmdable) ([]string, error) {
	var out []string
	iter := rdb.Scan(ctx, 0, cachekeys.APIKeyLineage("*").String(), 1000).Iterator()
	for iter.Next(ctx) {
		h := strings.TrimPrefix(iter.Val(), cachekeys.APIKeyLineage("").String())
		if !strings.HasPrefix(h, "owner:") {
			out = append(out, h)
		}
	}
	return out, iter.Err()
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
	return out + fmt.Sprintf("%s the lineage record of %d keys: %s\n",
		opsutil.WriteModeVerb(write, "created", "would create"), len(rep.LineageMissing), strings.Join(rep.LineageMissing, " ")) + orphans
}
