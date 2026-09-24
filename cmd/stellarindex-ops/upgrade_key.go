package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/storage/redisclient"
)

// upgradeKeyOpts is upgrade-key's validated argv.
type upgradeKeyOpts struct {
	cfgPath, keyID, actor, reason string
	rateLimit                     int
}

// keyRebudgeter is the slice of *auth.RedisAPIKeyStore upgrade-key uses.
type keyRebudgeter interface {
	GetByKeyID(ctx context.Context, keyID string) (auth.APIKeyRecord, error)
	UpdateRateLimit(ctx context.Context, keyID string, newRateLimitPerMin int) (auth.APIKeyRecord, error)
}

// upgradeKey lifts (or lowers) the per-minute rate-limit on an
// existing API key. Used by operators to set manual / partner
// rate-limit budgets (calls the internal
// `auth.RedisAPIKeyStore.UpdateRateLimit` path).
//
// Usage:
//
//	stellarindex-ops upgrade-key \
//	  -config /etc/stellarindex.toml \
//	  -key-id kid_515c8d94191f4e93 \
//	  -rate-limit-per-min 10000 \
//	  -reason 'partner contract 2026-09'
//
// Every change lands a "key.ratelimit.update" audit_log row (actor,
// reason, old and new budget); a change whose row cannot be written is
// rolled back.
//
// Tier suggestion (matches the /signup page tier table):
//
//	 1000  — Starter (free; what /v1/signup hands out)
//	10000  — Pro (paid)
//	50000  — Business (paid)
//	custom — Enterprise (per-deployment)
//
// Exit codes:
//
//	0 — upgraded
//	1 — error (Redis unreachable, key not found, etc.)
//	2 — usage error (missing flag)
func upgradeKey(args []string) error {
	opts, err := parseUpgradeKeyFlags(args)
	if err != nil {
		return err
	}
	cfg, err := config.LoadWithEnv(opts.cfgPath)
	if err != nil {
		return err
	}

	rdb := redisclient.Build(cfg.Storage)
	if rdb == nil {
		return errors.New("redis is not configured (storage.redis_addr / redis_sentinel_addrs both empty) — upgrade-key requires Redis")
	}
	defer func() { _ = rdb.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := rdb.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("redis ping: %w", err)
	}
	audit, closeAudit, err := openKeyAudit(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return err
	}
	defer closeAudit()

	rec, err := runUpgradeKey(ctx, auth.NewRedisAPIKeyStore(rdb), audit, opts)
	if err != nil {
		if errors.Is(err, auth.ErrKeyNotFound) {
			return fmt.Errorf("key_id %q not found in Redis (typo? or this deployment's apikey hash list is empty)", opts.keyID)
		}
		return err
	}
	printUpgradedKey(rec)
	return nil
}

// parseUpgradeKeyFlags parses and validates upgrade-key's argv without
// touching config, Redis or Postgres.
func parseUpgradeKeyFlags(args []string) (upgradeKeyOpts, error) {
	fs := flag.NewFlagSet("upgrade-key", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	keyID := fs.String("key-id", "", "KeyID of the API key to upgrade (kid_… prefix). Get this from /v1/account/me or the signup response (required)")
	rateLimit := fs.Int("rate-limit-per-min", 0,
		fmt.Sprintf("New per-minute rate-limit budget, 0..%d; 0 resets to the tier default (required)", auth.MaxKeyRateLimitPerMin))
	reason := fs.String("reason", "", "Why the budget changes; recorded in audit_log (required)")
	actor := fs.String("actor", "", "Who is changing it; recorded in audit_log. Defaults to the OS user.")
	hasRateLimit := false
	if err := fs.Parse(args); err != nil {
		return upgradeKeyOpts{}, err
	}
	// Detect whether -rate-limit-per-min was actually supplied —
	// without it the int default 0 is ambiguous with "reset".
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "rate-limit-per-min" {
			hasRateLimit = true
		}
	})

	if *cfgPath == "" {
		return upgradeKeyOpts{}, errors.New("-config is required")
	}
	if *keyID == "" {
		return upgradeKeyOpts{}, errors.New("-key-id is required")
	}
	if !hasRateLimit {
		return upgradeKeyOpts{}, errors.New("-rate-limit-per-min is required (use 0 for tier default)")
	}
	if *rateLimit < 0 {
		return upgradeKeyOpts{}, fmt.Errorf("-rate-limit-per-min must be >= 0 (got %d; use 0 for tier default)", *rateLimit)
	}
	if err := auth.ValidateKeyBounds(*rateLimit, nil); err != nil {
		return upgradeKeyOpts{}, fmt.Errorf("-rate-limit-per-min: %w", err)
	}
	if err := validateOpsKeyReason(*reason); err != nil {
		return upgradeKeyOpts{}, err
	}
	a, err := resolveOpsActor(*actor)
	if err != nil {
		return upgradeKeyOpts{}, err
	}
	return upgradeKeyOpts{cfgPath: *cfgPath, keyID: *keyID, actor: a, reason: *reason, rateLimit: *rateLimit}, nil
}

