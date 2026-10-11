package canonical

// Oracle-published raw symbol helpers; see docs/design/oracle-capture-totality-design.md.
//
// AssetOracleRaw holds a symbol an indexed oracle (Reflector / RedStone / Band) published that
// maps to NO canonical asset. The record layer must be total and only the interpretation layer
// selective: a symbol dropped at decode time needs a code change AND a lake replay to recover.
//
// Wire form `raw:<symbol>`, stored VERBATIM (source scoping comes from oracle_updates.source),
// with deliberately no allow-list.
//
// Record-layer ONLY: never a Pair leg (Pair.Validate refuses it), VWAP input or supply key.
// Consumers scanning oracle_updates without keying by canonical asset must exclude it via
// [Asset.IsMapped] or `asset NOT LIKE 'raw:%'`. An unmapped feed's orientation is unknown, so
// a raw row carries no Invert.

// rawSymbolMaxLen caps the verbatim symbol. Reflector/Band symbols
// are ScSymbol (≤ 32 bytes); RedStone feed_ids are ScString and the
// longest seen is well under 64. The cap bounds what a buggy or
// malicious relayer can make us persist.
const rawSymbolMaxLen = 64

// validateRawSymbol enforces the permissive raw charset: 1–64 bytes
// of printable ASCII 0x21–0x7E — no whitespace, no control bytes, no
// non-ASCII. `/`, `.`, `_` and `-` are allowed because RedStone
// feed_ids carry them (`SolvBTC.BBN_FUNDAMENTAL/USD`).
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

// NewOracleRawAsset constructs a verbatim oracle-symbol reference.
// Returns ErrInvalidAsset if the symbol fails [validateRawSymbol].
func NewOracleRawAsset(symbol string) (Asset, error) {
	if err := validateRawSymbol(symbol); err != nil {
		return Asset{}, err
	}
	return Asset{Type: AssetOracleRaw, Code: symbol}, nil
}

// IsMapped reports whether a is a canonical (interpretable) asset —
// i.e. anything but the record-layer-only [AssetOracleRaw] variant.
// The interpretation layer (pairs, VWAP, divergence, MEV scans) must
// check this before treating an oracle_updates row as comparable.
func (a Asset) IsMapped() bool {
	return a.Type != AssetOracleRaw
}

// MapOracleSymbol resolves a bare oracle symbol (Reflector `Asset::Other(Symbol)`, Band
// `symbol_rates` key) to its canonical asset, else a verbatim [AssetOracleRaw]. Precedence is
// fiat (ADR-0010) → crypto (ADR-0014) → RWA (ADR-0028) → raw, one shared order for every
// source; the lists are disjoint (TestMapOracleSymbol_allowListsDisjoint), so order only
// matters if that changes.
//
// The result is raw iff !IsMapped(). The only error is a symbol failing [validateRawSymbol]
// (empty / >64 bytes / non-printable ASCII); the record layer refuses what it cannot represent.
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
