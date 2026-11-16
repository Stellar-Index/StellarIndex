// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package scval

import (
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// jsonMaxDepth bounds recursion: the value is contract-supplied.
const jsonMaxDepth = 8

// ErrNotJSONable reports an ScVal ToJSON refuses to render rather than render lossily.
var ErrNotJSONable = errors.New("scval: value has no lossless JSON form")

// ToJSON converts an ScVal to a json.Marshal-ready value without losing precision: 64-bit and wider
// integers become decimal strings (invariant 1), addresses strkeys, bytes hex, maps objects keyed by
// the key's scalar text. Unlike Display it never truncates and errors instead of degrading.
func ToJSON(v xdr.ScVal) (any, error) {
	return toJSON(v, 0)
}

func toJSON(v xdr.ScVal, depth int) (any, error) {
	if depth > jsonMaxDepth {
		return nil, fmt.Errorf("%w: nested deeper than %d", ErrNotJSONable, jsonMaxDepth)
	}
	switch v.Type {
	case xdr.ScValTypeScvVec:
		vec, err := AsVec(v)
		if err != nil {
			return nil, err
		}
		out := make([]any, 0, len(vec))
		for i := range vec {
			e, err := toJSON(vec[i], depth+1)
			if err != nil {
				return nil, fmt.Errorf("[%d]: %w", i, err)
			}
			out = append(out, e)
		}
		return out, nil
	case xdr.ScValTypeScvMap:
		entries, err := AsMap(v)
		if err != nil {
			return nil, err
		}
		out := make(map[string]any, len(entries))
		for i := range entries {
			k, err := jsonKey(entries[i].Key)
			if err != nil {
				return nil, fmt.Errorf("key[%d]: %w", i, err)
			}
			if _, dup := out[k]; dup {
				return nil, fmt.Errorf("%w: duplicate map key %q", ErrNotJSONable, k)
			}
			val, err := toJSON(entries[i].Val, depth+1)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			out[k] = val
		}
		return out, nil
	default:
		return jsonScalar(v)
	}
}

// jsonKey renders a map key; only the scalar kinds contracts key maps on are accepted.
func jsonKey(v xdr.ScVal) (string, error) {
	switch v.Type {
	case xdr.ScValTypeScvSymbol, xdr.ScValTypeScvString, xdr.ScValTypeScvAddress, xdr.ScValTypeScvU32:
		s, err := jsonScalar(v)
		if err != nil {
			return "", err
		}
		return fmt.Sprint(s), nil
	default:
		return "", fmt.Errorf("%w: map key of type %s", ErrNotJSONable, v.Type)
	}
}

func jsonScalar(v xdr.ScVal) (any, error) {
	switch v.Type {
	case xdr.ScValTypeScvBool:
		return AsBool(v)
	case xdr.ScValTypeScvVoid:
		return nil, nil
	case xdr.ScValTypeScvU32:
		return AsU32(v)
	case xdr.ScValTypeScvI32:
		return AsI32(v)
	case xdr.ScValTypeScvU64:
		return fmt.Sprintf("%d", v.MustU64()), nil
	case xdr.ScValTypeScvI64:
		return fmt.Sprintf("%d", v.MustI64()), nil
	case xdr.ScValTypeScvTimepoint:
		return fmt.Sprintf("%d", v.MustTimepoint()), nil
	case xdr.ScValTypeScvDuration:
		return fmt.Sprintf("%d", v.MustDuration()), nil
	case xdr.ScValTypeScvU128:
		return amountString(AsAmountFromU128(v))
	case xdr.ScValTypeScvI128:
		return amountString(AsAmountFromI128(v))
	case xdr.ScValTypeScvU256:
		return amountString(AsAmountFromU256(v))
	case xdr.ScValTypeScvI256:
		return amountString(AsAmountFromI256(v))
	case xdr.ScValTypeScvSymbol:
		return AsSymbol(v)
	case xdr.ScValTypeScvString:
		return AsText(v)
	case xdr.ScValTypeScvBytes:
		b, err := AsBytes(v)
		if err != nil {
			return nil, err
		}
		return hex.EncodeToString(b), nil
	case xdr.ScValTypeScvAddress:
		return AsAddressStrkey(v)
	default:
		return nil, fmt.Errorf("%w: %s", ErrNotJSONable, v.Type)
	}
}

func amountString(a canonical.Amount, err error) (any, error) {
	if err != nil {
		return nil, err
	}
	return a.String(), nil
}
