//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// Integration coverage for `stellarindex-ops supply seed-claimable-balances`'s
// lake reader. The unit tests in internal/storage/clickhouse cover the Go-side
// reduction exhaustively; what only a real server can prove is the SQL — the
// PREWHERE on entry_type, the single argMax over a TUPLE of every projected
// column, and the tuple's within-ledger ordering — which is exactly where the
// SAC seed's audit-2026-07-16 C2-4 bug lived.
//
// Every test scopes its assertions to its OWN claimable ids, because the
// reader is deliberately network-wide (no watched set) and the shared test
// schema carries other suites' entry-change rows.
//
// What ids CANNOT scope is the walk itself: the reader steps the whole lake,
// min(ledger_seq) to max(ledger_seq), in 250k-ledger windows, so its cost is
// set by the highest ledger ANY test in the process left behind. One fixture
// at ledger 4,000,000,000 made every walk here ~16,000 empty windows (53-58 s
// each unloaded, five walks in this file) and breached the 5-minute deadline
// under machine load (CO-22). The fixtures that seed up there now remove
// their rows; cbsSeedsByID's window check turns any recurrence into an
// immediate, named failure instead of a load-dependent timeout, and
// TestClaimableSeed_WalkStaysBoundedAfterHighLedgerFixtures pins the two
// known offenders in any shard layout and any order.

const (
	cbsIssuer   = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
	cbsAssetKey = "AQUA:" + cbsIssuer
	// Well below the live claimable observer's floor (ledger 63,301,831,
	// measured on r1 2026-07-27) — the population this seed exists to
	// recover.
	cbsPreFloorLedger = uint32(33_000_000)

	// cbsWalkWindowLedgers is the reader's initial window width
	// (claimableSeedLedgerWindow, internal/storage/clickhouse).
	cbsWalkWindowLedgers = uint64(250_000)
	// cbsMaxWalkWindows is the most windows a seed walk over the SHARED test
	// lake may take: 2,000 windows = a 500M-ledger span, ~8x mainnet's real
	// tip (~60M, ~240 windows) and over twice the suite's highest legitimate
	// fixture (217M, ~870 windows). At the ~3.5 ms an empty window costs,
	// that is ~7 s.
	cbsMaxWalkWindows = uint64(2_000)
	// cbsWalkBudget is the wall-clock ceiling on ONE walk of a bounded lake:
	// ~4x the worst walk cbsMaxWalkWindows admits, so machine load alone
	// cannot breach it, and well under the 53 s one walk took with the
	// 4,000,000,000-ledger fixture in the lake.
	cbsWalkBudget = 30 * time.Second
)

func cbsID(t *testing.T, tag byte) [32]byte {
	t.Helper()
	var id [32]byte
	// Spread the tag so ids differ in the first byte (emit's sort key) and
	// can't collide with another suite's fixtures.
	id[0], id[1], id[31] = 0xC1, tag, tag
	return id
}

func cbsAsset(t *testing.T, code string) xdr.Asset {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteAccountID, cbsIssuer)
	if err != nil {
		t.Fatalf("strkey.Decode: %v", err)
	}
	var pk [32]byte
	copy(pk[:], raw)
	var ac xdr.AssetCode4
	copy(ac[:], code)
	return xdr.Asset{
		Type: xdr.AssetTypeAssetTypeCreditAlphanum4,
		AlphaNum4: &xdr.AlphaNum4{
			AssetCode: ac,
			Issuer:    xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: (*xdr.Uint256)(&pk)},
		},
	}
}

func cbsKeyXDR(t *testing.T, id [32]byte) string {
	t.Helper()
	h := xdr.Hash(id)
	b64, err := xdr.MarshalBase64(xdr.LedgerKey{
		Type: xdr.LedgerEntryTypeClaimableBalance,
		ClaimableBalance: &xdr.LedgerKeyClaimableBalance{
			BalanceId: xdr.ClaimableBalanceId{Type: xdr.ClaimableBalanceIdTypeClaimableBalanceIdTypeV0, V0: &h},
		},
	})
	if err != nil {
		t.Fatalf("MarshalBase64 key: %v", err)
	}
	return b64
}

func cbsEntryXDR(t *testing.T, id [32]byte, asset xdr.Asset, amount int64, lastMod uint32) string {
	t.Helper()
	h := xdr.Hash(id)
	b64, err := xdr.MarshalBase64(xdr.LedgerEntry{
		LastModifiedLedgerSeq: xdr.Uint32(lastMod),
		Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeClaimableBalance,
			ClaimableBalance: &xdr.ClaimableBalanceEntry{
				BalanceId: xdr.ClaimableBalanceId{Type: xdr.ClaimableBalanceIdTypeClaimableBalanceIdTypeV0, V0: &h},
				Claimants: []xdr.Claimant{},
				Asset:     asset,
				Amount:    xdr.Int64(amount),
			},
		},
	})
	if err != nil {
		t.Fatalf("MarshalBase64 entry: %v", err)
	}
	return b64
}

