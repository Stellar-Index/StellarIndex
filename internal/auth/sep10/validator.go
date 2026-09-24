package sep10

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/txnbuild"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
)

// ReplayGuard makes every issued challenge transaction redeemable
// exactly once, so the same signed XDR can't be re-used to mint
// multiple JWTs inside its time-bound window.
//
// SEP-10 §3.4 acknowledges the replay risk and leaves implementations
// to choose their own defence. Without one, a captured signed
// challenge XDR is reusable for the full ChallengeTTL (default
// 15 m) — long enough for an attacker who steals a single signed
// XDR (e.g. via an XSS exfil on the client wallet) to mint a steady
// stream of JWTs even after the user closes the tab.
//
// Reserve records a challenge's hash at issuance; Claim spends it at
// verification and returns [auth.ErrUnauthorized] when no reservation
// is present. Claim must require presence, not absence: a store that
// can evict keys early (Redis allkeys-lru) then refuses a redemption
// instead of re-admitting a replay.
//
// Wired in production via internal/auth/sep10/redisreplay.go
// (Redis-backed). Nil ReplayGuard preserves prior behaviour for
// callers that haven't opted in. F-1224 (audit-2026-05-12).
type ReplayGuard interface {
	Reserve(ctx context.Context, txHash string, ttl time.Duration) error
	Claim(ctx context.Context, txHash string) error
}

// reservationSlack keeps a challenge's reservation alive past its
// MaxTime, absorbing clock skew between this host and Redis so the
// reservation never expires under a still-valid challenge.
const reservationSlack = time.Minute

// challengeTxHash returns the dedupe key the [ReplayGuard] stores: the
// PARSED transaction's canonical hash (SHA-256 over the network-id-
// prefixed signature payload, the same value Stellar itself identifies a
// transaction by), hex-encoded and therefore bounded at 64 bytes.
//
// CON-05 (audit-2026-07-23): this used to be the SHA-256 of the raw
// caller-supplied base64 string, which is not a canonical identity for a
// transaction — distinct strings decode to the same envelope, so one
// redemption could be replayed simply by re-spelling it. Two
// re-spellings were confirmed against this SDK to pass ReadChallengeTx +
// VerifyChallengeTxSigners unchanged (see the CON-05 regression test):
// swapping the order of the envelope's decorated signatures, and
// inserting a "\n" into the base64 (Go's decoder skips "\r"/"\n"). Each
// produced a fresh, unused dedupe key for the identical challenge, so
// the F-1224 guard could be walked straight past: capture one signed XDR
// (XSS exfil from a client wallet is the threat model that motivated the
// guard) and mint a JWT stream for the rest of the challenge window.
// Hashing the parsed transaction removes the whole class — the identity
// is now the transaction, not its spelling.
func challengeTxHash(tx *txnbuild.Transaction, networkPassphrase string) (string, error) {
	h, err := tx.HashHex(networkPassphrase)
	if err != nil {
		return "", fmt.Errorf("sep10: hash challenge tx: %w", err)
	}
	return h, nil
}

// AccountSigners is a Stellar account's current on-chain signing
// configuration: what SEP-10's threshold check verifies signatures against.
type AccountSigners struct {
	// Exists is false when the account has no live ledger entry (never
	// funded, or merged away).
	Exists       bool
	MasterWeight uint8
	MedThreshold uint8
	// Signers are the account's additional signers; the master key is not
	// among them (its weight is MasterWeight).
	Signers []Signer
}

// Signer is one additional signer of an account: a signer-key strkey and
// its weight.
type Signer struct {
	Key    string
	Weight uint32
}

// AccountLoader reads an account's current signing configuration.
type AccountLoader interface {
	LoadAccountSigners(ctx context.Context, accountID string) (AccountSigners, error)
}

// ErrAccountLoaderUnavailable is returned when no [AccountLoader] can be
// supplied: without one the medium-threshold check cannot run.
var ErrAccountLoaderUnavailable = errors.New("sep10: AccountLoader required but not configured")

