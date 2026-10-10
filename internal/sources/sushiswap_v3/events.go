// Package sushiswap_v3 decodes on-chain events from SushiSwap V3 on Stellar,
// a Uniswap-V3-shaped concentrated-liquidity AMM on Soroban whose pools are
// created by a single factory ([MainnetFactory]).
//
// Every pool topic is a ONE-element Symbol vector and the pool address is not in
// it, so topic bytes identify nothing: gating must be on contract identity
// (ADR-0035). `swap`, `mint` and `burn` are among the most common symbols on
// pubnet (a 10k-ledger census puts `mint` at 33% and `burn` at 12% of all
// contract events).
//
// `swap` is the only trade-forming event: a 7-entry Map {amount0 i128, amount1
// i128, liquidity u128, recipient, sender, sqrt_price_x96 u256, tick i32}. The
// body is identical across both deployed WASM versions; decoding is by field
// name (docs/architecture/ingest-pipeline.md#contract-schema-evolution) so an
// appended field stays readable. The factory emitted `pool_created` twice for
// two pools inside their creation transaction, so seeding must be idempotent.
//
// A V3 pool prices from `sqrt_price_x96` and a tick range, not reserves, so
// reserve-based TVL or pricing would be WRONG and neither is derived. Trade rows
// carry the two realised amounts, which are exact and orientation-free.
package sushiswap_v3

