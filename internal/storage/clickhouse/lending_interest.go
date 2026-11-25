package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/lendinginterest"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// LendingInterestKey is one exact storage key the interest pass reads, with what it holds.
type LendingInterestKey struct {
	Key      string // base64 key_xdr
	Pool     string
	Protocol lendinginterest.Protocol
	Kind     lendinginterest.Kind
	Asset    string // reserve asset strkey, KindReserve only
}

// lendingInterestProtocol is a pool's accrual arithmetic; only the registry's protocol names it, never a decoded scale.
func lendingInterestProtocol(c LendingContract) (lendinginterest.Protocol, error) {
	if c.Backstop {
		return "", fmt.Errorf("clickhouse: lending interest: %s is a backstop, not a pool", c.ID)
	}
	switch c.Protocol {
	case LendingBlendV1, LendingBlendV2, LendingBLNT:
		return lendinginterest.Protocol(c.Protocol), nil
	}
	return "", fmt.Errorf("clickhouse: lending interest: pool %s has unknown protocol %q", c.ID, c.Protocol)
}

// ledgerHour is the UTC hour ledger seq closed in; ok=false when stellar.ledgers has no row for it.
func ledgerHour(ctx context.Context, conn driver.Conn, seq uint32) (time.Time, bool, error) {
	rows, err := conn.Query(ctx, `SELECT toStartOfHour(close_time) FROM stellar.ledgers WHERE ledger_seq = ? LIMIT 1`, seq)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("clickhouse: close time of ledger %d: %w", seq, err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return time.Time{}, false, rows.Err()
	}
	var h time.Time
	if err := rows.Scan(&h); err != nil {
		return time.Time{}, false, fmt.Errorf("clickhouse: close time of ledger %d: %w", seq, err)
	}
	return h.UTC(), true, nil
}

// lendingStateThrough refuses a read through ledger hi that lending_state_hourly is not complete through.
func lendingStateThrough(ctx context.Context, conn driver.Conn, hi uint32) error {
	state, err := entryWatermarkOn(ctx, conn, lendingStateWatermarkName)
	if err != nil {
		return err
	}
	if state < hi {
		return fmt.Errorf("%w: through %d but lending_state stops at %d", ErrLendingInterestAheadOfState, hi, state)
	}
	return nil
}

// lendingStateBefore refuses seeds before hour unless lending_state_hourly holds every ledger closing before it: the
// ledger after its watermark (or, if that is not stored, the watermark's own) closes at or after hour.
func lendingStateBefore(ctx context.Context, conn driver.Conn, hour time.Time) error {
	state, err := entryWatermarkOn(ctx, conn, lendingStateWatermarkName)
	if err != nil {
		return err
	}
	for _, seq := range []uint32{state + 1, state} {
		if state == 0 {
			break
		}
		h, ok, err := ledgerHour(ctx, conn, seq)
		if err != nil {
			return err
		}
		if ok {
			if !h.Before(hour) {
				return nil
			}
			break
		}
	}
	return fmt.Errorf("%w: seeds before %s but lending_state stops at ledger %d", ErrLendingInterestAheadOfState,
		hour.Format(time.RFC3339), state)
}

// lendingInterestResKeysQuery lists the ResData keys lending_state_hourly holds through an hour, with the protocol it
// derived each under. Bounded by hour, not ledger: a key first changed at or below hi whose hour ends past hi has its
// row at a later ledger.
const lendingInterestResKeysQuery = `SELECT DISTINCT contract_id, key_xdr, protocol FROM stellar.lending_state_hourly FINAL
	WHERE contract_id IN ? AND hour <= ? AND arrayExists(p -> startsWith(key_xdr, p), ?)
	ORDER BY contract_id, key_xdr, protocol`

// lendingInterestResRow is one ResData key lending_state_hourly holds.
type lendingInterestResRow struct{ Pool, Key, Protocol string }

// LendingInterestKeys lists the exact keys the interest pass reads through ledger hi: every ResData key
// lending_state_hourly holds for pools through hi's hour, each pool's instance keys, and BLNT's ProtocolFees key.
// It refuses an hi past the lending_state watermark, where a reserve first written in the window has no row yet.
func LendingInterestKeys(ctx context.Context, addr string, pools []LendingContract, hi uint32) ([]LendingInterestKey, error) {
	ps, err := LendingPrefixes(pools)
	if err != nil {
		return nil, err
	}
	var ids []any
	var resPrefixes []string
	for _, c := range pools {
		ids = append(ids, c.ID)
	}
	for _, p := range ps {
		if p.Key == "ResData" {
			resPrefixes = append(resPrefixes, p.Prefix)
		}
	}
	if len(ids) == 0 {
		return nil, errors.New("clickhouse: lending interest keys: no pools")
	}
	conn, err := openRead(ctx, addr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	if err := lendingStateThrough(ctx, conn, hi); err != nil {
		return nil, err
	}
	hour, ok, err := ledgerHour(ctx, conn, hi)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("clickhouse: lending interest keys through %d: the ledger is not in stellar.ledgers", hi)
	}
	rows, err := conn.Query(ctx, lendingInterestResKeysQuery, clickhouse.GroupSet{Value: ids}, hour, resPrefixes)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: lending interest keys through %d: %w", hi, err)
	}
	defer func() { _ = rows.Close() }()
	var res []lendingInterestResRow
	for rows.Next() {
		var r lendingInterestResRow
		if err := rows.Scan(&r.Pool, &r.Key, &r.Protocol); err != nil {
			return nil, fmt.Errorf("clickhouse: lending interest keys through %d: %w", hi, err)
		}
		res = append(res, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clickhouse: lending interest keys through %d: %w", hi, err)
	}
	return lendingInterestKeyList(pools, ps, res)
}

