package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/storage/redisclient"
)

// mintKeyIdentifierPattern enforces the shape this flag's own help
// text documents ("kebab-case slug, e.g. customer-acme-corp"):
// lowercase alphanumeric segments joined by single hyphens.
//
// input-validation (audit-2026-07-23): -identifier previously only
// checked non-empty — store.Create (internal/auth/store.go) does
// the same. Today the only caller is a trusted operator running this
// CLI by hand, so an out-of-shape value is low-risk; but any future
// HTTP-handler reuse of the SAME auth.RedisAPIKeyStore.Create path
// would make Identifier attacker-influenced rather than
// operator-typed. Enforcing the documented shape here — at the CLI's
// input boundary — closes the gap for this call site now, before
// that reuse happens.
var mintKeyIdentifierPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

const (
	mintKeyIdentifierMaxLen = 128
	mintKeyLabelMaxLen      = 256
)

// validateMintKeyIdentifierAndLabel checks -identifier and -label
// against the shape their own help text documents. Split out of
// mintKey to keep that function under the funlen ceiling.
func validateMintKeyIdentifierAndLabel(identifier, label string) error {
	if strings.TrimSpace(identifier) == "" {
		return errors.New("-identifier is required")
	}
	if len(identifier) > mintKeyIdentifierMaxLen {
		return fmt.Errorf("-identifier must be <= %d characters (got %d)", mintKeyIdentifierMaxLen, len(identifier))
	}
	if !mintKeyIdentifierPattern.MatchString(identifier) {
		return fmt.Errorf("-identifier %q must be a kebab-case slug (lowercase alphanumeric segments joined by single hyphens, e.g. customer-acme-corp)", identifier)
	}
	if strings.TrimSpace(label) == "" {
		return errors.New("-label is required")
	}
	if len(label) > mintKeyLabelMaxLen {
		return fmt.Errorf("-label must be <= %d characters (got %d)", mintKeyLabelMaxLen, len(label))
	}
	return nil
}

// mintKey issues an API key directly via the Redis API-key store.
// Operator-only path used to bootstrap a customer's first key
// before the self-service /v1/account/keys flow can be hit (which
// itself requires a pre-existing authenticated subject — chicken
// and egg).
//
// Usage:
//
//	stellarindex-ops mint-key \
//	  -config /etc/stellarindex.toml \
//	  -identifier customer-acme-corp \
//	  -label 'ACME Corp - production' \
//	  -tier apikey \
//	  -rate-limit-per-min 1000
//
// The plaintext key is printed to stdout ONCE — the store hashes
// it before persistence and there is no recovery path. Operators
// should pipe stdout to a secure transport (encrypted email, vault,
// 1Password) immediately. Stderr carries the public-safe record
// (KeyID, Identifier, Tier, CreatedAt) for the audit log.
func mintKey(args []string) error {
	fs := flag.NewFlagSet("mint-key", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	identifier := fs.String("identifier", "",
		"Owner identifier for the new key — kebab-case slug, e.g. customer-acme-corp (required)")
	label := fs.String("label", "",
		"Human-readable label surfaced in /v1/account/me (required)")
	tier := fs.String("tier", string(auth.TierAPIKey),
		fmt.Sprintf("Subject tier — one of %s | %s | %s. Defaults to apikey.",
			auth.TierAPIKey, auth.TierSEP10, auth.TierOperator))
	rateLimit := fs.Int("rate-limit-per-min", 0,
		"Per-key rate limit override. 0 = use the deployment default for the tier.")
	expires := fs.Duration("expires-in", 0,
		"Expiry — Go duration (e.g. 8760h for one year). 0 = never.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		return errors.New("-config is required")
	}
	if err := validateMintKeyIdentifierAndLabel(*identifier, *label); err != nil {
		return err
	}
	parsedTier := auth.Tier(*tier)
	switch parsedTier {
	case auth.TierAPIKey, auth.TierSEP10, auth.TierOperator:
		// ok
	default:
		return fmt.Errorf("-tier must be one of apikey, sep10, operator (got %q)", *tier)
	}
	// TierAnonymous is intentionally rejected — minting an "anonymous"
	// key is a category error.

	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return err
	}

	rdb := redisclient.Build(cfg.Storage)
	if rdb == nil {
		return errors.New("redis is not configured (storage.redis_addr / redis_sentinel_addrs both empty) — mint-key requires Redis")
	}
	defer func() { _ = rdb.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := rdb.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("redis ping: %w", err)
	}

	store := auth.NewRedisAPIKeyStore(rdb)

	req := auth.CreateAPIKeyRequest{
		Identifier:      *identifier,
		Label:           *label,
		Tier:            parsedTier,
		RateLimitPerMin: *rateLimit,
	}
	if *expires > 0 {
		req.ExpiresAt = time.Now().UTC().Add(*expires)
	}

	rec, plaintext, err := store.Create(ctx, req)
	if err != nil {
		return fmt.Errorf("store.Create: %w", err)
	}

	// Plaintext goes to stdout (machine-pipeable to a secure
	// transport). Audit metadata goes to stderr so a `> key.txt`
	// redirect captures only the secret.
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Audit record (safe to log):")
	fmt.Fprintf(os.Stderr, "  key_id:           %s\n", rec.KeyID)
	fmt.Fprintf(os.Stderr, "  identifier:       %s\n", rec.Identifier)
	fmt.Fprintf(os.Stderr, "  label:            %s\n", rec.Label)
	fmt.Fprintf(os.Stderr, "  tier:             %s\n", rec.Tier)
	fmt.Fprintf(os.Stderr, "  rate_limit_per_min: %d\n", rec.RateLimitPerMin)
	fmt.Fprintf(os.Stderr, "  created_at:       %s\n", rec.CreatedAt.UTC().Format(time.RFC3339))
	if !rec.ExpiresAt.IsZero() {
		fmt.Fprintf(os.Stderr, "  expires_at:       %s\n", rec.ExpiresAt.UTC().Format(time.RFC3339))
	} else {
		fmt.Fprintln(os.Stderr, "  expires_at:       never")
	}
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Plaintext key (shown ONCE — capture before this terminates):")
	fmt.Fprintln(os.Stderr, "")
	fmt.Println(plaintext)
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "The customer authenticates by sending Authorization: Bearer <key> on every request.")
	return nil
}
