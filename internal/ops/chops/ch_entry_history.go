package chops

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/big"
	"os"
	"reflect"
	"slices"
	"sort"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
)

// ch-entry-history derives the account- and asset-keyed entry-change history
// (stellar.account_entry_changes + stellar.asset_entry_changes,
// deploy/clickhouse/entry_history.sql) from ONE decode pass of
// stellar.ledger_entry_changes, windowed and resumable via
// stellar.entry_history_watermark. Without -write it writes nothing and
// reports the rows and field bytes each entry type would add — the sizing
// measurement to run before the from-genesis backfill.
func chEntryHistory(args []string) error {
	fs := flag.NewFlagSet("ch-entry-history", flag.ContinueOnError)
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	from := fs.Uint("from", 0, "first ledger (0 = resume from the watermark, or -floor-ledger on first run)")
	to := fs.Uint("to", 0, "last ledger (inclusive; 0 = current contiguous lake tip). Always clamped down to the contiguous tip.")
	window := fs.Uint("window", 10_000, "ledgers per derive window")
	floorLedger := fs.Uint("floor-ledger", 1, "first-run start when no watermark exists; clamped up to the lake's first ledger")
	maxDecodeErrs := registerDecodeBudget(fs)
	gate := opsutil.RegisterWriteGate(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *window == 0 {
		return fmt.Errorf("-window must be > 0")
	}
	if *floorLedger == 0 {
		return fmt.Errorf("-floor-ledger must be > 0 (the genesis ledger is 1)")
	}
	gate.Banner()
	dryRun := gate.DryRun()

	ctx, cancel := opsutil.SignalContext()
	defer cancel()

	start, last, err := resolveDeriveRange(ctx, *chAddr, "ch-entry-history", uint32(*from), uint32(*to), uint32(*floorLedger), clickhouse.EntryHistoryWatermark) //nolint:gosec // ledger sequences fit uint32
	if err != nil {
		return err
	}
	if last < start {
		fmt.Fprintf(os.Stderr, "ch-entry-history: nothing to derive (start %d, contiguous tip %d)\n", start, last)
		return nil
	}
	st := newEntryHistoryStats()
	if err := runEntryHistory(ctx, *chAddr, start, last, uint32(*window), dryRun, st); err != nil { //nolint:gosec // window fits uint32
		return err
	}
	st.report(start, last)
	return enforceDecodeBudget("ch-entry-history", st.skipped, *maxDecodeErrs)
}

// runEntryHistory derives [start, last] window by window, advancing the
// watermark after each written window.
func runEntryHistory(ctx context.Context, chAddr string, start, last, window uint32, dryRun bool, st *entryHistoryStats) error {
	runStart := time.Now()
	for lo := start; ; {
		hi := last
		if last-lo >= window {
			hi = lo + window - 1
		}
		n, txLedgers, err := deriveEntryHistoryWindow(ctx, chAddr, lo, hi, dryRun, st)
		if err != nil {
			return fmt.Errorf("window [%d,%d]: %w — resume with -from %d (or no -from: the watermark holds)", lo, hi, err, lo)
		}
		if !dryRun {
			if err := setEntryHistoryWatermark(ctx, chAddr, lo, hi, txLedgers); err != nil {
				return fmt.Errorf("advance watermark to %d: %w", hi, err)
			}
		}
		fmt.Fprintf(os.Stderr, "ch-entry-history: window [%d,%d] done — %d rows (elapsed %s)\n",
			lo, hi, n, time.Since(runStart).Round(time.Second))
		if hi >= last {
			return nil
		}
		lo = hi + 1
	}
}

// Seams so a window's derive is testable without a live lake.
var (
	streamEntryHistorySource  = clickhouse.StreamEntryHistorySource
	insertEntryHistory        = clickhouse.InsertEntryHistory
	setEntryHistoryWatermark  = clickhouse.SetEntryHistoryWatermark
	entryHistoryFlushRowCount = 50_000
)

// deriveEntryHistoryWindow projects one window and returns the rows written
// (or, on a dry run, the rows that would be) and the distinct ledgers it read
// transaction-scoped changes for — the watermark's coverage proof.
func deriveEntryHistoryWindow(ctx context.Context, addr string, lo, hi uint32, dryRun bool, st *entryHistoryStats) (int64, uint64, error) {
	p := newEntryHistoryProjector()
	var (
		accounts     []clickhouse.AccountEntryChange
		assets       []clickhouse.AssetEntryChange
		written      int64
		skipped      uint64
		txLedgers    uint64
		lastTxLedger uint32
	)
	flush := func() error {
		if !dryRun {
			if _, err := insertEntryHistory(ctx, addr, accounts, assets); err != nil {
				return err
			}
		}
		written += int64(len(accounts) + len(assets))
		accounts, assets = accounts[:0], assets[:0]
		return nil
	}
	err := streamEntryHistorySource(ctx, addr, lo, hi, func(r clickhouse.EntryHistorySourceRow) error {
		// The stream is ledger-ordered, so a ledger's tx-scoped rows are adjacent.
		if r.TxHash != "" && r.Ledger != lastTxLedger {
			txLedgers++
			lastTxLedger = r.Ledger
		}
		acc, ast, err := p.project(r)
		if err != nil {
			skipped++
			if skipped <= 3 {
				fmt.Fprintf(os.Stderr, "ch-entry-history: skip ledger %d tx %s op %d change %d: %v\n", r.Ledger, r.TxHash, r.OpIndex, r.ChangeIndex, err)
			}
			return nil
		}
		st.record(acc, ast)
		accounts = append(accounts, acc...)
		assets = append(assets, ast...)
		if len(accounts)+len(assets) >= entryHistoryFlushRowCount {
			return flush()
		}
		return nil
	})
	if err != nil {
		return written, txLedgers, err
	}
	if err := flush(); err != nil {
		return written, txLedgers, err
	}
	if skipped > 0 {
		fmt.Fprintf(os.Stderr, "ch-entry-history: window [%d,%d]: %d changes skipped (decode)\n", lo, hi, skipped)
	}
	st.skipped += skipped
	return written, txLedgers, nil
}

// entryHistoryProjector turns source rows into history rows. It must see a
// window's rows in source sort-key order: stellar-core emits each update's and
// removal's 'state' pre-image immediately before it in the same
// (tx, op_index) group, so the projector keeps the latest pre-image per key
// for the current group only.
type entryHistoryProjector struct {
	ledger uint32
	tx     string
	op     int32
	pre    map[string]xdr.LedgerEntry
}

func newEntryHistoryProjector() *entryHistoryProjector {
	return &entryHistoryProjector{pre: map[string]xdr.LedgerEntry{}}
}

func (p *entryHistoryProjector) project(r clickhouse.EntryHistorySourceRow) ([]clickhouse.AccountEntryChange, []clickhouse.AssetEntryChange, error) {
	if r.Ledger != p.ledger || r.TxHash != p.tx || r.OpIndex != p.op {
		p.ledger, p.tx, p.op = r.Ledger, r.TxHash, r.OpIndex
		clear(p.pre)
	}
	var post *xdr.LedgerEntry
	if r.EntryXDR != "" {
		var e xdr.LedgerEntry
		if err := xdr.SafeUnmarshalBase64(r.EntryXDR, &e); err != nil {
			return nil, nil, fmt.Errorf("decode entry: %w", err)
		}
		post = &e
	}
	if r.ChangeType == "state" {
		// A pre-image is never itself a change. It stays in the map (not
		// consumed) so an unmerged duplicate of the change sees it too.
		if post == nil {
			return nil, nil, fmt.Errorf("state row without an entry")
		}
		p.pre[r.KeyXDR] = *post
		return nil, nil, nil
	}
	if post == nil && r.ChangeType != "removed" {
		return nil, nil, fmt.Errorf("%s row without an entry", r.ChangeType)
	}
	var prev *xdr.LedgerEntry
	if e, ok := p.pre[r.KeyXDR]; ok {
		prev = &e
	}

	postFields, err := entryFields(post)
	if err != nil {
		return nil, nil, err
	}
	prevFields, err := entryFields(prev)
	if err != nil {
		return nil, nil, err
	}
	shown := postFields
	if post == nil {
		shown = prevFields // a removal records the entry's last state
	}
	if shown == nil {
		shown = map[string]any{}
	}
	fieldsJSON, err := json.Marshal(shown)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal fields: %w", err)
	}
	ref := clickhouse.EntryChangeRef{
		Ledger: r.Ledger, CloseTime: r.CloseTime, TxHash: r.TxHash, OpIndex: r.OpIndex,
		ChangeIndex: r.ChangeIndex, IntraLedgerSeq: r.IntraLedgerSeq,
		EntryType: r.EntryType, ChangeType: r.ChangeType,
		Changed: changedFields(r.ChangeType, prevFields, postFields),
		Fields:  string(fieldsJSON),
	}

	var key *xdr.LedgerKey
	if post == nil && prev == nil {
		var k xdr.LedgerKey
		if err := xdr.SafeUnmarshalBase64(r.KeyXDR, &k); err != nil {
			return nil, nil, fmt.Errorf("decode key: %w", err)
		}
		key = &k
	}
	return accountRows(ref, prev, post, key), assetRows(ref, prev, post, key), nil
}

