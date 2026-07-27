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

// ClaimableBalanceSeed is one CURRENTLY-LIVE ClaimableBalanceEntry
// reconstructed from the certified lake's append-log
// (stellar.ledger_entry_changes, ADR-0034), shaped for seeding the served
// tier's claimable_observations hypertable (ADR-0022 / migration 0012).
//
// Motivation (verified on r1 2026-07-27). claimable_observations was NEVER
// seeded from history: it holds 997 rows with a floor of ledger 63,301,831 —
// i.e. only what the live LedgerEntryChange observer
// (internal/sources/claimable_balances) has seen since it started. Every
// claimable balance created before that floor and still unclaimed is invisible
// to the Algorithm-2 classic-supply sum
// (supply.ClassicComputer = Trustline + Claimable + LPReserve + SACWrapped),
// so the claimable component reads ~4% populated. Measured against Horizon for
// AQUA: we serve 86,711,792,598 vs Horizon's component sum 99,923,674,166
// (−13.2%); against Horizon's total MINUS its claimable component
// (86,186,028,534) we are +0.61% — the claimable component IS the entire gap.
//
// This is the claimable analogue of the dormant-holder bootstraps that
// `supply seed-observations` closes for account_observations (ADR-0021) and
// `supply seed-sac-balances -full-history` closes for
// sac_balance_observations (incident 2026-07-06). Like those it reads
// AUTHORITATIVE on-chain state — the ClaimableBalanceEntry itself — so it is
// always correct to run; the live observer supersedes a seeded row on the next
// real change (a claim writes is_removal=true at a HIGHER ledger, which the
// served reader's `DISTINCT ON (claimable_id) … ORDER BY ledger DESC` picks).
type ClaimableBalanceSeed struct {
	// ClaimableID is the ClaimableBalanceId V0 hash, hex-encoded — byte for
	// byte the same string the live observer's claimableIDHex produces, so a
	// seeded row and a live-observed row for the same balance collide on
	// claimable_observations' natural key instead of double-counting.
	ClaimableID string

	// AssetKey is the supply.AssetKey CODE:ISSUER form of the classic credit
	// asset the balance pays out. Native (XLM) claimable balances are never
	// seeded — they belong to Algorithm 1, and the live observer skips them
	// too (claimable_balances.ErrUnsupportedClaimableAsset).
	AssetKey string

	// Balance is the entry's Amount in stroops. *big.Int per ADR-0003 — the
	// wire type is xdr.Int64 (classic amounts are int64 on the protocol), so
	// no precision is at risk here, but the served column is NUMERIC and the
	// live observer emits *big.Int, so the seed does too.
	Balance *big.Int

	// LedgerSeq is the ledger of the LATEST change to this entry — its true
	// last-modified ledger, not the seeding run's wall position. Seeding at
	// the true ledger is what keeps the served reader's at-or-before pick
	// correct: a live observation at a HIGHER ledger always wins.
	LedgerSeq uint32

	// CloseTime is that ledger's on-chain close time (UTC). It becomes
	// observed_at, which is claimable_observations' hypertable partition
	// column AND part of its primary key — so it must be the REAL close
	// time, both for point-in-time correctness and so a re-seed upserts the
	// same row instead of appending a second one.
	CloseTime time.Time
}

const (
	// claimableSeedLedgerWindow is the ledger span of one seed scan step.
	// Same value, same reasoning as [sacSeedLedgerWindow]: it divides
	// ledger_entry_changes' PARTITION BY intDiv(ledger_seq, 1000000) evenly
	// so a window never straddles a partition, and ledger_seq LEADS the
	// table's ORDER BY so a window is a primary-key range — the windows
	// partition the ledger range and their reads sum to roughly one pass.
	//
	// The SAC seed's measurements (r1, 2026-07-27) are the calibration:
	// 250,000 peaked at 1.48–1.75 GiB with zero spills through the densest
	// Soroban stretches, against a 1,000,000-ledger window that died above
	// 3.73 GiB. This scan is strictly LIGHTER per window than that one —
	// claimable_balance changes are a small slice of the append-log where
	// contract_data is the bulk, the PREWHERE drops non-matching rows before
	// the wide entry_xdr column is read at all, and a ClaimableBalanceEntry
	// is far smaller than a Soroban contract-data entry. The bisection below
	// still covers the airdrop-era bursts, where one window can touch
	// millions of distinct balances.
	claimableSeedLedgerWindow = 250_000
	// claimableSeedMinLedgerWindow is the bisection floor (250k >> 4). Below
	// this the window holds a few thousand keys; if THAT doesn't fit, the
	// window size is not the problem and the error should surface.
	claimableSeedMinLedgerWindow = claimableSeedLedgerWindow >> 4
)

