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
	// Removed marks a removal (archival eviction): a gap that leaves the key's last value in force.
	Removed        bool
	Ledger         uint32
	IntraLedgerSeq uint32 // 0 = apply order unknown
	TxHash         string
	OpIndex        uint32
	ChangeIndex    uint32
	CloseTime      time.Time
	Reserve        blend.ReserveData // KindReserve
	BstopRate      uint32            // KindConfig
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
	Protocols    map[string]Protocol // per pool; a pool absent here refuses the call
	MaxUnordered int                 // Fold errors when the emitted rows hold more unordered versions
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
}

const maxErrorSamples = 5

// Fold pairs every ResData version with its predecessor and sums the hours before commitBefore.
// Versions of later hours return in carry.Pending with every key's last value, so folding [A,B]
// then [B+1,C] through the carry equals folding [A,C]. Every version is summed or counted.
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
	sort.SliceStable(all, func(i, j int) bool { return compareOrder(all[i], all[j]) < 0 })
	committed, pending, err := split(all, commitBefore)
	if err != nil {
		return nil, Seeds{}, err
	}
	st, err := newState(seed, opts)
	if err != nil {
		return nil, Seeds{}, err
	}
	for _, l := range ledgerPools(committed) {
		if err := st.poolLedger(l); err != nil {
			return nil, Seeds{}, err
		}
	}
	rows := st.emit()
	unordered := 0
	for _, r := range rows {
		unordered += int(r.UnorderedVersions)
	}
	if unordered > opts.MaxUnordered {
		return nil, Seeds{}, fmt.Errorf("lendinginterest: %d unordered versions exceed the allowed %d; "+
			"re-derive the range's intra_ledger_seq or accept it explicitly", unordered, opts.MaxUnordered)
	}
	carry := Seeds{Reserves: st.reserves, Configs: st.configs, Fees: st.fees, Pending: pending, CommittedBefore: commitBefore}
	return rows, carry, nil
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
		if proto != BLNT {
			return fmt.Sprintf("ProtocolFees version on a %s pool", proto)
		}
		if !v.Removed && v.Fees == nil {
			return "ProtocolFees version without a value"
		}
	case KindConfig:
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
		if i > 0 {
			p := all[i-1]
			if compareOrder(p, v) == 0 {
				return nil, nil, fmt.Errorf("lendinginterest: ledger %d tx %s change %d/%d appears twice", v.Ledger, v.TxHash, v.OpIndex, v.ChangeIndex)
			}
			if p.Ledger == v.Ledger && !p.CloseTime.Equal(v.CloseTime) {
				return nil, nil, fmt.Errorf("lendinginterest: ledger %d has two close times", v.Ledger)
			}
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

// unorderedLedger is lendingAmbiguousOrderQuery's test per (pool, ledger): rows without apply order
// from more than one transaction.
func unorderedLedger(vs []Version) bool {
	zero, txs := false, map[string]struct{}{}
	for _, v := range vs {
		zero = zero || v.IntraLedgerSeq == 0
		txs[v.TxHash] = struct{}{}
	}
	return zero && len(txs) > 1
}

var errInterleaved = errors.New("lendinginterest: a transaction's changes are interleaved with another's")

// txGroups splits one ordered pool-ledger into its transactions, in apply order.
func txGroups(vs []Version) ([][]Version, error) {
	var out [][]Version
	seen := map[string]bool{}
	for i, v := range vs {
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
