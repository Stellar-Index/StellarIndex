package aquarius

import (
	"encoding/hex"
	"fmt"
	"time"

	"github.com/StellarIndex/stellar-index/internal/events"
	"github.com/StellarIndex/stellar-index/internal/scval"
)

// decode_admin.go decodes the eight governance/upgrade admin event
// kinds (ROADMAP #89, 2026-07-10 topic census). Same provenance caveat
// as decode_rewards.go: AquaToken's soroban-amm contract source is no
// longer publicly reachable, so every function below is
// reverse-engineered from real r1 ClickHouse lake bytes, not a cloned
// Rust source. Wire types/arity/positions are exact; business-meaning
// names beyond that are BEST-EFFORT where noted.
//
// GATING NOTE: every real occurrence of apply_upgrade / commit_upgrade
// / set_privileged_addrs / apply_transfer_ownership /
// commit_transfer_ownership sampled during this audit came from EITHER
// the canonical router (a handful of times each — confirmed via a
// full-history, router-scoped count: apply_upgrade x7,
// commit_upgrade x6, set_privileged_addrs x2,
// apply_transfer_ownership x1, commit_transfer_ownership x1) OR a
// small family of NEITHER-pool-NOR-router contracts (e.g.
// CDWVENDOPYZJV7VDIA55LDWVQOPXZPPGTHJ3HQJDBRM3YC5NC4IYWN5C,
// CAEYKKJ5LTBLVQ5EM6H433YFHKOUJRDWOW3NF355ZS3FHQZKHXLQIHKA) that
// consistently co-occur with the FLAGGED parallel router deployment
// CA7RQDMMV6E53P5EDZA5GPWBZ33AMW2ZNO42XLI2RGRIAP4QXIARUOJQ (see
// docs/protocols/aquarius.md "Flagged — excluded from the gate").
// enable_emergency_mode / disable_emergency_mode were NEVER observed
// from the canonical router at all (zero, full history). Per ADR-0035
// / CS-026, the dispatcher_adapter.go gate does NOT expand the trust
// boundary to these unidentified contracts — they fail-closed exactly
// like CA7RQDMM's trade events already do, a visible ADR-0033
// recognition gap pending Aquarius-team confirmation of what that
// contract family is (mirrors the "Remaining asks for the Aquarius
// team" list in docs/protocols/aquarius.md). The decode functions
// below are still exercised against these real bytes in
// decode_admin_test.go — decode correctness and gate membership are
// independent concerns (same split real_fixture_test.go /
// adapter_test.go already use for the trade path).

// decodeAdminEvent dispatches on the already-classified event kind
// and returns the decoded AdminEvent. Called from Decode() after
// Matches() has gated on contract identity.
func decodeAdminEvent(e *events.Event, kind string, closedAt time.Time) (AdminEvent, error) {
	switch kind {
	case EventApplyUpgrade:
		return decodeApplyUpgrade(e, closedAt)
	case EventCommitUpgrade:
		return decodeCommitUpgrade(e, closedAt)
	case EventSetPrivilegedAddrs:
		return decodeSetPrivilegedAddrs(e, closedAt)
	case EventApplyTransferOwnership:
		return decodeApplyTransferOwnership(e, closedAt)
	case EventCommitTransferOwnership:
		return decodeCommitTransferOwnership(e, closedAt)
	case EventEnableEmergencyMode:
		return decodeEnableEmergencyMode(e, closedAt)
	case EventDisableEmergencyMode:
		return decodeDisableEmergencyMode(e, closedAt)
	case EventPoolGaugeSwitchToken:
		return decodePoolGaugeSwitchToken(e, closedAt)
	default:
		return AdminEvent{}, fmt.Errorf("%w: unhandled admin kind %q", ErrUnknownEvent, kind)
	}
}

func adminEnvelope(e *events.Event, kind AdminAction, closedAt time.Time) AdminEvent {
	return AdminEvent{
		ContractID: e.ContractID,
		Ledger:     e.Ledger,
		TxHash:     e.TxHash,
		OpIndex:    uint32(e.OperationIndex), //nolint:gosec // non-negative by Soroban spec.
		EventIndex: uint32(e.EventIndex),     //nolint:gosec // non-negative by Soroban spec.
		ObservedAt: closedAt,
		Kind:       kind,
		Attributes: map[string]any{},
	}
}

