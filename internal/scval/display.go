// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package scval

import (
	"fmt"
	"math/big"
	"strings"
	"unicode/utf8"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// displayMaxDepth caps recursion so a pathological nested value can't
// blow the stack or the response size.
const displayMaxDepth = 3

// Display renders an ScVal as a compact human-readable string for
// explorer surfaces (site audit S-016: contract event rows showed only
// topic_0; the remaining topics + data rendered nothing). It is a
// DISPLAY format, not a wire format — lossy by design (long payloads
// truncate, exotic types degrade to their type name).
func Display(v xdr.ScVal) string {
	return display(v, 0)
}

// DisplayB64 parses a base64 ScVal and renders it; empty string on
// parse failure (display surfaces degrade, never error).
func DisplayB64(b64 string) string {
	if b64 == "" {
		return ""
	}
	v, err := Parse(b64)
	if err != nil {
		return ""
	}
	return Display(v)
}

func display(v xdr.ScVal, depth int) string {
	if depth > displayMaxDepth {
		return "…"
	}
	switch v.Type {
	case xdr.ScValTypeScvVec:
		vec := v.MustVec()
		if vec == nil {
			return "[]"
		}
		parts := make([]string, 0, len(*vec))
		for _, e := range *vec {
			parts = append(parts, display(e, depth+1))
		}
		return "[" + truncateDisplay(strings.Join(parts, ", ")) + "]"
	case xdr.ScValTypeScvMap:
		m := v.MustMap()
		if m == nil {
			return "{}"
		}
		parts := make([]string, 0, len(*m))
		for _, kv := range *m {
			parts = append(parts, display(kv.Key, depth+1)+": "+display(kv.Val, depth+1))
		}
		return "{" + truncateDisplay(strings.Join(parts, ", ")) + "}"
	default:
		return displayScalar(v)
	}
}

// displayScalar renders the non-container ScVal types.
func displayScalar(v xdr.ScVal) string {
	switch v.Type {
	case xdr.ScValTypeScvBool:
		// NOT v.MustB(): ScValTypeScvBool == 0, so the ZERO-VALUE xdr.ScVal
		// reports Type==Bool while carrying a nil B pointer, and MustB
		// dereferences it. scval.AsBool guards this exact case by name;
		// Display did not, and Display is the one accessor reachable from a
		// default-constructed ScVal — MapField returns xdr.ScVal{} on a miss,
		// so logging the value on a miss path panicked the goroutine
		// (cold audit 2026-08-04).
		if v.B == nil {
			return "bool"
		}
		return fmt.Sprintf("%t", *v.B)
	case xdr.ScValTypeScvVoid:
		return "void"
	case xdr.ScValTypeScvU32:
		return fmt.Sprintf("%d", v.MustU32())
	case xdr.ScValTypeScvI32:
		return fmt.Sprintf("%d", v.MustI32())
	case xdr.ScValTypeScvU64:
		return fmt.Sprintf("%d", v.MustU64())
	case xdr.ScValTypeScvI64:
		return fmt.Sprintf("%d", v.MustI64())
	case xdr.ScValTypeScvTimepoint:
		return fmt.Sprintf("t:%d", v.MustTimepoint())
	case xdr.ScValTypeScvDuration:
		return fmt.Sprintf("d:%d", v.MustDuration())
	case xdr.ScValTypeScvU128:
		p := v.MustU128()
		return canonical.FromUInt128Parts(uint64(p.Hi), uint64(p.Lo)).String()
	case xdr.ScValTypeScvI128:
		p := v.MustI128()
		return canonical.FromInt128Parts(int64(p.Hi), uint64(p.Lo)).String()
	case xdr.ScValTypeScvU256:
		p := v.MustU256()
		return canonical.FromUInt256Parts(
			uint64(p.HiHi), uint64(p.HiLo), uint64(p.LoHi), uint64(p.LoLo)).String()
	case xdr.ScValTypeScvI256:
		p := v.MustI256()
		return int256String(p)
	case xdr.ScValTypeScvSymbol:
		return string(v.MustSym())
	case xdr.ScValTypeScvString:
		return truncateDisplay(string(v.MustStr()))
	case xdr.ScValTypeScvBytes:
		return fmt.Sprintf("bytes[%d]", len(v.MustBytes()))
	case xdr.ScValTypeScvAddress:
		addr, err := v.MustAddress().String()
		if err != nil {
			return "address"
		}
		return addr
	default:
		return strings.TrimPrefix(v.Type.String(), "ScValTypeScv")
	}
}

// int256String renders a signed 256-bit value. HiHi is the only signed
// limb; the remaining three are unsigned magnitude, so the value is
// (HiHi << 192) + (HiLo << 128) + (LoHi << 64) + LoLo in two's complement.
func int256String(p xdr.Int256Parts) string {
	r := big.NewInt(int64(p.HiHi))
	r.Lsh(r, 64)
	r.Add(r, new(big.Int).SetUint64(uint64(p.HiLo)))
	r.Lsh(r, 64)
	r.Add(r, new(big.Int).SetUint64(uint64(p.LoHi)))
	r.Lsh(r, 64)
	r.Add(r, new(big.Int).SetUint64(uint64(p.LoLo)))
	return r.String()
}

// truncateDisplay bounds one rendered fragment.
//
// Cuts on a RUNE boundary. It used to slice bytes, so a >120-byte string
// whose multi-byte rune straddled byte 120 produced invalid UTF-8 —
// encoding/json substitutes U+FFFD so the API doesn't error, but a
// consumer writing the field to a strict-UTF-8 sink rejects the row. The
// string is contract-supplied, so the input is attacker-chosen
// (cold audit 2026-08-04).
func truncateDisplay(s string) string {
	const maxLen = 120
	if len(s) <= maxLen {
		return s
	}
	cut := maxLen
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
