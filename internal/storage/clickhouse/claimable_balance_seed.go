package clickhouse

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// ClaimableBalanceSeed is one currently-live ClaimableBalanceEntry rebuilt from the lake's
// append-log (ADR-0034), shaped for seeding claimable_observations.
// Without the seed, balances created before the live observer started and still unclaimed are
// invisible to the classic-supply sum, leaving that component under-populated. It reads
// authoritative on-chain state, so it is always safe to run; the live observer supersedes a seeded
// row on the next real change (a claim writes is_removal=true at a HIGHER ledger, which the served
// reader's DISTINCT ON ... ORDER BY ledger DESC picks).
type ClaimableBalanceSeed struct {
	// ClaimableID is the V0 hash hex, byte-identical to the live observer's claimableIDHex, so
	// seeded and live rows for one balance collide on the natural key instead of double-counting.
	ClaimableID string

	// AssetKey is the supply.AssetKey CODE:ISSUER form. Native balances are never seeded (Algorithm
	// 1; the live observer skips them too).
	AssetKey string

	// Balance is the Amount in stroops, *big.Int per ADR-0003 to match the NUMERIC column and the
	// live observer.
	Balance *big.Int

	// LedgerSeq is the TRUE last-modified ledger of the entry, so a live observation at a higher
	// ledger always wins the served reader's at-or-before pick.
	LedgerSeq uint32

	// CloseTime becomes observed_at, a partition column and part of the primary key: it must be the
	// real close time so a re-seed upserts the same row.
	CloseTime time.Time

	// IsRemoval marks a tombstone: a balance the served tier holds as live whose latest lake change
	// is its claim or clawback. Balance is zero and AssetKey is the served row's, so it supersedes
	// that row.
	IsRemoval bool
}

const (
	// claimableSeedLedgerWindow is one scan step. As for sacSeedLedgerWindow, it divides the
	// PARTITION BY intDiv(ledger_seq, 1000000) evenly and ledger_seq leads the ORDER BY, so a
	// window is a primary-key range within one partition. This scan is lighter per window than the
	// SAC seed (small slice of the log, PREWHERE before the wide entry_xdr), and bisection covers
	// airdrop-era bursts.
	claimableSeedLedgerWindow = 250_000
	// claimableSeedMinLedgerWindow is the bisection floor. Key density per ledger is unbounded (an
	// airdrop can mint millions of balances in a few thousand ledgers), so the floor must be low
	// enough for the densest range on the chain.
	claimableSeedMinLedgerWindow = 256
	// claimableSeedWidenAfter re-widens (doubling, capped at the initial width) after this many
	// clean windows, so one dense stretch does not pin the walk at the floor. The memory ceiling is
	// never raised.
	claimableSeedWidenAfter = 4
)

// StreamClaimableBalanceSeeds reduces the append-log to the LATEST change per claimable balance and
// calls fn for each still-live one in scope.
// `assets` scopes to supply.AssetKey strings; nil or empty means EVERY classic credit asset, since
// a partial default would under-report the rest.
// Reads ledger_entry_changes, NOT ledger_entries_current: the current-state projection only holds
// rows inserted after its view was created, so it misses balances created earlier and unclaimed
// since (see StreamSACBalanceSeedsFullHistory).
// Walks the whole chain: run under run-heavy-job.sh on r1; no output until the end, because the
// reduction can only emit after the last window. Silence is not a hang.
// Under walk.VerifyLake the range is proven intact first (a hole hiding a claim would resurrect the
// claimed balance); the SeedEvidence records it.
// `served` maps hex claimable id to asset_key for balances the served tier holds live; each whose
// latest lake change is a removal is emitted as a tombstone. Nil retracts nothing.
func StreamClaimableBalanceSeeds(ctx context.Context, addr string, assets map[string]struct{}, served map[string]string, walk SeedWalk, fn func(ClaimableBalanceSeed) error) (SeedEvidence, error) {
	red := newClaimableSeedReducer(assets)
	if err := red.retractServed(served); err != nil {
		return SeedEvidence{}, err
	}
	conn, err := openRead(ctx, addr)
	if err != nil {
		return SeedEvidence{}, err
	}
	defer func() { _ = conn.Close() }()

	ev, err := resolveSeedWalk(ctx, conn, addr, walk)
	if err != nil {
		return SeedEvidence{}, err
	}

	win := newAdaptiveLedgerWindow(claimableSeedLedgerWindow, claimableSeedMinLedgerWindow, claimableSeedWidenAfter)
	err = walkLedgerWindows(ev.FromLedger, ev.ToLedger, win, walk.progressScan(func(from, to uint32) error {
		if err := red.startWindow(from); err != nil {
			return err
		}
		return scanClaimableSeedWindow(ctx, conn, from, to, red)
	}))
	if err != nil {
		return SeedEvidence{}, err
	}
	return ev, red.emit(fn)
}