// decodeUpgradeHashBody is shared by apply_upgrade / commit_upgrade:
// both carry the identical wire shape, a single 32-byte Wasm hash.
//
//	topics: [Symbol(kind)]  (topic_count=1)
//	body:   Vec[Bytes]  (length 1: the 32-byte new/proposed Wasm hash)
func decodeUpgradeHashBody(e *events.Event, kindName string) (string, error) {
	body, err := scval.Parse(e.Value)
	if err != nil {
		return "", fmt.Errorf("%w: %s parse body: %w", ErrMalformedPayload, kindName, err)
	}
	elts, err := scval.AsVec(body)
	if err != nil {
		return "", fmt.Errorf("%w: %s body not a vec: %w", ErrMalformedPayload, kindName, err)
	}
	if len(elts) != 1 {
		return "", fmt.Errorf("%w: %s body length %d != 1", ErrMalformedPayload, kindName, len(elts))
	}
	raw, err := scval.AsBytes(elts[0])
	if err != nil {
		return "", fmt.Errorf("%w: %s wasm_hash: %w", ErrMalformedPayload, kindName, err)
	}
	return hex.EncodeToString(raw), nil
}

// decodeApplyUpgrade decodes `apply_upgrade` — see decodeUpgradeHashBody.
// Target is the newly-applied Wasm hash (hex).
func decodeApplyUpgrade(e *events.Event, closedAt time.Time) (AdminEvent, error) {
	hash, err := decodeUpgradeHashBody(e, EventApplyUpgrade)
	if err != nil {
		return AdminEvent{}, err
	}
	av := adminEnvelope(e, AdminApplyUpgrade, closedAt)
	av.Target = hash
	return av, nil
}

// decodeCommitUpgrade decodes `commit_upgrade` — see
// decodeUpgradeHashBody. Fires BEFORE the matching apply_upgrade
// (staged/two-phase upgrade). Target is the proposed Wasm hash (hex).
func decodeCommitUpgrade(e *events.Event, closedAt time.Time) (AdminEvent, error) {
	hash, err := decodeUpgradeHashBody(e, EventCommitUpgrade)
	if err != nil {
		return AdminEvent{}, err
	}
	av := adminEnvelope(e, AdminCommitUpgrade, closedAt)
	av.Target = hash
	return av, nil
}

// decodeSetPrivilegedAddrs decodes `set_privileged_addrs`.
//
//	topics: [Symbol("set_privileged_addrs")]  (topic_count=1)
//	body:   Vec[Address, Address, Address, Vec[Address]]  (length 4)
//
// Verified against r1 lake bytes 2026-07-10: three plain Address
// elements followed by a NESTED Vec of Address (observed length 1,
// but not assumed fixed — decoded as a list). BEST-EFFORT: likely a
// multi-role privileged-address set (e.g. emergency admin / rewards
// admin / operations admin + a pauser list) — unconfirmed against
// contract source. No single "target" column fits four addressed
// entities, so all land in Attributes.
func decodeSetPrivilegedAddrs(e *events.Event, closedAt time.Time) (AdminEvent, error) {
	body, err := scval.Parse(e.Value)
	if err != nil {
		return AdminEvent{}, fmt.Errorf("%w: set_privileged_addrs parse body: %w", ErrMalformedPayload, err)
	}
	elts, err := scval.AsVec(body)
	if err != nil {
		return AdminEvent{}, fmt.Errorf("%w: set_privileged_addrs body not a vec: %w", ErrMalformedPayload, err)
	}
	if len(elts) != 4 {
		return AdminEvent{}, fmt.Errorf("%w: set_privileged_addrs body length %d != 4", ErrMalformedPayload, len(elts))
	}
	addrs := make([]string, 3)
	for i := 0; i < 3; i++ {
		addr, err := scval.AsAddressStrkey(elts[i])
		if err != nil {
			return AdminEvent{}, fmt.Errorf("%w: set_privileged_addrs addr[%d]: %w", ErrMalformedPayload, i, err)
		}
		addrs[i] = addr
	}
	listElts, err := scval.AsVec(elts[3])
	if err != nil {
		return AdminEvent{}, fmt.Errorf("%w: set_privileged_addrs list: %w", ErrMalformedPayload, err)
	}
	list := make([]string, 0, len(listElts))
	for i, el := range listElts {
		addr, err := scval.AsAddressStrkey(el)
		if err != nil {
			return AdminEvent{}, fmt.Errorf("%w: set_privileged_addrs list[%d]: %w", ErrMalformedPayload, i, err)
		}
		list = append(list, addr)
	}
	av := adminEnvelope(e, AdminSetPrivilegedAddrs, closedAt)
	av.Attributes["addr_0"] = addrs[0]
	av.Attributes["addr_1"] = addrs[1]
	av.Attributes["addr_2"] = addrs[2]
	av.Attributes["addr_list"] = list
	return av, nil
}

