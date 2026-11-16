// Package blnt decodes the independently deployed BLNT V3 lending protocol.
//
// BLNT V3 is Blend-derived, but it is not an official Blend release and its
// event surface is not wire-compatible with every Blend V1/V2 table. In
// particular, V3 adds tiered backstops, a fourth auction type, clawback and
// reserve-loss events, and two distinct backfill contracts. This package keeps
// BLNT V3 under its own source identity and decodes every event against its
// own ABI (abi.go); it shares no decoder or table with Blend.
package blnt

import (
	"errors"
	"slices"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// SourceName is the stable source name used by configuration, projection,
// storage and the public protocol directory.
const SourceName = "blnt"

// GenesisLedger is the Emitter's creation ledger, the earliest contract in the
// canonical BLNT V3 mainnet deployment.
const GenesisLedger uint32 = 65_134_154

// Canonical mainnet BLNT V3 contracts. Pool contracts discovered from
// MainnetPoolFactory are admitted at runtime; the two currently deployed pools
// are also seeded so a fresh decoder is useful before its registry warm.
const (
	MainnetEmitter        = "CDSO6FQSBJ3U3AJHBNNLHHKSMKTTDDN3GLGF3KZTMXBFV7SVB43ZJERT"
	MainnetPoolFactory    = "CAKLFWAHB2RIYRBSPUL6EH43AIQ5ILBEM7HTMWYSJRL7PHZQ676KMCV5"
	MainnetBackfill       = "CANIE64BDFKLHROLOIY2KTQVBWUZNUKZP75DLXC272QM7TIN5N3M4LVA"
	MainnetBackstop       = "CAHCUKBPTW5DR5RNXOHCTJD5U76A6EEZXYZAQFRQEEQRXQ3CRUZ6Z3MO"
	MainnetLockupBackfill = "CBNRUFL5KYTT7TQMYKRMHQU5KY4LDQEUVXLI6ECKJU7OJGHC3OLGJFBV"
	MainnetFixedPool      = "CACSQWCMKG45TUHCQ3LXHOB7YEEJH4W376ZHQPXH2IZ3TE4NJHFZMCY7"
	MainnetYieldBlentPool = "CAUTPSQDAJEUV5XEU3EXFVYRGDJLEJGVTFJCFZCSPTDCREAMZ573VF4D"
)

// Role identifies which immutable BLNT V3 event ABI owns a contract. The role
// is persisted independently of EventType because symbols such as claim,
// withdraw, distribute and gulp_emissions intentionally overlap across modules.
type Role string

const (
	RolePool           Role = "pool"
	RoleFactory        Role = "factory"
	RoleBackstop       Role = "backstop"
	RoleEmitter        Role = "emitter"
	RoleBackfill       Role = "backfill"
	RoleLockupBackfill Role = "lockup_backfill"
)

// Consumer event kinds. Each role gets a distinct kind so the API and
// completeness machinery never conflate same-named events from different ABIs.
const (
	PoolEventKind           = "blnt.pool"
	FactoryEventKind        = "blnt.factory"
	BackstopEventKind       = "blnt.backstop"
	EmitterEventKind        = "blnt.emitter"
	BackfillEventKind       = "blnt.backfill"
	LockupBackfillEventKind = "blnt.lockup_backfill"
)

// MainnetGatedSet returns every non-factory canonical instance known at this
// release. Factory deploy events extend the pool subset at runtime.
func MainnetGatedSet() []string {
	out := []string{
		MainnetEmitter,
		MainnetBackfill,
		MainnetBackstop,
		MainnetLockupBackfill,
		MainnetFixedPool,
		MainnetYieldBlentPool,
	}
	slices.Sort(out)
	return out
}

var encodedRoleEvents = func() map[Role]map[string]string {
	out := make(map[Role]map[string]string, len(abi))
	for role, kinds := range abi {
		byEncoded := make(map[string]string, len(kinds))
		for kind := range kinds {
			byEncoded[scval.MustEncodeSymbol(kind)] = kind
		}
		out[role] = byEncoded
	}
	return out
}()

// Event is one decoded BLNT V3 contract event. The fields every reader filters or sums on are
// promoted; the rest of the payload is in Attributes under the contract's own field names. Amounts
// are decimal i128 strings ("" when the event carries none). Never a canonical.Trade, never in VWAP.
type Event struct {
	ContractID string
	Ledger     uint32
	TxHash     string
	OpIndex    uint32
	EventIndex uint32
	ObservedAt time.Time
	Role       Role
	EventType  string

	Pool        string // the pool the event concerns: the emitter itself for RolePool
	UserAddress string
	Asset       string
	Tier        string // backstop loss tier, as emitted: FirstLoss, SecondLoss, ThirdLoss
	Amount      string
	Amount2     string
	Attributes  map[string]any
}

// EventKind implements consumer.Event with a distinct kind per contract ABI.
func (e Event) EventKind() string {
	switch e.Role {
	case RolePool:
		return PoolEventKind
	case RoleFactory:
		return FactoryEventKind
	case RoleBackstop:
		return BackstopEventKind
	case RoleEmitter:
		return EmitterEventKind
	case RoleBackfill:
		return BackfillEventKind
	case RoleLockupBackfill:
		return LockupBackfillEventKind
	default:
		return SourceName + ".unknown"
	}
}

// Source implements consumer.Event.
func (Event) Source() string { return SourceName }

var _ consumer.Event = Event{}

var (
	ErrNotBLNTEvent   = errors.New("blnt: not a tracked BLNT V3 event")
	ErrMalformedEvent = errors.New("blnt: malformed event")
)
