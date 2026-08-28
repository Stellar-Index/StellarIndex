package canonical

// Oracle-published raw symbol helpers — see
// docs/design/oracle-capture-totality-design.md (Wave B, 2026-08-28).
//
// The Asset type carries an AssetOracleRaw variant for a symbol an
// indexed on-chain oracle (Reflector / RedStone / Band) published
// that maps to NO canonical asset. The record layer must be total —
// every price entry the oracle wrote on-chain is recorded — while
// only the interpretation layer is selective. Before this variant
// existed an unmapped symbol was dropped at decode time (counted on
// `stellarindex_source_unknown_symbols_total`) and recovering it
// needed a code change AND a lake replay.
//
// Wire form: `raw:<symbol>` (e.g. `raw:NOTACOIN`,
// `raw:SolvBTC.BBN_FUNDAMENTAL/USD`). The symbol is stored VERBATIM;
// source scoping comes from oracle_updates.source, not from the
// code. There is deliberately no allow-list: the whole point is to
// hold what we could not map.
//
// Record-layer ONLY. A raw asset is never a Pair leg (Pair.Validate
// refuses it), never a VWAP input, never compared by the
// interpretation layer, and never a supply key. Consumers that scan
// oracle_updates without keying by canonical asset must exclude it
// via [Asset.IsMapped] or `asset NOT LIKE 'raw:%'`. Orientation of
// an unmapped feed is unknown, so a raw row carries no Invert.

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