// StreamClaimableBalanceSeeds scans the certified append-log for every
// ClaimableBalanceEntry, reduces to the LATEST change per balance, and invokes
// fn once per balance that is still LIVE (its latest change is not a removal)
// and pays a classic credit asset in scope.
//
// `assets` scopes the seed to a set of supply.AssetKey CODE:ISSUER strings.
// NIL OR EMPTY MEANS EVERY CLASSIC CREDIT ASSET, which is the correct default:
// claimable balances have no operator-curated watched set the way SAC wrappers
// do, and a seed that quietly covered only some assets would leave the rest
// under-reported in exactly the way this function exists to fix.
//
// Read from ledger_entry_changes, NOT stellar.ledger_entries_current. The
// current-state projection is fed by a materialized view that only processes
// rows inserted after it was created (~ledger 62,000,000 on r1), so a
// claimable balance created before that floor and unclaimed since — precisely
// the population this seed is for — is invisible to it. See
// [StreamSACBalanceSeedsFullHistory] for the long-form derivation of that
// floor; the raw substrate is complete, only the projection of it is not.
//
// Cost. This walks the whole chain over a 150-billion-row table and MUST run
// under run-heavy-job.sh on r1 (CLAUDE.md heavy-job doctrine). Expect several
// hours and NO output until the end: the reduction can only emit once the last
// window has been folded, so every insert lands after the scan rather than
// interleaved with it. Silence is not a hang.
func StreamClaimableBalanceSeeds(ctx context.Context, addr string, assets map[string]struct{}, fn func(ClaimableBalanceSeed) error) error {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	minLedger, maxLedger, err := entryChangeLedgerBounds(ctx, conn)
	if err != nil {
		return err
	}

	red := newClaimableSeedReducer(assets)
	window := uint32(claimableSeedLedgerWindow)
	for start := minLedger; ; {
		end := start + window - 1
		if end < start || end > maxLedger { // uint32 overflow guard + clamp
			end = maxLedger
		}
		if err := red.startWindow(start); err != nil {
			return err
		}
		switch err := scanClaimableSeedWindow(ctx, conn, start, end, red); {
		case err == nil:
		case isMemoryLimitExceeded(err) && window > claimableSeedMinLedgerWindow:
			// Bisect and retry the SAME start — narrowing, never raising the
			// ceiling (chasing the ceiling is what failed three times on the
			// SAC seed). Monotonic: a narrowed window is never widened again.
			window /= 2
			continue
		default:
			return err
		}
		if end >= maxLedger {
			break
		}
		start = end + 1
	}
	return red.emit(fn)
}

// scanClaimableSeedWindow reduces one ledger window server-side to at most one
// row per claimable-balance storage key and offers each to red.
//
// A SINGLE argMax over a TUPLE of every projected column, keyed on the full
// within-ledger identity tuple (ledger_seq, intra_ledger_seq, tx_hash,
// op_index, change_index) — NOT ledger_seq alone, and NOT one argMax per
// column. audit-2026-07-16 C2-4: ledger_seq is not unique per key within a
// ledger, so independent per-column argMax lets ClickHouse resolve the tie
// differently for each column and stitch a row out of two different changes —
// entry_xdr from a still-present change and change_type from a later 'removed'
// one. For claimable balances that specific stitch RESURRECTS A CLAIMED
// BALANCE into the supply seed, which is the over-count direction (a claimable
// balance is created once and claimed once, so the create/claim pair sits in
// the same ledger constantly — a create-and-claim inside one transaction is an
// ordinary pattern). One aggregate over one tuple makes column coherence
// structural instead of a property of tie-impossibility.
//
// PREWHERE on entry_type, not WHERE. entry_type is NOT in this table's ORDER BY
// (ledger_seq, tx_hash, op_index, change_index) so it cannot prune granules —
// but claimable_balance rows are a small slice of an append-log dominated by
// contract_data and account/trustline changes, and PREWHERE guarantees the wide
// key_xdr / entry_xdr columns are only materialised for rows that already
// matched. The ledger_seq range stays in WHERE, where it is a primary-key range
// and prunes parts outright.
//
// Output aliases must NOT shadow the source column names: ClickHouse resolves a
// shadowing alias back into sibling aggregate arguments (ILLEGAL_AGGREGATION —
// caught live 2026-07-11 on the SAC seed), hence the win_ prefixes and the
// tupleElement unpack in an outer SELECT.
//
// SETTINGS mirror the SAC seed's post-incident posture: a per-query ceiling
// well BELOW what an unbounded version would ask for (a healthy bounded window
// needs a fraction of it, and a window that doesn't fit should bisect rather
// than eat the host's memory alongside galexie's captive core), and GROUP-BY
// SPILL OFF — ClickHouse compares max_bytes_before_external_group_by against
// the whole query's memory tracker, so a non-zero threshold made the aggregator
// flush a near-empty hash table on every block (116,753 temporary parts on one
// r1 window) and merging those is what exhausted the budget.
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