// cbsSeedsByID runs the reader and indexes what it emitted by claimable id,
// so a test can assert on its own fixtures without caring what else the shared
// schema holds.
func cbsSeedsByID(t *testing.T, ctx context.Context, addr string, assets map[string]struct{}) map[string]chstore.ClaimableBalanceSeed {
	t.Helper()
	if windows, culprit := cbsWalkWindows(t, ctx); windows > cbsMaxWalkWindows {
		t.Fatalf("the shared lake spans %d seed windows (limit %d): %s. Some fixture in this process left a far-future ledger in stellar.ledger_entry_changes; this walk would take minutes and time out under load. Remove it in that test's Cleanup (purgeLakeFixtureLedgers)",
			windows, cbsMaxWalkWindows, culprit)
	}
	out := map[string]chstore.ClaimableBalanceSeed{}
	if err := chstore.StreamClaimableBalanceSeeds(ctx, addr, assets, func(s chstore.ClaimableBalanceSeed) error {
		out[s.ClaimableID] = s
		return nil
	}); err != nil {
		t.Fatalf("StreamClaimableBalanceSeeds: %v", err)
	}
	return out
}

func cbsHex(id [32]byte) string { return xdr.Hash(id).HexString() }

// cbsWalkWindows reports how many initial-width windows a seed walk over the
// shared lake would take right now, and names the row holding the top of the
// range so an over-long walk can be traced to the fixture that caused it.
// Window count — not elapsed time — is the quantity asserted before a walk:
// it is exact and independent of machine load.
func cbsWalkWindows(t *testing.T, ctx context.Context) (uint64, string) {
	t.Helper()
	conn := dialClickHouse(t, ctx, "stellar")
	var rows uint64
	var lo, hi uint32
	if err := conn.QueryRow(ctx, `SELECT count(), min(ledger_seq), max(ledger_seq) FROM stellar.ledger_entry_changes`).Scan(&rows, &lo, &hi); err != nil {
		t.Fatalf("read lake ledger bounds: %v", err)
	}
	if rows == 0 {
		return 0, "empty lake"
	}
	var entryType, txHash string
	if err := conn.QueryRow(ctx, `SELECT toString(entry_type), tx_hash FROM stellar.ledger_entry_changes
		WHERE ledger_seq = ? ORDER BY tx_hash LIMIT 1`, hi).Scan(&entryType, &txHash); err != nil {
		t.Fatalf("read the lake's top row: %v", err)
	}
	windows := uint64(hi-lo)/cbsWalkWindowLedgers + 1
	return windows, fmt.Sprintf("ledgers [%d, %d], top row entry_type=%s tx_hash=%q", lo, hi, entryType, txHash)
}

// TestClaimableSeed_RecoversPreFloorBalance is the headline case: a claimable
// balance created long before the live observer existed, never claimed, is
// recovered from the append-log with its exact asset, amount and TRUE
// last-modified ledger. Before this reader existed that balance simply did not
// appear in claimable_observations, which is the whole of AQUA's 13.2%
// under-read vs Horizon (2026-07-27).
func TestClaimableSeed_RecoversPreFloorBalance(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	id := cbsID(t, 0x01)
	amount := int64(4_500_000_000_000)
	closeTime := time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC)

	if _, err := chstore.InsertEntryChanges(ctx, addr, []chstore.LedgerEntryChangeRow{{
		LedgerSeq: cbsPreFloorLedger, CloseTime: closeTime, TxHash: "cbs01", OpIndex: 0, ChangeIndex: 0,
		IntraLedgerSeq: 1, ChangeType: "created", EntryType: "claimable_balance",
		KeyXDR:   cbsKeyXDR(t, id),
		EntryXDR: cbsEntryXDR(t, id, cbsAsset(t, "AQUA"), amount, cbsPreFloorLedger),
	}}, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	got, ok := cbsSeedsByID(t, ctx, addr, nil)[cbsHex(id)]
	if !ok {
		t.Fatal("the pre-floor claimable balance was not recovered — the seed does not close the gap it exists for")
	}
	if got.AssetKey != cbsAssetKey {
		t.Errorf("AssetKey = %q, want %q", got.AssetKey, cbsAssetKey)
	}
	if got.Balance.Cmp(big.NewInt(amount)) != 0 {
		t.Errorf("Balance = %s, want %d", got.Balance, amount)
	}
	if got.LedgerSeq != cbsPreFloorLedger {
		t.Errorf("LedgerSeq = %d, want %d (seeding at the run's position instead of the entry's true ledger lets a live observation lose the at-or-before pick)", got.LedgerSeq, cbsPreFloorLedger)
	}
	if !got.CloseTime.Equal(closeTime) {
		t.Errorf("CloseTime = %v, want %v — observed_at is both the hypertable partition column and part of the PK", got.CloseTime, closeTime)
	}
}

