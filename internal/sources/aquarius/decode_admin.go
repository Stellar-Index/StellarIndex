package aquarius

import (
	"encoding/hex"
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// decode_admin.go decodes the eight governance/upgrade admin event kinds. The AquaToken source
// is not public, so wire types, arity and positions are exact (from lake bytes) while
// business-meaning names are BEST-EFFORT where noted.
//
// GATING NOTE: SEVEN kinds (all but config_rewards and pool_gauge_switch_token) are emitted by
// REGISTERED Aquarius POOLS as well as the router, so dispatcher_adapter.go gates them on
// `reg.Has || reg.IsFactory`; a router-only gate would fail-close real pool events into an
// ADR-0033 recognition gap. config_rewards and pool_gauge_switch_token are router-only.
//
// The FLAGGED parallel router CA7RQDMMV6E53P5EDZA5GPWBZ33AMW2ZNO42XLI2RGRIAP4QXIARUOJQ and the
// neither-pool-nor-router family (docs/protocols/aquarius.md) stay OUTSIDE the trust boundary and
// fail closed per ADR-0035: a visible recognition gap, never silent mis-attribution.
// decode_admin_test.go decodes bytes from both registered and flagged contracts, because decode
// correctness and gate membership are independent concerns.

// decodeAdminEvent dispatches on the classified kind; Decode() calls it after Matches() has gated on contract identity.
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

// decodeUpgradeHashBody is shared by apply/commit_upgrade: topics [Symbol(kind)], body Vec[Bytes] of 32-byte Wasm hashes.
// Arity varies (router 1 hash, pools 2 or 2-3), so decode by arity: element[0] is the primary hash (Target), the rest go to
// Attributes. Returns hashes in wire order; an empty body fails closed.
func decodeUpgradeHashBody(e *events.Event, kindName string) ([]string, error) {
	body, err := scval.Parse(e.Value)
	if err != nil {
		return nil, fmt.Errorf("%w: %s parse body: %w", ErrMalformedPayload, kindName, err)
	}
	elts, err := scval.AsVec(body)
	if err != nil {
		return nil, fmt.Errorf("%w: %s body not a vec: %w", ErrMalformedPayload, kindName, err)
	}
	if len(elts) == 0 {
		return nil, fmt.Errorf("%w: %s body is an empty vec", ErrMalformedPayload, kindName)
	}
	hashes := make([]string, len(elts))
	for i, el := range elts {
		raw, err := scval.AsBytes(el)
		if err != nil {
			return nil, fmt.Errorf("%w: %s wasm_hash[%d]: %w", ErrMalformedPayload, kindName, i, err)
		}
		hashes[i] = hex.EncodeToString(raw)
	}
	return hashes, nil
}

// setUpgradeHashes lands element[0] in Target and trailing hashes in Attributes["wasm_hash_N"] (1-indexed).
// A router body lands ONLY Target.
func setUpgradeHashes(av *AdminEvent, hashes []string) {
	av.Target = hashes[0]
	for i, h := range hashes[1:] {
		av.Attributes[fmt.Sprintf("wasm_hash_%d", i+1)] = h
	}
}

// decodeApplyUpgrade decodes `apply_upgrade` (see decodeUpgradeHashBody); Target is the newly-applied Wasm hash (hex).
func decodeApplyUpgrade(e *events.Event, closedAt time.Time) (AdminEvent, error) {
	hashes, err := decodeUpgradeHashBody(e, EventApplyUpgrade)
	if err != nil {
		return AdminEvent{}, err
	}
	av := adminEnvelope(e, AdminApplyUpgrade, closedAt)
	setUpgradeHashes(&av, hashes)
	return av, nil
}

// decodeCommitUpgrade decodes `commit_upgrade` (see decodeUpgradeHashBody); fires BEFORE apply_upgrade (two-phase).
// Target is the proposed Wasm hash (hex).
func decodeCommitUpgrade(e *events.Event, closedAt time.Time) (AdminEvent, error) {
	hashes, err := decodeUpgradeHashBody(e, EventCommitUpgrade)
	if err != nil {
		return AdminEvent{}, err
	}
	av := adminEnvelope(e, AdminCommitUpgrade, closedAt)
	setUpgradeHashes(&av, hashes)
	return av, nil
}

// decodeSetPrivilegedAddrs decodes `set_privileged_addrs`; topic [Symbol(kind)], body Vec[Address, Address, Address, Vec[Address]]
// (v1, len 4) or v1 + a trailing Address (v2, len 5, after ledger 57,697,794). Contracts upgrade in place so backfill sees both: do not pin
// arity 4. v2's extra address lands in Attributes["addr_3"]; other arities fail closed. Semantics are BEST-EFFORT.
func decodeSetPrivilegedAddrs(e *events.Event, closedAt time.Time) (AdminEvent, error) {
	body, err := scval.Parse(e.Value)
	if err != nil {
		return AdminEvent{}, fmt.Errorf("%w: set_privileged_addrs parse body: %w", ErrMalformedPayload, err)
	}
	elts, err := scval.AsVec(body)
	if err != nil {
		return AdminEvent{}, fmt.Errorf("%w: set_privileged_addrs body not a vec: %w", ErrMalformedPayload, err)
	}
	if len(elts) != 4 && len(elts) != 5 {
		return AdminEvent{}, fmt.Errorf("%w: set_privileged_addrs body length %d not in {4, 5}", ErrMalformedPayload, len(elts))
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
	if len(elts) == 5 {
		// v2 (post-57.7M WASM) trailing role address.
		addr, err := scval.AsAddressStrkey(elts[4])
		if err != nil {
			return AdminEvent{}, fmt.Errorf("%w: set_privileged_addrs addr[4]: %w", ErrMalformedPayload, err)
		}
		av.Attributes["addr_3"] = addr
	}
	return av, nil
}

// decodeTransferOwnershipBody is shared by apply/commit_transfer_ownership: topics [Symbol(kind), Symbol(role)], body Vec[Address]
// (len 1, the new holder). topic[1] is a role-name Symbol (observed "EmergencyAdmin"), not an address; other roles may exist.
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

// decodeApplyTransferOwnership decodes `apply_transfer_ownership` (see decodeTransferOwnershipBody); Target is the new role-holder.
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

// decodeCommitTransferOwnership decodes `commit_transfer_ownership` (see decodeTransferOwnershipBody); fires BEFORE the apply.
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

// decodeEnableEmergencyMode decodes `enable_emergency_mode`: topics [Symbol(kind)], body Void.
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

// decodePoolGaugeSwitchToken decodes `pool_gauge_switch_token` (router-scoped): topics [Symbol(kind), Address(new_reward_token)],
// body Vec[Bool] (len 1, always true). Target is the new gauge reward token.
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
