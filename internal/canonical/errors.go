package canonical

import (
	"errors"
	"fmt"
)

// errorf wraps a sentinel with a formatted message. Keeps the
// canonical package's error-construction pattern one-liner-short.
func errorf(sentinel error, format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{sentinel}, args...)...)
}

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

	// ErrInvalidAsset — an Asset struct is malformed (wrong Type tag,
	// missing required field, etc.).
	ErrInvalidAsset = errors.New("canonical: invalid asset")

	// ErrInvalidStrkey — a string did not parse as a Stellar strkey
	// (G-address, C-address, etc. per SEP-23).
	ErrInvalidStrkey = errors.New("canonical: invalid strkey")

	// ErrInvalidTrade — a Trade is missing a required field or has a
	// nonsense combination of values (e.g., zero base amount).
	ErrInvalidTrade = errors.New("canonical: invalid trade")

	// ErrInvalidOracle — an OracleUpdate is malformed (zero price,
	// out-of-range decimals, bad tx_hash, etc.).
	ErrInvalidOracle = errors.New("canonical: invalid oracle update")
)
