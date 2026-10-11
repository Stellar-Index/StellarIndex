package canonical

import (
	"fmt"

	"github.com/stellar/go-stellar-sdk/strkey"
)

// Strkey validators backed by the go-stellar-sdk strkey package: SEP-23 base32 + CRC-16 checksum
// verification, not just format. The SDK decoder is the single source of truth, so a strkey with
// matching prefix and length but a wrong checksum is rejected.

// IsAccountID reports whether s is a valid Stellar account / issuer
// public key (G-strkey), CRC-checked.
func IsAccountID(s string) bool {
	_, err := strkey.Decode(strkey.VersionByteAccountID, s)
	return err == nil
}

// IsContractID reports whether s is a valid Soroban contract
// address (C-strkey), CRC-checked.
func IsContractID(s string) bool {
	_, err := strkey.Decode(strkey.VersionByteContract, s)
	return err == nil
}

// IsMuxedAccount reports whether s is a valid Stellar muxed
// account address (M-strkey), CRC-checked. Added in CAP-67 / P23
// — SEP-41 transfer destinations can be M-addresses post-Whisk.
func IsMuxedAccount(s string) bool {
	_, err := strkey.Decode(strkey.VersionByteMuxedAccount, s)
	return err == nil
}

// MuxedAccountID returns the G-strkey underlying an M-strkey: the first 32
// bytes of the muxed payload are the ed25519 key. ok=false on a malformed M.
func MuxedAccountID(m string) (string, bool) {
	raw, err := strkey.Decode(strkey.VersionByteMuxedAccount, m)
	if err != nil || len(raw) < 32 {
		return "", false
	}
	g, err := strkey.Encode(strkey.VersionByteAccountID, raw[:32])
	if err != nil {
		return "", false
	}
	return g, true
}

// IsClaimableBalance reports whether s is a valid claimable-balance address (B-strkey),
// CRC-checked. CAP-67 extended SEP-41 transfer destinations to include them.
func IsClaimableBalance(s string) bool {
	_, err := strkey.Decode(strkey.VersionByteClaimableBalance, s)
	return err == nil
}

// IsLiquidityPool reports whether s is a valid liquidity-pool address (L-strkey), CRC-checked.
// CAP-67 extended SEP-41 transfer destinations to include LP addresses.
func IsLiquidityPool(s string) bool {
	_, err := strkey.Decode(strkey.VersionByteLiquidityPool, s)
	return err == nil
}

// IsAnyHolder reports whether s can plausibly hold a SEP-41 balance: G, C, M, B or L. It
// validates from/to at the /v1/sep41_transfers boundary; G alone would reject legitimate
// post-CAP-67 destinations.
func IsAnyHolder(s string) bool {
	return IsAccountID(s) || IsContractID(s) || IsMuxedAccount(s) ||
		IsClaimableBalance(s) || IsLiquidityPool(s)
}

// validateAccountID returns an error describing why s is not a
// valid account/issuer strkey, or nil on success.
func validateAccountID(s string) error {
	if !IsAccountID(s) {
		return fmt.Errorf("%w: %q is not a valid G-strkey (must be 56 chars starting with G with a valid CRC)",
			ErrInvalidStrkey, s)
	}
	return nil
}

// validateAddress accepts any valid Stellar address strkey — the full
// codomain of a Soroban `Address` (scval.AsAddressStrkey): G, C, M, B or L.
func validateAddress(s string) error {
	if !IsAnyHolder(s) {
		return fmt.Errorf("%w: %q is not a valid Stellar address strkey (G/C/M/B/L with a valid CRC)",
			ErrInvalidStrkey, s)
	}
	return nil
}

// validateContractID is the contract-address analogue of validateAccountID.
func validateContractID(s string) error {
	if !IsContractID(s) {
		return fmt.Errorf("%w: %q is not a valid C-strkey (must be 56 chars starting with C with a valid CRC)",
			ErrInvalidStrkey, s)
	}
	return nil
}
