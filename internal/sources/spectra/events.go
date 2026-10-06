// Package spectra decodes on-chain events from Spectra, the yield
// tokenisation protocol (PT/YT split of an interest-bearing token, IBT).
// See docs/protocols/spectra.md.
//
// Decoder and goldens only; not yet wired into the dispatcher, projector
// or sinks.
//
// Only (role, kind) pairs observed on mainnet are classified, each pinned
// by a lake fixture under test/fixtures/spectra; `unwrap` is the one
// exception, decoded from the published source because it shares wrap's
// body. Every other topic is NOT claimed: Matches returns false and the
// recognition audit surfaces it, rather than a guessed shape being
// recorded as expected-zero.
package spectra

import (
	"errors"
	"slices"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// SourceName is the dispatcher / projector source name.
const SourceName = "spectra"

// EventKind is the consumer.Event kind of [Event].
const EventKind = "spectra.event"

// Role is a gated contract's part in Spectra. The values are the
// spectra_events.role column's.
type Role string

const (
	RoleFactory     Role = "factory"
	RoleRegistry    Role = "registry"
	RoleRouter      Role = "router"
	RoleOrderEngine Role = "order_engine"
	RolePT          Role = "pt"
	RoleYT          Role = "yt"
	RoleIBT         Role = "ibt"
)

// ContractMeta describes one gated contract. Decimals is the market's
// token scale (7 / 13 / 18 differ per market); 0 for the registry.
// MarketPT is a YT's PT, which names the market of its transfer rows.
type ContractMeta struct {
	Role     Role
	Decimals uint8
	MarketPT string
}

// MainnetContracts is the curated trust root (ADR-0035): the registry, and
// the PT, YT and wrapper-IBT of each of the seven markets, from Spectra's
// operator API and the capture script's table. The earnXLM / earnUSDC IBTs
// are Upshift vaults, attributed by the upshift source, so not listed.
var MainnetContracts = map[string]ContractMeta{
	"CCUGRASBWD5SXDYMS7NM437FQ7KNKHFX74D2VRJVTRU4J2TWDMURUW3V": {RoleRegistry, 0, ""},
	// sw-USDC
	"CAAOR5F43GSQZYJESHIVLGZBMHMH3UVJMSBEHFKCUBKOUQSZMQC5UCMK": {RolePT, 7, ""},
	"CBDQZFWY735RH3PQLNK4BIOO7DTJY4ZQWK5YDFCZ7EIL5DAO2567TORX": {RoleYT, 7, "CAAOR5F43GSQZYJESHIVLGZBMHMH3UVJMSBEHFKCUBKOUQSZMQC5UCMK"},
	"CBRT4E5AH23GMRQI7H6HQW54HMDMK4C23OO2CEN5OHWEOSRYBQZCMYBC": {RoleIBT, 7, ""},
	// sw-EURC
	"CA7KTCVDJXQBC7PB6CANFEBPKAQUGVG6SIZVHGFPQCDX3APPTSROJ6AS": {RolePT, 7, ""},
	"CDNNGBZOH2OOJ2OKON3A3IVD2JS323BAI2DX55JEWKD327XBJDYDUPAR": {RoleYT, 7, "CA7KTCVDJXQBC7PB6CANFEBPKAQUGVG6SIZVHGFPQCDX3APPTSROJ6AS"},
	"CCECATRPUHLMFTIUQDQQPOU5GLXQGJYGBJHKSFMWJYOFEIFNN3MOQWU3": {RoleIBT, 7, ""},
	// earnXLM
	"CD5YZRFQCATFOFZPWE4XDYJZMXAZSW5ROTIAO4D65Q7KTMZIEWGB7H7W": {RolePT, 13, ""},
	"CAUKKZSHKNCP44C2CLNNDMI4H5JF4EJUJ7CRZR2Z7NTLI2N6BE2SRBYP": {RoleYT, 13, "CD5YZRFQCATFOFZPWE4XDYJZMXAZSW5ROTIAO4D65Q7KTMZIEWGB7H7W"},
	// earnUSDC
	"CCJ43PIDUBSVX4FAI3MJJTFWC3ZXLECARFNJUOUUTKHB5JP32Q75LVPN": {RolePT, 13, ""},
	"CCZVSODQH6UVMSENOAPBGVH3ZHMUSM5OXZJGMRYIO2V22HPI56U6PBFL": {RoleYT, 13, "CCJ43PIDUBSVX4FAI3MJJTFWC3ZXLECARFNJUOUUTKHB5JP32Q75LVPN"},
	// sw-deJTRSY, three maturities sharing one wrapper IBT
	"CDRK5SWZ7DQJ4BZUAZQABPSP7MZS4NO4PPW7LW6CVD5THZDVE63MVLJJ": {RolePT, 18, ""},
	"CC3MKDR62O4Q7EEBOXXCNFJH3Z6AZQLB3QUTUEKAMSIJIYAHLUPVQR5G": {RoleYT, 18, "CDRK5SWZ7DQJ4BZUAZQABPSP7MZS4NO4PPW7LW6CVD5THZDVE63MVLJJ"},
	"CAAQJ6CN3KWJUG2CTUFUV27IL2BBWEIQKQA27HR7KFFZXROLHN6ELMBI": {RolePT, 18, ""},
	"CBRI2RSGUJJOTP3W7HLICWOFHCDB64S73EIUYU653JG7GKZRTQDT72V7": {RoleYT, 18, "CAAQJ6CN3KWJUG2CTUFUV27IL2BBWEIQKQA27HR7KFFZXROLHN6ELMBI"},
	"CDHIBKKS53XQAMIDVL7SZLPM2DEHH3OCO7SU6IE3OW65LESYF5K5ABMU": {RolePT, 18, ""},
	"CAQPNK37HP3BVDNYNYJ7USCFCLXO4S7K75VSLF54BD25APTHSM5COT5L": {RoleYT, 18, "CDHIBKKS53XQAMIDVL7SZLPM2DEHH3OCO7SU6IE3OW65LESYF5K5ABMU"},
	"CAHPZLEH6O6WJPICJAVRLCTYCAYDN52F4SM6IX3XJKWYSAASSHGZEZBO": {RoleIBT, 18, ""},
}

// Infrastructure found in the lake. The factory is the trust root for
// `pt_deployed`; the registry's `*_change` events never admit a new one,
// a new id is a code change here.
const (
	MainnetFactory = "CC4ZVRIYM33M5FVAUDFWK7JXO3PWIVSKKEBXVEEC5E6KPYISXIMLUJCP"
	MainnetRouter  = "CB56R3NGNN7KNBGEH3CWK7SQIAR7SFAS3PKDQEJX7Y3U6TEFDJBVPY7F"
)

// MainnetOrderEngines is the v0 order engine (replaced at 63,780,164) and
// the current one; both carry the same WASM.
var MainnetOrderEngines = []string{
	"CC2CEV23OQVGALHWQTKA26DYQTDNS7XJSL75EHTLHKZH6W3HJAEUKKB7",
	"CCKNOCLH6QILGS6GYZWMQ6JCHWC2D75OCI5RLBPCUF7FJTONNSCZZAC5",
}

// MainnetInfrastructure is the factory, router and order engines.
var MainnetInfrastructure = append([]string{MainnetFactory, MainnetRouter}, MainnetOrderEngines...)

// PT and YT WASM hashes the registry deploys (its `*_wasm_hash_change`
// values). A different value means unaudited code: re-audit first.
const (
	PTWasmHash = "3bcf316f76a9c5db718ccd6e2292cad70d8e8ce4371dfa0a2a7fdd86b8749ffd"
	YTWasmHash = "daeb931b9507440b6dc4f73256ac33c6089964f5d035f7eec5fcedc10e732c8e"
)

// MainnetGatedSet returns the sorted contract ids, derived from
// [MainnetContracts] so the gate and the metadata cannot drift.
func MainnetGatedSet() []string {
	out := make([]string, 0, len(MainnetContracts))
	for id := range MainnetContracts {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// Event kinds that become rows (field mapping on [Event]).
const (
	EventPTDeployed      = "pt_deployed"
	EventYTDeployed      = "yt_deployed"
	EventPTAdded         = "pt_added"
	EventPTMinted        = "pt_minted"
	EventRedeem          = "redeem"
	EventYieldUpdated    = "yield_updated"
	EventTransfer        = "transfer"
	EventWrap            = "wrap"
	EventUnwrap          = "unwrap"
	EventDeposit         = "deposit"
	EventWithdraw        = "withdraw"
	EventOrderRegistered = "order_registered"
	EventOrderFilled     = "order_filled"
	EventOrderCancelled  = "order_cancelled"
)

// Kinds recognised but projected as zero rows (ADR-0033 expected-zero).
// PT/YT mint and burn restate pt_minted / redeem in the same transaction;
// approve carries no Spectra state; the rest is governance and set-up.
const (
	EventMint                   = "mint"
	EventBurn                   = "burn"
	EventApprove                = "approve"
	EventRoleGranted            = "role_granted"
	EventRoleRevoked            = "role_revoked"
	EventAdminTransferInitiated = "admin_transfer_initiated"
	EventAdminTransferCompleted = "admin_transfer_completed"
	EventFeeCollectorChange     = "fee_collector_change"
	EventFactoryChange          = "factory_change"
	EventRouterChange           = "router_change"
	EventOrderEngineChange      = "limit_order_engine_change"
	EventPTWasmHashChange       = "pt_wasm_hash_change"
	EventYTWasmHashChange       = "yt_wasm_hash_change"
	EventFactoryInitialized     = "factory_initialized"
	EventOrderEngineInitialized = "limit_order_engine_initialized"
	EventDeJTRSYInitialized     = "dejtrsy_wrapper_initialized"
)

var allRoles = []Role{RoleFactory, RoleRegistry, RoleRouter, RoleOrderEngine, RolePT, RoleYT, RoleIBT}

// kindRoles is the set of roles each kind was observed from (the lake
// census in docs/operations/wasm-audits/spectra.md). Matches claims a
// kind only from these roles. YTs emit no governance events.
var kindRoles = map[string][]Role{
	EventPTDeployed:      {RoleFactory},
	EventYTDeployed:      {RolePT},
	EventPTAdded:         {RoleRegistry},
	EventPTMinted:        {RolePT},
	EventRedeem:          {RolePT},
	EventYieldUpdated:    {RolePT},
	EventTransfer:        {RolePT, RoleYT, RoleIBT},
	EventWrap:            {RoleIBT},
	EventUnwrap:          {RoleIBT},
	EventDeposit:         {RoleIBT},
	EventWithdraw:        {RoleIBT},
	EventOrderRegistered: {RoleOrderEngine},
	EventOrderFilled:     {RoleOrderEngine},
	EventOrderCancelled:  {RoleOrderEngine},

	EventMint:                   {RolePT, RoleYT},
	EventBurn:                   {RolePT, RoleYT},
	EventApprove:                {RolePT, RoleYT, RoleIBT},
	EventRoleGranted:            governanceRoles,
	EventRoleRevoked:            governanceRoles,
	EventAdminTransferInitiated: governanceRoles,
	EventAdminTransferCompleted: governanceRoles,
	EventFeeCollectorChange:     {RoleRegistry, RoleOrderEngine},
	EventFactoryChange:          {RoleRegistry},
	EventRouterChange:           {RoleRegistry},
	EventOrderEngineChange:      {RoleRegistry},
	EventPTWasmHashChange:       {RoleRegistry},
	EventYTWasmHashChange:       {RoleRegistry},
	EventFactoryInitialized:     {RoleFactory},
	EventOrderEngineInitialized: {RoleOrderEngine},
	EventDeJTRSYInitialized:     {RoleIBT},
}

var governanceRoles = slices.DeleteFunc(slices.Clone(allRoles), func(r Role) bool { return r == RoleYT })

// topicKind maps a topic[0] Symbol encoding to its kind.
var topicKind = func() map[string]string {
	m := make(map[string]string, len(kindRoles))
	for kind := range kindRoles {
		m[scval.MustEncodeSymbol(kind)] = kind
	}
	return m
}()

var (
	ErrNotSpectraEvent  = errors.New("spectra: topic[0] is not a recognised Spectra event")
	ErrMalformedPayload = errors.New("spectra: event body does not match the proven schema")
	ErrShortTopic       = errors.New("spectra: topic vector too short for event variant")
	ErrNotGated         = errors.New("spectra: kind is not claimed from this contract's role")
	// ErrUnlistedInfrastructure: the registry names a factory, router,
	// order engine or token WASM outside the hand-kept, audited set. The
	// gate is then incomplete, so the event fails loudly instead of
	// admitting it.
	ErrUnlistedInfrastructure = errors.New("spectra: registry names infrastructure outside the hand-kept set")
)