// scanClaimableSeedWindow reduces one ledger window server-side to at most one row per storage key.
// A SINGLE argMax over a TUPLE of every projected column, keyed on the full within-ledger identity
// (ledger_seq, intra_ledger_seq, tx_hash, op_index, change_index): per-column argMax could resolve
// a same-ledger tie differently per column and stitch entry_xdr from a live change to change_type
// from a later 'removed', resurrecting a claimed balance (create-and-claim in one ledger is
// common).
// PREWHERE on entry_type (not in the ORDER BY, so it cannot prune) avoids materialising the wide
// key_xdr/entry_xdr for other rows; the ledger_seq range stays in WHERE as a primary-key range.
// Aliases must not shadow source columns (ILLEGAL_AGGREGATION), hence win_ and the outer
// tupleElement unpack.
// SETTINGS: a memory ceiling well below an unbounded query so an oversized window bisects, and
// group-by spill OFF: the threshold compares against the whole query's tracker and made the
// aggregator flush near-empty tables on every block.
func scanClaimableSeedWindow(ctx context.Context, conn driver.Conn, from, to uint32, red *claimableSeedReducer) error {
	const q = `SELECT key_xdr,
		       tupleElement(win, 1) AS win_ledger_seq,
		       tupleElement(win, 2) AS win_intra_ledger_seq,
		       tupleElement(win, 3) AS win_tx_hash,
		       tupleElement(win, 4) AS win_op_index,
		       tupleElement(win, 5) AS win_change_index,
		       tupleElement(win, 6) AS win_entry_xdr,
		       tupleElement(win, 7) AS win_change_type,
		       tupleElement(win, 8) AS win_close_time
		FROM (
		    SELECT key_xdr,
		           argMax((ledger_seq, intra_ledger_seq, tx_hash, op_index, change_index,
		                   entry_xdr, toString(change_type), close_time),
		                  (ledger_seq, intra_ledger_seq, tx_hash, op_index, change_index)) AS win
		    FROM stellar.ledger_entry_changes
		    PREWHERE entry_type = 'claimable_balance'
		    WHERE ledger_seq BETWEEN ? AND ?
		    GROUP BY key_xdr
		)
		SETTINGS max_memory_usage = 4000000000,
		         max_bytes_before_external_group_by = 0,
		         max_threads = 4`
	rows, err := conn.Query(ctx, q, from, to)
	if err != nil {
		return fmt.Errorf("clickhouse: scan claimable_balance changes [%d, %d]: %w", from, to, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			keyXDR, txHash, entryXDR, changeType string
			ord                                  lakeEntryChangeOrder
			closeTime                            time.Time
		)
		if err := rows.Scan(&keyXDR, &ord.ledgerSeq, &ord.intraLedgerSeq, &txHash,
			&ord.opIndex, &ord.changeIndex, &entryXDR, &changeType, &closeTime); err != nil {
			return fmt.Errorf("clickhouse: scan claimable_balance row [%d, %d]: %w", from, to, err)
		}
		ord.txHash = txHash
		if err := red.offer(keyXDR, entryXDR, changeType, closeTime, ord); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("clickhouse: stream claimable_balance changes [%d, %d]: %w", from, to, err)
	}
	return nil
}

