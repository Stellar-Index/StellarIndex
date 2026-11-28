// Package cometlp decodes the storage of the curated Comet pools whose LP shares the Blend and BLNT backstops hold, and
// values those shares from it. It is pure: no I/O, no float. Storage and compute both import it.
package cometlp

import (
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"slices"
	"sort"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
	"github.com/Stellar-Index/StellarIndex/internal/sources/comet"
)

// Pool storage as CometDEX/comet-contracts-v1 v1.0.0 and v1.1.0 lay it out (byte-identical in both;
// docs/operations/wasm-audits/comet.md "Storage layout"). Unit variants of DataKey are keyed Vec[Symbol(<Variant>)].
const (
	KeyAllRecordData = "AllRecordData"
	KeyTotalShares   = "TotalShares"
	keyController    = "Controller"
	keySwapFee       = "SwapFee"
	keyFreeze        = "Freeze"
	keyMetadata      = "METADATA" // soroban-token-sdk's bare Symbol key
)

// The audited WASM hashes of the curated pools (internal/wasmaudit/audited_wasm.json, source comet).
const (
	WASMV100 = "8abc28913035c07411ed5d134e6bfeab4723d97ddd4d1a22a0605d35c94d1a36"
	WASMV110 = "d735c3395f59510172cf5cf838823a1389b97cfec6bf24e580e9bd77d2b3e687"
)

// curatedPool is a pool's one WASM hash (neither version has an upgrade entry point) and its creation ledger, before
// which it has no storage (docs/operations/wasm-audits/comet.md).
type curatedPool struct {
	wasm     string
	deployed uint32
}

var curatedPools = map[string]curatedPool{
	comet.MainnetBackstopPool: {WASMV100, 51_499_546},
	comet.MainnetBLNTUSDCPool: {WASMV110, 65_134_030},
	comet.MainnetBLNTXLMPool:  {WASMV110, 65_134_032},
}

// Pools is the curated pools in strkey order.
func Pools() []string { return slices.Sorted(maps.Keys(curatedPools)) }

// AuditedPoolWASM returns the WASM hash pool's storage is decoded against; ok is false for a contract outside the
// curated set.
func AuditedPoolWASM(pool string) (string, bool) {
	p, ok := curatedPools[pool]
	return p.wasm, ok
}

// PoolDeployed returns pool's creation ledger; ok is false for a contract outside the curated set.
func PoolDeployed(pool string) (uint32, bool) {
	p, ok := curatedPools[pool]
	return p.deployed, ok
}

// Errors of the storage decoders.
var (
	// ErrNotGatedPool is storage of a contract outside the curated set: the same key shape from any Balancer-v1 fork.
	ErrNotGatedPool = errors.New("cometlp: contract is not a curated Comet pool")
	// ErrUnauditedWASM is a curated pool whose instance runs a WASM other than its audited hash.
	ErrUnauditedWASM = errors.New("cometlp: pool runs an unaudited WASM")
	// ErrMalformedStorage is an entry that is not the audited layout, including an unknown field or key.
	ErrMalformedStorage = errors.New("cometlp: malformed pool storage")
)

// Record is one token's DataKey::AllRecordData entry; Balance, Weight and Scalar are i128 (Weight at 1e7 scale).
type Record struct {
	Token                   string
	Balance, Weight, Scalar *big.Int
	Index                   uint32
}

// Instance is the instance-storage fields valuation reads; Freeze is false when the key is absent.
type Instance struct {
	WASM       string // hex WASM hash of the instance's executable
	Controller string
	SwapFee    *big.Int // 1e7 scale
	Freeze     bool
}

// PoolState is one pool's records, LP supply and instance, gated on contract identity and audited WASM.
type PoolState struct {
	Pool        string
	Records     []Record // by Index
	TotalShares *big.Int // LP base units (7 dp)
	Instance    Instance
}

// NewPoolState decodes a pool's three entries' values and refuses a contract outside the curated set or an instance
// running another WASM than the pool's audited hash.
func NewPoolState(pool string, records, totalShares, instance scval.ScVal) (PoolState, error) {
	want, ok := AuditedPoolWASM(pool)
	if !ok {
		return PoolState{}, fmt.Errorf("%w: %s", ErrNotGatedPool, pool)
	}
	inst, err := DecodeInstance(instance)
	if err != nil {
		return PoolState{}, err
	}
	if inst.WASM != want {
		return PoolState{}, fmt.Errorf("%w: %s runs %s, audited %s", ErrUnauditedWASM, pool, inst.WASM, want)
	}
	recs, err := DecodeAllRecordData(records)
	if err != nil {
		return PoolState{}, err
	}
	s, err := DecodeTotalShares(totalShares)
	if err != nil {
		return PoolState{}, err
	}
	return PoolState{Pool: pool, Records: recs, TotalShares: s, Instance: inst}, nil
}

