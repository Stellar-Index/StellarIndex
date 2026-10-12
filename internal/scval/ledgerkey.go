package scval

import (
	"fmt"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// ParseContractDataKey decodes a base64 XDR LedgerKey and, for a CONTRACT_DATA key, returns the owning C-strkey and key ScVal; wire failures wrap ErrScValDecode, a non-contract-data key wraps ErrScValType (skip, don't fail).
func ParseContractDataKey(b64 string) (contractID string, key xdr.ScVal, err error) {
	var lk xdr.LedgerKey
	if uerr := UnmarshalBase64(b64, &lk); uerr != nil {
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
