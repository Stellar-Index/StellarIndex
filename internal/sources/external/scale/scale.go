// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

// Package scale holds the shared decimal/float → scaled-integer helpers
// used by every off-chain (CEX/FX/aggregator) source under
// internal/sources/external. On-chain sources stamp amounts at per-asset
// decimals; off-chain sources normalise to a fixed integer scale (10^8
// for CEX + aggregators, 10^6 for FX). These helpers do the conversion.
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

// FloatToScaledInt converts a non-negative float to an integer scaled to
// decimals. Rejects negatives + NaN.
//
// Formats to EXACTLY `decimals` places so strconv.FormatFloat performs
// the rounding (round-to-nearest) — NOT `decimals+2` places then a
// truncate inside DecimalStringToScaledInt, which would drop the two
// extra fractional digits toward zero and give every value a one-signed
// downward bias. That is the same systematic bias InvertScaled's rounding
// removes (ADR-0003: no biased estimator in the money path). The callers
// here (ecb, coingecko, coinmarketcap, cryptocompare) feed only
// VWAP-excluded reference/oracle feeds, so the practical delta is <1 ulp,
// but the correction is free and consistent with the sibling rounding.
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

// SciDecimalStringToScaledInt is DecimalStringToScaledInt with
// scientific-notation tolerance: an "eE"-bearing input is normalised
// through float64 (formatted at targetDecimals+2 digits) before the
// exact decimal parse. Some FX vendors emit very small inverted rates
// in scientific notation (exchangeratesapi.io in particular); venues
// that never do should use the strict form so a surprise exponent
// fails loudly instead of round-tripping through float64.
func SciDecimalStringToScaledInt(s string, targetDecimals int) (*big.Int, error) {
	if s != "" && strings.ContainsAny(s, "eE") {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, fmt.Errorf("not a decimal: %q", s)
		}
		// Format to EXACTLY targetDecimals places, same as
		// FloatToScaledInt, so strconv.FormatFloat performs the
		// rounding (round-to-nearest) instead of DecimalStringToScaledInt
		// truncating two extra digits toward zero (a one-signed
		// downward bias, the same class ADR-0003 rejects — see
		// FloatToScaledInt's comment above).
		s = strconv.FormatFloat(f, 'f', targetDecimals, 64)
	}
	return DecimalStringToScaledInt(s, targetDecimals)
}

// InvertScaled returns the multiplicative inverse of a positive
// scaled integer at the same scale: 10^(2*decimals) / v, ROUNDED
// HALF-UP. The FX pollers use it to flip a vendor's "1 base = X
// quote" rate into our canonical "price of quote-currency in base
// units". v must be > 0 (callers skip non-positive rates before
// inverting).
//
// The rounding is the point. A plain Div truncates toward zero, so
// every inverted rate lands at or below the true value: a systematic,
// one-signed bias that accumulates across every poll of every inverted
// pair (ECB, exchangeratesapi, Chainlink). At DefaultDecimals the
// per-rate error is at most 1 ulp, but a biased estimator has no
// business in the money path (ADR-0003: exact big.Int arithmetic, never
// float, never silent truncation).
//
// The formula mirrors [redstone.reciprocalAtScale] for its Invert feeds;
// the two implementations must not disagree.
//
// InvertScaled is the srcDecimals == dstDecimals case of
// [InvertScaledToDecimals]; see that doc for why a caller inverting a
// weak-currency rate should usually widen the output scale instead.
func InvertScaled(v *big.Int, decimals int) *big.Int {
	return InvertScaledToDecimals(v, decimals, decimals)
}