// TestClaimableSeed_ClaimedBalanceIsNotSeeded — a balance claimed in a LATER
// window must not be seeded. This is the cross-window half of the reduction:
// the removal and the creation are resolved by separate server-side queries
// and reconciled in Go.
func TestClaimableSeed_ClaimedBalanceIsNotSeeded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	id := cbsID(t, 0x02)
	created := time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)
	claimed := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)

	if _, err := chstore.InsertEntryChanges(ctx, addr, []chstore.LedgerEntryChangeRow{
		{
			LedgerSeq: 34_000_000, CloseTime: created, TxHash: "cbs02a", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 1, ChangeType: "created", EntryType: "claimable_balance",
			KeyXDR:   cbsKeyXDR(t, id),
			EntryXDR: cbsEntryXDR(t, id, cbsAsset(t, "AQUA"), 999, 34_000_000),
		},
		{
			// A claim: stellar-core emits the pre-image STATE and then the
			// REMOVED. Both are in the lake; the removal must win.
			LedgerSeq: 48_000_000, CloseTime: claimed, TxHash: "cbs02b", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 1, ChangeType: "state", EntryType: "claimable_balance",
			KeyXDR:   cbsKeyXDR(t, id),
			EntryXDR: cbsEntryXDR(t, id, cbsAsset(t, "AQUA"), 999, 34_000_000),
		},
		{
			LedgerSeq: 48_000_000, CloseTime: claimed, TxHash: "cbs02b", OpIndex: 0, ChangeIndex: 1,
			IntraLedgerSeq: 2, ChangeType: "removed", EntryType: "claimable_balance",
			KeyXDR: cbsKeyXDR(t, id),
		},
	}, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	if got, ok := cbsSeedsByID(t, ctx, addr, nil)[cbsHex(id)]; ok {
		t.Errorf("a CLAIMED balance was seeded (%+v) — classic supply would over-report it forever", got)
	}
}

// TestClaimableSeed_SameLedgerRemovalCoherence is audit-2026-07-16 C2-4 on the
// real server. A claimable balance created AND claimed inside ONE ledger is an
// ordinary pattern (one transaction can do both), so ledger_seq alone cannot
// order the changes. With independent per-column argMax aggregates ClickHouse
// may resolve the tie differently per column — entry_xdr from the live change,
// change_type from the removal — and the removed-entry skip never fires,
// resurrecting a claimed balance into classic supply. One argMax over a tuple
// of every projected column, keyed on the full within-ledger identity tuple,
// makes that structurally impossible.
func TestClaimableSeed_SameLedgerRemovalCoherence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	id := cbsID(t, 0x03)
	const ledger = uint32(36_000_000)
	ct := time.Date(2022, 6, 1, 0, 0, 0, 0, time.UTC)

	if _, err := chstore.InsertEntryChanges(ctx, addr, []chstore.LedgerEntryChangeRow{
		{
			LedgerSeq: ledger, CloseTime: ct, TxHash: "cbs03a", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 10, ChangeType: "created", EntryType: "claimable_balance",
			KeyXDR:   cbsKeyXDR(t, id),
			EntryXDR: cbsEntryXDR(t, id, cbsAsset(t, "AQUA"), 123_456, ledger),
		},
		{
			// Different tx, LATER in the ledger's canonical walk. Only
			// intra_ledger_seq ranks these correctly: tx_hash "cbs03b" >
			// "cbs03a" here, so this test would also pass on the weaker
			// lexical tie-break — the intra_ledger_seq gap is what makes it
			// true by construction rather than by fixture luck.
			LedgerSeq: ledger, CloseTime: ct, TxHash: "cbs03b", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 11, ChangeType: "removed", EntryType: "claimable_balance",
			KeyXDR: cbsKeyXDR(t, id),
		},
	}, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	if got, ok := cbsSeedsByID(t, ctx, addr, nil)[cbsHex(id)]; ok {
		t.Errorf("same-ledger create-then-claim seeded a live balance (%+v) — the deleted entry was RESURRECTED", got)
	}
}

