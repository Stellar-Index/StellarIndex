// Package defindex decodes Soroban events from both layers of paltalabs'
// DeFindex protocol:
//
//  1. STRATEGY layer: Blend autocompound strategy contracts. Topic[0] =
//     ScvString("BlendStrategy"); body { from: Address, amount: i128 } where
//     `from` is the VAULT contract, not the end user.
//  2. VAULT layer: wrapper contracts users call directly. Topic[0] =
//     ScvString("DeFindexVault"); the body carries the end-user G-strkey,
//     multi-asset `amounts` (Vec<i128>) and share deltas (i128).
//
// Both layers are needed: end-user attribution lives in the 100+ factory-deployed wrappers.
// Topic only classifies an event; a match also requires contract identity (ADR-0035/0040).
// Flows come only from a registered vault or strategy, factory events only from
// MainnetFactories, and an unregistered emitter fails closed.
//
// Flows are attribution only: never price-discovery, never in VWAP. Out of scope: factory
// `create`/`n_fee`, vault `rebalance` and `n_wasm` (docs/operations/wasm-audits/defindex.md).
package defindex

import (
	"errors"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// SourceName is the registry key for this source; it stays "defindex" so registry, genesis and
// status-page keys stay stable.
const SourceName = "defindex"

// PrefixStrategy is topic[0] for every Blend strategy event. At 13 chars it exceeds
// `symbol_short!`'s 9-char cap, so it is an ScvString (like Soroswap's "SoroswapPair").
const PrefixStrategy = "BlendStrategy"

// PrefixVault is topic[0] for every DeFindex vault-wrapper event (user-facing layer).
// Also 13 chars, so also ScvString-encoded; emitted on every user deposit/withdraw with a
// G-strkey `depositor`/`withdrawer`.
const PrefixVault = "DeFindexVault"

// PrefixFactory is topic[0] for the DeFindex *factory* contract's events: `create` once per
// vault spawn and `n_fee` on fee-recipient governance updates. ScvString-encoded.
//
// Bodies are classified but not decoded: they drive no Trade or flow record. classify() must
// still enumerate every topic the source can emit (EVERY-event policy) so the dispatcher's
// drop-counter does not file factory events as "unmatched topic".
const PrefixFactory = "DeFindexFactory"

// Topic[1] symbols for the user-facing flow events we decode. The vault and strategy layers
// share `deposit` and `withdraw`, so neither needs its own constants.
const (
	EventDeposit  = "deposit"
	EventWithdraw = "withdraw"
	// Strategy-layer yield event: no canonical Trade, but it decodes as a
	// StrategyFlow (DirectionHarvest), kept apart from user deposits and
	// withdrawals.
	EventHarvest = "harvest"
	// Vault-layer governance / admin events (WASM audit doc). `dfees` is modelled as [DFee] and
	// the seven role/pause/rescue topics as [VaultAdmin]; `rebalance` is classification-only.
	EventRescue    = "rescue"
	EventPaused    = "paused"
	EventUnpaused  = "unpaused"
	EventNReceiver = "nreceiver"
	EventNManager  = "nmanager"
	EventNEManager = "nemanager"
	EventRBManager = "rbmanager"
	EventDFees     = "dfees"
	EventRebalance = "rebalance"
	// EventNWasm is the n_wasm vault topic, likely a WASM-upgrade announcement ("new wasm") by
	// the n_receiver/n_manager naming convention. That meaning is inferred from the name, not a
	// captured body: array-index filters on contract_events have no supporting skip index and
	// time out. Recognised only so classifyVault's drop-counter does not file it as "unmatched
	// topic" (EVERY-event policy); no decoder or flow yet.
	EventNWasm = "n_wasm"
	// Factory-layer events. `create` fires once per vault spawn (its body does NOT carry the new
	// vault address); `n_fee` fires on fee-recipient governance updates.
	EventCreate = "create"
	EventNFee   = "n_fee"
)

// Pre-encoded base64 SCVal blobs, byte-identical to what the contract emits, so the hot path
// classifies by byte equality without parsing events we do not decode. TestGolden_symbolBytes
// in internal/scval catches an SDK encoder shift.
var (
	TopicPrefixStrategy = scval.MustEncodeString(PrefixStrategy)
	TopicPrefixVault    = scval.MustEncodeString(PrefixVault)
	TopicPrefixFactory  = scval.MustEncodeString(PrefixFactory)
	TopicSymbolDeposit  = scval.MustEncodeSymbol(EventDeposit)
	TopicSymbolWithdraw = scval.MustEncodeSymbol(EventWithdraw)
	// Further topic[1] symbols. rebalance, n_wasm and the factory
	// symbols are classification-only; the rest have decoders.
	TopicSymbolHarvest   = scval.MustEncodeSymbol(EventHarvest)
	TopicSymbolRescue    = scval.MustEncodeSymbol(EventRescue)
	TopicSymbolPaused    = scval.MustEncodeSymbol(EventPaused)
	TopicSymbolUnpaused  = scval.MustEncodeSymbol(EventUnpaused)
	TopicSymbolNReceiver = scval.MustEncodeSymbol(EventNReceiver)
	TopicSymbolNManager  = scval.MustEncodeSymbol(EventNManager)
	TopicSymbolNEManager = scval.MustEncodeSymbol(EventNEManager)
	TopicSymbolRBManager = scval.MustEncodeSymbol(EventRBManager)
	TopicSymbolDFees     = scval.MustEncodeSymbol(EventDFees)
	TopicSymbolRebalance = scval.MustEncodeSymbol(EventRebalance)
	TopicSymbolNWasm     = scval.MustEncodeSymbol(EventNWasm)
	TopicSymbolCreate    = scval.MustEncodeSymbol(EventCreate)
	TopicSymbolNFee      = scval.MustEncodeSymbol(EventNFee)
)

// StrategyFlow is one Blend strategy deposit, withdraw or harvest. Both flow directions share
// the body `{from, amount}`, so one struct carries a Direction discriminator.
//
// From is typically the vault/router *contract* address (C-strkey), not the end user; user
// attribution needs the same-tx vault event. It can also be a G-strkey; scval.AsAddressStrkey
// renders both. Amount is a canonical.Amount (i128, never truncated; ADR-0003).
type StrategyFlow struct {
	Source     string
	Ledger     uint32
	ClosedAt   time.Time
	TxHash     string
	OpIndex    int
	EventIndex uint32 // in-tx contract event index — per-flow PK discriminator (migration 0055)
	ContractID string // the BlendStrategy contract that emitted
	Direction  Direction
	From       string           // account (G…) or contract (C…) strkey
	Amount     canonical.Amount // underlying-asset delta (i128)
}

// Direction discriminates the two flow types.
type Direction string

const (
	DirectionDeposit  Direction = "deposit"
	DirectionWithdraw Direction = "withdraw"
	// DirectionHarvest is strategy yield realised into the vault, not a user flow. Position sums
	// exclude it (their CASE arms name deposit/withdraw only); NAV math must include it or it
	// under-counts by the harvested yield.
	DirectionHarvest Direction = "harvest"
)

// RebalanceMethod is the sub-type of a ("DeFindexVault","rebalance") event: four actions share
// the topic, discriminated by a `rebalance_method` Symbol in the body.
//
// Field name and values come from upstream research, not an on-chain sample (the lake holds zero
// rebalance emits). [DecodeRebalanceMethod] returns the raw Symbol so a real sample can confirm
// the spelling; per-method payloads are not modelled because layouts must not be invented.
type RebalanceMethod string

const (
	// RebalanceMethodField is the body Map key carrying the discriminator Symbol (unconfirmed on-chain).
	RebalanceMethodField = "rebalance_method"

	// The four documented methods (spelling unconfirmed): for comparison and reporting only; the
	// decoder returns whatever the contract emits.
	RebalanceUnwind       RebalanceMethod = "unwind"
	RebalanceInvest       RebalanceMethod = "invest"
	RebalanceSwapExactIn  RebalanceMethod = "SwapExactIn"
	RebalanceSwapExactOut RebalanceMethod = "SwapExactOut"
)

// Known reports whether m is one of the four documented rebalance methods. False on a real event
// means upstream added or renamed one: recognised, sub-type unmodelled, no flow.
func (m RebalanceMethod) Known() bool {
	switch m {
	case RebalanceUnwind, RebalanceInvest, RebalanceSwapExactIn, RebalanceSwapExactOut:
		return true
	default:
		return false
	}
}

// Event wraps a StrategyFlow so it satisfies consumer.Event for the
// dispatcher / pipeline path. The sink persists it to defindex_flows
// with layer='strategy'.
type Event struct {
	Flow StrategyFlow
}

// EventKind implements [consumer.Event].
func (e Event) EventKind() string {
	return "defindex.strategy." + string(e.Flow.Direction)
}

// Source implements [consumer.Event].
func (e Event) Source() string { return SourceName }

// VaultFlow is one user-facing vault-wrapper deposit or withdraw. Each user deposit yields one
// VaultFlow, one StrategyFlow and one Blend Pool supply event in the same tx (correlate by
// tx_hash + op_index).
//
// User is the end user: a G-strkey, or a C-strkey when routed via an aggregator; the strategy
// layer's `from` is always the vault. Amounts is a Vec because vaults can be multi-asset, so
// the decoder assumes no length. DfTokens is the share-token delta, i128 per ADR-0003.
type VaultFlow struct {
	Source     string
	Ledger     uint32
	ClosedAt   time.Time
	TxHash     string
	OpIndex    int
	EventIndex uint32 // in-tx contract event index — per-flow PK discriminator (migration 0055)
	ContractID string // the DeFindex vault-wrapper contract
	Direction  Direction
	User       string             // depositor (G…) or withdrawer; may be C-strkey
	Amounts    []canonical.Amount // underlying-asset delta vec (i128 each)
	DfTokens   canonical.Amount   // share-token delta — mint on deposit, burn on withdraw
}

// VaultEvent wraps a VaultFlow for the dispatcher / pipeline path.
type VaultEvent struct {
	Flow VaultFlow
}

// EventKind implements [consumer.Event].
func (e VaultEvent) EventKind() string {
	return "defindex.vault." + string(e.Flow.Direction)
}

// Source implements [consumer.Event].
func (e VaultEvent) Source() string { return SourceName }

// DFee is one per-asset entry of a vault-layer `dfees` protocol-fee distribution. The body
// shape is taken from real lake blobs:
//
//	Map{ distributed_fees: Vec[ (token Address<contract>, amount i128) ] }
//
// Entries are per-asset, not per-recipient: each names the fee token contract and the i128
// amount distributed in it. An empty Vec is a real shape (nothing to distribute) and emits zero
// events. dfees fires in the same op as the vault deposit/withdraw flow, so it lands in its own
// table (defindex_fees) with FeeIndex discriminating the one-event to N-entries fan-out.
type DFee struct {
	Vault      string // the emitting DeFindex vault-wrapper contract C-strkey
	Ledger     uint32
	ClosedAt   time.Time
	TxHash     string
	OpIndex    int
	EventIndex uint32           // in-tx contract event index — PK discriminator vs the same-op vault flow
	FeeIndex   int              // position in the distributed_fees Vec — per-entry PK discriminator
	Token      string           // fee token contract C-strkey
	Amount     canonical.Amount // distributed amount in Token (i128, never truncated — ADR-0003)
}

// DFeesEvent wraps one DFee. The decoder emits ONE per distributed_fees entry so the ADR-0033
// reconcile's expected event count equals the defindex_fees row count 1:1.
type DFeesEvent struct {
	Fee DFee
}

var _ consumer.Event = DFeesEvent{}

// EventKind implements [consumer.Event].
func (e DFeesEvent) EventKind() string {
	return "defindex.vault." + EventDFees
}

// Source implements [consumer.Event].
func (e DFeesEvent) Source() string { return SourceName }

// VaultAdmin is one vault-layer governance event: a role rotation (nmanager / nemanager /
// rbmanager / nreceiver), a strategy pause toggle or an emergency rescue. A field a kind's body
// does not carry stays empty (nil for Amount).
type VaultAdmin struct {
	Vault      string // the emitting DeFindex vault-wrapper contract C-strkey
	Ledger     uint32
	ClosedAt   time.Time
	TxHash     string
	OpIndex    int
	EventIndex uint32
	Kind       string            // topic[1] symbol verbatim (EventRescue … EventRBManager)
	Caller     string            // rescue / paused / unpaused / nreceiver
	Strategy   string            // rescue / paused / unpaused: the strategy acted on
	NewAddress string            // nreceiver / nmanager / nemanager / rbmanager: the new role holder
	Amount     *canonical.Amount // rescue only: amount_withdrawn (i128, ADR-0003)
}

// AdminEvent wraps one VaultAdmin for the dispatcher / pipeline path.
type AdminEvent struct {
	Admin VaultAdmin
}

var _ consumer.Event = AdminEvent{}

// EventKind implements [consumer.Event]. One kind for all seven topics
// (the row's event_kind column carries the topic), so the reconcile
// target maps 1:1 onto defindex_admin_events.
func (e AdminEvent) EventKind() string { return "defindex.vault.admin" }

// Source implements [consumer.Event].
func (e AdminEvent) Source() string { return SourceName }

// Errors returned by the decode path. Callers classify via
// errors.Is.
var (
	// ErrUnknownEvent — topic shape matches no decoded event. The dispatcher's drop-counter records
	// these; a strategy emitting an event we do not decode is normal, not a failure.
	ErrUnknownEvent = errors.New("defindex: unknown strategy event topic")

	// ErrMalformedPayload — event body doesn't match the expected
	// {from, amount} schema (missing field, wrong type).
	ErrMalformedPayload = errors.New("defindex: malformed event payload")
)

// MainnetFactories is the DeFindex factory trust-root SET (ADR-0035: multi-factory is
// universal). CDKFHFJI... is the current factory; the other three are earlier deployments
// whose vaults still hold funds and emit events.
var MainnetFactories = []string{
	"CDKFHFJIET3A73A2YN4KV7NSV32S6YGQMUFH3DNJXLBWL4SKEGVRNFKI", // current (108 events, 57.06M → 62.97M)
	"CDHPT7OBQKIUFHIJMLI4W7TNOQUHEVOOVMCW7HA4O5SPFNLDRCE6DQ5F", // 10 events, 60.95M → 60.97M (n_fee only)
	"CAVP2QLPIG7FQNHI57KXF7KS6NIAAUQKHZZDM3AGVADE64WHFBC5YURX", // earliest (3 creates, 55.48M → 55.51M)
	"CDOIC7245ONYVOTEDLGKUM263EQ7SEEQ74ZQCN4SSH4TSYXOCMU6254O", // 2 creates, 56.89M → 56.93M
}

// GenesisLedger is the earliest ledger any routed defindex contract has
// data: CAVP2QLP…'s first factory `create`. It is the density denominator
// (ADR-0031) and gap-detector floor; a later value hides early-history gaps.
const GenesisLedger uint32 = 55_484_403

// MainnetVaults is the curated gated vault-wrapper set (ADR-0040 section 1, mechanism 2). The
// factory `create` event does not carry the new vault's address, so the deploy graph cannot
// self-register vaults and this in-code seed is the trust root. Every entry carries at least
// one independent proof, recorded per-contract in docs/protocols/defindex.md:
//
//	A. first event inside a factory create transaction;
//	B. listed in a factory create event body (strategies);
//	C. runs the team-published vault WASM;
//	D. listed in the team's own Dune vault registry.
//
// Emitters with no proof are deliberately excluded. A real vault missing here fail-closes into
// a visible ADR-0033 recognition gap, never silent mis-attribution; the unblock is an operator
// INSERT into protocol_contracts or extending this seed.
var MainnetVaults = []string{
	"CA25XTGHKQ6PUMFJ4SDNRFMUABIFX46U7VAZBFDZKAOX5C3KZXUAR2KQ",
	"CA2FIPJ7U6BG3N7EOZFI74XPJZOEOD4TYWXFVCIO5VDCHTVAGS6F4UKK",
	"CA4ZXVRFEB4QGN2CTIY57B3FF3AEOMWZYW3CJHDCI23VVOGJTR3L4SBR",
	"CA5RG7DCLMNJFRMG3LP2VDUBWCZ4QTZ776VCEQKWBPGDUAJAT26K2OXM",
	"CA7AURROFFMNWA4GE6LUEBXNIGUUGSSQYDCEBGJVUXL3UZZ4JHH7NGYC",
	"CAARFNLSJSACT7OWWJP6H5KFFD7T4BWL67OT5K3RRULHGW3C5DZP6Y6D",
	"CAB4JOLSCNELJVDQKZLVGHKWJCLXFDBZZMITJAFL4GBGTHIKWO47PYFH",
	"CAEPJIHET2TBI2VCLJZI6QHMN366KUGNK4AOKE3YY7AOKMU4KX4RDRGB",
	"CAFI7WOCU33VOVTTORUFGRLBT43LJUY62CGHLVKUA5XWUGXMK7CEHQP6",
	"CAFIN4DOSWRGSZO454VKGTVUQRZ4IEJMJJZKOND3Z3HWDY7YC6YY7JLF",
	"CAGERKFCDHHCES64L43EU242KIVQMPYAL37CFYIGMLBGJIQTYWXFRWIT",
	"CAGP3V7WKOJX2W3J24OZQRTGPZFPW2DXVIHY2TDP7CUPM6CKAL5WR4LU",
	"CAHGILQRWEGTAWIYGLVFKFPRPNH4NN6KZIDBGHMWABIWZZLW2ZHLHQOG",
	"CAIFV6BSPN2UHGDSOJK7RLOEVBLQX6EAGIVJWVWSEI7ROLUGI3U2XDTP",
	"CAIZ3NMNPEN5SQISJV7PD2YY6NI6DIPFA4PCRUBOGDE4I7A3DXDLK5OI",
	"CAK6SYGM3GXIHEBXI4FDCW47KL4QH7WYAOJQL4FFC4SVXE4AF3HWYJJP",
	"CAKHX4CGFV56MFAMMMNYBEX3IUYFDVTMVZXIZRM3BBQMDRDYZLAJVRNF",
	"CANBU7T77SCJOOAU6VQAOGR7DN36JBQFUN56XS2WA2VPJYUSRUBIPYDS",
	"CAQ6PAG4X6L7LJVGOKSQ6RU2LADWK4EQXRJGMUWL7SECS7LXUEQLM5U7",
	"CAVL4BSHMU5ECWZCB6ETYSBV4EWTRMHAGMVUEJ5PXM3P3E3AOJPX2TLU",
	"CAWM7NKSYG2ITJW2MYYJWJ5ULGCJLDB6MXZIWPL3VPRG5TDVLJ66IMWR",
	"CAXRLUOSI7DL3SYNZW5UGRIPVNRKKSZTW35OX5DWKZSJ4PFEVA2VEFCQ",
	"CB3FUMFGCF6DHSFK6N2TOKHRMYXS34HFKQR45UKVORCRUM35AF3ES7WQ",
	"CB3LKO733H3STIDCKWY4H25FH426HA7WSERMJ3CZBQTKPOESKZ7LGOWA",
	"CB5YXWIDBQAOTTPEQE3SRNUFM2PTOXFHKGUWCBJJSF2GPW37DN725FDA",
	"CBAU3UYY3WMTUZ23XS7I4YXU63GCKXFHO26MNZ7RPIRXP3YUJMYGJRAV",
	"CBBAH2OAJ6N3UBJGXNFYH4QF6C6OWO4RHGGOOGDER3IJB7SGLR3Y56JO",
	"CBCDAA5URMD55FQ5UQX3SCXLJTJQ35FNZHKNICVMAWLPSTJOX4BITYD6",
	"CBDZ2L4HHEPPL4ABHPORQC72E5S2GLNRPJ467XV3CW5FDWICUNH6SF4B",
	"CBDZYJVQJQT7QJ7ZTMGNGZ7RR3DF32LERLZ26A2HLW5FNJ4OOZCLI3OG",
	"CBGC65JVYZZTGPVHURM32GFMMTUQJZVRTB6QAQDLNP4FG3LVFS5XJ7L2",
	"CBGE43WF5GBDCHMN2XPKIAC7TYMWCR6FOJTVFMBR6QQM6WKZB7BM23LL",
	"CBHB2G4TMSVWE4YFDTFYRYNCP5KUT6RQVWQGIM4LQO2IKKHVDB7N5JJQ",
	"CBHK3QURQ7OFTNMYQS7TBKPRWN3QDGNLIGANFGUFHG2SEE7WRJEMWWGE",
	"CBM3VVKTQJBHI2LCZCVFJZCKX7UECRFE2MYBUKAMBBSGC4DB2CUO25IB",
	"CBMERS7MJHO6TGKUVWWU34ZSKWCFOWPG2ZCIRIT75IC3YDWBIPBMV5LB",
	"CBNKCU3HGFKHFOF7JTGXQCNKE3G3DXS5RDBQUKQMIIECYKXPIOUGB2S3",
	"CBP2R5KYAWJCOCVDTSNTEVL3O6JBTWOOH7SZOX7DX5DLGVZCAMLBDZM3",
	"CBSU24OXATTHBPNLWVEXIN3OZZSONBGVH6J4S3STQMR27ZR54MD4WEOL",
	"CBUJZL5QAD5TOPD7JMCBQ3RHR6RZWY34A4QF7UHILTDH2JF2Z3VJGY2Y",
	"CBWSIUHTONZRZJSJS7XABKCBMNGDVWH6UMCW665A5OIRJ7FGCJM6F2VP",
	"CBYTDU4JKTMFG5CNIUYJTOVMNIN5ADU2PDG4QWIUVT6SSVK3VTXYTF4K",
	"CBZWNB2B7TNLCSFDOXZ24F54CWWUPDQ76DSHFLHBYC4KSPBCFHOHNYYN",
	"CC24OISYJHWXZIFZBRJHFLVO5CNN3PQSKZE5BBBZLSSI5Z23TKC6GQY2",
	"CC3JKADE5M5KYIHLSS2GB55AT7FIO2VHQITUGMHS6NHDPVSCVDT3UMBX",
	"CC3KWIAIQGHJL7CRUBZMUO5R2M4IV352ACE2MLAI2B3236GGJ7X6Z5E7",
	"CC67IVEYVNW2TC7ELFDNQP2IYSYH6LJDWH6MNP2WOSWEQFXJQZZRN2I5",
	"CC767WIU5QGJMXYHDDYJAJEF2YWPHOXOZDWD3UUAZVS4KQPRXCKPT2YZ",
	"CCA2ZJP5BVRXYTQH4FAGHCAUMRYCXVC4CRYC2NXHWMR7TIVX36U7F5HR",
	"CCAOBAAWWWP7R24ZZ5S2C7GUUKZTOXPHFNQXYICBXMQZY2IBYNCVOWOL",
	"CCB2AR5X3KP4WQKE7HNSUSDS7SHFMC2WPVSZ2ZXJ6DHXOKHFFKOZE6GK",
	"CCDRFMZ7CH364ATQ5YSVTEJ3G3KPNFVM6TTC6N4T5REHWJS6LGVFP7MY",
	"CCEYLML2C7YLWQA4BAQMZKHA6X7FUBLM5PXWVDOYDOP7TNT54GPHP7ZV",
	"CCFWKCD52JNSQLN5OS4F7EG6BPDT4IRJV6KODIEIZLWPM35IKHOKT6S2",
	"CCIRVAW3IZVAYLHR7YYMZFOQVYEW67OKFFXR3J6ZR2T6YJC5V7GTSNQ5",
	"CCKTLDG6I2MMJCKFWXXBXMA42LJ3XN2IOW6M7TK6EWNPJTS736ETFF2N",
	"CCLJFYWNVMNLF3TFO2AJMYFGMI2EBP5U3BWPOL437IKGHNUJYOWIHTV3",
	"CCM3CKJI7BBMZ357644KLAE6NH4D7JQ6MUJHSV4UBRWJY7IMGHBJRNGR",
	"CCPKQH3K5XUGP5GXCT6WTABS7TGXRR745BJ4MEFSGNATB7AOBRL4VEOT",
	"CCSZUL5AVTHWCDV32HFPKOXRBDWIFJFSRSRYJI64OS2ESMNZDJD7HED3",
	"CCURKY2V3URC6COTTBZZNL33L5AJLXWEIFIKVNCJOSMIJ2KNJQJQTQ6V",
	"CCUZC3HC5TH2VCYZFUG57E6IGKPL45YUN2SI3UEYQUBA7RCYHUIZBSFV",
	"CCV43DIK4TLUHFKNE3XL4QMNU6P7EGKMYNCDFRIB4HX4FTRM2IBDQEPS",
	"CCW3PEFVDPRCLQ7YTSLGC3P37VEU6RZ7M3DEVD4NZSM5ZNVDG4N5NFOI",
	"CD3HR7WNGPDUGK5ITNMZSRM36O2IFJF3N4RFHOITP4DCXMVGHMANN3XR",
	"CD455S6D4A2G36TXWSYUQNDX4YJBFJJSFRSXBSU7H6TVM6FC67ZMIFGQ",
	"CD45F76UVOSMUMSHLP2OMCNF7N662DQA76IM6PP7PWTCITGMZITULHZ7",
	"CD4B5WJDJQ6G5K6MVC3VHTBI2PNLWJBWLXHV75S245Q3PIQWC262UZ4C",
	"CD4JGS6BB5NZVSNKRNI43GUC6E3OBYLCLBQZJVTZLDVHQ5KDAOHVOIQF",
	"CD65RR656FIX5LRC7M5RP46IE2MQFK5OEWRM6L6KLIJVUU222U3PFUAP",
	"CD6GVZTGH7L6NELM2YFCMBF7QAI6DFR25GPOQFKKAZQHVRLB5ZP46CYM",
	"CD7T34Y5SZ6MBEZDMXDIQWQ6JICO7TYH7E6DKZJ7BHXOMR2EQ65WYSZG",
	"CDI2ZW5CKT4OIHX3IGMVJ4VGOH6Z64N2M3URKATYJIX7JRITJFQJPFD7",
	"CDI7QVDTNDFEHB25VFQGMNFALGCXXKAWUSHOTQR2D4O44CATQJ5ZQMN6",
	"CDIHXKZ4PFKAIONK52JAR6ZNMP62F3UP7XTIBSJTQLMLHQ44PQ5Q2H3J",
	"CDKNDBBVLTSO2DSLTZOIF2A4NJWPXTGHD3WYSWBHYBJDKAX4JCKEFMHT",
	"CDONBLOOTYZ7QN62ZLJFHK7CT3JCP3JEZDCRSG3VLGAP73QAXS7HF6HU",
	"CDPJEMZOYZLITC4MRLGJQHPMNCIB3TZ4R42J6M37PWP5Q2FGO4WFIXAD",
	"CDQ272FPRZQFBGUOZSSERBMDD7AYVO3CUOIZYRA7LGDHKU4VO5QWMOR6",
	"CDQPH6PYFPMZU37OBZU44UOTXHCMH2RFRMXZXGOX3KAR6REDSWY6W3KI",
	"CDRSZ4OGRVUU5ONTI6C6UNF5QFJ3OGGQCNTC5UXXTZQFVRTILJFSVG5D",
	"CDSM6RP3GP6MSV7PXN7OSXCJ5EGMSLGLYFJ4QEPPMQWABD5JU5UPAOZM",
	"CDTBBS6KNIWKG6PJUQBWWGBMIE5AANF7CBDT5JRAJDD3L5JHN75LZBET",
	"CDTCSXSKRIFYLDMMF3UABU63LEXSAR2CRCJVSL2PUJGVLNCQWU7XGWCN",
	"CDYH7U7YYI4AGRXK3NFEN637TR6ID7WP2PW47QZ7KSVPU35LB2ZNDIDG",
}

// MainnetStrategies is the curated gated strategy set, on the same evidence chain as
// MainnetVaults. Strategies are deployed independently and referenced by vault configs, so
// appearing in a factory create body (proof B) is the primary signal.
var MainnetStrategies = []string{
	"CA33NXYN7H3EBDSA3U2FPSULGJTTL3FQRHD2ADAAPTKS3FUJOE73735A",
	"CA3SO5RRKOONAPWVR5XY6CMOYZGN4M4QKVIGX5DFRIIJUJW2SFSELBXL",
	"CAHXQWU2HB74PIBT2BUIPYUZXMGZJEQUCNMQLEZR4OMNXMCEHYNEUWZQ",
	"CAZ3LLLKPWEOVK6K4G5NCQ2VXWABLFIPKKNMN5GLKMZKEN7JSKTEMIKN",
	"CB5FP32DQKDA7Z7SJ7DGP2FRJRQLPXBSRMSK2KNLGC3V4SXWIIWLJWKM",
	"CBDOIGFO2QOOZTWQZ7AFPH5JOUS2SBN5CTTXR665NHV6GOCM6OUGI5KP",
	"CBTSRJLN5CVVOWLTH2FY5KNQ47KW5KKU3VWGASDN72STGMXLRRNHPRIL",
	"CBTX63BX2I6E2VG2SMFQXDHLAPDOANUWBTMXQNWBV2FT6DIMVQPCSOBW",
	"CBWD2EKIMVG6PM6VZEOCFJ3HPRZ2MDOIT3JCCK6WX2JURISQ3LWWBUIT",
	"CC5CE6MWISDXT3MLNQ7R3FVILFVFEIH3COWGH45GJKL6BD2ZHF7F7JVI",
	"CCBTSHPUVNKCT5V675AAVYNANHXBU26PTZK2QLS7ZLFNYRJZT5HW3VL6",
	"CCSRX5E4337QMCMC3KO3RDFYI57T5NZV5XB3W3TWE4USCASKGL5URKJL",
	"CDB2WMKQQNVZMEBY7Q7GZ5C7E7IAFSNMZ7GGVD6WKTCEWK7XOIAVZSAP",
	"CDDXPBOF727FDVTNV4I3G4LL4BHTJHE5BBC4W6WZAHMUPFDPBQBL6K7Y",
	"CDPWNUW7UMCSVO36VAJSQHQECISPJLCVPDASKHRC5SEROAAZDUQ5DG2Z",
	"CDSCVJHJWUZQMR64FVK3XMND5NKSN7Z23KPRCHKFHVGOEJBWPVH5B5XA",
}

// MainnetGatedSet is the full curated child set the decoder seeds:
// vault wrappers + strategies. The factories are the trust roots
// (WithFactories), not children.
func MainnetGatedSet() []string {
	out := make([]string, 0, len(MainnetVaults)+len(MainnetStrategies))
	out = append(out, MainnetVaults...)
	out = append(out, MainnetStrategies...)
	return out
}
