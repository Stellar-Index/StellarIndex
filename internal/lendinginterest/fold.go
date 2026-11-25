// Package lendinginterest folds decoded lending-pool state versions into exact hourly interest rows.
package lendinginterest

import (
	"errors"
	"fmt"
	"math/big"
	"sort"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
)

// Protocol is the accrual arithmetic a pool runs; it is an input, never inferred from rate scale.
type Protocol string

// The protocols Fold can accrue.
const (
	BlendV1 Protocol = "blend_v1"
	BlendV2 Protocol = "blend_v2"
	BLNT    Protocol = "blnt"
)

func (p Protocol) poolVersion() (blend.PoolVersion, bool) {
	switch p {
	case BlendV1:
		return blend.PoolV1, true
	case BlendV2, BLNT:
		return blend.PoolV2, true
	}
	return blend.PoolVersionUnknown, false
}

// Kind is the pool storage key a Version changed.
type Kind uint8

// The keys Fold reads.
const (
	KindReserve Kind = iota + 1 // PoolDataKey::ResData(asset)
	KindConfig                  // the instance's pool Config, for bstop_rate
	KindFees                    // BLNT PoolDataKey::ProtocolFees
)

// Version is one stored change of one key in one transaction, already decoded by the caller.
type Version struct {
	Pool  string
	Kind  Kind
	Asset string // reserve asset strkey, KindReserve only
	// Removed marks a reserve's archival eviction: a gap that leaves its last value in force. Refused on
	// the other kinds, whose removal is a delete.
	Removed        bool
	Ledger         uint32
	IntraLedgerSeq uint32 // 0 = apply order unknown
	TxHash         string
	OpIndex        uint32
	ChangeIndex    uint32
	CloseTime      time.Time
	Created        bool              // the key's creation; the caller's check that an unseeded key is new
	Reserve        blend.ReserveData // KindReserve
	BstopRate      uint32            // KindConfig
	WasmHash       string            // KindConfig: the instance's executable, hex; the caller's WASM cross-check
	Fees           blnt.ProtocolFees // KindFees
}

// ReserveKey names one reserve of one pool.
type ReserveKey struct{ Pool, Asset string }

// Seeds is the state before a window: each key's candidate last values, in stored order, and the
// open hour's versions. A key has several candidates only after a ledger whose order is unknown.
type Seeds struct {
	Reserves map[ReserveKey][]blend.ReserveData
	Configs  map[string][]uint32 // per pool: bstop_rate
	// Fees is per BLNT pool; an empty map, not an absent one, states that the entry was never stored.
	Fees            map[string][]blnt.ProtocolFees
	Pending         []Version
	CommittedBefore time.Time // hours before this were emitted by an earlier call
}

// Options are Fold's inputs other than the data.
type Options struct {
	Protocols map[string]Protocol // per pool; a pool absent here refuses the call
	// MaxUnordered bounds each emitted row's UnorderedVersions; per row, so no verdict depends on where calls cut.
	MaxUnordered int
}

// HourRow is the interest one reserve accrued in one UTC hour; the column comments are in lending_interest.sql.
type HourRow struct {
	Pool, Asset          string
	Protocol             Protocol
	Hour                 time.Time
	FirstLedger          uint32
	LastLedger           uint32
	Versions             uint32
	Baselines            uint32 // versions with no predecessor; not stored, Versions minus the other three
	AccruedVersions      uint32
	Gross                *big.Int
	ProtocolFee          *big.Int
	Backstop             *big.Int
	SupplierIntended     *big.Int
	SupplierRealised     *big.Int
	BackstopCreditDelta  *big.Int
	UnreconciledVersions uint32
	BRateMismatches      uint32
	BackstopResidual     *big.Int
	SupplyResidual       *big.Int
	FeeMismatches        uint32
	FeeCreditResidual    *big.Int
	UnorderedVersions    uint32
	AccrualErrors        uint32
	ErrorSamples         []string
	// ExcludedCreditDelta is Σ backstop_credit deltas of unordered and errored versions, for the
	// in-line backstop identity; not stored.
	ExcludedCreditDelta *big.Int
	// Excluded are the versions whose pair with their predecessor is excluded (one per unordered or errored
	// version), for the in-line gross check; not stored.
	Excluded []Version
	// FeeOnlyCreditDelta and FeeOnlyCarryDelta are the asset's ProtocolFees movement in the hour by
	// transactions that wrote no ResData of it (a fee claim, protocol_credit_loss); FeeOnlyVersions
	// counts them. FeeOnlyUnknown counts such movements the apply order leaves open. None is stored.
	FeeOnlyCreditDelta, FeeOnlyCarryDelta *big.Int
	FeeOnlyVersions, FeeOnlyUnknown       uint32
}

