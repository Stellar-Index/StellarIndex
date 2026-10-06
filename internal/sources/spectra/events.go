// Package spectra decodes on-chain events from Spectra, the yield
// tokenisation protocol (PT/YT split of an interest-bearing token, IBT).
// See docs/protocols/spectra.md.
//
// Decoder and goldens only; not yet wired into the dispatcher, projector
// or sinks.
//
// Only kinds proven by captured mainnet fixtures (test/fixtures/spectra)
// are classified. Every other topic is NOT claimed: Matches returns false
// and the recognition audit surfaces it, rather than a guessed shape being
// recorded as expected-zero. PT, registry and factory kinds join once
// fixtures exist for them.
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

// Role is a gated contract's part in a Spectra market.
type Role string

const (
	RoleRegistry Role = "registry"
	RolePT       Role = "pt"
	RoleYT       Role = "yt"
	RoleIBT      Role = "ibt"
)

// ContractMeta describes one gated contract. Decimals is the market's
// token scale (7 / 13 / 18 differ per market); 0 for the registry.
type ContractMeta struct {
	Role     Role
	Decimals uint8
}

// MainnetContracts is the curated trust root (ADR-0035): the registry, and
// the PT, YT and wrapper-IBT of each of the seven markets, from Spectra's
// operator API and the capture script's table. The earnXLM / earnUSDC IBTs
// are Upshift vaults, attributed by the upshift source, so not listed.
var MainnetContracts = map[string]ContractMeta{
	"CCUGRASBWD5SXDYMS7NM437FQ7KNKHFX74D2VRJVTRU4J2TWDMURUW3V": {RoleRegistry, 0},
	// sw-USDC
	"CAAOR5F43GSQZYJESHIVLGZBMHMH3UVJMSBEHFKCUBKOUQSZMQC5UCMK": {RolePT, 7},
	"CBDQZFWY735RH3PQLNK4BIOO7DTJY4ZQWK5YDFCZ7EIL5DAO2567TORX": {RoleYT, 7},
	"CBRT4E5AH23GMRQI7H6HQW54HMDMK4C23OO2CEN5OHWEOSRYBQZCMYBC": {RoleIBT, 7},
	// sw-EURC
	"CA7KTCVDJXQBC7PB6CANFEBPKAQUGVG6SIZVHGFPQCDX3APPTSROJ6AS": {RolePT, 7},
	"CDNNGBZOH2OOJ2OKON3A3IVD2JS323BAI2DX55JEWKD327XBJDYDUPAR": {RoleYT, 7},
	"CCECATRPUHLMFTIUQDQQPOU5GLXQGJYGBJHKSFMWJYOFEIFNN3MOQWU3": {RoleIBT, 7},
	// earnXLM
	"CD5YZRFQCATFOFZPWE4XDYJZMXAZSW5ROTIAO4D65Q7KTMZIEWGB7H7W": {RolePT, 13},
	"CAUKKZSHKNCP44C2CLNNDMI4H5JF4EJUJ7CRZR2Z7NTLI2N6BE2SRBYP": {RoleYT, 13},
	// earnUSDC
	"CCJ43PIDUBSVX4FAI3MJJTFWC3ZXLECARFNJUOUUTKHB5JP32Q75LVPN": {RolePT, 13},
	"CCZVSODQH6UVMSENOAPBGVH3ZHMUSM5OXZJGMRYIO2V22HPI56U6PBFL": {RoleYT, 13},
	// sw-deJTRSY, three maturities sharing one wrapper IBT
	"CDRK5SWZ7DQJ4BZUAZQABPSP7MZS4NO4PPW7LW6CVD5THZDVE63MVLJJ": {RolePT, 18},
	"CC3MKDR62O4Q7EEBOXXCNFJH3Z6AZQLB3QUTUEKAMSIJIYAHLUPVQR5G": {RoleYT, 18},
	"CAAQJ6CN3KWJUG2CTUFUV27IL2BBWEIQKQA27HR7KFFZXROLHN6ELMBI": {RolePT, 18},
	"CBRI2RSGUJJOTP3W7HLICWOFHCDB64S73EIUYU653JG7GKZRTQDT72V7": {RoleYT, 18},
	"CDHIBKKS53XQAMIDVL7SZLPM2DEHH3OCO7SU6IE3OW65LESYF5K5ABMU": {RolePT, 18},
	"CAQPNK37HP3BVDNYNYJ7USCFCLXO4S7K75VSLF54BD25APTHSM5COT5L": {RoleYT, 18},
	"CAHPZLEH6O6WJPICJAVRLCTYCAYDN52F4SM6IX3XJKWYSAASSHGZEZBO": {RoleIBT, 18},
}

// MainnetInfrastructure is the factory, router and order engines (v0 and
// current) found in the lake. They are audited with the roster but not part
// of the curated trust root above until the factory gate lands.
var MainnetInfrastructure = []string{
	"CC4ZVRIYM33M5FVAUDFWK7JXO3PWIVSKKEBXVEEC5E6KPYISXIMLUJCP", // factory
	"CB56R3NGNN7KNBGEH3CWK7SQIAR7SFAS3PKDQEJX7Y3U6TEFDJBVPY7F", // router
	"CC2CEV23OQVGALHWQTKA26DYQTDNS7XJSL75EHTLHKZH6W3HJAEUKKB7", // order engine v0
	"CCKNOCLH6QILGS6GYZWMQ6JCHWC2D75OCI5RLBPCUF7FJTONNSCZZAC5", // order engine
}

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

// Decoded kinds (wrapper contract, role ibt). Topics:
//
//	wrap   [Symbol, caller, receiver]         body Map{shares, vault_shares}
//	unwrap [Symbol, caller, receiver, owner]  body Map{shares, vault_shares}
//
// wrap is proven by a mainnet fixture; unwrap's shape is from the public
// source and shares wrap's body field names.
const (
	EventWrap   = "wrap"
	EventUnwrap = "unwrap"
)

// Recognised but projected as zero rows: SEP-41 allowance events carry no
// Spectra state (balances come from the sep41 sources, slice 7).
const EventApprove = "approve"

var (
	TopicSymbolWrap    = scval.MustEncodeSymbol(EventWrap)
	TopicSymbolUnwrap  = scval.MustEncodeSymbol(EventUnwrap)
	TopicSymbolApprove = scval.MustEncodeSymbol(EventApprove)
)

var (
	ErrNotSpectraEvent  = errors.New("spectra: topic[0] is not a recognised Spectra event")
	ErrMalformedPayload = errors.New("spectra: event body does not match the proven schema")
	ErrShortTopic       = errors.New("spectra: topic vector too short for event variant")
)