// InvertScaledToDecimals returns the multiplicative inverse of a
// positive integer scaled at srcDecimals, re-expressed at dstDecimals:
// round_half_up(10^(srcDecimals+dstDecimals) / v).
//
// Precision needed for a value and precision available for its
// reciprocal are different quantities: a rate scaled at srcDecimals
// significant digits (e.g. "1 EUR = 25335 VND" at 6dp) inverts to a
// value whose true magnitude is far smaller (~0.0000395), and
// re-emitting that inverse at the SAME scale as the input rate leaves
// only one or two significant digits — a ~1.2% quantisation error for
// VND, more than double the 50bps divergence threshold this feed
// exists to police. dstDecimals lets a caller widen the
// output scale independently of the scale it inverted, without
// touching the source rate's own precision.
func InvertScaledToDecimals(v *big.Int, srcDecimals, dstDecimals int) *big.Int {
	num := Pow10(srcDecimals + dstDecimals)
	// round half-up = floor((2*num + v) / (2*v)) for v > 0.
	twoNum := new(big.Int).Lsh(num, 1)
	twoNum.Add(twoNum, v)
	twoV := new(big.Int).Lsh(v, 1)
	return twoNum.Quo(twoNum, twoV)
}

// SyntheticTxHash derives a stable 64-char lowercase-hex pseudo
// tx hash from a seed string: the seed's bytes hex-encoded, truncated
// to 64 chars and right-padded with '0'. canonical Trade/OracleUpdate
// Validate() requires a 64-char hex tx_hash; off-chain venues have no
// real one, so each poller formats a deterministic seed
// ("<VENUE>-<base>-<quote>-<zero-padded ts>") and hashes it through
// here — reruns at the same vendor timestamp collide on purpose, so
// the idempotent insert path dedupes repeat polls.
//
// NOTE: this deliberately preserves the historical truncated-hex form
// (it is NOT a digest): tx_hash is the dedup identity of persisted
// rows, so changing the derivation would re-insert history under new
// identities. Distinct venue prefixes keep cross-venue seeds from
// colliding within the shared 64-char window.
func SyntheticTxHash(seed string) string {
	h := hex.EncodeToString([]byte(seed))
	if len(h) >= 64 {
		return h[:64]
	}
	return h + strings.Repeat("0", 64-len(h))
}

// MaxSyntheticSeedBytes is the longest seed SyntheticTxHash encodes
// whole: 64 hex chars carry 32 seed bytes and the rest is dropped.
const MaxSyntheticSeedBytes = 32

// ErrSyntheticSeedTooLong is returned by StrictSyntheticTxHash for a
// seed SyntheticTxHash would truncate.
var ErrSyntheticSeedTooLong = errors.New("synthetic tx_hash seed exceeds 32 bytes")

// StrictSyntheticTxHash is SyntheticTxHash for a seed whose tail is part
// of the identity (a trade id, a candle close time). Truncation would
// drop that tail and merge distinct rows on the trades PK, so an
// over-long seed is refused instead. For a seed that fits, the result is
// byte-identical to SyntheticTxHash, so stored identities are unchanged.
func StrictSyntheticTxHash(seed string) (string, error) {
	if len(seed) > MaxSyntheticSeedBytes {
		return "", fmt.Errorf("%w: %q is %d bytes", ErrSyntheticSeedTooLong, seed, len(seed))
	}
	return SyntheticTxHash(seed), nil
}

// LegacyCandleGranularity is backfill-external's default granularity.
// Its candles keep the pre-granularity identity, so re-running the
// default over history upserts the rows already written.
const LegacyCandleGranularity = time.Hour

// CandleTxHash is the synthetic tx_hash of one backfilled CEX candle:
// symbol is the venue-normalised symbol, closeTs the bucket's close time
// in the venue's native unit (the value its Trade.Timestamp carries).
//
// Candles of different granularities close at the same instant (the last
// 1m candle of an hour and the 1h candle), and tx_hash is the only trades
// PK column that can tell them apart, so granularity is part of the
// identity. LegacyCandleGranularity keeps the "<SYM>-BF-<close>" seed
// byte-for-byte; every other granularity is a SHA-256 over a seed that
// names it. A symbol whose legacy seed would truncate is refused at every
// granularity, so representability does not depend on the flag.
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

// CandleClosed reports whether a backfilled candle whose bucket ends
// (exclusive) at end may be emitted for a window ending at to. A candle
// still open at now carries partial volume, and one ending past to would
// be stamped outside the window the caller checks for overlap.
func CandleClosed(end, to, now time.Time) bool {
	return !end.After(to) && !end.After(now)
}