// decodeTransferOwnershipBody is shared by apply_transfer_ownership /
// commit_transfer_ownership: both carry the identical wire shape.
//
//	topics: [Symbol(kind), Symbol(role)]  (topic_count=2)
//	body:   Vec[Address]  (length 1: the new address for that role)
//
// Verified against r1 lake bytes 2026-07-10: topic[1] is a Symbol
// naming the role being transferred (observed value: "EmergencyAdmin"
// — a role-name enum on the wire, not a G/C address), so there can be
// more than one role kind even though only one was sampled.
func decodeTransferOwnershipBody(e *events.Event, kindName string) (role, newAddr string, err error) {
	if len(e.Topic) != 2 {
		return "", "", fmt.Errorf("%w: %s expected 2 topics, got %d", ErrMalformedPayload, kindName, len(e.Topic))
	}
	roleSv, err := scval.Parse(e.Topic[1])
	if err != nil {
		return "", "", fmt.Errorf("%w: %s role topic: %w", ErrMalformedPayload, kindName, err)
	}
	role, err = scval.AsSymbol(roleSv)
	if err != nil {
		return "", "", fmt.Errorf("%w: %s role: %w", ErrMalformedPayload, kindName, err)
	}
	body, err := scval.Parse(e.Value)
	if err != nil {
		return "", "", fmt.Errorf("%w: %s parse body: %w", ErrMalformedPayload, kindName, err)
	}
	elts, err := scval.AsVec(body)
	if err != nil {
		return "", "", fmt.Errorf("%w: %s body not a vec: %w", ErrMalformedPayload, kindName, err)
	}
	if len(elts) != 1 {
		return "", "", fmt.Errorf("%w: %s body length %d != 1", ErrMalformedPayload, kindName, len(elts))
	}
	newAddr, err = scval.AsAddressStrkey(elts[0])
	if err != nil {
		return "", "", fmt.Errorf("%w: %s new_admin: %w", ErrMalformedPayload, kindName, err)
	}
	return role, newAddr, nil
}

// decodeApplyTransferOwnership decodes `apply_transfer_ownership` —
// see decodeTransferOwnershipBody. Target is the newly-applied
// role-holder address.
func decodeApplyTransferOwnership(e *events.Event, closedAt time.Time) (AdminEvent, error) {
	role, newAddr, err := decodeTransferOwnershipBody(e, EventApplyTransferOwnership)
	if err != nil {
		return AdminEvent{}, err
	}
	av := adminEnvelope(e, AdminApplyTransferOwnership, closedAt)
	av.Target = newAddr
	av.Attributes["role"] = role
	return av, nil
}

// decodeCommitTransferOwnership decodes `commit_transfer_ownership` —
// see decodeTransferOwnershipBody. Fires BEFORE the matching
// apply_transfer_ownership (staged/two-phase transfer).
func decodeCommitTransferOwnership(e *events.Event, closedAt time.Time) (AdminEvent, error) {
	role, newAddr, err := decodeTransferOwnershipBody(e, EventCommitTransferOwnership)
	if err != nil {
		return AdminEvent{}, err
	}
	av := adminEnvelope(e, AdminCommitTransferOwnership, closedAt)
	av.Target = newAddr
	av.Attributes["role"] = role
	return av, nil
}

