package canonical

import "sort"

// Off-chain tokenized real-world-asset helpers — see ADR-0028.
//
// AssetRWA carries tokenized real-world assets (treasuries, money-market funds, ETFs) as bare
// oracle tickers from RedStone's RWA push feeds; they are not Stellar assets. A separate variant
// from AssetCrypto, since sharing one would mis-feed every crypto-scoped surface. Wire form
// `rwa:<CODE>`.

// knownRWACodes is the allow-list of RWA codes from RedStone's Stellar mainnet RWA push feeds
// (ADR-0028). The code is the human-meaningful identifier; the decoder's feed registry maps the
// raw on-chain feed_id (which may carry suffixes like `_ETHEREUM_FUNDAMENTAL`) onto it.
var knownRWACodes = map[string]struct{}{
	"BENJI":   {}, // Franklin Templeton OnChain US Government Money Fund
	"iBENJI":  {}, // BENJI index variant
	"GILTS":   {}, // tokenized UK gilts
	"CETES":   {}, // tokenized Mexican treasury (Cetes)
	"KTB":     {}, // tokenized Korean treasury bonds
	"TESOURO": {}, // tokenized Brazilian treasury (Tesouro)
	"USTRY":   {}, // tokenized US treasury
	"SPXU":    {}, // ProShares UltraPro Short S&P 500 (inverse ETF)
	// The code strips the feed-id suffix (`_FUNDAMENTAL`, `/USD`); the feed registry maps the
	// raw feed_id onto it.
	"USDY":    {}, // Ondo US Dollar Yield — tokenized note backed by short-term US Treasuries + bank deposits
	"USST":    {}, // STBL treasury-backed stablecoin (feed_id USST_FUNDAMENTAL)
	"XAUm":    {}, // Matrixdock tokenized gold (1 token ≈ 1 troy oz)
	"deJAAA":  {}, // Centrifuge deRWA token of the Janus Henderson AAA CLO ETF (JAAA)
	"deJTRSY": {}, // Centrifuge deRWA token of the Janus Henderson treasury fund (JTRSY)
	// Reflector FX oracle's spot-gold slot (1 troy oz in USD): a commodity, kept OFF the fiat
	// list and OFF crypto. It shares the rwa: namespace with XAUm but is a DISTINCT asset
	// (spot vs the Matrixdock token).
	"XAU": {}, // spot gold, troy ounce (Reflector FX slot)
}

// IsKnownRWA reports whether code is in the ADR-0028 allow-list; the RedStone decoder gates
// feed-registry entries on it, so an unknown code is a decoder bug, not silent coercion.
//
// Match is EXACT on purpose: a near-miss there is a bug. A caller matching an on-chain asset
// code (case chosen by its issuer) against these tickers wants [KnownRWACodes] and its own
// comparison.
func IsKnownRWA(code string) bool {
	_, ok := knownRWACodes[code]
	return ok
}

// KnownRWACodes returns the ADR-0028 allow-list in a stable order.
// Exported for callers that must compare an on-chain asset code
// against the list under their own folding rule — [IsKnownRWA] stays
// exact for the decoder.
func KnownRWACodes() []string {
	out := make([]string, 0, len(knownRWACodes))
	for c := range knownRWACodes {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// NewRWAAsset constructs a tokenized-real-world-asset reference.
// Returns ErrInvalidAsset if the code isn't allow-listed.
func NewRWAAsset(code string) (Asset, error) {
	if !IsKnownRWA(code) {
		return Asset{}, errorf(ErrInvalidAsset, "unknown rwa code %q (see ADR-0028)", code)
	}
	return Asset{Type: AssetRWA, Code: code}, nil
}