// Options configures a [Validator].
type Options struct {
	// ServerSeed is the secret-seed for the server's SEP-10 signing
	// account. The corresponding G-address is what clients sign
	// against. Per SEP-10 best practice this account is dedicated to
	// SEP-10 (no other purpose) and rotated on a schedule.
	ServerSeed string

	// NetworkPassphrase is the Stellar network the challenge is
	// crafted for. Typically one of:
	//   - "Public Global Stellar Network ; September 2015" (pubnet)
	//   - "Test SDF Network ; September 2015" (testnet)
	NetworkPassphrase string

	// WebAuthDomain is the SEP-10 `web_auth_domain` — the host that
	// serves the auth endpoints. Exactly one entry. The challenge
	// transaction's manage_data op carries this so a client can
	// verify it isn't signing for the wrong domain.
	WebAuthDomain string

	// HomeDomain is the issuer's home domain (typically same as
	// WebAuthDomain). The challenge tx's first manage_data op carries
	// `<HomeDomain> auth`; SEP-10 verifies this matches.
	HomeDomain string

	// ChallengeTTL is how long a challenge stays valid (between
	// IssuedAt and ValidUntil). Default 15 minutes per SEP-10
	// recommendation. Operators tune higher for slow clients but
	// 15 m is the standard.
	ChallengeTTL time.Duration

	// JWTTTL is how long an issued JWT stays valid. Default 1 hour;
	// clients refresh by repeating the challenge → verify flow.
	JWTTTL time.Duration

	// JWTSecret is the HMAC-SHA256 key used to sign issued JWTs.
	// MUST be at least 32 bytes of entropy. Operators rotate this
	// at the same cadence as ServerSeed.
	JWTSecret []byte

	// Now overrides time.Now for tests. Production leaves this nil.
	Now func() time.Time

	// ReplayGuard, when non-nil, makes each issued challenge
	// redeemable once so a captured signed XDR cannot be re-submitted
	// inside its time-bound window. Nil = no replay defence (prior
	// behaviour). See [ReplayGuard] for production wiring.
	// F-1224 (audit-2026-05-12).
	ReplayGuard ReplayGuard

	// AccountLoader supplies the client account's signers and medium
	// threshold. Required: an account that exists on chain is
	// authenticated against them, never against its master key alone.
	AccountLoader AccountLoader
}

// Validator implements [auth.SEP10Validator] using the
// go-stellar-sdk txnbuild helpers for challenge construction +
// verification, plus a hand-rolled HMAC-SHA256 JWT for issuance.
//
// Safe for concurrent use; fields are read-only after construction.
type Validator struct {
	serverKP     *keypair.Full
	network      string
	webDomain    string
	homeDomain   string
	homeDomains  []string
	challengeTTL time.Duration
	jwtTTL       time.Duration
	jwtSecret    []byte
	now          func() time.Time
	replayGuard  ReplayGuard
	accounts     AccountLoader
}