// claimableSeedWinner is the latest change seen for one live balance, decoded eagerly to the fields
// the seed persists: with no watched set, retaining KB-scale base64 per key would OOM, and the
// decoded form is ~30x smaller.
// decodeErr defers corrupt XDR to emit: corrupt XDR on a superseded change is irrelevant, on the
// surviving change it is lake corruption, and dropping it would read as "holds nothing".
type claimableSeedWinner struct {
	order     lakeEntryChangeOrder
	assetKey  string // interned; see claimableSeedReducer.intern
	balance   int64  // stroops (xdr.Int64 on the wire; widened at emit per ADR-0003)
	closeTime time.Time
	decodeErr error
}

// claimableSeedReducer finishes in Go the latest-write-wins reduction the per-window queries only
// complete within a window.
// Memory is bounded two ways, since a map of every balance ever created would grow with chain
// history: `live` holds only balances whose latest change is a live in-scope classic-credit entry
// (a removal deletes it), so it tracks the live set; `dead` holds ordering tombstones so a removal
// seen in window N still suppresses a live row re-offered from N-1, and startWindow compacts
// tombstones no future offer could lose to.
type claimableSeedReducer struct {
	// assets is the CODE:ISSUER scope; nil/empty = every classic credit asset.
	assets map[string]struct{}
	// intern collapses asset_key strings to one copy per asset; a few assets own most balances.
	intern map[string]string

	live map[[32]byte]claimableSeedWinner
	dead map[[32]byte]lakeEntryChangeOrder

	// served is the served tier's live set; retired holds a latest-seen removal for those ids only.
	// Never compacted: emit turns it into tombstones, bounded by the served set.
	served  map[[32]byte]string
	retired map[[32]byte]claimableSeedRemoval

	windowStart    uint32
	haveWindowSeen bool
}

func newClaimableSeedReducer(assets map[string]struct{}) *claimableSeedReducer {
	return &claimableSeedReducer{
		assets:  assets,
		intern:  make(map[string]string),
		live:    make(map[[32]byte]claimableSeedWinner),
		dead:    make(map[[32]byte]lakeEntryChangeOrder),
		retired: make(map[[32]byte]claimableSeedRemoval),
	}
}

// claimableSeedRemoval is where a served-live balance left the ledger.
type claimableSeedRemoval struct {
	order     lakeEntryChangeOrder
	closeTime time.Time
}

// retractServed arms tombstones for the served live set (hex id to asset_key).
func (r *claimableSeedReducer) retractServed(served map[string]string) error {
	r.served = make(map[[32]byte]string, len(served))
	for hexID, assetKey := range served {
		raw, err := hex.DecodeString(hexID)
		if err != nil || len(raw) != 32 {
			return fmt.Errorf("clickhouse: claimable seed: served claimable_id %q is not a 32-byte hex id", hexID)
		}
		r.served[[32]byte(raw)] = assetKey
	}
	return nil
}

// startWindow declares that rows offered until the next call are from ledgers >= from, and compacts
// tombstones. Safe: lakeEntryChangeOrder compares ledger_seq first, so a tombstone below `from` can
// never win again.
// A bisection retry re-declares the same start, so the threshold never moves backwards; the check
// below errors on out-of-order walks (e.g. parallel) rather than resurrect claimed balances.
func (r *claimableSeedReducer) startWindow(from uint32) error {
	if r.haveWindowSeen && from < r.windowStart {
		return fmt.Errorf("clickhouse: claimable seed: windows must be walked in non-decreasing ledger order (got start %d after %d)", from, r.windowStart)
	}
	r.windowStart, r.haveWindowSeen = from, true
	for id, ord := range r.dead {
		if ord.ledgerSeq < from {
			delete(r.dead, id)
		}
	}
	return nil
}