import (
	"errors"
	"sort"

	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// SourceName is the canonical string stamped on every event this package
// emits — the trades.source value, the projector cursor sub_source key,
// the metrics label, and the config.IngestionConfig.EnabledSources entry.
// Must be stable.
const SourceName = "sushiswap_v3"

// Event names — topic[0] of every SushiSwap V3 pool/factory event is a
// one-element Symbol topic vector holding one of these literals. Verified
// against the lake: these seven pool symbols plus the factory's four are
// the COMPLETE emitted surface over the protocol's whole history.
const (
	// Pool events.
	EventSwap    = "swap"
	EventMint    = "mint"
	EventBurn    = "burn"
	EventCollect = "collect"
	EventInit    = "init"

	// Pool lifecycle events emitted by the pool itself on a WASM
	// upgrade / storage migration driven by the factory admin.
	EventUpgraded = "upgraded"
	EventMigrated = "migrated"

	// EventPoolCreated is the factory's creation event — the ADR-0035
	// trust anchor. It is the ONLY place token0 / token1 appear on-chain,
	// so it is both the gate seed and the money mapping.
	EventPoolCreated = "pool_created"
)

// Pre-encoded base64 SCVal topic bytes for byte-equality routing. Built
// once at package init via scval.MustEncodeSymbol so the hot path never
// re-encodes.
var (
	TopicSymbolSwap        = scval.MustEncodeSymbol(EventSwap)
	TopicSymbolMint        = scval.MustEncodeSymbol(EventMint)
	TopicSymbolBurn        = scval.MustEncodeSymbol(EventBurn)
	TopicSymbolCollect     = scval.MustEncodeSymbol(EventCollect)
	TopicSymbolInit        = scval.MustEncodeSymbol(EventInit)
	TopicSymbolUpgraded    = scval.MustEncodeSymbol(EventUpgraded)
	TopicSymbolMigrated    = scval.MustEncodeSymbol(EventMigrated)
	TopicSymbolPoolCreated = scval.MustEncodeSymbol(EventPoolCreated)
)

var (
	// ErrMalformedPayload — the event body does not match the schema this
	// package decodes (wrong SCVal shape, missing field, wrong field type).
	ErrMalformedPayload = errors.New("sushiswap_v3: malformed event payload")

	// ErrNonDirectionalSwap — a swap body decoded cleanly but carries no
	// cross-token exchange: the two signed pool deltas are not one strictly
	// positive and one strictly negative. A price-forming V3 swap moves
	// value BOTH ways (the pool gains one token and pays out the other), so
	// any other sign combination has no derivable direction and no
	// derivable price.
	//
	// This is not hypothetical. Ledger 62,712,211 on pool CCR2CH4G… carries
	// amount0=1, amount1=0 — a one-unit dust swap that rounded its output to
	// nothing. It is the ONLY such event in the protocol's whole history
	// (1 of 97,349 swaps; every other swap has exactly opposite signs), and
	// it must be refused rather than reported as a trade at an infinite or
	// zero price. Treated by the Decoder as a recognized no-op — projected
	// as expected-zero rather than as a decode error — the same contract
	// soroswap's non-directional swaps carry.
	ErrNonDirectionalSwap = errors.New("sushiswap_v3: non-directional swap (no cross-token exchange)")

	// ErrUnknownPool — a gated pool emitted a swap but its (token0, token1)
	// mapping is not in the registry, so the trade cannot be given asset
	// identities. Fails CLOSED (no row) rather than inventing a bare-code
	// or NULL-asset trade; counted so the gap is visible.
	ErrUnknownPool = errors.New("sushiswap_v3: swap from a pool with no token mapping")
)

// MainnetFactory is the SushiSwap V3 pool factory on pubnet — the ADR-0035
// trust root. Verified from the lake rather than from a third-party
// listing: it is the emitter of the `pool_created` event that appears in
// the SAME transaction as, and immediately before, the `init` event of
// every pool the public pool listings name (first proven on tx
// 01d797bc…4765, ledger 61,487,379, which creates CCR2CH4G…). It is the
// only contract on pubnet emitting `pool_created` alongside these pools.
//
// A protocol may have more than one factory (Blend and Soroswap both do),
// so the gate takes a SET; exactly one has ever emitted `pool_created` for
// these pools, and a second would be admitted by extending
// [MainnetFactories].
const MainnetFactory = "CD3KRKGDRVWPXVB3VXLUMQKMX6XZ6Q2H334IVZD4XXNAMKSRVQL5GLYF"

// MainnetFactories is the complete verified factory trust-root set.
var MainnetFactories = []string{MainnetFactory}

// FactoryGenesisLedger is the ledger of the factory's first `pool_created`
// event — the first ledger at which this source can have data, and the
// lower bound for any re-derive or gap scan.
const FactoryGenesisLedger uint32 = 61_487_379

// PoolMeta is the immutable creation record of one pool, as carried by the
// factory's `pool_created` event.
//
// Token0 / Token1 are the C-strkeys of the pool's two token contracts
// (Stellar Asset Contracts for classic assets, plain Soroban tokens
// otherwise). They are held as strkeys rather than canonical.Asset so this
// table stays a plain data literal; [PoolTokensFor] converts.
//
// FeePips is the swap fee in hundredths of a basis point (500 = 0.05%,
// 3000 = 0.30%, 10000 = 1.00%) and TickSpacing the pool's tick granularity.
// The deployed set uses exactly the three canonical V3 tiers, each paired
// with its canonical spacing (500/10, 3000/60, 10000/200). Neither field
// participates in trade derivation; both are recorded because a V3 pool is
// identified by (token0, token1, fee), not by the token pair alone — two
// pools over the same pair at different fee tiers are different markets.
type PoolMeta struct {
	Token0      string
	Token1      string
	FeePips     uint32
	TickSpacing int32
	CreatedAt   uint32
}

// MainnetPools is the curated pool table: every pool the factory has created,
// keyed by pool C-strkey, decoded from the factory's `pool_created` events (not
// from any third-party listing).
//
// It carries tokens, not just ids, because it does two jobs: the ADR-0035 gate
// seed (via [MainnetGatedSet]), and the money mapping, since token0 / token1
// appear ONLY in the creation event.
//
// A pool created after the table was frozen is picked up live from
// `pool_created` and persisted to protocol_contracts. A process that starts
// after such a creation ledger admits the pool via the protocol_contracts warm
// but has no token mapping until that event is replayed, so its swaps fail
// closed into a counted gap (ErrUnknownPool) rather than a mis-assetted trade.
// See the README.
var MainnetPools = map[string]PoolMeta{
	"CA5MIPAAG3UULVAHK7U3U6VBBM52YIHMCZOOSHNTPUSLYR7NKNHVD6WK": {
		Token0:      "CCKCKCPHYVXQD4NECBFJTFSCU2AMSJGCNG4O6K4JVRE2BLPR7WNDBQIQ",
		Token1:      "CD3X4GOWBPDU57NIPMPEMH7LFNAMBDTY5SKJCHLY7IDDWJQVUTU7CBBK",
		FeePips:     10000,
		TickSpacing: 200,
		CreatedAt:   61_918_509,
	},
	"CA5R5L7QE7WC2M4YAPSBITV7M2R6LX5366UURH3REHQMOJV6R5QWTH2K": {
		Token0:      "CBEM2CAIYLM3HBOPU5HLQL7V5BUAKM3N77DYQKX4FNHTQLQUUD2ZFBOX",
		Token1:      "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75",
		FeePips:     500,
		TickSpacing: 10,
		CreatedAt:   61_918_847,
	},
	"CA6LYAEDN7XHOKD5TNRFBM3IDFD22VRVVXTGPGK77FZFC7X2OYUQ7BAZ": {
		Token0:      "CCKCKCPHYVXQD4NECBFJTFSCU2AMSJGCNG4O6K4JVRE2BLPR7WNDBQIQ",
		Token1:      "CDCKFBZYF2AQCSM3JOF2ZM27O3Y6AJAI4OTCQKAFNZ3FHBYUTFOKICIY",
		FeePips:     10000,
		TickSpacing: 200,
		CreatedAt:   61_919_353,
	},
	"CA75VVHLWSM7W6ULNQI7ZJYDFOMQCCPKIDDDHBAL5KOKHWWKWQ5S7MHO": {
		Token0:      "CB3YA656OYIHU57657I5KGSBRHE5I3OZU4VFC22PYAOANFZHEWNYGAGP",
		Token1:      "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75",
		FeePips:     500,
		TickSpacing: 10,
		CreatedAt:   61_525_536,
	},
	"CABMZD6BYKKLHRJNS5MURYOBX77NPAH767AI7EVFGWV3WZV55QFN5YNE": {
		Token0:      "CBEM2CAIYLM3HBOPU5HLQL7V5BUAKM3N77DYQKX4FNHTQLQUUD2ZFBOX",
		Token1:      "CD3X4GOWBPDU57NIPMPEMH7LFNAMBDTY5SKJCHLY7IDDWJQVUTU7CBBK",
		FeePips:     500,
		TickSpacing: 10,
		CreatedAt:   61_919_082,
	},
	"CACU7KU33MFWMOP334RNQT7CZV3M7DNDAAXL5O3I4ATQRZCMXFJI4RMZ": {
		Token0:      "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75",
		Token1:      "CCXU7Z4FUOG4PV7GMXI5IF4YIN52I3XKDIJCKFYWBELBPJTUG7OSU2Y7",
		FeePips:     500,
		TickSpacing: 10,
		CreatedAt:   62_952_420,
	},
	"CAFLJXGUAURAMBA3AIHC7ZJOAQKGZ7WEFFGMH5XRC35IMNU7PWIBXVTP": {
		Token0:      "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75",
		Token1:      "CD3X4GOWBPDU57NIPMPEMH7LFNAMBDTY5SKJCHLY7IDDWJQVUTU7CBBK",
		FeePips:     500,
		TickSpacing: 10,
		CreatedAt:   61_919_062,
	},
	"CAKWXQDEVVUF2ABUEM3M2G7QJGJNDZNNVXJZYG4Z4QP6K54QTWV4DW2S": {
		Token0:      "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA",
		Token1:      "CBEM2CAIYLM3HBOPU5HLQL7V5BUAKM3N77DYQKX4FNHTQLQUUD2ZFBOX",
		FeePips:     500,
		TickSpacing: 10,
		CreatedAt:   61_918_281,
	},
	"CALM7JTAJC7AJ7ZGTQKXZNNILJUCD2AZNN7QA7FVM3YYIJBCJGUABEDH": {
		Token0:      "CB226ZOEYXTBPD3QEGABTJYSKZVBP2PASEISLG3SBMTN5CE4QZUVZ3CE",
		Token1:      "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75",
		FeePips:     500,
		TickSpacing: 10,
		CreatedAt:   62_144_875,
	},
	"CAMUA6N6SLCMSIQRICXIQBRYYG3SCEPILS2HJTBXGMCOBCRWQUHAS2OJ": {
		Token0:      "CAU7TR4L52CSCYOLOPXPJJ7T6EMEXTEE4XKRH2ASY23EFYEMU5WUFYP2",
		Token1:      "CCUMQ5V3I3V3TQ4B2VNTK7JZNZWTGZBWLIBY6WSXCKJXTR74ZGN3XD5V",
		FeePips:     3000,
		TickSpacing: 60,
		CreatedAt:   61_903_837,
	},
	"CAOGXY6DW2KWUVOWCGGPLW7MIJNP7XCMXY736LNLOKYEQA3CBKXVIDEA": {
		Token0:      "CAL6ER2TI6CTRAY6BFXWNWA7WTYXUXTQCHUBCIBU5O6KM3HJFG6Z6VXV",
		Token1:      "CD6RUGRIRWRTIXQOAACOZTM4HBVGXNAQXK2VFMH54II73OATVKLJHJN3",
		FeePips:     500,
		TickSpacing: 10,
		CreatedAt:   62_342_715,
	},
	"CAPT5THGW7WOCX47TICCB5JZZK4Y24CHQIBSM57Y472WFFV6FGTRKJQD": {
		Token0:      "CAUIKL3IYGMERDRUN6YSCLWVAKIFG5Q4YJHUKM4S4NJZQIA3BAS6OJPK",
		Token1:      "CCG27OZ5AV4WUXS6XTECWAXEY5UOMEFI2CWFA3LHZGBTLYZWTJF3MJYQ",
		FeePips:     3000,
		TickSpacing: 60,
		CreatedAt:   61_918_562,
	},
	"CAPUAZDFH4VBQTC7PYL7UM2KSXER2ZY3D462WW6DCE2HGUSO646S4F2X": {
		Token0:      "CAUIKL3IYGMERDRUN6YSCLWVAKIFG5Q4YJHUKM4S4NJZQIA3BAS6OJPK",
		Token1:      "CD3X4GOWBPDU57NIPMPEMH7LFNAMBDTY5SKJCHLY7IDDWJQVUTU7CBBK",
		FeePips:     10000,
		TickSpacing: 200,
		CreatedAt:   61_918_648,
	},
	"CAUBW4ARD42U2UEIA7GDUB5LNKTRTVYJHXKL3CV27YZRDFADDGKLZWFD": {
		Token0:      "CAUIKL3IYGMERDRUN6YSCLWVAKIFG5Q4YJHUKM4S4NJZQIA3BAS6OJPK",
		Token1:      "CBEM2CAIYLM3HBOPU5HLQL7V5BUAKM3N77DYQKX4FNHTQLQUUD2ZFBOX",
		FeePips:     500,
		TickSpacing: 10,
		CreatedAt:   61_918_580,
	},
	"CAWN3BM2ADBMA4CQZLIHTBXA3BQHV4VAPK42LWT5ONAKZW6PH2BBCKLS": {
		Token0:      "CD6M4R2322BYCY2LNWM74PEBQAQ63SA3DUJLI3L4225U4ZVCLMSCBCIS",
		Token1:      "CD6RUGRIRWRTIXQOAACOZTM4HBVGXNAQXK2VFMH54II73OATVKLJHJN3",
		FeePips:     500,
		TickSpacing: 10,
		CreatedAt:   62_342_709,
	},
	"CAWWOFOEGWPPNP6QKVHTJYB7UHRXC6W6EAFMUPGHMJL7K46E6UCOSNDM": {
		Token0:      "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA",
		Token1:      "CBEM2CAIYLM3HBOPU5HLQL7V5BUAKM3N77DYQKX4FNHTQLQUUD2ZFBOX",
		FeePips:     3000,
		TickSpacing: 60,
		CreatedAt:   61_918_664,
	},
	"CAXJ2FDV6S3L46EFEFRXUBLQ5U5CZLZOG35RPCJRNQVLM5MH2HCK5I7J": {
		Token0:      "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA",
		Token1:      "CD3X4GOWBPDU57NIPMPEMH7LFNAMBDTY5SKJCHLY7IDDWJQVUTU7CBBK",
		FeePips:     3000,
		TickSpacing: 60,
		CreatedAt:   61_918_865,
	},
	"CB5NNLVWQWN26VBOI576UVA4EGDGPKOHGNVTVYXREXAN2TO3XT6SCJL4": {
		Token0:      "CBEM2CAIYLM3HBOPU5HLQL7V5BUAKM3N77DYQKX4FNHTQLQUUD2ZFBOX",
		Token1:      "CBZVSNVB55ANF24QVJL2K5QCLOAB6XITGTGXYEAF6NPTXYKEJUYQOHFC",
		FeePips:     10000,
		TickSpacing: 200,
		CreatedAt:   61_918_241,
	},
	"CBBHUYY3YE7AOCLHFFVTOFHPMV3WBANNDIQBJ3NCNLXC7BLHRAM5YD7M": {
		Token0:      "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA",
		Token1:      "CD3X4GOWBPDU57NIPMPEMH7LFNAMBDTY5SKJCHLY7IDDWJQVUTU7CBBK",
		FeePips:     10000,
		TickSpacing: 200,
		CreatedAt:   61_918_196,
	},
	"CBBXZDNNIVCGGLLCH43KTH6WDZ5MFVYFQ5LTGD236A4A6OJMH2K4H6LA": {
		Token0:      "CBIJBDNZNF4X35BJ4FFZWCDBSCKOP5NB4PLG4SNENRMLAPYG4P5FM6VN",
		Token1:      "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75",
		FeePips:     3000,
		TickSpacing: 60,
		CreatedAt:   61_725_403,
	},
	"CBGBQUEHEDIMA2P5JVHROPYVENY7Y3EA3DDYEMIW2HICRKFW7YDI635B": {
		Token0:      "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA",
		Token1:      "CCG27OZ5AV4WUXS6XTECWAXEY5UOMEFI2CWFA3LHZGBTLYZWTJF3MJYQ",
		FeePips:     10000,
		TickSpacing: 200,
		CreatedAt:   61_918_601,
	},
	"CBOLCGXDSRU22SLUIJMFCJBOOURQLZHX5ZFIFJBYK2VAB3AEK5M2T5GM": {
		Token0:      "CBI7UCH5KGSVQRO5H4SUCZUTZABCITZLRHQQZTWL2TK4RZ72TAR6IHRV",
		Token1:      "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75",
		FeePips:     3000,
		TickSpacing: 60,
		CreatedAt:   62_653_278,
	},
	"CBQIPFKHCXBLXJKSFSZJJLODGZDA6C33D5TUL3VAPXM25LH2YKNAROC2": {
		Token0:      "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA",
		Token1:      "CCKCKCPHYVXQD4NECBFJTFSCU2AMSJGCNG4O6K4JVRE2BLPR7WNDBQIQ",
		FeePips:     10000,
		TickSpacing: 200,
		CreatedAt:   61_919_417,
	},
	"CBRKPTX4TWYEVGDTTVLSBVN6RKTFC2PKD6F5ZRSGBHQE7GC2UYMALTXY": {
		Token0:      "CAUIKL3IYGMERDRUN6YSCLWVAKIFG5Q4YJHUKM4S4NJZQIA3BAS6OJPK",
		Token1:      "CBZVSNVB55ANF24QVJL2K5QCLOAB6XITGTGXYEAF6NPTXYKEJUYQOHFC",
		FeePips:     3000,
		TickSpacing: 60,
		CreatedAt:   63_147_255,
	},
	"CBTZO2GHYS23PW4QSHRNT66X62YZQJU7X3ZCZOKJTNEXKLR4WBYV54D7": {
		Token0:      "CBZVSNVB55ANF24QVJL2K5QCLOAB6XITGTGXYEAF6NPTXYKEJUYQOHFC",
		Token1:      "CD3X4GOWBPDU57NIPMPEMH7LFNAMBDTY5SKJCHLY7IDDWJQVUTU7CBBK",
		FeePips:     500,
		TickSpacing: 10,
		CreatedAt:   61_919_373,
	},
	"CBV7CK2DDLODRLTWFBWE7CN5WKN5B6O26ERTULNQIWJQ7ZBTHTGQ5YFI": {
		Token0:      "CAIDUWYDM25GBIQVQQ5C7EFVLL2AKX35L6265ZOW2UKINZ6X6IYPDLZ4",
		Token1:      "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA",
		FeePips:     3000,
		TickSpacing: 60,
		CreatedAt:   61_903_924,
	},
	"CBVHBZSZOS6KRDJ4D44FU2YLIENOVSSLM3UGKW6XQMVIFUAMWIWCVH2U": {
		Token0:      "CBSJZEIO5C7KC2SF3MKSNXXJSW5G3VTNBX4ATMKUI3B2MR4JKM4R26YF",
		Token1:      "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75",
		FeePips:     500,
		TickSpacing: 10,
		CreatedAt:   64_116_662,
	},
	"CBVKO35SAF2ZT75FCLCGLYQG3S6B32YZTOJ2G5F7M746UGBRAWZ5BNZ6": {
		Token0:      "CBEM2CAIYLM3HBOPU5HLQL7V5BUAKM3N77DYQKX4FNHTQLQUUD2ZFBOX",
		Token1:      "CBIJBDNZNF4X35BJ4FFZWCDBSCKOP5NB4PLG4SNENRMLAPYG4P5FM6VN",
		FeePips:     3000,
		TickSpacing: 60,
		CreatedAt:   61_918_259,
	},
	"CBVZXBW5E5Q72J5ETZT4ZJLNS6GTLQC5BLUOERJNFQMMTKQCVY4N5YSX": {
		Token0:      "CCCRWH6Q3FNP3I2I57BDLM5AFAT7O6OF6GKQOC6SSJNDAVRZ57SPHGU2",
		Token1:      "CD3X4GOWBPDU57NIPMPEMH7LFNAMBDTY5SKJCHLY7IDDWJQVUTU7CBBK",
		FeePips:     10000,
		TickSpacing: 200,
		CreatedAt:   61_918_465,
	},
	"CC22BLZAFTS7M6Z25HOMKDLV65PBP5CIHFER2OTZB5IRNL3YBWDXKDFF": {
		Token0:      "CCCRWH6Q3FNP3I2I57BDLM5AFAT7O6OF6GKQOC6SSJNDAVRZ57SPHGU2",
		Token1:      "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75",
		FeePips:     500,
		TickSpacing: 10,
		CreatedAt:   61_525_514,
	},
	"CC22KKT4G3MSL5STSZDOC4KA5CTLCFTQX4ASASIGOE7Z3HI7HOY5F33H": {
		Token0:      "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA",
		Token1:      "CCUMQ5V3I3V3TQ4B2VNTK7JZNZWTGZBWLIBY6WSXCKJXTR74ZGN3XD5V",
		FeePips:     3000,
		TickSpacing: 60,
		CreatedAt:   61_903_770,
	},
	"CCAQO6MKWCD463JZEIY5JUSU425H77LNYM3R6ZNQPO4KBU5C3SAEJDDB": {
		Token0:      "CB3YA656OYIHU57657I5KGSBRHE5I3OZU4VFC22PYAOANFZHEWNYGAGP",
		Token1:      "CD3X4GOWBPDU57NIPMPEMH7LFNAMBDTY5SKJCHLY7IDDWJQVUTU7CBBK",
		FeePips:     10000,
		TickSpacing: 200,
		CreatedAt:   61_918_544,
	},
	"CCF7MYFIILNDCDZ37QGYJQRYW455MPHA5PJIQ5D3KB3KEYLZA2WB26S2": {
		Token0:      "CAU7TR4L52CSCYOLOPXPJJ7T6EMEXTEE4XKRH2ASY23EFYEMU5WUFYP2",
		Token1:      "CBEM2CAIYLM3HBOPU5HLQL7V5BUAKM3N77DYQKX4FNHTQLQUUD2ZFBOX",
		FeePips:     3000,
		TickSpacing: 60,
		CreatedAt:   61_919_188,
	},
	"CCFWRAC3MB7JSPKAWSWT3MCRHVQECG3SU2JAMNAL3K2UHBQIYHBSR5LY": {
		Token0:      "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA",
		Token1:      "CBEM2CAIYLM3HBOPU5HLQL7V5BUAKM3N77DYQKX4FNHTQLQUUD2ZFBOX",
		FeePips:     10000,
		TickSpacing: 200,
		CreatedAt:   61_919_228,
	},
	"CCI2P2I4MWU3J2RQ6YKJXQN46CB42LWAFCUYNWY2J2JVHZBIDR74TLBG": {
		Token0:      "CAESLMGW5LYTIEJI7FJHK6SFSWRELLNVX5Q4WR4UZEALMTRWQDBKDPAG",
		Token1:      "CBEM2CAIYLM3HBOPU5HLQL7V5BUAKM3N77DYQKX4FNHTQLQUUD2ZFBOX",
		FeePips:     10000,
		TickSpacing: 200,
		CreatedAt:   61_919_136,
	},
	"CCKC4L6LI5ZNFKJCE7TVOEGGNJQPVHCCRJWMDYWEBL26SY62INCJLZMP": {
		Token0:      "CBHBD77PWZ3AXPQVYVDBHDKEMVNOR26UZUZHWCB6QC7J5SETQPRUQAS4",
		Token1:      "CD3X4GOWBPDU57NIPMPEMH7LFNAMBDTY5SKJCHLY7IDDWJQVUTU7CBBK",
		FeePips:     10000,
		TickSpacing: 200,
		CreatedAt:   61_919_169,
	},
	"CCLXJ4F5STIZ7WWC5D2J57VCLLFIQMWUI5PSYJORPZTAUUQTS2XFU5ZK": {
		Token0:      "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA",
		Token1:      "CCLE7VXLGF3RCLL7FT2L7Q54HNRF37RGJDZIZMNN5275TI6ATBH2ATG3",
		FeePips:     500,
		TickSpacing: 10,
		CreatedAt:   62_785_922,
	},
	"CCMPJ2PZDNG7DCQWU6YHF56U3QULE3JGMUVEZWBIWQOYHU6Z6QLEOEJQ": {
		Token0:      "CAUIKL3IYGMERDRUN6YSCLWVAKIFG5Q4YJHUKM4S4NJZQIA3BAS6OJPK",
		Token1:      "CD3X4GOWBPDU57NIPMPEMH7LFNAMBDTY5SKJCHLY7IDDWJQVUTU7CBBK",
		FeePips:     500,
		TickSpacing: 10,
		CreatedAt:   61_918_690,
	},
	"CCOLSF35JVRGLONMJJMFYXUAEQ5IM4QOMYEWMXEFJEOQK3WVN4EJ3OUR": {
		Token0:      "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA",
		Token1:      "CAU7TR4L52CSCYOLOPXPJJ7T6EMEXTEE4XKRH2ASY23EFYEMU5WUFYP2",
		FeePips:     3000,
		TickSpacing: 60,
		CreatedAt:   61_903_819,
	},
	"CCQ4WKGF5PDJZ3PTDQBOOWD6HE67DEB3QHOGLF2VAWO2RBCGDT2DPDTI": {
		Token0:      "CBEM2CAIYLM3HBOPU5HLQL7V5BUAKM3N77DYQKX4FNHTQLQUUD2ZFBOX",
		Token1:      "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75",
		FeePips:     10000,
		TickSpacing: 200,
		CreatedAt:   61_918_446,
	},
	"CCR2CH4GQVCZHG7CHFVMNANCK45CU5DVKXZIIITDZQAU3CEJZ7RQH2MQ": {
		Token0:      "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA",
		Token1:      "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75",
		FeePips:     3000,
		TickSpacing: 60,
		CreatedAt:   61_487_379,
	},
	"CCRKQ2RHBWB5ZCHOSBSYEC2QNVSU3MGVUF56BWWKJMJIJ3ZF2A6W7KEC": {
		Token0:      "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75",
		Token1:      "CD6M4R2322BYCY2LNWM74PEBQAQ63SA3DUJLI3L4225U4ZVCLMSCBCIS",
		FeePips:     500,
		TickSpacing: 10,
		CreatedAt:   62_343_147,
	},
	"CCT74MCKWWPKHK5T7LXBPXK7AAPZIH35HJZY5CRZGVEJ4UUCMFQXBNVH": {
		Token0:      "CD25MNVTZDL4Y3XBCPCJXGXATV5WUHHOWMYFF4YBEGU5FCPGMYTVG5JY",
		Token1:      "CD3X4GOWBPDU57NIPMPEMH7LFNAMBDTY5SKJCHLY7IDDWJQVUTU7CBBK",
		FeePips:     10000,
		TickSpacing: 200,
		CreatedAt:   61_919_119,
	},
	"CCUVVQJNVI3UBXHZ6LDW2BTULOKMFZWVYLY26QTP62AWUQCDPVNZWEPJ": {
		Token0:      "CAIDUWYDM25GBIQVQQ5C7EFVLL2AKX35L6265ZOW2UKINZ6X6IYPDLZ4",
		Token1:      "CAU7TR4L52CSCYOLOPXPJJ7T6EMEXTEE4XKRH2ASY23EFYEMU5WUFYP2",
		FeePips:     10000,
		TickSpacing: 200,
		CreatedAt:   61_903_977,
	},
	"CCWJAQURY64U4VWI5GDLF42SH4RWFJZTYXO5BGA7SLNLVBGGV2L4EBIP": {
		Token0:      "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA",
		Token1:      "CD3X4GOWBPDU57NIPMPEMH7LFNAMBDTY5SKJCHLY7IDDWJQVUTU7CBBK",
		FeePips:     500,
		TickSpacing: 10,
		CreatedAt:   61_919_041,
	},
	"CCXRRORTOXXP53HEKJ6RCG7CDRWZAJHIS4N7PDL32PUNMNN7VWPJVQWS": {
		Token0:      "CAL6ER2TI6CTRAY6BFXWNWA7WTYXUXTQCHUBCIBU5O6KM3HJFG6Z6VXV",
		Token1:      "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75",
		FeePips:     500,
		TickSpacing: 10,
		CreatedAt:   62_343_134,
	},
	"CD2JLIZ5TSF746SVPNU224CY3ZIXTEII37CC4DNCTSDRLK4NAXBCQZLB": {
		Token0:      "CAUIKL3IYGMERDRUN6YSCLWVAKIFG5Q4YJHUKM4S4NJZQIA3BAS6OJPK",
		Token1:      "CAU7TR4L52CSCYOLOPXPJJ7T6EMEXTEE4XKRH2ASY23EFYEMU5WUFYP2",
		FeePips:     10000,
		TickSpacing: 200,
		CreatedAt:   61_903_895,
	},
	"CD6BHNV26Z7FOUU7VCFMYPRIB5JOG7724R42O5EJ4KGQXK3USO4NYOLS": {
		Token0:      "CBEM2CAIYLM3HBOPU5HLQL7V5BUAKM3N77DYQKX4FNHTQLQUUD2ZFBOX",
		Token1:      "CDTKPWPLOURQA2SGTKTUQOWRCBZEORB4BWBOMJ3D3ZTQQSGE5F6JBQLV",
		FeePips:     10000,
		TickSpacing: 200,
		CreatedAt:   61_918_698,
	},
	"CD7ZJUQEJODTJXWJMDJRV7UHCANOBX3FC6KXJV3DMIFX3JXUWMF3U3T5": {
		Token0:      "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75",
		Token1:      "CC64WBDGS6QQP22QTTIACYIXT3WF7BBQEYOQPLTP7GTKYY7PZ74QYGSL",
		FeePips:     500,
		TickSpacing: 10,
		CreatedAt:   62_669_385,
	},
	"CDAUN7SBYWRAVKJA453ZGLEP4LFCR25ANTI6EIHKTWSSTWHYJXLD3EXT": {
		Token0:      "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75",
		Token1:      "CD25MNVTZDL4Y3XBCPCJXGXATV5WUHHOWMYFF4YBEGU5FCPGMYTVG5JY",
		FeePips:     3000,
		TickSpacing: 60,
		CreatedAt:   61_725_448,
	},
	"CDDPO65MGM3HYYMFJJB7Y7NCRV2DAJ7YAFZYQQRMBLCR3WHFIPAZQCTS": {
		Token0:      "CBEM2CAIYLM3HBOPU5HLQL7V5BUAKM3N77DYQKX4FNHTQLQUUD2ZFBOX",
		Token1:      "CD25MNVTZDL4Y3XBCPCJXGXATV5WUHHOWMYFF4YBEGU5FCPGMYTVG5JY",
		FeePips:     10000,
		TickSpacing: 200,
		CreatedAt:   61_919_436,
	},
	"CDEXGN6YJXIPSMJZPU4NTJH6IVD47ZOG6GNTTMYK4X3U4JKLKD34F4XT": {
		Token0:      "CDCKFBZYF2AQCSM3JOF2ZM27O3Y6AJAI4OTCQKAFNZ3FHBYUTFOKICIY",
		Token1:      "CD3X4GOWBPDU57NIPMPEMH7LFNAMBDTY5SKJCHLY7IDDWJQVUTU7CBBK",
		FeePips:     500,
		TickSpacing: 10,
		CreatedAt:   61_919_010,
	},
	"CDGIQQBPGXATIEXWTFN5O6J7LM5IMQLMVIQ47Q4H44VIMMOBZ4N6KRVZ": {
		Token0:      "CAIDUWYDM25GBIQVQQ5C7EFVLL2AKX35L6265ZOW2UKINZ6X6IYPDLZ4",
		Token1:      "CAUIKL3IYGMERDRUN6YSCLWVAKIFG5Q4YJHUKM4S4NJZQIA3BAS6OJPK",
		FeePips:     10000,
		TickSpacing: 200,
		CreatedAt:   61_903_939,
	},
	"CDMJRRH5MAJLB7T5SQAWQOOL7UJ3BABGTURSVKIE6AEDSXESDDJIBVCT": {
		Token0:      "CBI7UCH5KGSVQRO5H4SUCZUTZABCITZLRHQQZTWL2TK4RZ72TAR6IHRV",
		Token1:      "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75",
		FeePips:     500,
		TickSpacing: 10,
		CreatedAt:   62_653_663,
	},
	"CDNHCFJ6LGPV4OWZL2SAQPFQHAUFFVMWGBZFVPA5Z5HTL4A6RWEZYNM4": {
		Token0:      "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA",
		Token1:      "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75",
		FeePips:     500,
		TickSpacing: 10,
		CreatedAt:   63_434_881,
	},
	"CDO6MTO4RYHFWWIG4DA3ZDHJ7GQMWA3BCZW4YTSBHEEDOJZV4BTJZB7M": {
		Token0:      "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA",
		Token1:      "CBZVSNVB55ANF24QVJL2K5QCLOAB6XITGTGXYEAF6NPTXYKEJUYQOHFC",
		FeePips:     3000,
		TickSpacing: 60,
		CreatedAt:   61_725_489,
	},
	"CDSOL7SBO2ZEASXAFZNDXGJUMIM7YOUBHQOXCCVQEKKYACIR43ZBH6MZ": {
		Token0:      "CCUMQ5V3I3V3TQ4B2VNTK7JZNZWTGZBWLIBY6WSXCKJXTR74ZGN3XD5V",
		Token1:      "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75",
		FeePips:     3000,
		TickSpacing: 60,
		CreatedAt:   61_995_405,
	},
	"CDVBYETOFG7UYJAD6CMOAQZXBHEK3PD5ZDZKWMWIY5OXIWATPX4VGMY2": {
		Token0:      "CCG27OZ5AV4WUXS6XTECWAXEY5UOMEFI2CWFA3LHZGBTLYZWTJF3MJYQ",
		Token1:      "CD3X4GOWBPDU57NIPMPEMH7LFNAMBDTY5SKJCHLY7IDDWJQVUTU7CBBK",
		FeePips:     10000,
		TickSpacing: 200,
		CreatedAt:   61_918_332,
	},
}

// MainnetGatedSet returns the curated pool set — the ADR-0035 gate trust
// root seeded into every Decoder. Sorted for determinism.
func MainnetGatedSet() []string {
	out := make([]string, 0, len(MainnetPools))
	for id := range MainnetPools {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// classify decides what kind of SushiSwap V3 event this is, by
// byte-equality on the pre-encoded topic[0] Symbol. Returns "" for
// anything unrecognized.
//
// Topic shape carries NO identity: every event in this protocol has a
// one-element Symbol topic vector, and `swap` / `mint` / `burn` are among
// the most-emitted symbols on pubnet. classify therefore answers only
// "which of our event names is this", never "is this ours" — that is
// [Decoder.Matches]'s job and it answers it from contract identity.
func classify(e *events.Event) string {
	if len(e.Topic) != 1 {
		return ""
	}
	switch e.Topic[0] {
	case TopicSymbolSwap:
		return EventSwap
	case TopicSymbolMint:
		return EventMint
	case TopicSymbolBurn:
		return EventBurn
	case TopicSymbolCollect:
		return EventCollect
	case TopicSymbolInit:
		return EventInit
	case TopicSymbolUpgraded:
		return EventUpgraded
	case TopicSymbolMigrated:
		return EventMigrated
	case TopicSymbolPoolCreated:
		return EventPoolCreated
	}
	return ""
}
