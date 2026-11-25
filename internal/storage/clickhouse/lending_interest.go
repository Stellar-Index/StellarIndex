package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"math/big"
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

// lendingInterestResKeysQuery lists the ResData keys lending_state_hourly holds through an hour. Bounded by hour, not
// ledger: a key first changed at or below hi whose hour ends past hi has its row at a later ledger.
const lendingInterestResKeysQuery = `SELECT DISTINCT contract_id, key_xdr FROM stellar.lending_state_hourly FINAL
	WHERE contract_id IN ? AND hour <= ? AND arrayExists(p -> startsWith(key_xdr, p), ?)
	ORDER BY contract_id, key_xdr`

// LendingInterestKeys lists the exact keys the interest pass reads through ledger hi: every ResData key
// lending_state_hourly holds for pools through hi's hour, each pool's instance keys, and BLNT's ProtocolFees key.
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
	var res [][2]string
	for rows.Next() {
		var pool, key string
		if err := rows.Scan(&pool, &key); err != nil {
			return nil, fmt.Errorf("clickhouse: lending interest keys through %d: %w", hi, err)
		}
		res = append(res, [2]string{pool, key})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clickhouse: lending interest keys through %d: %w", hi, err)
	}
	return lendingInterestKeyList(pools, ps, res)
}

// lendingInterestKeyList assembles the key list from the pools' prefixes and the (pool, ResData key) pairs read.
func lendingInterestKeyList(pools []LendingContract, ps []LendingPrefix, res [][2]string) ([]LendingInterestKey, error) {
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
		proto, ok := protos[r[0]]
		if !ok {
			return nil, fmt.Errorf("clickhouse: lending interest: ResData key %s of unrequested pool %s", r[1], r[0])
		}
		asset, err := resDataKeyAsset(r[1], r[0])
		if err != nil {
			return nil, err
		}
		out = append(out, LendingInterestKey{Key: r[1], Pool: r[0], Protocol: proto, Kind: lendinginterest.KindReserve, Asset: asset})
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

// LendingInterestVersions streams every change of keys in [lo, hi], in apply order, decoded, to fn. A change that does
// not decode stops the read with an error naming it: a decoder gap refuses the window, it never reads as zero.
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
	rows, err := conn.Query(ctx, lendingInterestVersionsQuery, lo, hi, clickhouse.GroupSet{Value: list})
	if err != nil {
		return fmt.Errorf("clickhouse: lending interest versions [%d,%d]: %w", lo, hi, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var c lendingInterestChange
		if err := rows.Scan(&c.Key, &c.Ledger, &c.Intra, &c.TxHash, &c.OpIndex, &c.ChangeIndex, &c.ChangeType,
			&c.CloseTime, &c.Entry); err != nil {
			return fmt.Errorf("clickhouse: lending interest versions [%d,%d]: %w", lo, hi, err)
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
		v.Removed = true
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
// no ProtocolFees row gets an explicit empty map, the state a pool that never stored the entry is in.
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
		if r.Entry == "" {
			// The table keeps an hour's last change only: a value written earlier in that hour is lost behind the removal.
			return lendinginterest.Seeds{}, fmt.Errorf("clickhouse: lending interest seed: key %s of pool %s was last removed "+
				"(ledger %d), so its value in force before %s is not in lending_state_hourly", r.Key, k.Pool, r.Ledger, beforeHour)
		}
		var v lendinginterest.Version
		if err := decodeLendingInterestEntry(k, r.Entry, &v); err != nil {
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

const lendingInterestWatermarkName = "lending_interest"

// ErrLendingInterestAheadOfState reports a refused advance past the lending_state watermark: the interest rows are
// seeded from and checked against lending_state_hourly, so they never cover a ledger it does not.
var ErrLendingInterestAheadOfState = errors.New("clickhouse: lending interest window runs ahead of the lending_state watermark")

// ErrLendingInterestNotHourEnd reports a refused advance to a ledger that is not the last of its hour: only complete
// hours are written, so the watermark rests on hour ends.
var ErrLendingInterestNotHourEnd = errors.New("clickhouse: lending interest watermark is not the last ledger of an hour")

// LendingInterestWatermark returns the ledger lending_interest_hourly is complete through (0 = never run).
func LendingInterestWatermark(ctx context.Context, addr string) (uint32, error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()
	return entryWatermarkOn(ctx, conn, lendingInterestWatermarkName)
}

// SetLendingInterestWatermark records the interest rows complete through thru: never past lending_state's watermark,
// and only at the last ledger of an hour. A window at or below the watermark is a no-op.
func SetLendingInterestWatermark(ctx context.Context, addr string, from, thru uint32) error {
	return advanceEntryWatermark(ctx, addr, lendingInterestWatermarkName, from, thru, func(ctx context.Context, conn driver.Conn) error {
		state, err := entryWatermarkOn(ctx, conn, lendingStateWatermarkName)
		if err != nil {
			return err
		}
		if state < thru {
			return fmt.Errorf("%w: [%d,%d] but lending_state stops at %d", ErrLendingInterestAheadOfState, from, thru, state)
		}
		return lendingInterestHourEnd(ctx, conn, thru)
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
