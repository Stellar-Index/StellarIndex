package scval

import (
	"fmt"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// ParseContractDataKey base64-decodes an XDR LedgerKey and, when it is a
// CONTRACT_DATA key, returns the owning contract's C-strkey and the entry's
// key ScVal. This is the decoder-side reader for
// [events.Event.StateWriteKeys]: sources that attribute event bodies via
// the entries their operation wrote (Redstone's accepted-feed subset —
// each written key is ScString(feed_id)) parse the plumbed keys here and
// then apply the usual typed accessors ([AsString], [AsSymbol], …).
//
// Returns [ErrScValDecode]-wrapped errors for wire-level failures and an
// [ErrScValType]-wrapped error when the LedgerKey is not contract-data —
// callers filtering a mixed key list should skip that class, not fail.
func ParseContractDataKey(b64 string) (contractID string, key xdr.ScVal, err error) {
	var lk xdr.LedgerKey
	if uerr := xdr.SafeUnmarshalBase64(b64, &lk); uerr != nil {
		return "", xdr.ScVal{}, fmt.Errorf("%w: ledger key: %w", ErrScValDecode, uerr)
	}
	cd, ok := lk.GetContractData()
	if !ok {
		return "", xdr.ScVal{}, fmt.Errorf("%w: ledger key type %s, want CONTRACT_DATA", ErrScValType, lk.Type)
	}
	if cd.Contract.Type != xdr.ScAddressTypeScAddressTypeContract {
		return "", xdr.ScVal{}, fmt.Errorf("%w: contract-data key owner is %s, want contract address", ErrScValType, cd.Contract.Type)
	}
	cid := cd.Contract.MustContractId()
	strk, serr := strkey.Encode(strkey.VersionByteContract, cid[:])
	if serr != nil {
		return "", xdr.ScVal{}, fmt.Errorf("%w: contract strkey: %w", ErrScValDecode, serr)
	}
	return strk, cd.Key, nil
}
