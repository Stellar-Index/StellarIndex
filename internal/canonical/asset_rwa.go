package canonical

// Off-chain tokenized real-world-asset helpers — see ADR-0028.
//
// The Asset type carries an AssetRWA variant for tokenized
// real-world assets (tokenized treasuries, money-market funds,
// equity ETFs — BENJI, GILTS, SPXU, …). Like AssetCrypto these are
// NOT Stellar assets — they're bare-ticker references published by
// an oracle (RedStone's RWA push feeds). They are deliberately a
// separate variant from AssetCrypto: a tokenized T-bill is not a
// cryptocurrency, and a shared variant would mis-feed every
// crypto-scoped surface (explorer views, crypto aggregations).
//
// Wire form: `rwa:<CODE>` (e.g. `rwa:BENJI`). The `rwa:` prefix is
// unambiguous, so ParseAsset dispatches in O(1).

// knownRWACodes is the allow-list of recognized RWA codes. Extension
// is a one-line amendment to ADR-0028 (never a superseding ADR).
// Codes chosen from RedStone's Stellar mainnet RWA push feeds
// (captured 2026-05-22 — see ADR-0028 §The RedStone 19-feed
// registry). The code is the human-meaningful identifier; the
// decoder's feed registry maps the raw on-chain feed_id (which may
// carry suffixes like `_ETHEREUM_FUNDAMENTAL`) onto it.
var knownRWACodes = map[string]struct{}{
	"BENJI":   {}, // Franklin Templeton OnChain US Government Money Fund
	"iBENJI":  {}, // BENJI index variant
	"GILTS":   {}, // tokenized UK gilts
	"CETES":   {}, // tokenized Mexican treasury (Cetes)
	"KTB":     {}, // tokenized Korean treasury bonds
	"TESOURO": {}, // tokenized Brazilian treasury (Tesouro)
	"USTRY":   {}, // tokenized US treasury
	"SPXU":    {}, // ProShares UltraPro Short S&P 500 (inverse ETF)
}

// IsKnownRWA reports whether code is in the ADR-0028 allow-list.
// The RedStone decoder uses this to gate feed-registry entries —
// an unrecognized code is a decoder bug, not silent coercion.
func IsKnownRWA(code string) bool {
	_, ok := knownRWACodes[code]
	return ok
}

// NewRWAAsset constructs a tokenized-real-world-asset reference.
// Returns ErrInvalidAsset if the code isn't allow-listed.
func NewRWAAsset(code string) (Asset, error) {
	if !IsKnownRWA(code) {
		return Asset{}, errorf(ErrInvalidAsset, "unknown rwa code %q (see ADR-0028)", code)
	}
	return Asset{Type: AssetRWA, Code: code}, nil
}
