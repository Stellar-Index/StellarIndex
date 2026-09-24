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
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
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

// mintKeyOpts is mint-key's validated argv.
type mintKeyOpts struct {
	cfgPath, identifier, label, actor, reason string
	tier                                      auth.Tier
	scopes                                    []string
	rateLimit                                 int
	expires                                   time.Duration
}

// keyMinter is the slice of *auth.RedisAPIKeyStore mint-key uses.
type keyMinter interface {
	Create(ctx context.Context, req auth.CreateAPIKeyRequest) (auth.APIKeyRecord, string, error)
	RevokeKeyByID(ctx context.Context, identifier, keyID string) error
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
//	  -rate-limit-per-min 1000 \
//	  -reason 'onboarding ticket 1234'
//
// Every mint lands a "key.mint" audit_log row (actor, reason, tier,
// scopes, budget, expiry), the same record POST /v1/admin/keys writes; a
// mint whose row cannot be written is revoked and its plaintext never
// shown. -tier operator also needs -confirm-operator.
//
// The plaintext key is printed to stdout ONCE — the store hashes
// it before persistence and there is no recovery path. Operators
// should pipe stdout to a secure transport (encrypted email, vault,
// 1Password) immediately.
func mintKey(args []string) error {
	opts, err := parseMintKeyFlags(args)
	if err != nil {
		return err
	}
	cfg, err := config.LoadWithEnv(opts.cfgPath)
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
	audit, closeAudit, err := openKeyAudit(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return err
	}
	defer closeAudit()

	rec, plaintext, err := runMintKey(ctx, auth.NewRedisAPIKeyStore(rdb), audit, opts)
	if err != nil {
		return err
	}
	printMintedKey(rec, plaintext, opts)
	return nil
}

// parseMintKeyFlags parses and validates mint-key's argv without touching
// config, Redis or Postgres.
func parseMintKeyFlags(args []string) (mintKeyOpts, error) {
	fs := flag.NewFlagSet("mint-key", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	identifier := fs.String("identifier", "",
		"Owner identifier for the new key — kebab-case slug, e.g. customer-acme-corp (required)")
	label := fs.String("label", "",
		"Human-readable label surfaced in /v1/account/me (required)")
	tier := fs.String("tier", string(auth.TierAPIKey),
		fmt.Sprintf("Subject tier — one of %s | %s | %s. Defaults to apikey.",
			auth.TierAPIKey, auth.TierSEP10, auth.TierOperator))
	confirmOperator := fs.Bool("confirm-operator", false,
		"Required with -tier operator: the key unlocks /v1/admin/*.")
	scopes := fs.String("scopes", "",
		"Comma-separated capability scopes ("+strings.Join(platform.KnownKeyScopes(), ", ")+"). Empty = full access.")
	rateLimit := fs.Int("rate-limit-per-min", 0,
		fmt.Sprintf("Per-key rate limit override, 0..%d. 0 = use the deployment default for the tier.", auth.MaxKeyRateLimitPerMin))
	expires := fs.Duration("expires-in", 0,
		"Expiry — Go duration (e.g. 8760h for one year). 0 = never.")
	reason := fs.String("reason", "", "Why this key is minted; recorded in audit_log (required)")
	actor := fs.String("actor", "", "Who is minting; recorded in audit_log. Defaults to the OS user.")
	if err := fs.Parse(args); err != nil {
		return mintKeyOpts{}, err
	}
	if *cfgPath == "" {
		return mintKeyOpts{}, errors.New("-config is required")
	}
	if err := validateMintKeyIdentifierAndLabel(*identifier, *label); err != nil {
		return mintKeyOpts{}, err
	}
	opts := mintKeyOpts{
		cfgPath: *cfgPath, identifier: *identifier, label: *label, reason: *reason,
		tier: auth.Tier(*tier), scopes: opsutil.SplitCSV(*scopes), rateLimit: *rateLimit, expires: *expires,
	}
	if err := validateMintKeyGrant(opts, *confirmOperator); err != nil {
		return mintKeyOpts{}, err
	}
	if err := validateOpsKeyReason(opts.reason); err != nil {
		return mintKeyOpts{}, err
	}
	a, err := opsutil.ResolveActor(*actor)
	if err != nil {
		return mintKeyOpts{}, err
	}
	opts.actor = a
	return opts, nil
}

// validateMintKeyGrant checks what the key would be allowed to do: tier,
// the operator acknowledgement, the store's rate/scope bounds and expiry.
func validateMintKeyGrant(opts mintKeyOpts, confirmOperator bool) error {
	switch opts.tier {
	case auth.TierAPIKey, auth.TierSEP10:
	case auth.TierOperator:
		if !confirmOperator {
			return errors.New("-tier operator mints a key that unlocks /v1/admin/*; pass -confirm-operator to acknowledge")
		}
	default:
		// TierAnonymous is a category error for a minted key.
		return fmt.Errorf("-tier must be one of apikey, sep10, operator (got %q)", opts.tier)
	}
	if err := auth.ValidateKeyBounds(opts.rateLimit, opts.scopes); err != nil {
		return fmt.Errorf("-rate-limit-per-min / -scopes: %w", err)
	}
	if opts.expires < 0 {
		return fmt.Errorf("-expires-in must be >= 0 (got %s)", opts.expires)
	}
	return nil
}

// runMintKey creates the key and records it in audit_log. A key whose
// audit row cannot be written is revoked: no record, no credential.
func runMintKey(ctx context.Context, store keyMinter, audit keyAuditSink, opts mintKeyOpts) (auth.APIKeyRecord, string, error) {
	req := auth.CreateAPIKeyRequest{
		Identifier:      opts.identifier,
		Label:           opts.label,
		Tier:            opts.tier,
		Scopes:          opts.scopes,
		RateLimitPerMin: opts.rateLimit,
	}
	if opts.expires > 0 {
		req.ExpiresAt = time.Now().UTC().Add(opts.expires)
	}
	rec, plaintext, err := store.Create(ctx, req)
	if err != nil {
		return auth.APIKeyRecord{}, "", fmt.Errorf("store.Create: %w", err)
	}
	expiresAt := "never"
	if !rec.ExpiresAt.IsZero() {
		expiresAt = rec.ExpiresAt.UTC().Format(time.RFC3339)
	}
	aerr := appendKeyAudit(ctx, audit, "key.mint", "mint-key", rec.KeyID, opts.actor, opts.reason, map[string]any{
		"target_identifier":  rec.Identifier,
		"label":              rec.Label,
		"tier":               rec.Tier,
		"scopes":             rec.Scopes,
		"full_access":        len(rec.Scopes) == 0,
		"rate_limit_per_min": rec.RateLimitPerMin,
		"monthly_quota":      rec.MonthlyQuota,
		"expires_at":         expiresAt,
	})
	if aerr == nil {
		return rec, plaintext, nil
	}
	if rerr := store.RevokeKeyByID(ctx, rec.Identifier, rec.KeyID); rerr != nil {
		return auth.APIKeyRecord{}, "", fmt.Errorf("audit_log append failed (%w) and revoking the unaudited key %s also failed: %w — revoke it by hand",
			aerr, rec.KeyID, rerr)
	}
	return auth.APIKeyRecord{}, "", fmt.Errorf("audit_log append failed, so key %s was revoked and its plaintext discarded: %w", rec.KeyID, aerr)
}

// printMintedKey writes the public-safe record to stderr and the plaintext
// to stdout, so a `> key.txt` redirect captures only the secret.
func printMintedKey(rec auth.APIKeyRecord, plaintext string, opts mintKeyOpts) {
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Minted (recorded in audit_log as key.mint):")
	fmt.Fprintf(os.Stderr, "  key_id:           %s\n", rec.KeyID)
	fmt.Fprintf(os.Stderr, "  identifier:       %s\n", rec.Identifier)
	fmt.Fprintf(os.Stderr, "  label:            %s\n", rec.Label)
	fmt.Fprintf(os.Stderr, "  tier:             %s\n", rec.Tier)
	if len(rec.Scopes) == 0 {
		fmt.Fprintln(os.Stderr, "  scopes:           (none — full access)")
	} else {
		fmt.Fprintf(os.Stderr, "  scopes:           %s\n", strings.Join(rec.Scopes, ","))
	}
	fmt.Fprintf(os.Stderr, "  rate_limit_per_min: %d\n", rec.RateLimitPerMin)
	fmt.Fprintf(os.Stderr, "  monthly_quota:    %d\n", rec.MonthlyQuota)
	fmt.Fprintf(os.Stderr, "  created_at:       %s\n", rec.CreatedAt.UTC().Format(time.RFC3339))
	if !rec.ExpiresAt.IsZero() {
		fmt.Fprintf(os.Stderr, "  expires_at:       %s\n", rec.ExpiresAt.UTC().Format(time.RFC3339))
	} else {
		fmt.Fprintln(os.Stderr, "  expires_at:       never")
	}
	fmt.Fprintf(os.Stderr, "  actor:            %s\n", opts.actor)
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Plaintext key (shown ONCE — capture before this terminates):")
	fmt.Fprintln(os.Stderr, "")
	fmt.Println(plaintext)
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "The customer authenticates by sending Authorization: Bearer <key> on every request.")
}