// claimableSeedWinner is the latest change seen so far for one LIVE claimable
// balance, already decoded down to the two fields the seed persists.
//
// Decoded eagerly (unlike the SAC reducer, which retains entry_xdr and decodes
// at emit) because this reduction is NOT scoped to an operator-curated watched
// set: it sees every claimable balance on the network, so retaining KB-scale
// base64 per key is the difference between a bounded run and an OOM. The
// decoded form is ~30x smaller and drops native-XLM balances — which never
// belong in claimable_observations — before they cost anything.
//
// decodeErr defers a corrupt-XDR failure to emit time, which preserves the SAC
// seed's error contract exactly: corrupt XDR on a SUPERSEDED change is not the
// seed's problem (a later change overwrites the winner and the error with it);
// corrupt XDR on the SURVIVING change is real lake corruption, and silently
// dropping it would masquerade as "this balance holds nothing" — the exact
// under-count this seed exists to fix.
type claimableSeedWinner struct {
	order     lakeEntryChangeOrder
	assetKey  string // interned; see claimableSeedReducer.intern
	balance   int64  // stroops (xdr.Int64 on the wire; widened at emit per ADR-0003)
	closeTime time.Time
	decodeErr error
}

// claimableSeedReducer finishes, in Go, the latest-write-wins reduction that
// the per-window queries can only complete WITHIN their window.
//
// # Memory
//
// This is the load-bearing difference from [sacSeedReducer]. That one is
// bounded by the watched wrappers' Balance keys — a small set by construction.
// This one has no watched set, so a naive "one map entry per key ever seen"
// would grow with every claimable balance ever CREATED on the network, most of
// which have long since been claimed. Two mechanisms bound it instead:
//
//   - `live` holds only balances whose latest-seen change is a live,
//     in-scope, classic-credit entry. A removal DELETES the entry. So its
//     size tracks the number of claimable balances live at the walk's current
//     position — which is exactly this seed's output cardinality, the same
//     rows it is about to write to Postgres.
//   - `dead` holds a tombstone (the ordering tuple only) per balance whose
//     latest-seen change is a removal, so a removal seen in window N still
//     suppresses a live entry re-offered from window N-1 — the property that
//     keeps [claimableSeedReducer.offer] a pure order-independent maximum
//     rather than a "last offer wins" fold. [claimableSeedReducer.startWindow]
//     then COMPACTS tombstones that no future offer could possibly lose to,
//     so `dead` stays bounded by the removals inside one window instead of by
//     all history. See startWindow for why that is safe.
type claimableSeedReducer struct {
	// assets is the CODE:ISSUER scope. Nil/empty = every classic credit
	// asset (the default — see StreamClaimableBalanceSeeds).
	assets map[string]struct{}
	// intern collapses the per-key asset_key strings onto one copy per
	// distinct asset. A handful of assets own most claimable balances
	// (airdrops), so this is the difference between one string header per
	// live balance and one backing array per live balance.
	intern map[string]string

	live map[[32]byte]claimableSeedWinner
	dead map[[32]byte]lakeEntryChangeOrder

	windowStart    uint32
	haveWindowSeen bool
}

func newClaimableSeedReducer(assets map[string]struct{}) *claimableSeedReducer {
	return &claimableSeedReducer{
		assets: assets,
		intern: make(map[string]string),
		live:   make(map[[32]byte]claimableSeedWinner),
		dead:   make(map[[32]byte]lakeEntryChangeOrder),
	}
}

// startWindow declares that every row offered from now until the next call
// comes from ledgers >= from, and compacts the tombstone set accordingly.
//
// Why the compaction is safe. A tombstone exists only to REJECT a later-offered
// change that is EARLIER in canonical order. After this call every offered row
// has ledger_seq >= from, and lakeEntryChangeOrder compares ledger_seq first —
// so a tombstone at ledger_seq < from can never win a comparison again: any
// row that reaches it would be strictly after it and would displace it anyway.
// Dropping it therefore cannot change any outcome. It is NOT a heuristic.
//
// The bisection retry re-declares the SAME start with a smaller window, so the
// threshold never moves backwards mid-window and a tombstone recorded from the
// failed wider attempt survives into the retry. The monotonicity check below
// makes that a machine-checked precondition rather than a comment: if a caller
// ever walks windows out of order (say, in parallel), this errors instead of
// silently resurrecting claimed balances into the supply seed.
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