// offer folds one window-winning row into the per-key reduction as a maximum under
// lakeEntryChangeOrder.after, so it is idempotent and order-independent (a bisected retry re-reads
// rows already delivered).
// A removal must be able to win (a claim in window N suppresses the creation in N-1), so removals
// are tombstones dropped at emit, never short-circuited.
func (r *claimableSeedReducer) offer(keyXDR, entryXDR, changeType string, closeTime time.Time, ord lakeEntryChangeOrder) error {
	id, ok, err := claimableIDFromKeyXDR(keyXDR)
	if err != nil {
		// An undecodable key on a removed change identifies no balance; only a live entry's corrupt
		// key is an error.
		if changeType == "removed" {
			return nil
		}
		return err
	}
	if !ok {
		return nil // defensive: PREWHERE already scopes to claimable_balance
	}
	if prev, seen := r.live[id]; seen && !ord.after(prev.order) {
		return nil
	}
	if prev, seen := r.dead[id]; seen && !ord.after(prev) {
		return nil
	}
	if prev, seen := r.retired[id]; seen && !ord.after(prev.order) {
		return nil
	}
	if changeType == "removed" {
		delete(r.live, id)
		r.dead[id] = ord
		if _, isServed := r.served[id]; isServed {
			r.retired[id] = claimableSeedRemoval{order: ord, closeTime: closeTime.UTC()}
		}
		return nil
	}
	delete(r.retired, id)

	win, inScope, err := r.decodeLiveEntry(entryXDR)
	if err == nil && !inScope {
		// Native XLM (Algorithm 1) or outside -assets: the latest state is not ours to seed, so
		// drop any earlier record; the winner stays authoritative.
		delete(r.live, id)
		return nil
	}
	win.order, win.closeTime, win.decodeErr = ord, closeTime.UTC(), err
	delete(r.dead, id)
	r.live[id] = win
	return nil
}

// decodeLiveEntry decodes entry_xdr to the persisted fields; inScope=false with nil error is the
// deliberate skip (native, or outside -assets).
func (r *claimableSeedReducer) decodeLiveEntry(entryXDR string) (claimableSeedWinner, bool, error) {
	if entryXDR == "" {
		// A live change always carries its entry; empty is a lake inconsistency, and "holds
		// nothing" would be an under-count.
		return claimableSeedWinner{}, false, errors.New("clickhouse: claimable seed: live change carries no entry_xdr")
	}
	var le xdr.LedgerEntry
	if err := xdr.SafeUnmarshalBase64(entryXDR, &le); err != nil {
		return claimableSeedWinner{}, false, fmt.Errorf("clickhouse: decode claimable_balance entry_xdr: %w", err)
	}
	if le.Data.Type != xdr.LedgerEntryTypeClaimableBalance || le.Data.ClaimableBalance == nil {
		return claimableSeedWinner{}, false, fmt.Errorf("clickhouse: entry_xdr is %s, not ClaimableBalance", le.Data.Type.String())
	}
	cb := le.Data.ClaimableBalance
	assetKey, ok := claimableSeedAssetKey(cb.Asset)
	if !ok {
		return claimableSeedWinner{}, false, nil // native / unrepresentable
	}
	if len(r.assets) > 0 {
		if _, want := r.assets[assetKey]; !want {
			return claimableSeedWinner{}, false, nil
		}
	}
	return claimableSeedWinner{assetKey: r.internAssetKey(assetKey), balance: int64(cb.Amount)}, true, nil
}

func (r *claimableSeedReducer) internAssetKey(k string) string {
	if got, ok := r.intern[k]; ok {
		return got
	}
	r.intern[k] = k
	return k
}