const maxErrorSamples = 5

// Fold pairs every ResData version with its predecessor and sums the hours before commitBefore.
// Versions of later hours return in carry.Pending with every key's last value, so folding [A,B]
// then [B+1,C] through the carry equals folding [A,C]. Every version is summed or counted; a change
// stored twice with the same content is one version. The carry shares no memory with the inputs.
func Fold(seed Seeds, versions []Version, commitBefore time.Time, opts Options) ([]HourRow, Seeds, error) {
	commitBefore = commitBefore.UTC()
	if !commitBefore.Equal(commitBefore.Truncate(time.Hour)) {
		return nil, Seeds{}, fmt.Errorf("lendinginterest: commitBefore %s is not an hour boundary", commitBefore)
	}
	if commitBefore.Before(seed.CommittedBefore) {
		return nil, Seeds{}, fmt.Errorf("lendinginterest: commitBefore %s precedes the carry's %s", commitBefore, seed.CommittedBefore)
	}
	all := make([]Version, 0, len(seed.Pending)+len(versions))
	all = append(append(all, seed.Pending...), versions...)
	for _, v := range all {
		if err := checkVersion(v, opts, seed.CommittedBefore); err != nil {
			return nil, Seeds{}, err
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return readLess(all[i], all[j]) })
	committed, pending, err := split(dedupe(all), commitBefore)
	if err != nil {
		return nil, Seeds{}, err
	}
	st, err := newState(seed, opts)
	if err != nil {
		return nil, Seeds{}, err
	}
	for _, l := range ledgerPools(committed) {
		st.poolLedger(l)
	}
	rows := st.emit()
	for _, r := range rows {
		if int(r.UnorderedVersions) > opts.MaxUnordered {
			return nil, Seeds{}, fmt.Errorf("lendinginterest: pool %s asset %s hour %s holds %d unordered versions, over the allowed %d; "+
				"re-derive the range's intra_ledger_seq or accept it explicitly",
				r.Pool, r.Asset, r.Hour.Format(time.RFC3339), r.UnorderedVersions, opts.MaxUnordered)
		}
	}
	return rows, st.carry(pending, commitBefore), nil
}

func hourOf(v Version) time.Time { return v.CloseTime.UTC().Truncate(time.Hour) }

func checkVersion(v Version, opts Options, committedBefore time.Time) error {
	where := fmt.Sprintf("lendinginterest: pool %s ledger %d tx %s", v.Pool, v.Ledger, v.TxHash)
	proto, ok := opts.Protocols[v.Pool]
	want, known := proto.poolVersion()
	switch {
	case !ok || !known:
		return fmt.Errorf("%s: no protocol for the pool", where)
	case v.CloseTime.IsZero():
		return fmt.Errorf("%s: no close time", where)
	case hourOf(v).Before(committedBefore):
		return fmt.Errorf("%s: version in hour %s, already committed", where, hourOf(v))
	}
	if msg := payloadProblem(v, proto, want); msg != "" {
		return fmt.Errorf("%s: %s", where, msg)
	}
	return nil
}

func payloadProblem(v Version, proto Protocol, want blend.PoolVersion) string {
	switch v.Kind {
	case KindReserve:
		switch {
		case v.Asset == "":
			return "reserve version without an asset"
		case v.Removed:
		case !complete(v.Reserve):
			return "reserve version with a missing field"
		case v.Reserve.Version != want:
			return fmt.Sprintf("reserve decoded for pool version %d, protocol %s needs %d", v.Reserve.Version, proto, want)
		}
	case KindFees:
		switch {
		case proto != BLNT:
			return fmt.Sprintf("ProtocolFees version on a %s pool", proto)
		case v.Removed:
			// A delete stores no fees; a gap would keep the deleted value in force.
			return "ProtocolFees removal; pass the empty map it leaves"
		case v.Fees == nil:
			return "ProtocolFees version without a value"
		}
	case KindConfig:
		if v.Removed {
			return "instance removal; the pool's Config is gone"
		}
	default:
		return fmt.Sprintf("unknown kind %d", v.Kind)
	}
	return ""
}

// compareOrder is the state derive's order: (ledger_seq, intra_ledger_seq, tx_hash, op_index, change_index).
func compareOrder(a, b Version) int {
	switch {
	case a.Ledger != b.Ledger:
		return cmpUint(a.Ledger, b.Ledger)
	case a.IntraLedgerSeq != b.IntraLedgerSeq:
		return cmpUint(a.IntraLedgerSeq, b.IntraLedgerSeq)
	case a.TxHash != b.TxHash:
		if a.TxHash < b.TxHash {
			return -1
		}
		return 1
	case a.OpIndex != b.OpIndex:
		return cmpUint(a.OpIndex, b.OpIndex)
	}
	return cmpUint(a.ChangeIndex, b.ChangeIndex)
}

