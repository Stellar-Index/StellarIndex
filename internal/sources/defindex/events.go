// Package defindex decodes Soroban contract events emitted by both
// layers of paltalabs' DeFindex protocol on Stellar mainnet:
//
//  1. STRATEGY layer — Blend autocompound *strategy* contracts that
//     hold the underlying lending position. Topic[0] =
//     ScvString("BlendStrategy"). Body { from: Address, amount: i128 }.
//     `from` here is the VAULT contract (a C-strkey), not the end
//     user — useful for capital-flow attribution between layers.
//
//  2. VAULT layer — DeFindex *vault wrapper* contracts that users
//     interact with directly. Topic[0] = ScvString("DeFindexVault").
//     Body has the end-user G-strkey (`depositor` / `withdrawer`),
//     multi-asset amounts (`amounts` / `amounts_withdrawn`,
//     Vec<i128>) and share-token deltas (`df_tokens_minted` /
//     `df_tokens_burned`, i128).
//
// Phase A (2026-05-19) shipped only the strategy layer because the
// initial WASM walk confirmed only the strategy WASM
// (`11329c24…988`) on the 3 named "fixed strategy" vault contracts
// in `mainnet.contracts.json`. That walk MISSED the wrapper
// contracts deployed by the factory (different WASM `ae3409a4…468b`
// or its upgraded `07097f83…84b0`); we now know there are 100+
// such wrappers spawned over the protocol's life (factory
// `CDKFHFJI…NFKI` emits one `create` event per spawn). The vault
// wrappers ARE where end-user attribution lives, and missing them
// is what the 2026-05-21 cross-check vs Soroban RPC revealed
// (~27% coverage in a 12-hour sample; pre-rc.63 walker only 14%).
//
// Phase B (this revision, 2026-05-21) adds the DeFindexVault
// topic-match. Dispatch is still PURELY by topic — we don't
// hardcode any contract addresses — so any current or future
// DeFindex vault wrapper, whether listed in mainnet.contracts.json
// or spawned later, gets decoded automatically. This mirrors the
// comet/aquarius shared-emitter topology elsewhere in the codebase.
//
// We surface vault + strategy deposit/withdraw events for flow
// attribution only — they are NOT price-discovery events and never
// contribute to VWAP. Out of scope here: factory `create`/`n_fee`
// events, strategy `harvest` events, vault `rebalance`/admin events
// — all flagged in docs/operations/wasm-audits/defindex.md as
// Phase-B-or-later follow-ups.
//
// See README.md for scope.
package defindex