// emit hands every surviving live balance, and the tombstones for served-live balances whose latest
// change is a removal (disjoint ids), to fn in ascending id order so output is reproducible;
// raw-byte order equals lowercase-hex order.
func (r *claimableSeedReducer) emit(fn func(ClaimableBalanceSeed) error) error {
	ids := make([][32]byte, 0, len(r.live)+len(r.retired))
	for id := range r.live {
		ids = append(ids, id)
	}
	for id := range r.retired {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return bytes.Compare(ids[i][:], ids[j][:]) < 0 })

	for _, id := range ids {
		if rm, retired := r.retired[id]; retired {
			if err := fn(ClaimableBalanceSeed{
				ClaimableID: hex.EncodeToString(id[:]),
				AssetKey:    r.served[id],
				Balance:     big.NewInt(0),
				LedgerSeq:   rm.order.ledgerSeq,
				CloseTime:   rm.closeTime,
				IsRemoval:   true,
			}); err != nil {
				return err
			}
			continue
		}
		w := r.live[id]
		if w.decodeErr != nil {
			return fmt.Errorf("clickhouse: claimable seed %s at ledger %d: %w",
				hex.EncodeToString(id[:]), w.order.ledgerSeq, w.decodeErr)
		}
		if err := fn(ClaimableBalanceSeed{
			ClaimableID: hex.EncodeToString(id[:]),
			AssetKey:    w.assetKey,
			Balance:     big.NewInt(w.balance),
			LedgerSeq:   w.order.ledgerSeq,
			CloseTime:   w.closeTime,
		}); err != nil {
			return err
		}
	}
	return nil
}

// claimableIDFromKeyXDR pulls the V0 hash out of a claimable-balance LedgerKey; the raw [32]byte is
// the reducer's map key. ok=false for other keys (defensive; the SQL already scopes).
func claimableIDFromKeyXDR(keyXDR string) ([32]byte, bool, error) {
	var lk xdr.LedgerKey
	if err := xdr.SafeUnmarshalBase64(keyXDR, &lk); err != nil {
		return [32]byte{}, false, fmt.Errorf("clickhouse: decode claimable_balance key_xdr: %w", err)
	}
	if lk.Type != xdr.LedgerEntryTypeClaimableBalance || lk.ClaimableBalance == nil {
		return [32]byte{}, false, nil
	}
	id := lk.ClaimableBalance.BalanceId
	if id.V0 == nil {
		return [32]byte{}, false, fmt.Errorf("clickhouse: ClaimableBalanceId variant %d has no V0 hash", id.Type)
	}
	return [32]byte(*id.V0), true, nil
}

// claimableSeedAssetKey converts the asset to the CODE:ISSUER form the live observer's
// assetKeyFromAsset produces; ok=false for native, a future variant, or a non-Ed25519 issuer.
// Reimplemented, not imported: lint-imports.sh L/storage-below-compute forbids internal/storage
// importing internal/sources. It deliberately avoids canonical.AssetFromXDR, which validates the
// code as ASCII-alphanumeric; the observer does not, so validating here would seed a strict subset
// and miss control-byte codes that occur on pubnet.
func claimableSeedAssetKey(a xdr.Asset) (string, bool) {
	var (
		code   string
		issuer xdr.AccountId
	)
	switch a.Type {
	case xdr.AssetTypeAssetTypeCreditAlphanum4:
		a4 := a.AlphaNum4
		if a4 == nil {
			return "", false
		}
		code, issuer = canonical.TrimTrailingNulls(a4.AssetCode[:]), a4.Issuer
	case xdr.AssetTypeAssetTypeCreditAlphanum12:
		a12 := a.AlphaNum12
		if a12 == nil {
			return "", false
		}
		code, issuer = canonical.TrimTrailingNulls(a12.AssetCode[:]), a12.Issuer
	default:
		return "", false // native XLM + any future variant
	}
	pk, ok := issuer.GetEd25519()
	if !ok {
		return "", false
	}
	strk, err := strkey.Encode(strkey.VersionByteAccountID, pk[:])
	if err != nil {
		return "", false
	}
	return code + ":" + strk, true
}