// TestClaimableSeed_NativeAndAssetScope — native (XLM) claimable balances are
// never seeded (Algorithm 1 does not read claimable_observations, and the live
// observer declines them), and -assets scoping filters classic ones. The
// default (nil) scope must include every classic asset.
func TestClaimableSeed_NativeAndAssetScope(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	nativeID, aquaID, usdcID := cbsID(t, 0x04), cbsID(t, 0x05), cbsID(t, 0x06)
	ct := time.Date(2022, 9, 9, 0, 0, 0, 0, time.UTC)
	const ledger = uint32(38_000_000)

	rows := []chstore.LedgerEntryChangeRow{
		{
			LedgerSeq: ledger, CloseTime: ct, TxHash: "cbs04", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 1, ChangeType: "created", EntryType: "claimable_balance",
			KeyXDR:   cbsKeyXDR(t, nativeID),
			EntryXDR: cbsEntryXDR(t, nativeID, xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}, 100, ledger),
		},
		{
			LedgerSeq: ledger, CloseTime: ct, TxHash: "cbs05", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 2, ChangeType: "created", EntryType: "claimable_balance",
			KeyXDR:   cbsKeyXDR(t, aquaID),
			EntryXDR: cbsEntryXDR(t, aquaID, cbsAsset(t, "AQUA"), 200, ledger),
		},
		{
			LedgerSeq: ledger, CloseTime: ct, TxHash: "cbs06", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 3, ChangeType: "created", EntryType: "claimable_balance",
			KeyXDR:   cbsKeyXDR(t, usdcID),
			EntryXDR: cbsEntryXDR(t, usdcID, cbsAsset(t, "USDC"), 300, ledger),
		},
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	all := cbsSeedsByID(t, ctx, addr, nil)
	if _, seeded := all[cbsHex(nativeID)]; seeded {
		t.Error("a NATIVE claimable balance was seeded; it belongs to Algorithm 1 and the live observer skips it")
	}
	if _, seeded := all[cbsHex(aquaID)]; !seeded {
		t.Error("default scope missed a classic AQUA balance — the default must cover EVERY classic credit asset")
	}
	if _, seeded := all[cbsHex(usdcID)]; !seeded {
		t.Error("default scope missed a classic USDC balance — the default must cover EVERY classic credit asset")
	}

	scoped := cbsSeedsByID(t, ctx, addr, map[string]struct{}{cbsAssetKey: {}})
	if _, seeded := scoped[cbsHex(aquaID)]; !seeded {
		t.Error("-assets scope dropped the asset it was scoped to")
	}
	if got, seeded := scoped[cbsHex(usdcID)]; seeded {
		t.Errorf("-assets scope leaked an out-of-scope asset: %+v", got)
	}
}

// TestClaimableSeed_WalkStaysBoundedAfterHighLedgerFixtures is CO-22's
// regression test. The seed reader walks the process-shared lake from
// min(ledger_seq) to max(ledger_seq), so a fixture another test leaves at a
// far-future ledger is paid for by every walk that follows it in the process.
// Two tests seed up there on purpose — TestBlendPoolReserves_SameLedgerLastChangeWins
// (ledger 4,000,000,000) and TestBlendPoolReserves_CurrentStateProjectionBoundsTheRead
// (3,999,900,000 and up) — and each walk after them took ~16,000 empty windows,
// 53-58 s unloaded, against a 5-minute deadline the two-walk test above
// breached on a loaded machine.
//
// Which shard and which order those tests land in is an accident of the
// sorted test list, so this test does not depend on it: it RUNS both as
// subtests, lets their Cleanups fire, and then asserts on the lake they left
// behind — first the window count (exact, load-independent), then one real
// walk against a wall-clock budget. Without the purge in either seeder the
// window count is ~16,000 and the walk exhausts cbsWalkBudget.
func TestClaimableSeed_WalkStaysBoundedAfterHighLedgerFixtures(t *testing.T) {
	addr := clickhouseAddr(t)

	if !t.Run("argmax-fixture", TestBlendPoolReserves_SameLedgerLastChangeWins) ||
		!t.Run("blend504-fixture", TestBlendPoolReserves_CurrentStateProjectionBoundsTheRead) {
		t.Fatal("a high-ledger fixture test failed; the lake it left behind says nothing about its cleanup")
	}

	ctx, cancel := context.WithTimeout(context.Background(), cbsWalkBudget)
	defer cancel()

	windows, top := cbsWalkWindows(t, ctx)
	if windows > cbsMaxWalkWindows {
		t.Errorf("after the high-ledger fixtures finished the lake spans %d seed windows, want <= %d (%s) — a fixture outlived its test",
			windows, cbsMaxWalkWindows, top)
	}

	start := time.Now()
	err := chstore.StreamClaimableBalanceSeeds(ctx, addr, nil, func(chstore.ClaimableBalanceSeed) error { return nil })
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("seed walk over %d windows failed after %s (budget %s): %v", windows, elapsed.Round(time.Millisecond), cbsWalkBudget, err)
	}
	t.Logf("seed walk: %d windows in %s (budget %s)", windows, elapsed.Round(time.Millisecond), cbsWalkBudget)
}