// DecodeAllRecordData decodes Map<Address, Record{balance, weight, scalar: i128, index: u32}>; a missing, unknown or
// mistyped field, a duplicate index or a non-address key is an error.
func DecodeAllRecordData(v scval.ScVal) ([]Record, error) {
	m, err := scval.AsMap(v)
	if err != nil {
		return nil, fmt.Errorf("%w: AllRecordData: %w", ErrMalformedStorage, err)
	}
	if len(m) == 0 {
		return nil, fmt.Errorf("%w: AllRecordData is empty", ErrMalformedStorage)
	}
	out := make([]Record, 0, len(m))
	for _, e := range m {
		token, err := scval.AsAddressStrkey(e.Key)
		if err != nil {
			return nil, fmt.Errorf("%w: AllRecordData key: %w", ErrMalformedStorage, err)
		}
		r, err := decodeRecord(e.Val)
		if err != nil {
			return nil, fmt.Errorf("%w: record of %s: %w", ErrMalformedStorage, token, err)
		}
		r.Token = token
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	for i := 1; i < len(out); i++ {
		if out[i].Index == out[i-1].Index {
			return nil, fmt.Errorf("%w: two records at index %d", ErrMalformedStorage, out[i].Index)
		}
	}
	return out, nil
}

func decodeRecord(v scval.ScVal) (Record, error) {
	m, err := scval.AsMap(v)
	if err != nil {
		return Record{}, err
	}
	var r Record
	seen := map[string]bool{}
	for _, e := range m {
		name, err := scval.AsSymbol(e.Key)
		if err != nil {
			return Record{}, err
		}
		if seen[name] {
			return Record{}, fmt.Errorf("field %q twice", name)
		}
		seen[name] = true
		switch name {
		case "balance":
			r.Balance, err = i128(e.Val)
		case "weight":
			r.Weight, err = i128(e.Val)
		case "scalar":
			r.Scalar, err = i128(e.Val)
		case "index":
			r.Index, err = scval.AsU32(e.Val)
		default:
			return Record{}, fmt.Errorf("unknown field %q", name)
		}
		if err != nil {
			return Record{}, fmt.Errorf("field %q: %w", name, err)
		}
	}
	if len(seen) != 4 {
		return Record{}, fmt.Errorf("fields %v, want balance, weight, scalar and index", slices.Sorted(maps.Keys(seen)))
	}
	return r, nil
}

// DecodeTotalShares decodes DataKey::TotalShares, an i128 of LP base units.
func DecodeTotalShares(v scval.ScVal) (*big.Int, error) {
	s, err := i128(v)
	if err != nil {
		return nil, fmt.Errorf("%w: TotalShares: %w", ErrMalformedStorage, err)
	}
	return s, nil
}

// DecodeInstance decodes a pool's ScvContractInstance: its WASM hash and the Controller, SwapFee and Freeze keys. Any
// instance key other than those and METADATA is an error.
func DecodeInstance(v scval.ScVal) (Instance, error) {
	ci, ok := v.GetInstance()
	if !ok || ci.Executable.WasmHash == nil { // set for a WASM executable only, never for a Stellar Asset Contract
		return Instance{}, fmt.Errorf("%w: not a WASM contract instance", ErrMalformedStorage)
	}
	inst := Instance{WASM: hex.EncodeToString(ci.Executable.WasmHash[:])}
	if ci.Storage != nil {
		for _, e := range *ci.Storage {
			if err := inst.set(e); err != nil {
				return Instance{}, fmt.Errorf("%w: instance: %w", ErrMalformedStorage, err)
			}
		}
	}
	if inst.Controller == "" || inst.SwapFee == nil {
		return Instance{}, fmt.Errorf("%w: instance lacks Controller or SwapFee", ErrMalformedStorage)
	}
	return inst, nil
}

func (inst *Instance) set(e scval.ScMapEntry) error {
	if sym, err := scval.AsSymbol(e.Key); err == nil && sym == keyMetadata {
		return nil
	}
	vec, err := scval.AsVec(e.Key)
	if err != nil || len(vec) != 1 {
		return fmt.Errorf("unknown key %s", scval.Display(e.Key))
	}
	name, err := scval.AsSymbol(vec[0])
	if err != nil {
		return fmt.Errorf("unknown key %s", scval.Display(e.Key))
	}
	switch name {
	case keyController:
		inst.Controller, err = scval.AsAddressStrkey(e.Val)
	case keySwapFee:
		inst.SwapFee, err = i128(e.Val)
	case keyFreeze:
		inst.Freeze, err = scval.AsBool(e.Val)
	default:
		return fmt.Errorf("unknown key %q", name)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

func i128(v scval.ScVal) (*big.Int, error) {
	a, err := scval.AsAmountFromI128(v)
	if err != nil {
		return nil, err
	}
	return a.BigInt(), nil
}
