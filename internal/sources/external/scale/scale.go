// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

// Package scale holds the shared decimal/float → scaled-integer helpers
// used by every off-chain (CEX/FX/aggregator) source under
// internal/sources/external. On-chain sources stamp amounts at per-asset
// decimals; off-chain sources normalise to a fixed integer scale (10^8
// for CEX + aggregators, 10^6 for FX). These helpers do the conversion.
package scale

import (
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
