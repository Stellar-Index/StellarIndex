// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

// Package scale holds the shared decimal/float to scaled-integer helpers used by every off-chain
// (CEX/FX/aggregator) source under internal/sources/external. On-chain sources stamp amounts at
// per-asset decimals; off-chain sources normalise to a fixed scale (10^8 for CEX and
// aggregators, 10^6 for FX).
package scale

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"
)

// DecimalStringToScaledInt parses a base-10 decimal string into an integer
// scaled to targetDecimals (e.g. "1.5" at 8 dp -> 150000000). Over-precision
// truncates (does not error); scientific notation is rejected.
func DecimalStringToScaledInt(s string, targetDecimals int) (*big.Int, error) {
	if s == "" {
		return nil, fmt.Errorf("empty decimal string")
	}
	if strings.ContainsAny(s, "eE") {
		return nil, fmt.Errorf("scientific notation %q not supported", s)
	}
	orig := s
	neg := false
	if s[0] == '-' || s[0] == '+' {
		neg = s[0] == '-'
		s = s[1:]
	}
	intPart, fracPart := s, ""
	if dot := strings.IndexByte(s, '.'); dot >= 0 {
		intPart = s[:dot]
		fracPart = s[dot+1:]
	}
	// Validate every digit before truncating: big.Int.SetString would
	// accept a second sign ("--1.5" → +1.5) and truncation would hide
	// junk past targetDecimals.
	if intPart == "" && fracPart == "" || !isDigits(intPart) || !isDigits(fracPart) {
		return nil, fmt.Errorf("not a decimal: %q", orig)
	}
	if intPart == "" {
		intPart = "0"
	}
	if len(fracPart) > targetDecimals {
		fracPart = fracPart[:targetDecimals]
	}
	for len(fracPart) < targetDecimals {
		fracPart += "0"
	}
	combined := intPart + fracPart
	v, ok := new(big.Int).SetString(combined, 10)
	if !ok {
		return nil, fmt.Errorf("not a decimal: %q", s)
	}
	if neg {
		v.Neg(v)
	}
	return v, nil
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// FloatToScaledInt converts a non-negative float to an integer scaled to decimals; negatives and
// NaN are rejected. It formats to EXACTLY decimals places so FormatFloat rounds to nearest;
// formatting more and truncating would bias every value downward.
func FloatToScaledInt(v float64, decimals int) (*big.Int, error) {
	if v < 0 || v != v {
		return nil, fmt.Errorf("bad value %v", v)
	}
	return DecimalStringToScaledInt(strconv.FormatFloat(v, 'f', decimals, 64), decimals)
}

// Pow10 returns 10^n as a *big.Int.
func Pow10(n int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

// SciDecimalStringToScaledInt is DecimalStringToScaledInt with scientific-notation tolerance: an
// "eE"-bearing input is normalised through float64 before the exact decimal parse. Some FX vendors
// emit very small inverted rates that way; venues that never do should use the strict form so a
// surprise exponent fails loudly instead of round-tripping through float64.
func SciDecimalStringToScaledInt(s string, targetDecimals int) (*big.Int, error) {
	if s != "" && strings.ContainsAny(s, "eE") {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, fmt.Errorf("not a decimal: %q", s)
		}
		// Format to EXACTLY targetDecimals places so FormatFloat rounds to nearest rather than
		// truncating toward zero (see FloatToScaledInt).
		s = strconv.FormatFloat(f, 'f', targetDecimals, 64)
	}
	return DecimalStringToScaledInt(s, targetDecimals)
}

// InvertScaled returns 10^(2*decimals) / v, ROUNDED HALF-UP, for a positive v. FX pollers use it
// to flip a vendor's "1 base = X quote" rate into the price of quote-currency in base units.
// A plain Div would truncate toward zero, a one-signed bias on every poll. It mirrors
// [redstone.reciprocalAtScale], and the two must not disagree.
func InvertScaled(v *big.Int, decimals int) *big.Int {
	return InvertScaledToDecimals(v, decimals, decimals)
}

// InvertScaledToDecimals returns round_half_up(10^(srcDecimals+dstDecimals) / v) for a positive v
// scaled at srcDecimals, re-expressed at dstDecimals. A weak-currency rate (1 EUR = 25335 VND at
// 6dp) inverts to a tiny magnitude; at the SAME scale it keeps one or two significant digits, a
// ~1.2% error above the 50bps divergence threshold, so dstDecimals widens the output.
func InvertScaledToDecimals(v *big.Int, srcDecimals, dstDecimals int) *big.Int {
	num := Pow10(srcDecimals + dstDecimals)
	// round half-up = floor((2*num + v) / (2*v)) for v > 0.
	twoNum := new(big.Int).Lsh(num, 1)
	twoNum.Add(twoNum, v)
	twoV := new(big.Int).Lsh(v, 1)
	return twoNum.Quo(twoNum, twoV)
}

// SyntheticTxHash derives a stable 64-char hex pseudo tx hash: the seed's bytes hex-encoded,
// truncated to 64 chars and right-padded with '0'. Reruns at the same vendor timestamp collide on
// purpose so the idempotent insert dedupes repeat polls. It is deliberately NOT a digest: tx_hash
// is the dedup identity of persisted rows, so changing the derivation re-inserts history.
func SyntheticTxHash(seed string) string {
	h := hex.EncodeToString([]byte(seed))
	if len(h) >= 64 {
		return h[:64]
	}
	return h + strings.Repeat("0", 64-len(h))
}

// MaxSyntheticSeedBytes is the longest seed SyntheticTxHash encodes whole (64 hex chars).
const MaxSyntheticSeedBytes = 32

// ErrSyntheticSeedTooLong is returned by StrictSyntheticTxHash for a
// seed SyntheticTxHash would truncate.
var ErrSyntheticSeedTooLong = errors.New("synthetic tx_hash seed exceeds 32 bytes")

// StrictSyntheticTxHash is SyntheticTxHash for seeds whose tail is identity (trade id, candle
// close time). Truncation would merge distinct trades-PK rows, so an over-long seed is refused.
func StrictSyntheticTxHash(seed string) (string, error) {
	if len(seed) > MaxSyntheticSeedBytes {
		return "", fmt.Errorf("%w: %q is %d bytes", ErrSyntheticSeedTooLong, seed, len(seed))
	}
	return SyntheticTxHash(seed), nil
}

// LegacyCandleGranularity is backfill-external's default; its candles keep the original seed
// identity, so re-running the default upserts the rows already written.
const LegacyCandleGranularity = time.Hour

// CandleTxHash is the synthetic tx_hash of one backfilled CEX candle (closeTs in the venue's
// native unit). Candles of different granularities close at the same instant and tx_hash is the
// only trades PK column that tells them apart, so granularity is identity. The legacy granularity
// keeps the "<SYM>-BF-<close>" seed byte-for-byte; others are a SHA-256 over a seed naming it.
func CandleTxHash(symbol string, closeTs int64, granularity time.Duration) (string, error) {
	legacy, err := StrictSyntheticTxHash(fmt.Sprintf("%s-BF-%020d", symbol, closeTs))
	if err != nil || granularity == LegacyCandleGranularity {
		return legacy, err
	}
	if granularity <= 0 {
		return "", fmt.Errorf("candle tx_hash: granularity %v must be positive", granularity)
	}
	sum := sha256.Sum256(fmt.Appendf(nil, "%s-BF-%s-%020d", symbol, granularity, closeTs))
	return hex.EncodeToString(sum[:]), nil
}

// CandleClosed reports whether a candle whose bucket ends (exclusive) at end may be emitted for a
// window ending at to. One still open at now has partial volume; one past to falls outside the window.
func CandleClosed(end, to, now time.Time) bool {
	return !end.After(to) && !end.After(now)
}
