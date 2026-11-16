// Package blnt decodes the independently deployed BLNT V3 lending protocol.
//
// BLNT V3 is Blend-derived, but it is not an official Blend release and its
// event surface is not wire-compatible with every Blend V1/V2 table. In
// particular, V3 adds tiered backstops, a fourth auction type, clawback and
// reserve-loss events, and two distinct backfill contracts. This package keeps
// BLNT V3 under its own source identity and persists the complete event XDR so
// later, narrower state projections can be reproduced without borrowing V2
// semantics.
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

var roleEvents = map[Role][]string{
	RolePool: {
		"set_admin", "update_pool", "queue_set_reserve", "cancel_set_reserve", "set_reserve", "set_status",
		"reserve_emission_update", "gulp_emissions", "claim", "bad_debt", "defaulted_debt",
		"supply", "withdraw", "supply_collateral", "withdraw_collateral", "borrow", "repay", "flash_loan",
		"clawback", "gulp", "reconcile_loss", "protocol_credit_loss",
		"new_auction", "fill_auction", "delete_auction",
	},
	RoleFactory: {"deploy"},
	RoleBackstop: {
		"migration_activated", "backfill_funded", "deposit", "queue_withdrawal", "dequeue_withdrawal", "withdraw",
		"distribute", "gulp_emissions", "rw_zone_add", "rw_zone_remove", "claim", "draw", "donate",
	},
	RoleEmitter:        {"distribute", "q_swap", "del_swap", "swap", "drop"},
	RoleBackfill:       {"claim", "claim_grant", "swap_blnd", "burn_expired"},
	RoleLockupBackfill: {"fund", "claim", "claim_fund", "expire"},
}

var encodedRoleEvents = func() map[Role]map[string]string {
	out := make(map[Role]map[string]string, len(roleEvents))
	for role, kinds := range roleEvents {
		byEncoded := make(map[string]string, len(kinds))
		for _, kind := range kinds {
			byEncoded[scval.MustEncodeSymbol(kind)] = kind
		}
		out[role] = byEncoded
	}
	return out
}()

// Event is one lossless BLNT V3 contract event. EventType is the decoded
// topic[0] symbol; TopicsXDR and ValueXDR retain the complete base64 SCVal wire
// values. This is deliberately not a canonical.Trade and never enters VWAP.
type Event struct {
	ContractID string
	Ledger     uint32
	TxHash     string
	OpIndex    uint32
	EventIndex uint32
	ObservedAt time.Time
	Role       Role
	EventType  string
	TopicsXDR  []string
	ValueXDR   string
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