// lendingInterestKeyList assembles the key list from the pools' prefixes and the ResData rows read.
func lendingInterestKeyList(pools []LendingContract, ps []LendingPrefix, res []lendingInterestResRow) ([]LendingInterestKey, error) {
	protos := make(map[string]lendinginterest.Protocol, len(pools))
	for _, c := range pools {
		p, err := lendingInterestProtocol(c)
		if err != nil {
			return nil, err
		}
		protos[c.ID] = p
	}
	var out []LendingInterestKey
	for _, r := range res {
		proto, ok := protos[r.Pool]
		if !ok {
			return nil, fmt.Errorf("clickhouse: lending interest: ResData key %s of unrequested pool %s", r.Key, r.Pool)
		}
		if r.Protocol != string(proto) {
			return nil, fmt.Errorf("clickhouse: lending interest: pool %s is %s in the registry but lending_state_hourly "+
				"derived its key %s as %s", r.Pool, proto, r.Key, r.Protocol)
		}
		asset, err := resDataKeyAsset(r.Key, r.Pool)
		if err != nil {
			return nil, err
		}
		out = append(out, LendingInterestKey{Key: r.Key, Pool: r.Pool, Protocol: proto, Kind: lendinginterest.KindReserve, Asset: asset})
	}
	for _, p := range ps {
		k := LendingInterestKey{Key: p.Prefix, Pool: p.Contract, Protocol: protos[p.Contract]}
		switch p.Key {
		case "Instance":
			k.Kind = lendinginterest.KindConfig
		case "ProtocolFees":
			k.Kind = lendinginterest.KindFees
		default:
			continue
		}
		out = append(out, k)
	}
	return out, nil
}

// resDataKeyAsset decodes the asset strkey of pool's PoolDataKey::ResData(asset) key.
func resDataKeyAsset(b64, pool string) (string, error) {
	fail := func(why string) (string, error) {
		return "", fmt.Errorf("clickhouse: lending interest: key %s of pool %s: %s", b64, pool, why)
	}
	var lk xdr.LedgerKey
	if err := xdr.SafeUnmarshalBase64(b64, &lk); err != nil {
		return fail(err.Error())
	}
	cd, ok := lk.GetContractData()
	if !ok || cd.Contract.ContractId == nil {
		return fail("not a contract's data key")
	}
	if owner, err := scval.AsAddressStrkey(xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &cd.Contract}); err != nil || owner != pool {
		return fail("held by another contract")
	}
	vec, ok := cd.Key.GetVec()
	if !ok || vec == nil || len(*vec) != 2 || (*vec)[0].Sym == nil || string(*(*vec)[0].Sym) != "ResData" {
		return fail("not ResData(asset)")
	}
	asset, err := scval.AsAddressStrkey((*vec)[1])
	if err != nil {
		return fail(err.Error())
	}
	return asset, nil
}

// lendingInterestChange is one ledger_entry_changes row of a listed key.
type lendingInterestChange struct {
	Key, TxHash, ChangeType, Entry string
	Ledger, Intra, ChangeIndex     uint32
	OpIndex                        int32
	CloseTime                      time.Time
}

// lendingInterestVersionsQuery reads every post-change image of the listed keys in apply order. FINAL as in
// lendingMatchedChanges; the exact IN list lets the key_xdr bloom index prune.
const lendingInterestVersionsQuery = `SELECT key_xdr, ledger_seq, intra_ledger_seq, tx_hash, op_index, change_index, change_type,
		close_time, entry_xdr
	FROM stellar.ledger_entry_changes FINAL
	WHERE ledger_seq BETWEEN ? AND ? AND entry_type = 'contract_data' AND change_type != 'state' AND key_xdr IN ?
	ORDER BY ledger_seq, intra_ledger_seq, tx_hash, op_index, change_index`

