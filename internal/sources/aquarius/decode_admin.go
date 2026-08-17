package aquarius

import (
	"encoding/hex"
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// decode_admin.go decodes the eight governance/upgrade admin event
// kinds (ROADMAP #89, 2026-07-10 topic census). Same provenance caveat
// as decode_rewards.go: AquaToken's soroban-amm contract source is no
// longer publicly reachable, so every function below is
// reverse-engineered from real r1 ClickHouse lake bytes, not a cloned
// Rust source. Wire types/arity/positions are exact; business-meaning
// names beyond that are BEST-EFFORT where noted.
//
// GATING NOTE (corrected 2026-08-17): SEVEN of these kinds —
// apply_upgrade, commit_upgrade, set_privileged_addrs,
// apply_transfer_ownership, commit_transfer_ownership,
// enable_emergency_mode, disable_emergency_mode — are emitted by the
// REGISTERED Aquarius POOLS as well as the router, so
// dispatcher_adapter.go gates them on `reg.Has || reg.IsFactory` (the
// same protocol trust boundary as the pool-flow kinds, plus the
// router). A full-history r1 census (2026-08-17) counts ~1,679
// pool-emitted events across these seven kinds (earliest ledger
// 55,363,632): a protocol-wide staged WASM upgrade upgraded 320/337
// pools (apply_upgrade / commit_upgrade), plus pool-level ownership
// transfers, privileged-address sets, and emergency-mode toggles. The
// PRIOR gate (reg.IsFactory ONLY) fail-closed every one of these into
// an ADR-0033 recognition gap AND dropped it from Decode — real
// governance history silently lost. Two kinds STAY router-only —
// config_rewards and pool_gauge_switch_token — because the same census
// finds ZERO pool-emitted occurrences of either.
//
// The FLAGGED parallel router CA7RQDMMV6E53P5EDZA5GPWBZ33AMW2ZNO42XLI2RGRIAP4QXIARUOJQ
// and a small family of NEITHER-pool-NOR-router contracts (e.g.
// CDWVENDOPYZJV7VDIA55LDWVQOPXZPPGTHJ3HQJDBRM3YC5NC4IYWN5C,
// CAEYKKJ5LTBLVQ5EM6H433YFHKOUJRDWOW3NF355ZS3FHQZKHXLQIHKA — see
// docs/protocols/aquarius.md "Flagged — excluded from the gate") remain
// OUTSIDE the trust boundary: being in neither reg.Has nor
// reg.IsFactory, they still fail-closed per ADR-0035 / CS-026, a
// visible ADR-0033 recognition gap, never a silent mis-attribution.
// The decode functions below are exercised against real bytes from BOTH
// registered pools and the flagged/sibling contracts in
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

// decodeUpgradeHashBody is shared by apply_upgrade / commit_upgrade.
// Both carry a Vec[Bytes] body of 32-byte Wasm hashes whose ARITY
// varies by emitter and wire generation (real r1 lake bytes, full
// history 2026-08-17):
//
//	topics: [Symbol(kind)]  (topic_count=1)
//	body:   Vec[Bytes]  (length >= 1, each a 32-byte Wasm hash)
//
// Observed arities: the canonical / flagged router emits a SINGLE hash
// (1); the registered POOLS emit 2 (apply_upgrade) or 2–3
// (commit_upgrade) across the protocol-wide staged WASM upgrade. This
// used to pin `len(elts) != 1` and reject every pool body — 1,372 of
// the ~1,679 dropped pool-governance events (apply_upgrade x686 +
// commit_upgrade x686). We now decode BY ARITY: element[0] is the
// PRIMARY (new/applied/proposed) hash; the caller lands it in Target
// and carries any trailing staged hashes in Attributes. Returns every
// hash in wire order (>= 1); an empty body still fails closed.
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

// setUpgradeHashes lands the decoded Wasm hashes onto av: element[0]
// in Target (the primary new/applied/proposed hash), each trailing
// staged hash in Attributes["wasm_hash_N"] (1-indexed) — MIRRORS the
// addr_3 pattern decodeSetPrivilegedAddrs uses for its v2 trailing
// element. A single-hash (router) body lands ONLY Target, leaving
// Attributes empty.
func setUpgradeHashes(av *AdminEvent, hashes []string) {
	av.Target = hashes[0]
	for i, h := range hashes[1:] {
		av.Attributes[fmt.Sprintf("wasm_hash_%d", i+1)] = h
	}
}

// decodeApplyUpgrade decodes `apply_upgrade` — see decodeUpgradeHashBody.
// Target is the newly-applied Wasm hash (hex); pool bodies carry a
// second staged hash in Attributes["wasm_hash_1"].
func decodeApplyUpgrade(e *events.Event, closedAt time.Time) (AdminEvent, error) {
	hashes, err := decodeUpgradeHashBody(e, EventApplyUpgrade)
	if err != nil {
		return AdminEvent{}, err
	}
	av := adminEnvelope(e, AdminApplyUpgrade, closedAt)
	setUpgradeHashes(&av, hashes)
	return av, nil
}

// decodeCommitUpgrade decodes `commit_upgrade` — see
// decodeUpgradeHashBody. Fires BEFORE the matching apply_upgrade
// (staged/two-phase upgrade). Target is the proposed Wasm hash (hex);
// pool bodies carry the further staged hashes in
// Attributes["wasm_hash_1"] / Attributes["wasm_hash_2"].
func decodeCommitUpgrade(e *events.Event, closedAt time.Time) (AdminEvent, error) {
	hashes, err := decodeUpgradeHashBody(e, EventCommitUpgrade)
	if err != nil {
		return AdminEvent{}, err
	}
	av := adminEnvelope(e, AdminCommitUpgrade, closedAt)
	setUpgradeHashes(&av, hashes)
	return av, nil
}

// decodeSetPrivilegedAddrs decodes `set_privileged_addrs` — BOTH wire
// generations (contract-schema-evolution: pools/router upgrade in
// place, and the body grew an element across a WASM upgrade):
//
//	topics: [Symbol("set_privileged_addrs")]  (topic_count=1)
//	body v1: Vec[Address, Address, Address, Vec[Address]]           (length 4)
//	body v2: Vec[Address, Address, Address, Vec[Address], Address]  (length 5)
//
// Lake census (2026-08-02, full history): every event at ledgers
// ≤ 57,604,772 is the 4-element form; every event from 57,697,794
// (2025-06-25) onward is the 5-element form — same first four
// elements, plus ONE trailing plain Address. The 2026-07-10 audit
// sampled a 4-element exemplar and pinned arity==4, which left the
// canonical router's single 5-element event (ledger 57,711,797)
// undecodable — one of the 41 blind events on aquarius's first
// full-range completeness reconcile (2026-08-01). BEST-EFFORT
// semantics as before: a multi-role privileged-address set; the v2
// trailing address lands in Attributes["addr_3"]. Any other arity
// still fails closed.
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