func entryFields(e *xdr.LedgerEntry) (map[string]any, error) {
	if e == nil {
		return nil, nil //nolint:nilnil // no image is not an error
	}
	f, ok := xdrjson.LedgerEntryFields(*e)
	if !ok {
		return nil, fmt.Errorf("unsupported entry type %s", e.Data.Type)
	}
	return f, nil
}

// changedFields names the decoded fields an update or restore altered. With
// no pre-image to compare against, an update reports every field rather than
// none, so a field-filtered history read can never miss it.
func changedFields(changeType string, prev, post map[string]any) []string {
	if changeType != "updated" && changeType != "restored" {
		return nil
	}
	if prev == nil {
		if changeType == "restored" {
			return nil
		}
		return fieldNames(post)
	}
	var out []string
	for k, v := range post {
		if pv, ok := prev[k]; !ok || !reflect.DeepEqual(pv, v) {
			out = append(out, k)
		}
	}
	for k := range prev {
		if _, ok := post[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func fieldNames(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// entryParty is one account an entry concerns, in one role, with the asset and
// amount that account holds through the entry (owner of an account/trustline,
// claimant of a claimable balance), if any.
type entryParty struct {
	account, role, asset string
	balance              int64
}

func entryParties(e *xdr.LedgerEntry) []entryParty {
	var out []entryParty
	switch e.Data.Type {
	case xdr.LedgerEntryTypeAccount:
		a := e.Data.MustAccount()
		out = append(out, entryParty{account: a.AccountId.Address(), role: "owner", asset: "native", balance: int64(a.Balance)})
		for _, s := range a.SignerSponsoringIDs() {
			if s != nil {
				out = append(out, entryParty{account: s.Address(), role: "sponsor"})
			}
		}
	case xdr.LedgerEntryTypeTrustline:
		t := e.Data.MustTrustLine()
		out = append(out, entryParty{account: t.AccountId.Address(), role: "owner", asset: xdrjson.TrustLineAssetID(t.Asset), balance: int64(t.Balance)})
	case xdr.LedgerEntryTypeOffer:
		out = append(out, entryParty{account: e.Data.MustOffer().SellerId.Address(), role: "owner"})
	case xdr.LedgerEntryTypeData:
		out = append(out, entryParty{account: e.Data.MustData().AccountId.Address(), role: "owner"})
	case xdr.LedgerEntryTypeClaimableBalance:
		cb := e.Data.MustClaimableBalance()
		for _, c := range cb.Claimants {
			if v0, ok := c.GetV0(); ok {
				out = append(out, entryParty{account: v0.Destination.Address(), role: "claimant", asset: xdrjson.AssetID(cb.Asset), balance: int64(cb.Amount)})
			}
		}
	}
	if s := e.SponsoringID(); s != nil {
		out = append(out, entryParty{account: s.Address(), role: "sponsor"})
	}
	return out
}

// keyParties is the owner a removal's key names when no pre-image was seen.
func keyParties(k *xdr.LedgerKey) []entryParty {
	switch k.Type {
	case xdr.LedgerEntryTypeAccount:
		return []entryParty{{account: k.MustAccount().AccountId.Address(), role: "owner", asset: "native"}}
	case xdr.LedgerEntryTypeTrustline:
		t := k.MustTrustLine()
		return []entryParty{{account: t.AccountId.Address(), role: "owner", asset: xdrjson.TrustLineAssetID(t.Asset)}}
	case xdr.LedgerEntryTypeOffer:
		return []entryParty{{account: k.MustOffer().SellerId.Address(), role: "owner"}}
	case xdr.LedgerEntryTypeData:
		return []entryParty{{account: k.MustData().AccountId.Address(), role: "owner"}}
	}
	return nil
}

// accountRows fans a change out to every account it concerns before or after:
// a sponsor whose sponsorship the change revoked still sees it. A party only
// the pre-image names holds nothing through the entry any more (balance 0).
func accountRows(ref clickhouse.EntryChangeRef, prev, post *xdr.LedgerEntry, key *xdr.LedgerKey) []clickhouse.AccountEntryChange {
	var parties []entryParty
	if post != nil {
		parties = entryParties(post)
	}
	var gone []entryParty
	switch {
	case prev != nil:
		gone = entryParties(prev)
	case key != nil:
		gone = keyParties(key)
	}
	for _, g := range gone {
		if !slices.ContainsFunc(parties, func(p entryParty) bool { return p.account == g.account && p.role == g.role }) {
			g.balance = 0
			parties = append(parties, g)
		}
	}
	out := make([]clickhouse.AccountEntryChange, 0, len(parties))
	for _, p := range parties {
		if slices.ContainsFunc(out, func(o clickhouse.AccountEntryChange) bool { return o.Account == p.account && o.Role == p.role }) {
			continue // one account sponsoring both the entry and a signer
		}
		out = append(out, clickhouse.AccountEntryChange{
			EntryChangeRef: ref, Account: p.account, Role: p.role, Asset: p.asset, Balance: big.NewInt(p.balance),
		})
	}
	return out
}

// entryAsset is one asset an entry concerns, in one role, with the amount of
// it the entry holds and the account holding, selling or sponsoring it.
type entryAsset struct {
	asset, role, account string
	balance              int64
}

func entryAssets(e *xdr.LedgerEntry) []entryAsset {
	switch e.Data.Type {
	case xdr.LedgerEntryTypeTrustline:
		t := e.Data.MustTrustLine()
		return []entryAsset{{asset: xdrjson.TrustLineAssetID(t.Asset), role: "holder", account: t.AccountId.Address(), balance: int64(t.Balance)}}
	case xdr.LedgerEntryTypeOffer:
		o := e.Data.MustOffer()
		seller := o.SellerId.Address()
		return []entryAsset{
			{asset: xdrjson.AssetID(o.Selling), role: "selling", account: seller, balance: int64(o.Amount)},
			{asset: xdrjson.AssetID(o.Buying), role: "buying", account: seller},
		}
	case xdr.LedgerEntryTypeClaimableBalance:
		cb := e.Data.MustClaimableBalance()
		var sponsor string
		if s := e.SponsoringID(); s != nil {
			sponsor = s.Address()
		}
		return []entryAsset{{asset: xdrjson.AssetID(cb.Asset), role: "claimable", account: sponsor, balance: int64(cb.Amount)}}
	case xdr.LedgerEntryTypeLiquidityPool:
		lp := e.Data.MustLiquidityPool()
		out := []entryAsset{{asset: poolAssetID(lp.LiquidityPoolId), role: "pool"}}
		if cp, ok := lp.Body.GetConstantProduct(); ok {
			out[0].balance = int64(cp.TotalPoolShares)
			out = append(out,
				entryAsset{asset: xdrjson.AssetID(cp.Params.AssetA), role: "reserve_a", balance: int64(cp.ReserveA)},
				entryAsset{asset: xdrjson.AssetID(cp.Params.AssetB), role: "reserve_b", balance: int64(cp.ReserveB)})
		}
		return out
	}
	return nil
}

func keyAssets(k *xdr.LedgerKey) []entryAsset {
	switch k.Type {
	case xdr.LedgerEntryTypeTrustline:
		t := k.MustTrustLine()
		return []entryAsset{{asset: xdrjson.TrustLineAssetID(t.Asset), role: "holder", account: t.AccountId.Address()}}
	case xdr.LedgerEntryTypeLiquidityPool:
		return []entryAsset{{asset: poolAssetID(k.MustLiquidityPool().LiquidityPoolId), role: "pool"}}
	}
	return nil
}

// poolAssetID spells a pool the way a pool-share trustline's asset is spelled.
func poolAssetID(id xdr.PoolId) string {
	return xdrjson.TrustLineAssetID(xdr.TrustLineAsset{Type: xdr.AssetTypeAssetTypePoolShare, LiquidityPoolId: &id})
}

// assetRows fans a change out to every asset it concerns. Account entries are
// deliberately absent: native's per-holder history is every account-entry
// change, already served account-keyed.
func assetRows(ref clickhouse.EntryChangeRef, prev, post *xdr.LedgerEntry, key *xdr.LedgerKey) []clickhouse.AssetEntryChange {
	var assets []entryAsset
	if post != nil {
		assets = entryAssets(post)
	}
	var gone []entryAsset
	switch {
	case prev != nil:
		gone = entryAssets(prev)
	case key != nil:
		gone = keyAssets(key)
	}
	for _, g := range gone {
		if !slices.ContainsFunc(assets, func(a entryAsset) bool { return a.asset == g.asset && a.role == g.role }) {
			g.balance = 0
			assets = append(assets, g)
		}
	}
	out := make([]clickhouse.AssetEntryChange, 0, len(assets))
	for _, a := range assets {
		out = append(out, clickhouse.AssetEntryChange{
			EntryChangeRef: ref, Asset: a.asset, Role: a.role, Account: a.account, Balance: big.NewInt(a.balance),
		})
	}
	return out
}

// feeSeqFields are the account fields a transaction's fee charge, refund and
// sequence bump touch.
var feeSeqFields = map[string]bool{"balance": true, "seq_num": true, "seq_ledger": true, "seq_time": true}

// entryHistoryStats is the sizing measurement: rows per table and entry type,
// decoded-field JSON bytes, and how many account rows are tx-level
// fee/sequence-only updates (the candidate to leave out of the backfill).
type entryHistoryStats struct {
	accountRows, assetRows, fieldBytes map[string]int64
	feeSeqOnly                         int64
	skipped                            uint64
}

func newEntryHistoryStats() *entryHistoryStats {
	return &entryHistoryStats{accountRows: map[string]int64{}, assetRows: map[string]int64{}, fieldBytes: map[string]int64{}}
}

func (s *entryHistoryStats) record(acc []clickhouse.AccountEntryChange, ast []clickhouse.AssetEntryChange) {
	for _, a := range acc {
		s.accountRows[a.EntryType]++
		s.fieldBytes[a.EntryType] += int64(len(a.Fields))
		if a.EntryType == "account" && a.Role == "owner" && a.OpIndex == -1 && isFeeSeqOnly(a.Changed) {
			s.feeSeqOnly++
		}
	}
	for _, a := range ast {
		s.assetRows[a.EntryType]++
		s.fieldBytes[a.EntryType] += int64(len(a.Fields))
	}
}

func isFeeSeqOnly(changed []string) bool {
	if len(changed) == 0 {
		return false
	}
	for _, c := range changed {
		if !feeSeqFields[c] {
			return false
		}
	}
	return true
}

func (s *entryHistoryStats) report(from, to uint32) {
	types := map[string]bool{}
	for t := range s.accountRows {
		types[t] = true
	}
	for t := range s.assetRows {
		types[t] = true
	}
	names := make([]string, 0, len(types))
	for t := range types {
		names = append(names, t)
	}
	sort.Strings(names)
	var totalAcc, totalAst, totalBytes int64
	fmt.Fprintf(os.Stderr, "ch-entry-history: measured [%d,%d] (pre-merge counts; fields_bytes = uncompressed decoded-field JSON)\n", from, to)
	for _, t := range names {
		fmt.Fprintf(os.Stderr, "  entry_type=%-17s account_rows=%d asset_rows=%d fields_bytes=%d\n", t, s.accountRows[t], s.assetRows[t], s.fieldBytes[t])
		totalAcc += s.accountRows[t]
		totalAst += s.assetRows[t]
		totalBytes += s.fieldBytes[t]
	}
	fmt.Fprintf(os.Stderr, "  total account_rows=%d asset_rows=%d fields_bytes=%d\n", totalAcc, totalAst, totalBytes)
	fmt.Fprintf(os.Stderr, "  fee/seq-only account rows (tx-level, only balance/seq_* changed)=%d of %d account-entry rows\n",
		s.feeSeqOnly, s.accountRows["account"])
	if s.skipped > 0 {
		fmt.Fprintf(os.Stderr, "  skipped (undecodable)=%d\n", s.skipped)
	}
}