// LendingInterestVersions streams every change of keys in [lo, hi], in apply order, decoded, to fn. The lake must hold
// entry changes for every tx-bearing ledger of the window before and after the read; a change that does not decode, a
// renumbering leftover or a deleted ResData or instance stops the read with an error naming it. Any error, even one
// returned after fn has run, refuses the whole window.
func LendingInterestVersions(ctx context.Context, addr string, lo, hi uint32, keys []LendingInterestKey,
	fn func(lendinginterest.Version) error,
) error {
	if lo > hi || len(keys) == 0 {
		return fmt.Errorf("clickhouse: lending interest versions [%d,%d]: empty range or no keys", lo, hi)
	}
	byKey := make(map[string]LendingInterestKey, len(keys))
	list := make([]any, 0, len(keys))
	for _, k := range keys {
		byKey[k.Key] = k
		list = append(list, k.Key)
	}
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	ctx = clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{
		"max_execution_time": 3600, "do_not_merge_across_partitions_select_final": 1,
	}))
	if err := lendingInterestLakeCovered(ctx, conn, lo, hi); err != nil {
		return err
	}
	if err := streamLendingInterestVersions(ctx, conn, lo, hi, list, byKey, fn); err != nil {
		return err
	}
	// Again after: a ch-rebuild or orphan delete that ran during the read would otherwise leave it short.
	return lendingInterestLakeCovered(ctx, conn, lo, hi)
}

// lendingInterestLakeCovered proves the lake holds entry changes for every tx-bearing ledger of [lo, hi].
func lendingInterestLakeCovered(ctx context.Context, conn driver.Conn, lo, hi uint32) error {
	n, err := lakeTxLedgers(ctx, conn, lendingInterestWatermarkName, lo, hi)
	if err != nil {
		return err
	}
	return entryHistoryWindowCovered(ctx, conn, lendingInterestWatermarkName, lo, hi, n)
}

func streamLendingInterestVersions(ctx context.Context, conn driver.Conn, lo, hi uint32, list []any,
	byKey map[string]LendingInterestKey, fn func(lendinginterest.Version) error,
) error {
	rows, err := conn.Query(ctx, lendingInterestVersionsQuery, lo, hi, clickhouse.GroupSet{Value: list})
	if err != nil {
		return fmt.Errorf("clickhouse: lending interest versions [%d,%d]: %w", lo, hi, err)
	}
	defer func() { _ = rows.Close() }()
	var leftovers lendingInterestLeftovers
	for rows.Next() {
		var c lendingInterestChange
		if err := rows.Scan(&c.Key, &c.Ledger, &c.Intra, &c.TxHash, &c.OpIndex, &c.ChangeIndex, &c.ChangeType,
			&c.CloseTime, &c.Entry); err != nil {
			return fmt.Errorf("clickhouse: lending interest versions [%d,%d]: %w", lo, hi, err)
		}
		if err := leftovers.check(c); err != nil {
			return err
		}
		v, err := lendingInterestVersion(byKey[c.Key], c)
		if err != nil {
			return err
		}
		if err := fn(v); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("clickhouse: lending interest versions [%d,%d]: %w", lo, hi, err)
	}
	return nil
}

// lendingInterestLeftovers finds renumbering leftovers in one ledger: rows of one key with equal tx_hash, op_index,
// change_type and entry_xdr at different intra_ledger_seq. FINAL keeps both (their change_index differs) and Fold would
// keep whichever sorts first, possibly the leftover at the other walk's position. A transaction's legitimate second
// row of a key (P23 auto-restore's restored then updated) differs in change_type, so it never matches.
type lendingInterestLeftovers struct {
	ledger uint32
	seen   map[string]uint32
}

func (l *lendingInterestLeftovers) check(c lendingInterestChange) error {
	if l.seen == nil || c.Ledger != l.ledger {
		l.ledger, l.seen = c.Ledger, map[string]uint32{}
	}
	id := strings.Join([]string{c.Key, c.TxHash, strconv.Itoa(int(c.OpIndex)), c.ChangeType, c.Entry}, "\x00")
	if intra, ok := l.seen[id]; ok && intra != c.Intra {
		return fmt.Errorf("clickhouse: lending interest: ledger %d tx %s key %s holds one change at intra_ledger_seq %d and %d, "+
			"a renumbering leftover; run docs/operations/runbooks/entry-walk-renumbering.md's cleanup over the ledger first",
			c.Ledger, c.TxHash, c.Key, intra, c.Intra)
	}
	l.seen[id] = c.Intra
	return nil
}

