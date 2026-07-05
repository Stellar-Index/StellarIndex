// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

// Package scale holds the shared decimal/float → scaled-integer helpers
// used by every off-chain (CEX/FX/aggregator) source under
// internal/sources/external. On-chain sources stamp amounts at per-asset
// decimals; off-chain sources normalise to a fixed integer scale (10^8
// for CEX + aggregators, 10^6 for FX). These helpers do the conversion.
package scale

import (
	"encoding/hex"
	"fmt"
	"math/big"
	"strconv"
	"strings"
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
	neg := false
	if s[0] == '-' {
		neg = true
		s = s[1:]
	}
	intPart, fracPart := s, ""
	if dot := strings.IndexByte(s, '.'); dot >= 0 {
		intPart = s[:dot]
		fracPart = s[dot+1:]
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

// FloatToScaledInt converts a non-negative float to an integer scaled to
// decimals. Rejects negatives + NaN.
func FloatToScaledInt(v float64, decimals int) (*big.Int, error) {
	if v < 0 || v != v {
		return nil, fmt.Errorf("bad value %v", v)
	}
	return DecimalStringToScaledInt(strconv.FormatFloat(v, 'f', decimals+2, 64), decimals)
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
		s = strconv.FormatFloat(f, 'f', targetDecimals+2, 64)
	}
	return DecimalStringToScaledInt(s, targetDecimals)
}

// InvertScaled returns the multiplicative inverse of a positive
// scaled integer at the same scale: 10^(2*decimals) / v. The FX
// pollers use it to flip a vendor's "1 base = X quote" rate into our
// canonical "price of quote-currency in base units". v must be > 0
// (callers skip non-positive rates before inverting).
func InvertScaled(v *big.Int, decimals int) *big.Int {
	p := Pow10(decimals)
	return new(big.Int).Div(new(big.Int).Mul(p, p), v)
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