// NewValidator constructs a [Validator] from [Options]. Returns an
// error when required fields are missing or the seed isn't a
// parseable Stellar S-strkey.
func NewValidator(opts Options) (*Validator, error) {
	if opts.ServerSeed == "" {
		return nil, errors.New("sep10: ServerSeed is required")
	}
	if opts.NetworkPassphrase == "" {
		return nil, errors.New("sep10: NetworkPassphrase is required")
	}
	if opts.WebAuthDomain == "" {
		return nil, errors.New("sep10: WebAuthDomain is required")
	}
	if opts.HomeDomain == "" {
		return nil, errors.New("sep10: HomeDomain is required")
	}
	if len(opts.JWTSecret) < 32 {
		return nil, errors.New("sep10: JWTSecret must be at least 32 bytes")
	}
	if opts.AccountLoader == nil {
		return nil, ErrAccountLoaderUnavailable
	}

	parsed, err := keypair.Parse(opts.ServerSeed)
	if err != nil {
		return nil, fmt.Errorf("sep10: parse ServerSeed: %w", err)
	}
	full, ok := parsed.(*keypair.Full)
	if !ok {
		return nil, errors.New("sep10: ServerSeed must be a secret seed (S-strkey), not a public key")
	}

	challengeTTL := opts.ChallengeTTL
	if challengeTTL <= 0 {
		challengeTTL = 15 * time.Minute
	}
	jwtTTL := opts.JWTTTL
	if jwtTTL <= 0 {
		jwtTTL = 1 * time.Hour
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	return &Validator{
		serverKP:     full,
		network:      opts.NetworkPassphrase,
		webDomain:    opts.WebAuthDomain,
		homeDomain:   opts.HomeDomain,
		homeDomains:  []string{opts.HomeDomain},
		challengeTTL: challengeTTL,
		jwtTTL:       jwtTTL,
		jwtSecret:    append([]byte(nil), opts.JWTSecret...),
		now:          now,
		replayGuard:  opts.ReplayGuard,
		accounts:     opts.AccountLoader,
	}, nil
}

// Challenge implements [auth.SEP10Validator]. Generates a SEP-10
// challenge transaction for clientAccount (a G-strkey) using the
// txnbuild SDK helper.
//
// Returns [auth.ErrUnauthorized] when clientAccount isn't a
// parseable G-strkey — better to fail loudly at challenge issuance
// than to issue a valid-looking transaction the client can never
// satisfy.
func (v *Validator) Challenge(ctx context.Context, clientAccount string) (auth.Challenge, error) {
	if _, err := keypair.ParseAddress(clientAccount); err != nil {
		return auth.Challenge{}, errors.Join(auth.ErrUnauthorized,
			fmt.Errorf("parse clientAccount: %w", err))
	}

	tx, err := txnbuild.BuildChallengeTx(
		v.serverKP.Seed(),
		clientAccount,
		v.webDomain,
		v.homeDomain,
		v.network,
		v.challengeTTL,
		nil, // no muxed-account memo
	)
	if err != nil {
		return auth.Challenge{}, fmt.Errorf("sep10: BuildChallengeTx: %w", err)
	}

	if v.replayGuard != nil {
		txHash, err := challengeTxHash(tx, v.network)
		if err != nil {
			return auth.Challenge{}, err
		}
		if err := v.replayGuard.Reserve(ctx, txHash, v.challengeTTL+reservationSlack); err != nil {
			return auth.Challenge{}, err
		}
	}

	xdr, err := tx.Base64()
	if err != nil {
		return auth.Challenge{}, fmt.Errorf("sep10: serialise challenge: %w", err)
	}

	now := v.now().UTC()
	return auth.Challenge{
		TransactionXDR:    xdr,
		NetworkPassphrase: v.network,
		IssuedAt:          now,
		ValidUntil:        now.Add(v.challengeTTL),
	}, nil
}

// Verify implements [auth.SEP10Validator]. Validates a signed
// challenge transaction: structure (per SEP-10 §3.3), server +
// client signatures, time bounds, web auth domain, and home domain.
// On success issues a JWT bearing the authenticated client account.
//
// Errors:
//   - [auth.ErrTokenMalformed] — transaction XDR doesn't parse or
//     the server's signature is missing.
//   - [auth.ErrUnauthorized] — client signature missing/wrong, a
//     signature from a key that cannot sign for the account, or signer
//     weight below the account's medium threshold.
//   - any other error — the account's signers could not be loaded.
//   - [auth.ErrTokenExpired] — challenge's time-bound window has
//     elapsed.
func (v *Validator) Verify(ctx context.Context, signedXDR string) (auth.Token, error) {
	// ReadChallengeTx parses the transaction and validates structure
	// (server source account, single sequence number, manage_data
	// ops with the web auth domain, time bounds present). Returns
	// the client account id extracted from the first manage_data op.
	challengeTx, clientAccountID, _, _, err := txnbuild.ReadChallengeTx(
		signedXDR,
		v.serverKP.Address(),
		v.network,
		v.webDomain,
		v.homeDomains,
	)
	if err != nil {
		return auth.Token{}, classifyReadChallengeError(err)
	}

	if err := v.verifyClientSignatures(ctx, signedXDR, clientAccountID); err != nil {
		return auth.Token{}, err
	}

	// Replay defence: spend the reservation [Validator.Challenge] made
	// for this challenge TRANSACTION — keyed on the parsed tx's
	// canonical hash, not on the caller's spelling of the XDR (CON-05;
	// see [challengeTxHash]). A second submission, however re-encoded,
	// or one whose reservation was evicted, returns ErrUnauthorized
	// before we issue a JWT. Claiming AFTER verifyClientSignatures
	// means bogus / unsigned XDR can never burn a real reservation.
	//
	// Falls open (continues to issue JWT) when no ReplayGuard is
	// configured, preserving prior behaviour for callers that
	// haven't opted in.
	if v.replayGuard != nil {
		txHash, hashErr := challengeTxHash(challengeTx, v.network)
		if hashErr != nil {
			// Unreachable for a transaction the SDK just parsed. Fail
			// CLOSED rather than issue a JWT we could not dedupe.
			return auth.Token{}, hashErr
		}
		if err := v.replayGuard.Claim(ctx, txHash); err != nil {
			return auth.Token{}, err
		}
	}

	now := v.now().UTC()
	expiresAt := now.Add(v.jwtTTL)
	jwt, err := v.issueJWT(clientAccountID, now, expiresAt)
	if err != nil {
		return auth.Token{}, fmt.Errorf("sep10: issue JWT: %w", err)
	}

	return auth.Token{
		JWT:       jwt,
		IssuedAt:  now,
		ExpiresAt: expiresAt,
		Subject: auth.Subject{
			Identifier: clientAccountID,
			Tier:       auth.TierSEP10,
			CreatedAt:  now,
		},
	}, nil
}

// verifyClientSignatures checks the challenge's client signatures as SEP-10
// requires: an account that exists on chain must be signed by its own
// signers with combined weight meeting its medium threshold; one that does
// not exist yet can only be proven by its master key. A failed account
// lookup fails closed.
func (v *Validator) verifyClientSignatures(ctx context.Context, signedXDR, clientAccountID string) error {
	acct, err := v.accounts.LoadAccountSigners(ctx, clientAccountID)
	if err != nil {
		return fmt.Errorf("sep10: load signers of %s: %w", clientAccountID, err)
	}
	var signersFound []string
	if acct.Exists {
		signersFound, err = txnbuild.VerifyChallengeTxThreshold(
			signedXDR, v.serverKP.Address(), v.network, v.webDomain, v.homeDomains,
			txnbuild.Threshold(acct.MedThreshold), signerSummary(clientAccountID, acct),
		)
	} else {
		signersFound, err = txnbuild.VerifyChallengeTxSigners(
			signedXDR, v.serverKP.Address(), v.network, v.webDomain, v.homeDomains,
			clientAccountID,
		)
	}
	if err != nil {
		return classifyVerifyError(err)
	}
	if len(signersFound) == 0 {
		return fmt.Errorf("%w: no signers verified", auth.ErrUnauthorized)
	}
	return nil
}

// signerSummary maps each key able to sign for the account to its weight.
// Zero-weight keys are left out, as stellar-core ignores them: a master key
// rotated to weight 0 must be an unrecognised signature, not a weight-0
// match that passes a medium threshold of 0.
func signerSummary(accountID string, acct AccountSigners) txnbuild.SignerSummary {
	summary := txnbuild.SignerSummary{}
	if acct.MasterWeight > 0 {
		summary[accountID] = int32(acct.MasterWeight)
	}
	for _, s := range acct.Signers {
		if s.Weight > 0 {
			summary[s.Key] = int32(min(s.Weight, math.MaxUint8))
		}
	}
	return summary
}

// VerifyJWT implements [auth.SEP10Validator]. Validates a JWT
// previously issued by [Verify] and returns the [auth.Subject] it
// represents. Returns [auth.ErrTokenExpired] when the exp claim has
// passed; [auth.ErrUnauthorized] for any other validation failure
// (bad signature, malformed body, wrong issuer, or an nbf that is
// still in the future).
func (v *Validator) VerifyJWT(_ context.Context, jwt string) (auth.Subject, error) {
	claims, err := v.parseJWT(jwt)
	if err != nil {
		return auth.Subject{}, err
	}
	now := v.now().Unix()
	if claims.Exp < now {
		return auth.Subject{}, auth.ErrTokenExpired
	}
	// nbf ("not before") enforcement — a token whose validity window
	// hasn't opened yet is rejected. [issueJWT] sets nbf == iat, so a
	// legitimately-issued token is immediately valid; this rejects a
	// forged/clock-skewed token that claims to become valid later.
	// Mirrors the exp check's strict, no-leeway posture. Tokens minted
	// without an nbf claim carry Nbf == 0 (omitempty) and are
	// unaffected, so this stays backward-compatible.
	if claims.Nbf > now {
		return auth.Subject{}, fmt.Errorf("%w: JWT nbf is in the future (not valid yet)",
			auth.ErrUnauthorized)
	}
	return auth.Subject{
		Identifier: claims.Sub,
		Tier:       auth.TierSEP10,
		CreatedAt:  time.Unix(claims.Iat, 0).UTC(),
	}, nil
}

// classifyVerifyError maps the SDK's error returns to our typed
// auth-error vocabulary. The SDK returns wrapped errors with
// readable messages; we pattern-match on substring rather than a
// brittle errors.Is chain because the SDK doesn't expose its
// internal sentinels.
func classifyVerifyError(err error) error {
	msg := err.Error()
	switch {
	case containsAny(msg, "transaction is not within range", "expired", "transaction has expired"):
		return errors.Join(auth.ErrTokenExpired, err)
	default:
		return errors.Join(auth.ErrUnauthorized, err)
	}
}

// classifyReadChallengeError maps ReadChallengeTx errors. Distinct
// from classifyVerifyError because ReadChallengeTx surfaces both
// genuine parse failures (→ ErrTokenMalformed) and time-bound
// expiry (→ ErrTokenExpired) — and we need to keep them distinct
// for callers to render the right HTTP status.
func classifyReadChallengeError(err error) error {
	msg := err.Error()
	wrapped := fmt.Errorf("read challenge: %w", err)
	switch {
	case containsAny(msg, "transaction is not within range", "expired", "transaction has expired"):
		return errors.Join(auth.ErrTokenExpired, wrapped)
	case containsAny(msg, "could not parse", "invalid", "malformed", "unable to unmarshal"):
		return errors.Join(auth.ErrTokenMalformed, wrapped)
	default:
		return errors.Join(auth.ErrUnauthorized, wrapped)
	}
}

// containsAny reports whether s contains any of the substrings.
// stdlib has strings.Contains but not a multi-needle variant; this
// keeps the switch above readable.
func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if len(s) >= len(n) && indexOf(s, n) >= 0 {
			return true
		}
	}
	return false
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// Compile-time check.
var _ auth.SEP10Validator = (*Validator)(nil)