// offer folds one window-winning row into the running per-key reduction.
//
// Idempotent and order-independent: it keeps the maximum under
// [lakeEntryChangeOrder.after], so re-offering rows (a bisected retry re-reads a
// window whose stream already delivered part of its output) can never change
// the outcome.
//
// A REMOVAL must be able to win: a claim seen in window N must suppress the
// creation seen in window N-1. Removals are therefore tracked as tombstones and
// dropped at emit time, never short-circuited on sight.
func (r *claimableSeedReducer) offer(keyXDR, entryXDR, changeType string, closeTime time.Time, ord lakeEntryChangeOrder) error {
	id, ok, err := claimableIDFromKeyXDR(keyXDR)
	if err != nil {
		// An undecodable key on a REMOVED change identifies no balance and
		// held nothing; only a live entry's corrupt key is a hard error.
		// Mirrors sacSeedReducer.offer exactly.
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
	if changeType == "removed" {
		delete(r.live, id)
		r.dead[id] = ord
		return nil
	}

	win, inScope, err := r.decodeLiveEntry(entryXDR)
	if err == nil && !inScope {
		// Native XLM (Algorithm 1, not this table) or outside -assets. The
		// latest state of this balance is not ours to seed, so any earlier
		// record of it must go too — a claimable balance's asset is immutable,
		// so in practice there is nothing to delete, but making the winner
		// authoritative here means no code path can leave a stale row behind.
		delete(r.live, id)
		return nil
	}
	win.order, win.closeTime, win.decodeErr = ord, closeTime.UTC(), err
	delete(r.dead, id)
	r.live[id] = win
	return nil
}

// decodeLiveEntry decodes a body-carrying change's entry_xdr down to the two
// persisted fields. inScope=false (with a nil error) is the deliberate skip:
// a native-XLM claimable balance, or a classic one outside the -assets scope.
func (r *claimableSeedReducer) decodeLiveEntry(entryXDR string) (claimableSeedWinner, bool, error) {
	if entryXDR == "" {
		// A created/updated/state row always carries its entry; an empty one
		// is a lake inconsistency, and treating it as "holds nothing" is the
		// under-count this seed fixes.
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

// emit hands every surviving live balance to fn, in ascending claimable-id
// order so a run's output (and any log tail of it) is reproducible even though
// the reduction happens in a Go map. Sorting the raw 32-byte ids is the same
// order as sorting their lowercase hex, which is what the ClaimableID strings
// carry.
func (r *claimableSeedReducer) emit(fn func(ClaimableBalanceSeed) error) error {
	ids := make([][32]byte, 0, len(r.live))
	for id := range r.live {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return bytes.Compare(ids[i][:], ids[j][:]) < 0 })

	for _, id := range ids {
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

// claimableIDFromKeyXDR pulls the ClaimableBalanceId V0 hash out of a
// claimable-balance LedgerKey. The raw [32]byte is the reducer's map key (32
// inline bytes, no allocation, no pointer) and hex-encodes at emit into the
// exact string the live observer's claimableIDHex writes.
//
// ok=false (no error) for a key that is not a claimable balance — defensive
// only, since the SQL already scopes to entry_type='claimable_balance'.
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

// claimableSeedAssetKey converts a claimable balance's asset to the
// supply.AssetKey CODE:ISSUER form the live observer's assetKeyFromAsset
// produces (internal/sources/claimable_balances/decode.go). ok=false for
// native XLM (Algorithm 1 does not read claimable_observations), for a future
// asset variant, and for an issuer that is not Ed25519 — the same three cases
// the observer declines to attribute.
//
// It is a deliberate REIMPLEMENTATION rather than a call into the observer
// package: scripts/ci/lint-imports.sh's L/storage-below-compute rule forbids
// internal/storage from importing internal/sources, and the baseline is
// shrink-only. The shared, drift-prone half — trimming an asset code's
// trailing null padding — comes from canonical.TrimTrailingNulls, the same
// leaf helper canonical.AssetFromXDR uses.
//
// NOTE it deliberately does NOT go through canonical.AssetFromXDR, which
// additionally VALIDATES the code (ASCII-alphanumeric, per C2-010). The live
// claimable observer applies no such rule, so routing through it here would
// seed a strict SUBSET of what the observer records and re-open a silent gap
// for the control-byte / non-ASCII asset codes that do occur on pubnet. The
// seed's job is to be indistinguishable from the observer, including where the
// observer is permissive.
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