// runUpgradeKey re-budgets the key and records it in audit_log. A change
// whose audit row cannot be written is rolled back to the old budget.
func runUpgradeKey(ctx context.Context, store keyRebudgeter, audit keyAuditSink, opts upgradeKeyOpts) (auth.APIKeyRecord, error) {
	prev, err := store.GetByKeyID(ctx, opts.keyID)
	if err != nil {
		return auth.APIKeyRecord{}, err
	}
	rec, err := store.UpdateRateLimit(ctx, opts.keyID, opts.rateLimit)
	if err != nil {
		return auth.APIKeyRecord{}, err
	}
	aerr := appendKeyAudit(ctx, audit, "key.ratelimit.update", "upgrade-key", rec.KeyID, opts.actor, opts.reason, map[string]any{
		"target_identifier":       rec.Identifier,
		"from_rate_limit_per_min": prev.RateLimitPerMin,
		"to_rate_limit_per_min":   rec.RateLimitPerMin,
	})
	if aerr == nil {
		return rec, nil
	}
	if _, rerr := store.UpdateRateLimit(ctx, opts.keyID, prev.RateLimitPerMin); rerr != nil {
		return auth.APIKeyRecord{}, fmt.Errorf("audit_log append failed (%w) and restoring key %s to %d/min also failed: %w — it is at %d/min unaudited",
			aerr, rec.KeyID, prev.RateLimitPerMin, rerr, rec.RateLimitPerMin)
	}
	return auth.APIKeyRecord{}, fmt.Errorf("audit_log append failed, so key %s was restored to %d/min: %w", rec.KeyID, prev.RateLimitPerMin, aerr)
}

// printUpgradedKey writes the updated public-safe record to stderr.
func printUpgradedKey(rec auth.APIKeyRecord) {
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Updated record (recorded in audit_log as key.ratelimit.update):")
	fmt.Fprintf(os.Stderr, "  key_id:           %s\n", rec.KeyID)
	fmt.Fprintf(os.Stderr, "  identifier:       %s\n", rec.Identifier)
	fmt.Fprintf(os.Stderr, "  label:            %s\n", rec.Label)
	fmt.Fprintf(os.Stderr, "  tier:             %s\n", rec.Tier)
	fmt.Fprintf(os.Stderr, "  rate_limit_per_min: %d  ← updated\n", rec.RateLimitPerMin)
	fmt.Fprintf(os.Stderr, "  created_at:       %s\n", rec.CreatedAt.UTC().Format(time.RFC3339))
	if !rec.ExpiresAt.IsZero() {
		fmt.Fprintf(os.Stderr, "  expires_at:       %s\n", rec.ExpiresAt.UTC().Format(time.RFC3339))
	}
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "The customer's existing plaintext key keeps working — they don't need to rotate to pick up the new budget. Effective on the next request.")
}
