package canonical

// Oracle-published raw symbol helpers; see docs/design/oracle-capture-totality-design.md.
//
// AssetOracleRaw holds a symbol an indexed oracle (Reflector / RedStone / Band) published that maps
// to NO canonical asset. A symbol dropped at decode time needs a code change AND a lake replay to
// recover, so the record layer is total: wire form `raw:<symbol>`, stored VERBATIM, no allow-list.
//
// Record-layer ONLY: never a Pair leg (Pair.Validate refuses it), VWAP input or supply key.
// Consumers scanning oracle_updates must exclude it via [Asset.IsMapped] or
// `asset NOT LIKE 'raw:%'`. An unmapped feed's orientation is unknown, so a raw row carries no
// Invert.

// rawSymbolMaxLen caps the verbatim symbol: Reflector/Band symbols are ScSymbol (<= 32 bytes) and
// RedStone feed_ids stay well under 64. It bounds what a buggy or malicious relayer can persist.
const rawSymbolMaxLen = 64

// validateRawSymbol enforces 1–64 bytes of printable ASCII 0x21–0x7E. `/`, `.`, `_` and `-` are
// allowed because RedStone feed_ids carry them (`SolvBTC.BBN_FUNDAMENTAL/USD`).
func validateRawSymbol(code string) error {
	l := len(code)
	if l == 0 || l > rawSymbolMaxLen {
		return errorf(ErrInvalidAsset, "raw symbol %q length %d (expected 1-%d)",
			code, l, rawSymbolMaxLen)
	}
	for i := 0; i < l; i++ {
		c := code[i]
		if c < 0x21 || c > 0x7E {
			return errorf(ErrInvalidAsset, "raw symbol %q contains non-printable-ASCII byte %q at %d",
				code, c, i)
		}
	}
	return nil
}

// NewOracleRawAsset constructs a verbatim oracle-symbol reference, returning ErrInvalidAsset if the symbol fails [validateRawSymbol].
func NewOracleRawAsset(symbol string) (Asset, error) {
	if err := validateRawSymbol(symbol); err != nil {
		return Asset{}, err
	}
	return Asset{Type: AssetOracleRaw, Code: symbol}, nil
}

// IsMapped reports whether a is anything but the record-layer-only [AssetOracleRaw]; pairs, VWAP, divergence and MEV scans must check it first.
func (a Asset) IsMapped() bool {
	return a.Type != AssetOracleRaw
}

// MapOracleSymbol resolves a bare oracle symbol to fiat, crypto, RWA, else a verbatim [AssetOracleRaw] (check !IsMapped()); it errors only if the symbol fails [validateRawSymbol].
func MapOracleSymbol(sym string) (Asset, error) {
	switch {
	case IsKnownFiat(sym):
		return NewFiatAsset(sym)
	case IsKnownCrypto(sym):
		return NewCryptoAsset(sym)
	case IsKnownRWA(sym):
		return NewRWAAsset(sym)
	}
	return NewOracleRawAsset(sym)
}
