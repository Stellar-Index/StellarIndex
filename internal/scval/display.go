// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package scval

import (
	"fmt"
	"strings"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/StellarIndex/stellar-index/internal/canonical"
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
		return fmt.Sprintf("%t", v.MustB())
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

// truncateDisplay bounds one rendered fragment.
func truncateDisplay(s string) string {
	const maxLen = 120
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "…"
}
