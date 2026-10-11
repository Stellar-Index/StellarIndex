package platform

import "errors"

// Sentinel errors for the platform stores. Wrap with %w so
// callers can errors.Is() — never compare error strings.

var (
	// ErrNotFound is returned by every Get*/Consume*/Accept*
	// method when the requested record doesn't exist (or, for
	// magic-link tokens, has already been consumed).
	ErrNotFound = errors.New("platform: not found")

	// ErrTokenExpired: the token row exists but is past expires_at; distinct from ErrNotFound so the handler can say "link expired".
	ErrTokenExpired = errors.New("platform: token expired")

	// ErrConflict signals a uniqueness violation (duplicate email, slug or key_hash).
	ErrConflict = errors.New("platform: conflict")

	// ErrLastOwner is returned when an operation would leave an
	// account with zero owner-role users — demotion + deletion
	// both refuse rather than orphan the account.
	ErrLastOwner = errors.New("platform: cannot remove the last owner")

	// ErrWebhookQuotaExceeded: the per-account cap is met; enforced atomically in one CTE-gated INSERT so concurrent requests cannot slip past.
	ErrWebhookQuotaExceeded = errors.New("platform: webhook quota exceeded")

	// ErrAPIKeyQuotaExceeded mirrors ErrWebhookQuotaExceeded for
	// the dashboard API-key store: the per-tier active-key cap is
	// enforced atomically inside the INSERT to defeat the same race.
	ErrAPIKeyQuotaExceeded = errors.New("platform: api key quota exceeded")

	// ErrPriceAlertQuotaExceeded mirrors ErrWebhookQuotaExceeded, enforced atomically inside the CreatePriceAlert INSERT.
	ErrPriceAlertQuotaExceeded = errors.New("platform: price alert quota exceeded")
)