// decodeEnableEmergencyMode decodes `enable_emergency_mode` — a bare
// marker event with no payload.
//
//	topics: [Symbol("enable_emergency_mode")]  (topic_count=1)
//	body:   Void
func decodeEnableEmergencyMode(e *events.Event, closedAt time.Time) (AdminEvent, error) {
	if err := requireVoidBody(e, EventEnableEmergencyMode); err != nil {
		return AdminEvent{}, err
	}
	return adminEnvelope(e, AdminEnableEmergencyMode, closedAt), nil
}

// decodeDisableEmergencyMode decodes `disable_emergency_mode` — see
// decodeEnableEmergencyMode.
func decodeDisableEmergencyMode(e *events.Event, closedAt time.Time) (AdminEvent, error) {
	if err := requireVoidBody(e, EventDisableEmergencyMode); err != nil {
		return AdminEvent{}, err
	}
	return adminEnvelope(e, AdminDisableEmergencyMode, closedAt), nil
}

// decodePoolGaugeSwitchToken decodes `pool_gauge_switch_token`.
//
//	topics: [Symbol("pool_gauge_switch_token"), Address(new_reward_token)]  (topic_count=2)
//	body:   Vec[Bool]  (length 1)
//
// Verified against r1 lake bytes 2026-07-10: 100% router-scoped (all
// 31 lifetime events are on the canonical router — confirmed via a
// full-history, router-scoped count); every sampled body value is
// `true`. Target is the pool's new gauge reward-token address.
func decodePoolGaugeSwitchToken(e *events.Event, closedAt time.Time) (AdminEvent, error) {
	if len(e.Topic) != 2 {
		return AdminEvent{}, fmt.Errorf("%w: pool_gauge_switch_token expected 2 topics, got %d", ErrMalformedPayload, len(e.Topic))
	}
	newToken, err := decodeAddressTopic(e.Topic[1])
	if err != nil {
		return AdminEvent{}, fmt.Errorf("%w: pool_gauge_switch_token new_token: %w", ErrMalformedPayload, err)
	}
	body, err := scval.Parse(e.Value)
	if err != nil {
		return AdminEvent{}, fmt.Errorf("%w: pool_gauge_switch_token parse body: %w", ErrMalformedPayload, err)
	}
	elts, err := scval.AsVec(body)
	if err != nil {
		return AdminEvent{}, fmt.Errorf("%w: pool_gauge_switch_token body not a vec: %w", ErrMalformedPayload, err)
	}
	if len(elts) != 1 {
		return AdminEvent{}, fmt.Errorf("%w: pool_gauge_switch_token body length %d != 1", ErrMalformedPayload, len(elts))
	}
	switched, err := scval.AsBool(elts[0])
	if err != nil {
		return AdminEvent{}, fmt.Errorf("%w: pool_gauge_switch_token switched: %w", ErrMalformedPayload, err)
	}
	av := adminEnvelope(e, AdminPoolGaugeSwitchToken, closedAt)
	av.Target = newToken
	av.Attributes["switched"] = switched
	return av, nil
}

// requireVoidBody asserts e.Value parses to ScvVoid — the shape
// enable_emergency_mode / disable_emergency_mode use.
func requireVoidBody(e *events.Event, kindName string) error {
	if len(e.Topic) != 1 {
		return fmt.Errorf("%w: %s expected 1 topic, got %d", ErrMalformedPayload, kindName, len(e.Topic))
	}
	body, err := scval.Parse(e.Value)
	if err != nil {
		return fmt.Errorf("%w: %s parse body: %w", ErrMalformedPayload, kindName, err)
	}
	if !scval.IsVoid(body) {
		return fmt.Errorf("%w: %s expected void body, got %s", ErrMalformedPayload, kindName, body.Type.String())
	}
	return nil
}