// lendingInterestVersion decodes one change of k.
func lendingInterestVersion(k LendingInterestKey, c lendingInterestChange) (lendinginterest.Version, error) {
	fail := func(format string, a ...any) (lendinginterest.Version, error) {
		return lendinginterest.Version{}, fmt.Errorf("clickhouse: lending interest: ledger %d tx %s key %s: %s",
			c.Ledger, c.TxHash, c.Key, fmt.Sprintf(format, a...))
	}
	if k.Key != c.Key {
		return fail("not a requested key")
	}
	if c.OpIndex < 0 {
		return fail("transaction-level change (op_index %d) has no apply position", c.OpIndex)
	}
	v := lendinginterest.Version{
		Pool: k.Pool, Kind: k.Kind, Asset: k.Asset, Ledger: c.Ledger, IntraLedgerSeq: c.Intra, TxHash: c.TxHash,
		OpIndex: uint32(c.OpIndex), ChangeIndex: c.ChangeIndex, CloseTime: c.CloseTime.UTC(),
	}
	switch c.ChangeType {
	case "removed":
		if err := lendingInterestDeleted(k); err != nil {
			return fail("%v", err)
		}
		v.Fees = noBLNTFees()
		return v, nil
	case "created", "updated", "restored":
	default:
		return fail("unknown change type %q", c.ChangeType)
	}
	if err := decodeLendingInterestEntry(k, c.Entry, &v); err != nil {
		return fail("%v", err)
	}
	return v, nil
}

// lendingInterestDeleted is the meaning of a removal of k. The lake writes no eviction of a persistent key, so a
// removal is the contract's own delete: BLNT deleting ProtocolFees stores no fees, the explicit empty map. Blend v1/v2
// remove only temporary entries, so a deleted ResData or instance is outside the audited code and refuses.
func lendingInterestDeleted(k LendingInterestKey) error {
	if k.Kind == lendinginterest.KindFees {
		return nil
	}
	return fmt.Errorf("pool %s deleted its %s entry, which the audited pool code never does", k.Pool, lendingInterestKindName(k.Kind))
}

func lendingInterestKindName(k lendinginterest.Kind) string {
	switch k {
	case lendinginterest.KindReserve:
		return "ResData"
	case lendinginterest.KindConfig:
		return "instance"
	case lendinginterest.KindFees:
		return "ProtocolFees"
	}
	return "unknown-kind"
}

// decodeLendingInterestEntry decodes entry into v's payload for k's kind.
func decodeLendingInterestEntry(k LendingInterestKey, entry string, v *lendinginterest.Version) error {
	val, _, ok := contractDataEntry(entry)
	if !ok {
		return errors.New("entry is not contract data")
	}
	switch k.Kind {
	case lendinginterest.KindReserve:
		model, ok := historyModel(string(k.Protocol))
		if !ok {
			return fmt.Errorf("no reserve decoder for protocol %q", k.Protocol)
		}
		rd, err := model.decode(val)
		if err != nil {
			return fmt.Errorf("ResData: %w", err)
		}
		v.Reserve = rd
	case lendinginterest.KindConfig:
		inst, ok := poolInstanceFromStorage(val)
		if !ok {
			// 0 is a legal backstop rate, so an instance without a readable Config cannot stand for one.
			return errors.New("instance has no decodable pool Config")
		}
		v.BstopRate = inst.Config.BstopRate
	case lendinginterest.KindFees:
		f, err := decodeBLNTFees(val)
		if err != nil {
			return fmt.Errorf("ProtocolFees: %w", err)
		}
		v.Fees = f
	default:
		return fmt.Errorf("unknown key kind %d", k.Kind)
	}
	return nil
}

// lendingInterestSeedsQuery takes each key's last row before an hour.
const lendingInterestSeedsQuery = `SELECT key_xdr, last.1, last.2 FROM (
		SELECT key_xdr, argMax((ledger_seq, entry_xdr), hour) AS last
		FROM stellar.lending_state_hourly FINAL
		WHERE contract_id IN ? AND key_xdr IN ? AND hour < ?
		GROUP BY key_xdr
	)`

// lendingInterestSeedRow is one key's last lending_state_hourly row before the seed hour.
type lendingInterestSeedRow struct {
	Key    string
	Ledger uint32
	Entry  string
}

