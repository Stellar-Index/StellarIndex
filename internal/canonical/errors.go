package canonical

import "errors"

// Error taxonomy for the canonical package.
//
// Every error returned from canonical either IS one of these
// sentinels or wraps one. Callers classify via errors.Is.
//
// See docs/discovery/engineering-standards.md §4.5.
var (
	// ErrInvalidAmount — a value could not be interpreted as a
	// valid Amount (bad string format, wrong Scan type, etc.).
	ErrInvalidAmount = errors.New("canonical: invalid amount")

	// ErrI128Overflow — a value that should fit in 128 bits
	// does not. Observation of this error in production means
	// an int64 has snuck in somewhere it shouldn't have;
	// firing SEV-1 is appropriate.
	ErrI128Overflow = errors.New("canonical: i128 overflow (this is always a bug)")

	// ErrUnknownAsset — an asset identifier did not resolve to a
	// known classic-asset pair, Soroban contract, or native XLM.
	ErrUnknownAsset = errors.New("canonical: unknown asset")

	// ErrPairMismatch — two rates expected to share the same
	// (base, quote) pair had different pairs.
	ErrPairMismatch = errors.New("canonical: pair mismatch")
)
