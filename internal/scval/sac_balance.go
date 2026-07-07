package scval

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// ErrUnknownBalanceValShape is returned by [SEP41BalanceAmount] when a
// SEP-41 / SAC balance value is neither a bare i128 nor a map carrying
// an `amount` i128 field. Callers errors.Is against it to distinguish
// an unexpected value shape from a wire-decode failure.
var ErrUnknownBalanceValShape = errors.New("scval: SEP-41 balance value is neither i128 nor map-with-amount")

// balanceKeySymbol is the base64 SCVal::Symbol("Balance") blob the
// SEP-41 contracttype enum encodes for `DataKey::Balance(addr)`.
// Pre-encoded once for cheap byte-equality on the hot decode path.
var balanceKeySymbol = MustEncodeSymbol("Balance")

// IsSEP41BalanceKey reports whether key matches the
// `DataKey::Balance(Address)` shape — a Vec of length 2 with
// Symbol("Balance") at index 0.
//
// This is the single home for the SEP-41 / SAC balance-key predicate,
// shared by the live sac_balances LedgerEntryChange observer
// (internal/sources/sac_balances) and the lake-seed reader
// (internal/storage/clickhouse) so both recognise exactly the same
// on-wire shape.
func IsSEP41BalanceKey(key xdr.ScVal) bool {
	vec, err := AsVec(key)
	if err != nil || len(vec) != 2 {
		return false
	}
	if vec[0].Type != xdr.ScValTypeScvSymbol {
		return false
	}
	// Compare the encoded symbol against our pre-encoded Balance blob —
	// byte-equality is cheap and avoids a string parse on the hot path.
	enc, err := EncodeSymbol(string(*vec[0].Sym))
	if err != nil {
		return false
	}
	return enc == balanceKeySymbol
}

// HolderFromBalanceKey extracts the holder Address (index 1) from a
// confirmed-shape Balance(Address) key, returning its strkey
// (G… / C… / M… / …).
func HolderFromBalanceKey(key xdr.ScVal) (string, error) {
	vec, err := AsVec(key)
	if err != nil || len(vec) < 2 {
		return "", errors.New("scval: balance key Vec too short")
	}
	addr, err := AsAddressStrkey(vec[1])
	if err != nil {
		return "", fmt.Errorf("scval: extract balance holder: %w", err)
	}
	return addr, nil
}

// SEP41BalanceAmount returns the i128 amount from a SEP-41 / SAC balance
// value as a *big.Int (ADR-0003 — never truncated to int64). The value
// is EITHER a bare i128 OR a map carrying an `amount` i128 field (the
// native SAC's BalanceValue shape). Returns [ErrUnknownBalanceValShape]
// for any other shape.
func SEP41BalanceAmount(val xdr.ScVal) (*big.Int, error) {
	switch val.Type {
	case xdr.ScValTypeScvI128:
		amt, err := AsAmountFromI128(val)
		if err != nil {
			return nil, err
		}
		return amt.BigInt(), nil
	case xdr.ScValTypeScvMap:
		entries, err := AsMap(val)
		if err != nil {
			return nil, err
		}
		amountVal, ok := MapField(entries, "amount")
		if !ok {
			return nil, fmt.Errorf("%w: map has no `amount` field", ErrUnknownBalanceValShape)
		}
		if amountVal.Type != xdr.ScValTypeScvI128 {
			return nil, fmt.Errorf("%w: map.amount is %s, want I128", ErrUnknownBalanceValShape, amountVal.Type)
		}
		amt, err := AsAmountFromI128(amountVal)
		if err != nil {
			return nil, err
		}
		return amt.BigInt(), nil
	default:
		return nil, fmt.Errorf("%w: val type %s", ErrUnknownBalanceValShape, val.Type)
	}
}

// ContractIDFromScAddress returns the C-strkey form of addr when it is a
// Contract-variant ScAddress, and ok=false otherwise. Unlike
// [AsAddressStrkey] (which encodes any of the five ScAddress variants),
// this narrows to the Contract case — the SAC-wrapper contract id — so
// a non-contract holder scAddress can't masquerade as a wrapper.
func ContractIDFromScAddress(addr xdr.ScAddress) (string, bool) {
	if addr.Type != xdr.ScAddressTypeScAddressTypeContract {
		return "", false
	}
	raw := addr.MustContractId()
	s, err := strkey.Encode(strkey.VersionByteContract, raw[:])
	if err != nil {
		return "", false
	}
	return s, true
}