// LendingInterestSeeds reads each key's value in force at beforeHour from lending_state_hourly: the state a window
// starting at that hour folds from. A reserve with no row before the hour gets no seed (it is new); a BLNT pool with
// no ProtocolFees row, or whose last one is a delete, gets an explicit empty map. It refuses unless lending_state is
// complete through every ledger closing before beforeHour.
func LendingInterestSeeds(ctx context.Context, addr string, pools []LendingContract, keys []LendingInterestKey,
	beforeHour time.Time,
) (lendinginterest.Seeds, error) {
	beforeHour = beforeHour.UTC()
	if !beforeHour.Equal(beforeHour.Truncate(time.Hour)) {
		return lendinginterest.Seeds{}, fmt.Errorf("clickhouse: lending interest seeds: %s is not an hour boundary", beforeHour)
	}
	if len(pools) == 0 || len(keys) == 0 {
		return lendinginterest.Seeds{}, errors.New("clickhouse: lending interest seeds: no pools or keys")
	}
	ids := make([]any, 0, len(pools))
	for _, c := range pools {
		ids = append(ids, c.ID)
	}
	list := make([]any, 0, len(keys))
	for _, k := range keys {
		list = append(list, k.Key)
	}
	conn, err := openRead(ctx, addr)
	if err != nil {
		return lendinginterest.Seeds{}, err
	}
	defer func() { _ = conn.Close() }()
	if err := lendingStateBefore(ctx, conn, beforeHour); err != nil {
		return lendinginterest.Seeds{}, err
	}
	rows, err := conn.Query(ctx, lendingInterestSeedsQuery, clickhouse.GroupSet{Value: ids}, clickhouse.GroupSet{Value: list}, beforeHour)
	if err != nil {
		return lendinginterest.Seeds{}, fmt.Errorf("clickhouse: lending interest seeds before %s: %w", beforeHour, err)
	}
	defer func() { _ = rows.Close() }()
	var got []lendingInterestSeedRow
	for rows.Next() {
		var r lendingInterestSeedRow
		if err := rows.Scan(&r.Key, &r.Ledger, &r.Entry); err != nil {
			return lendinginterest.Seeds{}, fmt.Errorf("clickhouse: lending interest seeds before %s: %w", beforeHour, err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		return lendinginterest.Seeds{}, fmt.Errorf("clickhouse: lending interest seeds before %s: %w", beforeHour, err)
	}
	return lendingInterestSeeds(pools, keys, got, beforeHour)
}

// lendingInterestSeeds decodes the seed rows. lending_state_hourly refuses an hour whose last ledger leaves a key's
// order unknown, so each key has one candidate.
func lendingInterestSeeds(pools []LendingContract, keys []LendingInterestKey, got []lendingInterestSeedRow,
	beforeHour time.Time,
) (lendinginterest.Seeds, error) {
	s := lendinginterest.Seeds{CommittedBefore: beforeHour}
	byKey := make(map[string]LendingInterestKey, len(keys))
	for _, k := range keys {
		byKey[k.Key] = k
	}
	for _, r := range got {
		k, ok := byKey[r.Key]
		if !ok {
			return lendinginterest.Seeds{}, fmt.Errorf("clickhouse: lending interest seed for unrequested key %s", r.Key)
		}
		var v lendinginterest.Version
		if r.Entry == "" {
			if err := lendingInterestDeleted(k); err != nil {
				return lendinginterest.Seeds{}, fmt.Errorf("clickhouse: lending interest seed: ledger %d key %s: %w", r.Ledger, r.Key, err)
			}
			v.Fees = noBLNTFees()
		} else if err := decodeLendingInterestEntry(k, r.Entry, &v); err != nil {
			return lendinginterest.Seeds{}, fmt.Errorf("clickhouse: lending interest seed: ledger %d key %s: %w", r.Ledger, r.Key, err)
		}
		switch k.Kind {
		case lendinginterest.KindReserve:
			seedOne(&s.Reserves, lendinginterest.ReserveKey{Pool: k.Pool, Asset: k.Asset}, v.Reserve)
		case lendinginterest.KindConfig:
			seedOne(&s.Configs, k.Pool, v.BstopRate)
		case lendinginterest.KindFees:
			seedOne(&s.Fees, k.Pool, v.Fees)
		}
	}
	for _, c := range pools {
		p, err := lendingInterestProtocol(c)
		if err != nil {
			return lendinginterest.Seeds{}, err
		}
		if _, ok := s.Fees[c.ID]; !ok && p == lendinginterest.BLNT {
			// Empty, not absent: Fold reads an absent entry as unknown fees and refuses the pair.
			seedOne(&s.Fees, c.ID, noBLNTFees())
		}
	}
	return s, nil
}

// seedOne sets key k's only candidate.
func seedOne[K comparable, V any](m *map[K][]V, k K, v V) {
	if *m == nil {
		*m = map[K][]V{}
	}
	(*m)[k] = []V{v}
}

// InsertLendingInterestRows writes the fold's hour rows to stellar.lending_interest_hourly under derive revision
// rev; the event-reconciliation columns are written as zero. A row missing an amount is refused, never zeroed.
func InsertLendingInterestRows(ctx context.Context, addr string, rows []lendinginterest.HourRow, rev uint16) error {
	if len(rows) == 0 {
		return nil
	}
	for _, r := range rows {
		if err := checkLendingInterestRow(r); err != nil {
			return err
		}
	}
	conn, err := openAccountMovementsWrite(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	batch, err := conn.PrepareBatch(ctx, `INSERT INTO stellar.lending_interest_hourly
		(contract_id, protocol, asset, hour, first_ledger, last_ledger, versions, accrued_versions,
		 gross, protocol_fee, backstop, supplier_intended, supplier_realised, backstop_credit_delta,
		 unreconciled_versions, brate_mismatches, backstop_residual, supply_residual, fee_mismatches, fee_credit_residual,
		 residual_explained, residual_attributed, explained_versions, attributed_versions,
		 unordered_versions, accrual_errors, error_samples, derive_rev)`)
	if err != nil {
		return fmt.Errorf("prepare lending_interest_hourly batch: %w", err)
	}
	for _, r := range rows {
		samples := r.ErrorSamples
		if samples == nil {
			samples = []string{}
		}
		if err := batch.Append(r.Pool, string(r.Protocol), r.Asset, r.Hour.UTC(), r.FirstLedger, r.LastLedger, r.Versions,
			r.AccruedVersions, r.Gross, r.ProtocolFee, r.Backstop, r.SupplierIntended, r.SupplierRealised,
			r.BackstopCreditDelta, r.UnreconciledVersions, r.BRateMismatches, r.BackstopResidual, r.SupplyResidual,
			r.FeeMismatches, r.FeeCreditResidual, new(big.Int), new(big.Int), uint32(0), uint32(0),
			r.UnorderedVersions, r.AccrualErrors, samples, rev); err != nil {
			return fmt.Errorf("append lending_interest_hourly %s/%s/%s: %w", r.Pool, r.Asset, r.Hour, err)
		}
	}
	return wrapSend(batch.Send(), "lending_interest_hourly")
}

func checkLendingInterestRow(r lendinginterest.HourRow) error {
	for _, n := range []*big.Int{
		r.Gross, r.ProtocolFee, r.Backstop, r.SupplierIntended, r.SupplierRealised,
		r.BackstopCreditDelta, r.BackstopResidual, r.SupplyResidual, r.FeeCreditResidual,
	} {
		if n == nil {
			return fmt.Errorf("clickhouse: lending interest row %s/%s/%s has a missing amount", r.Pool, r.Asset, r.Hour)
		}
	}
	if r.Pool == "" || r.Asset == "" || r.Protocol == "" {
		return fmt.Errorf("clickhouse: lending interest row %q/%q/%s lacks its identity", r.Pool, r.Asset, r.Hour)
	}
	return nil
}

// DeleteSupersededLendingInterestRows deletes pools' rows of hours [fromHour, toHour] written before `before`. A
// re-derive calls it after InsertLendingInterestRows, with ServerNow read before that insert, so an hour it no longer
// writes loses its old row while every hour it does write stays readable throughout. It waits for the delete to apply.
func DeleteSupersededLendingInterestRows(ctx context.Context, addr string, pools []LendingContract, fromHour, toHour,
	before time.Time,
) error {
	if len(pools) == 0 || toHour.Before(fromHour) {
		return fmt.Errorf("clickhouse: delete superseded lending interest rows: no pools or hours [%s,%s]", fromHour, toHour)
	}
	ids := make([]any, 0, len(pools))
	for _, c := range pools {
		ids = append(ids, c.ID)
	}
	conn, err := openAccountMovementsWrite(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	ctx = clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{"lightweight_deletes_sync": 2, "max_execution_time": 3600}))
	if err := conn.Exec(ctx, `DELETE FROM stellar.lending_interest_hourly
		WHERE contract_id IN ? AND hour BETWEEN ? AND ? AND derived_at < fromUnixTimestamp64Milli(?)`,
		clickhouse.GroupSet{Value: ids}, fromHour.UTC(), toHour.UTC(), before.UnixMilli()); err != nil {
		return fmt.Errorf("clickhouse: delete superseded lending_interest_hourly [%s,%s]: %w", fromHour, toHour, err)
	}
	return nil
}

const lendingInterestWatermarkName = "lending_interest"

// lendingInterestFloorName's row holds the first ledger the interest rows cover; max() over unmerged rows can only
// raise it, which narrows the claim rather than widening it.
const lendingInterestFloorName = "lending_interest_floor"

// lendingInterestPoolPrefix + pool names the row recording the ledger that pool's interest rows are complete through;
// a pool is covered only up to it, whatever the shared watermark says.
const lendingInterestPoolPrefix = "lending_interest:"

// ErrLendingInterestAheadOfState reports a refused read or advance past the lending_state watermark: the interest rows
// are keyed from, seeded from and checked against lending_state_hourly, so they never cover a ledger it does not.
var ErrLendingInterestAheadOfState = errors.New("clickhouse: lending interest window runs ahead of the lending_state watermark")

// ErrLendingInterestNotHourEnd reports a refused advance to a ledger that is not the last of its hour: only complete
// hours are written, so the watermark rests on hour ends.
var ErrLendingInterestNotHourEnd = errors.New("clickhouse: lending interest watermark is not the last ledger of an hour")

// ErrLendingInterestNotHourStart reports a refused advance from a ledger that is neither the first of its hour nor the
// floor: seeds are hour ends, so a window starting mid-hour would claim that hour's earlier ledgers unread.
var ErrLendingInterestNotHourStart = errors.New("clickhouse: lending interest window does not start at an hour's first ledger")

// ErrLendingInterestPoolUncovered reports a refused advance while a pool's interest rows stop below the watermark.
var ErrLendingInterestPoolUncovered = errors.New("clickhouse: lending interest leaves a pool uncovered below the watermark")

// LendingInterestWatermark returns the ledger lending_interest_hourly is complete through (0 = never run).
func LendingInterestWatermark(ctx context.Context, addr string) (uint32, error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()
	return entryWatermarkOn(ctx, conn, lendingInterestWatermarkName)
}

// SetLendingInterestWatermark records pools' interest rows complete through thru: from an hour's first ledger (or the
// floor) to an hour's last, never past lending_state's watermark, over entry changes the lake holds, and only while
// LendingInterestUncoveredPools lists none of pools. The first advance records from as the floor. A window at or below
// the watermark is a no-op.
func SetLendingInterestWatermark(ctx context.Context, addr string, from, thru uint32, pools []LendingContract) error {
	if len(pools) == 0 {
		return errors.New("clickhouse: lending interest watermark: no pools")
	}
	return advanceEntryWatermark(ctx, addr, lendingInterestWatermarkName, from, thru, func(ctx context.Context, conn driver.Conn) error {
		if err := lendingInterestWindowProof(ctx, conn, from, thru); err != nil {
			return err
		}
		wm, err := entryWatermarkOn(ctx, conn, lendingInterestWatermarkName)
		if err != nil {
			return err
		}
		gaps, err := lendingInterestPoolGaps(ctx, conn, pools, wm)
		if err != nil {
			return err
		}
		if len(gaps) > 0 {
			return fmt.Errorf("%w: [%d,%d] but %+v; bring each up with SetLendingInterestPoolWatermark", ErrLendingInterestPoolUncovered, from, thru, gaps)
		}
		// Floor and pool rows first, so the watermark never stands without them.
		if wm == 0 {
			if err := conn.Exec(ctx, `INSERT INTO stellar.entry_history_watermark (name, thru_ledger) VALUES (?, ?)`,
				lendingInterestFloorName, from); err != nil {
				return fmt.Errorf("clickhouse: record lending interest floor %d: %w", from, err)
			}
		}
		return setLendingInterestPoolRows(ctx, conn, pools, thru)
	})
}

func setLendingInterestPoolRows(ctx context.Context, conn driver.Conn, pools []LendingContract, thru uint32) error {
	batch, err := conn.PrepareBatch(ctx, `INSERT INTO stellar.entry_history_watermark (name, thru_ledger)`)
	if err != nil {
		return fmt.Errorf("clickhouse: prepare lending interest pool rows: %w", err)
	}
	for _, c := range pools {
		if err := batch.Append(lendingInterestPoolPrefix+c.ID, thru); err != nil {
			return fmt.Errorf("clickhouse: append lending interest pool row %s: %w", c.ID, err)
		}
	}
	return wrapSend(batch.Send(), "entry_history_watermark")
}

// lendingInterestWindowProof is what any interest advance over [from, thru] proves: lending_state covers it, it runs
// from an hour's first ledger (or the floor) to an hour's last, and the lake holds every tx-bearing ledger's changes.
func lendingInterestWindowProof(ctx context.Context, conn driver.Conn, from, thru uint32) error {
	if err := lendingStateThrough(ctx, conn, thru); err != nil {
		return err
	}
	if err := lendingInterestHourEnd(ctx, conn, thru); err != nil {
		return err
	}
	if err := lendingInterestHourStart(ctx, conn, from); err != nil {
		return err
	}
	return lendingInterestLakeCovered(ctx, conn, from, thru)
}

// LendingInterestPoolGap is a pool whose interest rows stop at Thru (0 = never written) below the watermark.
type LendingInterestPoolGap struct {
	Pool           string
	Deployed, Thru uint32
}

// LendingInterestUncoveredPools lists the pools the watermark does not cover: added after the watermark passed their
// deploy ledger (or with it unknown), or left out of an advance. SetLendingInterestWatermark refuses while it lists
// any; SetLendingInterestPoolWatermark brings each one up to the watermark.
func LendingInterestUncoveredPools(ctx context.Context, addr string, pools []LendingContract) ([]LendingInterestPoolGap, error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	wm, err := entryWatermarkOn(ctx, conn, lendingInterestWatermarkName)
	if err != nil {
		return nil, err
	}
	return lendingInterestPoolGaps(ctx, conn, pools, wm)
}

func lendingInterestPoolGaps(ctx context.Context, conn driver.Conn, pools []LendingContract, wm uint32) ([]LendingInterestPoolGap, error) {
	names := make([]any, 0, len(pools))
	for _, c := range pools {
		if _, err := lendingInterestProtocol(c); err != nil {
			return nil, err
		}
		names = append(names, lendingInterestPoolPrefix+c.ID)
	}
	rows, err := conn.Query(ctx, `SELECT name, max(thru_ledger) FROM stellar.entry_history_watermark WHERE name IN ? GROUP BY name`,
		clickhouse.GroupSet{Value: names})
	if err != nil {
		return nil, fmt.Errorf("clickhouse: lending interest pool coverage: %w", err)
	}
	defer func() { _ = rows.Close() }()
	thru := map[string]uint32{}
	for rows.Next() {
		var name string
		var n uint32
		if err := rows.Scan(&name, &n); err != nil {
			return nil, fmt.Errorf("clickhouse: lending interest pool coverage: %w", err)
		}
		thru[strings.TrimPrefix(name, lendingInterestPoolPrefix)] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clickhouse: lending interest pool coverage: %w", err)
	}
	var gaps []LendingInterestPoolGap
	for _, c := range pools {
		pw := thru[c.ID]
		if pw >= wm || (pw == 0 && c.Deployed > wm) {
			continue
		}
		gaps = append(gaps, LendingInterestPoolGap{Pool: c.ID, Deployed: c.Deployed, Thru: pw})
	}
	return gaps, nil
}

// SetLendingInterestPoolWatermark records one pool's interest rows complete through thru, for a bounded backfill of a
// pool LendingInterestUncoveredPools lists: never past the shared watermark, and a first window must start at or below
// the later of the pool's deploy ledger and the floor. Otherwise as SetLendingInterestWatermark.
func SetLendingInterestPoolWatermark(ctx context.Context, addr string, pool LendingContract, from, thru uint32) error {
	if _, err := lendingInterestProtocol(pool); err != nil {
		return err
	}
	name := lendingInterestPoolPrefix + pool.ID
	return advanceEntryRow(ctx, addr, name, entryWatermarkRow, from, thru, func(ctx context.Context, conn driver.Conn) error {
		wm, err := entryWatermarkOn(ctx, conn, lendingInterestWatermarkName)
		if err != nil {
			return err
		}
		if thru > wm {
			return fmt.Errorf("clickhouse: lending interest pool %s [%d,%d] runs past the watermark %d; the shared pass covers it",
				pool.ID, from, thru, wm)
		}
		pw, err := entryWatermarkRow(ctx, conn, name)
		if err != nil {
			return err
		}
		if pw == 0 {
			floor, err := entryWatermarkRow(ctx, conn, lendingInterestFloorName)
			if err != nil {
				return err
			}
			if need := max(floor, pool.Deployed); from > need {
				return fmt.Errorf("%w: pool %s's first window starts at %d, past its deploy ledger %d and the floor %d",
					ErrLendingInterestPoolUncovered, pool.ID, from, pool.Deployed, floor)
			}
		}
		return lendingInterestWindowProof(ctx, conn, from, thru)
	})
}

// lendingInterestHourEnd proves thru is its hour's last ledger: ledger thru+1 is stored and closes in a later hour.
func lendingInterestHourEnd(ctx context.Context, conn driver.Conn, thru uint32) error {
	h, ok, err := ledgerHour(ctx, conn, thru)
	if err != nil {
		return err
	}
	next, nextOK, err := ledgerHour(ctx, conn, thru+1)
	if err != nil {
		return err
	}
	if !ok || !nextOK {
		return fmt.Errorf("%w: ledger %d or %d is not in stellar.ledgers", ErrLendingInterestNotHourEnd, thru, thru+1)
	}
	if !next.After(h) {
		return fmt.Errorf("%w: ledgers %d and %d both close in %s", ErrLendingInterestNotHourEnd, thru, thru+1, h.Format(time.RFC3339))
	}
	return nil
}

// lendingInterestHourStart proves from is the recorded floor or its hour's first ledger: ledger from-1 is stored and
// closes in an earlier hour.
func lendingInterestHourStart(ctx context.Context, conn driver.Conn, from uint32) error {
	floor, err := entryWatermarkRow(ctx, conn, lendingInterestFloorName)
	if err != nil {
		return err
	}
	if floor > 0 && from == floor {
		return nil
	}
	prev, prevOK, err := ledgerHour(ctx, conn, from-1)
	if err != nil {
		return err
	}
	h, ok, err := ledgerHour(ctx, conn, from)
	if err != nil {
		return err
	}
	if !ok || !prevOK {
		return fmt.Errorf("%w: ledger %d or %d is not in stellar.ledgers", ErrLendingInterestNotHourStart, from-1, from)
	}
	if !h.After(prev) {
		return fmt.Errorf("%w: ledgers %d and %d both close in %s", ErrLendingInterestNotHourStart, from-1, from, h.Format(time.RFC3339))
	}
	return nil
}