// readLess is compareOrder with the key and the content as tie-breaks, so input order never matters.
func readLess(a, b Version) bool {
	if c := compareOrder(a, b); c != 0 {
		return c < 0
	}
	switch {
	case a.Pool != b.Pool:
		return a.Pool < b.Pool
	case a.Kind != b.Kind:
		return a.Kind < b.Kind
	case a.Asset != b.Asset:
		return a.Asset < b.Asset
	}
	return content(a) < content(b)
}

// content renders everything a version stores besides its identity and position.
func content(v Version) string {
	return fmt.Sprintf("%t|%v|%d|%s|%v", v.Removed, v.Reserve, v.BstopRate, v.WasmHash, v.Fees)
}

type changeID struct {
	pool, asset, tx, content string
	kind                     Kind
	ledger                   uint32
}

// dedupe keeps the first, in read order, of a key's versions in one transaction with the same content:
// a re-read, or a re-derive's orphan at another change_index or intra_ledger_seq, is one change.
func dedupe(sorted []Version) []Version {
	seen := map[changeID]bool{}
	out := sorted[:0]
	for _, v := range sorted {
		id := changeID{v.Pool, v.Asset, v.TxHash, content(v), v.Kind, v.Ledger}
		if !seen[id] {
			seen[id] = true
			out = append(out, v)
		}
	}
	return out
}

func cmpUint(a, b uint32) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// split cuts the sorted versions at commitBefore and refuses inputs the cut could not keep exact.
func split(all []Version, commitBefore time.Time) (committed, pending []Version, err error) {
	cut := len(all)
	for i, v := range all {
		if i > 0 && all[i-1].Ledger == v.Ledger && !all[i-1].CloseTime.Equal(v.CloseTime) {
			return nil, nil, fmt.Errorf("lendinginterest: ledger %d has two close times", v.Ledger)
		}
		open := !hourOf(v).Before(commitBefore)
		if open && cut == len(all) {
			cut = i
		}
		if !open && cut < i {
			return nil, nil, fmt.Errorf("lendinginterest: ledger %d closes before an earlier ledger's hour", v.Ledger)
		}
	}
	return all[:cut], append([]Version(nil), all[cut:]...), nil
}

// ledgerPools groups sorted versions by ledger, then by pool in pool order.
func ledgerPools(vs []Version) [][]Version {
	var out [][]Version
	for i := 0; i < len(vs); {
		j := i
		for j < len(vs) && vs[j].Ledger == vs[i].Ledger {
			j++
		}
		byPool := map[string][]Version{}
		var pools []string
		for _, v := range vs[i:j] {
			if _, ok := byPool[v.Pool]; !ok {
				pools = append(pools, v.Pool)
			}
			byPool[v.Pool] = append(byPool[v.Pool], v)
		}
		sort.Strings(pools)
		for _, p := range pools {
			out = append(out, byPool[p])
		}
		i = j
	}
	return out
}

// UnorderedLedger is lendingAmbiguousOrderQuery's test per (pool, ledger): rows without apply order
// from more than one transaction. A removal changes no value, so it does not count.
func UnorderedLedger(vs []Version) bool {
	zero, txs := false, map[string]struct{}{}
	for _, v := range vs {
		if v.Removed {
			continue
		}
		zero = zero || v.IntraLedgerSeq == 0
		txs[v.TxHash] = struct{}{}
	}
	return zero && len(txs) > 1
}

var (
	errInterleaved  = errors.New("lendinginterest: a transaction's changes are interleaved with another's")
	errSamePosition = errors.New("lendinginterest: two values of a key at one position")
)

// txGroups splits one ordered pool-ledger into its transactions, in apply order. An error means the
// stored positions contradict each other, so the apply order is unknown.
func txGroups(vs []Version) ([][]Version, error) {
	var out [][]Version
	seen := map[string]bool{}
	for i, v := range vs {
		if i > 0 && compareOrder(v, vs[i-1]) == 0 && v.Kind == vs[i-1].Kind && v.Asset == vs[i-1].Asset {
			return nil, fmt.Errorf("%w: ledger %d tx %s change %d/%d", errSamePosition, v.Ledger, v.TxHash, v.OpIndex, v.ChangeIndex)
		}
		if i > 0 && v.TxHash == vs[i-1].TxHash {
			out[len(out)-1] = append(out[len(out)-1], v)
			continue
		}
		if seen[v.TxHash] {
			return nil, fmt.Errorf("%w: ledger %d tx %s", errInterleaved, v.Ledger, v.TxHash)
		}
		seen[v.TxHash] = true
		out = append(out, []Version{v})
	}
	return out, nil
}