import (
	"errors"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// SourceName is the registry key for this source. Kept as
// "defindex" (rather than renamed to e.g. "blend-strategy") so the
// registry / genesis / status-page keys stay stable; a rename is a
// separate product-taxonomy decision tracked in defindex.md.
const SourceName = "defindex"

// PrefixStrategy is topic[0] for every Blend strategy event. It is
// 13 chars, exceeds `symbol_short!`'s 9-char cap, so the SDK
// serialises it as ScvString (same pattern as Soroswap's
// "SoroswapPair"). Confirmed on-chain via scan-soroban-events.
const PrefixStrategy = "BlendStrategy"

// PrefixVault is topic[0] for every DeFindex vault-wrapper event
// (user-facing layer). Also 13 chars, also ScvString-encoded.
// Confirmed on-chain via Soroban-RPC getEvents on a known wrapper
// (CCA2ZJP5… runs WASM ae3409a4…468b, emits this topic on every
// user deposit/withdraw with a G-strkey `depositor`/`withdrawer`).
const PrefixVault = "DeFindexVault"

// PrefixFactory is topic[0] for the DeFindex *factory* contract's
// events. The factory (`CDKFHFJI…NFKI`) emits `create` once per
// vault spawn and `n_fee` on protocol-fee-recipient governance
// updates. 15 chars → ScvString-encoded (same shape as
// PrefixStrategy / PrefixVault).
//
// We classify these but do NOT decode bodies today — they don't
// drive a Trade or a flow record. Recognising them is what the
// EVERY-event policy requires (project_every_event_principle):
// classify() must enumerate every topic the source can emit so the
// dispatcher's drop-counter doesn't silently file factory events as
// "unmatched topic." Body decode is Phase-C scope (would give us a
// live notification feed for new wrapper deployments).
const PrefixFactory = "DeFindexFactory"

// Topic[1] symbols for the user-facing flow events we decode. The
// strategy contract publishes more (harvest / keeper admin / …);
// Phase A only decodes deposit + withdraw at the strategy layer.
// The vault layer reuses the same two symbols (`deposit`,
// `withdraw`) — they're shared between layers, so Phase B doesn't
// need new symbol constants.
const (
	EventDeposit  = "deposit"
	EventWithdraw = "withdraw"
	// Strategy-layer governance / yield events that don't produce a
	// canonical Trade today but are valid topics to recognise so
	// classify() enumerates the full upstream event surface per the
	// EVERY-event policy (project_every_event_principle).
	EventHarvest = "harvest"
	// Vault-layer governance / admin events. Per Phase-B audit doc:
	//   rescue, paused, unpaused, nreceiver, nmanager, nemanager,
	//   rbmanager, dfees, rebalance
	// Of these only `rebalance` is multiplexed (four bodies share the
	// topic — discriminate by `rebalance_method` Symbol in body).
	EventRescue    = "rescue"
	EventPaused    = "paused"
	EventUnpaused  = "unpaused"
	EventNReceiver = "nreceiver"
	EventNManager  = "nmanager"
	EventNEManager = "nemanager"
	EventRBManager = "rbmanager"
	EventDFees     = "dfees"
	EventRebalance = "rebalance"
	// EventNWasm — ROADMAP #89 residual (2026-07-10): a read-only lake
	// topic census against the gated vault set found 2 real n_wasm
	// events classifyVault didn't recognize. Likely a WASM-upgrade
	// announcement ("new wasm"), matching the n_receiver/n_manager/
	// n_emanager "new-X" naming convention among the vault's other
	// admin topics — but that reading is inferred from the name, not
	// from a captured body (2 lake occurrences was too small a count
	// to justify the ClickHouse scan cost to pull them: several
	// contract_id + array-index-filtered queries against the raw
	// 233M-row contract_events table each timed out past 400s without
	// a supporting skip index on non-contract_id predicates). Recognised
	// so classifyVault's drop-counter doesn't file it as "unmatched
	// topic" (EVERY-event policy) — no decoder/flow yet, same as the
	// other 9 admin topics below.
	EventNWasm = "n_wasm"
	// Factory-layer events. `create` fires once per vault spawn
	// (body holds roles / vault_fee / assets but NOT the new vault
	// address — see audit doc "Surprising gotcha #2"); `n_fee`
	// fires on protocol-fee-recipient governance updates.
	EventCreate = "create"
	EventNFee   = "n_fee"
)

// Pre-encoded base64 SCVal blobs — byte-identical to what the
// contract emits — for cheap byte-equality classification on the
// hot path (no SCVal parsing for events we don't decode).
//
// Golden wire-format regression covered by
// internal/scval/scval_test.go::TestGolden_symbolBytes — if the SDK
// encoder shifts under us, that test fires before this ships.
var (
	TopicPrefixStrategy = scval.MustEncodeString(PrefixStrategy)
	TopicPrefixVault    = scval.MustEncodeString(PrefixVault)
	TopicPrefixFactory  = scval.MustEncodeString(PrefixFactory)
	TopicSymbolDeposit  = scval.MustEncodeSymbol(EventDeposit)
	TopicSymbolWithdraw = scval.MustEncodeSymbol(EventWithdraw)
	// Classification-only topic[1] symbols (no decoder today).
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

// StrategyFlow is the canonical wire shape for one Blend strategy
// deposit or withdraw. Both directions share an identical body
// (`{from, amount}` — verified on-chain), so a single struct with a
// Direction discriminator is the natural shape.
//
// From is the caller moving capital — for these strategies it is
// typically the vault/router *contract* address (a C-strkey), not
// the end-user; end-user attribution requires correlating with the
// same-tx vault event (a Phase-B follow-up). It can also be a
// plain account G-strkey; scval.AsAddressStrkey renders both.
//
// Amount is the underlying-asset delta as a big-int-backed
// canonical.Amount (i128, never truncated — ADR-0003).
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
)

// RebalanceMethod is the multiplexed sub-type of a
// ("DeFindexVault","rebalance") event. Four rebalance actions share
// the single `rebalance` topic; per docs/operations/wasm-audits/
// defindex.md (surprising-gotcha #3) they are discriminated by a
// `rebalance_method` Symbol field inside the event body.
//
// SCOPE / HONESTY (do-not-invent; CLAUDE.md "Soroban DeFi contracts
// upgrade in place"): the discriminator FIELD NAME + method values
// below come from upstream research, NOT from an observed on-chain
// sample — the r1 lake has ZERO ("DeFindexVault","rebalance") emits
// as of 2026-07-06, so this is forward-looking scaffolding.
// [DecodeRebalanceMethod] reads only this one discriminator field and
// returns the raw Symbol verbatim (so a real sample validates the
// exact wire spelling — snake_case vs CamelCase is unconfirmed); the
// per-method PAYLOAD (amounts, swap legs, routes) is deliberately NOT
// modelled because inventing field layouts is forbidden. When a real
// rebalance sample lands, the per-method decode slots in behind the
// discriminator here.
type RebalanceMethod string

const (
	// RebalanceMethodField is the body Map key that carries the
	// discriminator Symbol. Documented, UNCONFIRMED on-chain.
	RebalanceMethodField = "rebalance_method"

	// The four documented methods. Wire spelling unconfirmed — see the
	// type godoc. The decoder returns whatever the contract emits; these
	// constants are for comparison/reporting, not assumed by the decode.
	RebalanceUnwind       RebalanceMethod = "unwind"
	RebalanceInvest       RebalanceMethod = "invest"
	RebalanceSwapExactIn  RebalanceMethod = "SwapExactIn"
	RebalanceSwapExactOut RebalanceMethod = "SwapExactOut"
)

// Known reports whether m is one of the four documented rebalance
// methods. A false here on a real event means the upstream contract
// added or renamed a method — recognised (topic classified) but its
// sub-type is unmodelled, so it is handled as "recognised, no flow".
func (m RebalanceMethod) Known() bool {
	switch m {
	case RebalanceUnwind, RebalanceInvest, RebalanceSwapExactIn, RebalanceSwapExactOut:
		return true
	default:
		return false
	}
}

// Event wraps a StrategyFlow so it satisfies consumer.Event for the
// dispatcher / pipeline path. Log-only sink for now; a per-flow
// persist hypertable is a Phase-C follow-up (see audit doc).
type Event struct {
	Flow StrategyFlow
}

// EventKind implements [consumer.Event].
func (e Event) EventKind() string {
	return "defindex.strategy." + string(e.Flow.Direction)
}

// Source implements [consumer.Event].
func (e Event) Source() string { return SourceName }

// VaultFlow is the canonical wire shape for one user-facing
// DeFindex *vault wrapper* deposit or withdraw — what end users
// see when they interact with the protocol. Distinct from
// StrategyFlow (the underlying strategy-layer flow that fires from
// the strategy contract with `from` = vault address); each user
// deposit produces one VaultFlow + one StrategyFlow + one Blend
// Pool supply event in the same tx (correlate by tx_hash +
// op_index).
//
// User is the end user moving capital — a G-strkey for direct
// interactions, occasionally a C-strkey if the user came via
// another aggregator/router. The vault layer is where actual
// end-user attribution lives (the strategy layer's `from` is
// always the vault contract).
//
// Amounts is a Vec because DeFindex supports multi-asset vaults
// (one Vec entry per asset in the vault's basket). The
// `mainnet.contracts.json` Phase-A trio (USDC / EURC / XLM blend
// autocompound) are all single-asset (vec length 1), but the
// etherfuse-strategy variants (cetes, ustry, tesouro) may have
// multiple — the decoder makes no length assumption.
//
// DfTokens is the share-token delta — `df_tokens_minted` (deposit)
// or `df_tokens_burned` (withdraw). i128, ADR-0003 (never
// truncated).
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

// Errors returned by the decode path. Callers classify via
// errors.Is.
var (
	// ErrUnknownEvent — topic shape doesn't match a deposit/withdraw
	// BlendStrategy event. The dispatcher's drop-counter records
	// these; not a failure ("strategy emits an event we don't
	// decode" — harvest / keeper admin — is normal).
	ErrUnknownEvent = errors.New("defindex: unknown strategy event topic")

	// ErrMalformedPayload — event body doesn't match the expected
	// {from, amount} schema (missing field, wrong type).
	ErrMalformedPayload = errors.New("defindex: malformed event payload")
)

// MainnetFactories is the DeFindex factory trust-root SET (ADR-0035:
// multi-factory is universal — DeFindex has been redeployed; all four
// emit DeFindexFactory events in the r1 lake). CDKFHFJI… is the
// current factory (also the one the team's own
// mainnet.contracts.json names); the other three are earlier
// deployments whose vaults still hold funds and emit events.
// Lake-verified 2026-07-05: no fifth DeFindexFactory emitter exists.
var MainnetFactories = []string{
	"CDKFHFJIET3A73A2YN4KV7NSV32S6YGQMUFH3DNJXLBWL4SKEGVRNFKI", // current (108 events, 57.06M → 62.97M)
	"CDHPT7OBQKIUFHIJMLI4W7TNOQUHEVOOVMCW7HA4O5SPFNLDRCE6DQ5F", // 10 events, 60.95M → 60.97M (n_fee only)
	"CAVP2QLPIG7FQNHI57KXF7KS6NIAAUQKHZZDM3AGVADE64WHFBC5YURX", // earliest (3 creates, 55.48M → 55.51M)
	"CDOIC7245ONYVOTEDLGKUM263EQ7SEEQ74ZQCN4SSH4TSYXOCMU6254O", // 2 creates, 56.89M → 56.93M
}

// MainnetVaults is the curated gated vault-wrapper set (ADR-0040
// §1 mechanism 2 — curated-set registry). The factory `create`
// event does NOT carry the new vault's address (verified: 0 of the
// lake's vault emitters appear in any create body), so unlike
// blend/soroswap the deploy-graph cannot self-register vaults —
// this in-code seed is the trust root. Every entry carries at
// least one of four independent proofs, recorded per-contract in
// docs/protocols/defindex.md ("Verification 2026-07-05"):
//
//	A. first event inside a factory create transaction (71/110);
//	B. listed in a factory create event body (strategies);
//	C. runs the team-published vault WASM ae3409a4…468b
//	   (mainnet.contracts.json "hashes".defindex_vault);
//	D. listed in the team's own Dune vault registry.
//
// 9 lake emitters with NONE of the four proofs are deliberately
// EXCLUDED + flagged on the protocol page (155 events, 0.13% of
// the source's lake activity). A real vault missing here
// fail-closes into an ADR-0033 recognition gap (visible, never
// silently mis-attributed); the unblock is an operator INSERT
// into protocol_contracts (DB warm) or extending this seed.
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

// MainnetStrategies is the curated gated strategy set (same
// evidence chain as MainnetVaults; strategies are deployed
// independently and referenced by vault configs, so proof B —
// appearing in a factory create body — is the primary signal,
// plus the team-published strategy WASM 11329c24…988 and the 7
// strategies mainnet.contracts.json names).
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
