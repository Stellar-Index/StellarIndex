//go:build integration

package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// Integration coverage for `stellarindex-ops supply seed-claimable-balances`'s
// lake reader. The unit tests in internal/storage/clickhouse cover the Go-side
// reduction exhaustively; what only a real server can prove is the SQL — the
// PREWHERE on entry_type, the single argMax over a TUPLE of every projected
// column, and the tuple's within-ledger ordering — which is exactly where the
// SAC seed's tie-break bug lived.
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
// under machine load. The fixtures that seed up there now remove
// their rows; cbsSeedsByID's window check turns any recurrence into an
// immediate, named failure instead of a load-dependent timeout, and
// TestClaimableSeed_WalkStaysBoundedAfterHighLedgerFixtures pins the two
// known offenders in any shard layout and any order.

const (
	cbsIssuer   = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
	cbsAssetKey = "AQUA:" + cbsIssuer
	// Well below the live claimable observer's floor (ledger 63,301,831)
	// — the population this seed exists to
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
	if _, err := chstore.StreamClaimableBalanceSeeds(ctx, addr, assets, nil, chstore.SeedWalk{}, func(s chstore.ClaimableBalanceSeed) error {
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
// last-modified ledger. Without this reader that balance would not
// appear in claimable_observations, under-reading AQUA's supply.
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

// TestClaimableSeed_SameLedgerRemovalCoherence is the same-ledger tie-break on the
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

// TestClaimableSeed_WalkStaysBoundedAfterHighLedgerFixtures is the
// regression test for that. The seed reader walks the process-shared lake from
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
	_, err := chstore.StreamClaimableBalanceSeeds(ctx, addr, nil, nil, chstore.SeedWalk{}, func(chstore.ClaimableBalanceSeed) error { return nil })
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("seed walk over %d windows failed after %s (budget %s): %v", windows, elapsed.Round(time.Millisecond), cbsWalkBudget, err)
	}
	t.Logf("seed walk: %d windows in %s (budget %s)", windows, elapsed.Round(time.Millisecond), cbsWalkBudget)
}

// TestClaimableSeed_RetractsServedClaim is the end-to-end proof: a claimable
// balance an earlier seed wrote as live, claimed while the live observer was
// not recording, must stop counting toward classic supply after a re-seed.
// A seed that could only add rows would leave the served reader
// summing the claimed balance forever.
func TestClaimableSeed_RetractsServedClaim(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	claimed, kept := cbsID(t, 0x61), cbsID(t, 0x62)
	created := time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)
	claimedAt := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)

	// The served tier as an earlier (add-only) seed left it: both live.
	for _, id := range [][32]byte{claimed, kept} {
		if err := store.InsertClaimableObservation(ctx, timescale.ClaimableObservation{
			ClaimableID: cbsHex(id), AssetKey: cbsAssetKey, Ledger: 34_000_000, ObservedAt: created,
			Balance: big.NewInt(999), IntraLedgerSeq: timescale.SeedIntraLedgerSeq,
		}); err != nil {
			t.Fatalf("InsertClaimableObservation: %v", err)
		}
	}
	// A removed row for an id nobody serves as live must not be listed.
	if err := store.InsertClaimableObservation(ctx, timescale.ClaimableObservation{
		ClaimableID: cbsHex(cbsID(t, 0x63)), AssetKey: cbsAssetKey, Ledger: 35_000_000, ObservedAt: created,
		Balance: big.NewInt(0), IsRemoval: true,
	}); err != nil {
		t.Fatalf("InsertClaimableObservation (removal): %v", err)
	}

	served, err := store.LiveClaimableObservations(ctx)
	if err != nil {
		t.Fatalf("LiveClaimableObservations: %v", err)
	}
	if len(served) != 2 || served[cbsHex(claimed)] != (timescale.LiveClaimable{AssetKey: cbsAssetKey, Ledger: 34_000_000}) {
		t.Fatalf("served live set = %+v, want exactly the two live balances at 34000000", served)
	}

	if _, err := chstore.InsertEntryChanges(ctx, addr, []chstore.LedgerEntryChangeRow{
		{
			LedgerSeq: 34_000_000, CloseTime: created, TxHash: "cbr61a", IntraLedgerSeq: 1,
			ChangeType: "created", EntryType: "claimable_balance", KeyXDR: cbsKeyXDR(t, claimed),
			EntryXDR: cbsEntryXDR(t, claimed, cbsAsset(t, "AQUA"), 999, 34_000_000),
		},
		{
			LedgerSeq: 48_000_000, CloseTime: claimedAt, TxHash: "cbr61b", IntraLedgerSeq: 1,
			ChangeType: "removed", EntryType: "claimable_balance", KeyXDR: cbsKeyXDR(t, claimed),
		},
		{
			LedgerSeq: 34_000_000, CloseTime: created, TxHash: "cbr62a", IntraLedgerSeq: 1,
			ChangeType: "created", EntryType: "claimable_balance", KeyXDR: cbsKeyXDR(t, kept),
			EntryXDR: cbsEntryXDR(t, kept, cbsAsset(t, "AQUA"), 999, 34_000_000),
		},
	}, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	servedKeys := map[string]string{}
	for id, row := range served {
		servedKeys[id] = row.AssetKey
	}
	var rows []timescale.ClaimableObservation
	if _, err := chstore.StreamClaimableBalanceSeeds(ctx, addr, nil, servedKeys, chstore.SeedWalk{}, func(s chstore.ClaimableBalanceSeed) error {
		if _, ours := served[s.ClaimableID]; ours {
			rows = append(rows, timescale.ClaimableObservation{
				ClaimableID: s.ClaimableID, AssetKey: s.AssetKey, Ledger: s.LedgerSeq, ObservedAt: s.CloseTime,
				Balance: s.Balance, IsRemoval: s.IsRemoval, IntraLedgerSeq: timescale.SeedIntraLedgerSeq,
			})
		}
		return nil
	}); err != nil {
		t.Fatalf("StreamClaimableBalanceSeeds: %v", err)
	}
	if err := store.InsertClaimableObservationBatch(ctx, rows); err != nil {
		t.Fatalf("InsertClaimableObservationBatch: %v", err)
	}

	sum, err := store.SumClaimableBalancesAtOrBefore(ctx, cbsAssetKey, 50_000_000)
	if err != nil {
		t.Fatalf("SumClaimableBalancesAtOrBefore: %v", err)
	}
	if sum.Cmp(big.NewInt(999)) != 0 {
		t.Errorf("served claimable sum after re-seed = %s, want 999 (the claimed balance retracted, the live one kept)", sum)
	}
	after, err := store.LiveClaimableObservations(ctx)
	if err != nil {
		t.Fatalf("LiveClaimableObservations (after): %v", err)
	}
	if _, still := after[cbsHex(claimed)]; still || len(after) != 1 {
		t.Errorf("served live set after re-seed = %+v, want only the unclaimed balance", after)
	}
}

// TestClaimableSeedProvenanceRoundTrip executes migration 0184's table through
// the store: an absent asset reads ok=false, an upsert round-trips every
// column, a second upsert overwrites, and an unverified pass is refused.
func TestClaimableSeedProvenanceRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if _, ok, err := store.ClaimableSeedProvenanceFor(ctx, cbsAssetKey); err != nil || ok {
		t.Fatalf("never stamped: ok=%v err=%v, want ok=false", ok, err)
	}
	if err := store.UpsertClaimableSeedProvenance(ctx, timescale.ClaimableSeedProvenance{AssetKey: cbsAssetKey, ClaimablesSeeded: 1}); err == nil {
		t.Fatal("a row without LakeVerifiedThrough was stamped")
	}

	minL, maxL := uint32(33_000_000), uint32(63_000_000)
	first := timescale.ClaimableSeedProvenance{
		AssetKey: cbsAssetKey, ClaimablesSeeded: 1200, ClaimablesRetracted: 7,
		MinLedgerSeen: &minL, MaxLedgerSeen: &maxL, LakeVerifiedThrough: 63_100_000,
	}
	if err := store.UpsertClaimableSeedProvenance(ctx, first); err != nil {
		t.Fatalf("UpsertClaimableSeedProvenance: %v", err)
	}
	got, ok, err := store.ClaimableSeedProvenanceFor(ctx, cbsAssetKey)
	if err != nil || !ok {
		t.Fatalf("read back: ok=%v err=%v", ok, err)
	}
	if got.ClaimablesSeeded != 1200 || got.ClaimablesRetracted != 7 || got.LakeVerifiedThrough != 63_100_000 ||
		got.MinLedgerSeen == nil || *got.MinLedgerSeen != minL || got.MaxLedgerSeen == nil || *got.MaxLedgerSeen != maxL || got.SeededAt.IsZero() {
		t.Errorf("read back %+v, want %+v", got, first)
	}

	second := timescale.ClaimableSeedProvenance{AssetKey: cbsAssetKey, ClaimablesRetracted: 3, LakeVerifiedThrough: 64_000_000}
	if err := store.UpsertClaimableSeedProvenance(ctx, second); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	got, _, err = store.ClaimableSeedProvenanceFor(ctx, cbsAssetKey)
	if err != nil {
		t.Fatal(err)
	}
	if got.ClaimablesSeeded != 0 || got.ClaimablesRetracted != 3 || got.LakeVerifiedThrough != 64_000_000 || got.MinLedgerSeen != nil || got.MaxLedgerSeen != nil {
		t.Errorf("after overwrite %+v, want %+v with nil ledger bounds", got, second)
	}
}

// TestSACFullHistorySeed_RecoversDormantPoolHolder reproduces the exact
// PHO/BLND VERDICT shape (docs/architecture/
// supply-pipeline.md "Dormant contract-held SAC balances"): a pool
// contract's SAC Balance(Address) entry whose last write predates the
// ClickHouse ledger_entries_current current-state MV's ~62M coverage
// floor. It writes the row into stellar.ledger_entry_changes (the raw
// append-log a real ch-backfill would have populated) and then
// synchronously deletes the mirrored row that the LIVE
// ledger_entries_current_mv trigger writes on every insert
// (fhSuppressFromCurrentState) — reproducing the FLOOR'S END STATE
// (a row present in the raw append-log but absent from the current-state
// projection) deterministically in a fresh test schema, where the real
// mechanism (the MV having been created strictly after some historical
// rows already existed on r1) can't be replicated because the test
// schema always creates the MV before any row is inserted. Then asserts:
//
//  1. StreamSACBalanceSeeds (the default, current-state-backed reader)
//     finds NOTHING for the dormant holder — reproducing the bug.
//  2. StreamSACBalanceSeedsFullHistory (the -full-history reader, reading
//     ledger_entry_changes directly) DOES find it — proving the fix.
func TestSACFullHistorySeed_RecoversDormantPoolHolder(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		sac   = "CBZ7M5B3Y4WWBZ5XK5UZCAFOEZ23KSSZXYECYX3IXM6E2JOLQC52DK32" // PHO SAC wrapper (real mainnet id)
		asset = "PHO:GAX5TXB5RYJNLBUR477PEXM4X75APK2PGMTN6KEFQSESGWFXEAKFSXJO"
	)
	// Well below the ~62,000,000 current-state floor — this is the ledger
	// the dormant pool contract actually acquired the SAC token at.
	const dormantLedger = uint32(41_500_000)
	closeTime := time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC)

	dormantBalance, ok := new(big.Int).SetString("599880000000000000000", 10) // ~6e20, matches the incident's magnitude
	if !ok {
		t.Fatal("bad test fixture: dormantBalance parse failed")
	}

	sacContract := fhContractScAddr(t, sac)
	// A synthetic-but-structurally-valid contract address standing in for
	// the dormant Phoenix/Blend pool holder — its own identity is
	// incidental to the test; what matters is that it's a CONTRACT
	// address (not a G-account) holding the SAC's Balance(Address) entry,
	// the exact shape the incident's pool holders had.
	poolAddr, poolContract := fhSyntheticContractAddr(t, 0xA1)
	balanceKey := fhBalanceKey(t, poolContract)
	keyXDR := fhKeyXDR(t, sacContract, balanceKey)
	entryXDR := fhEntryXDR(t, sacContract, balanceKey, fhI128Val(dormantBalance), dormantLedger)

	row := chstore.LedgerEntryChangeRow{
		LedgerSeq:   dormantLedger,
		CloseTime:   closeTime,
		TxHash:      "",
		OpIndex:     -1,
		ChangeIndex: 1,
		ChangeType:  "created",
		EntryType:   "contract_data",
		KeyXDR:      keyXDR,
		EntryXDR:    entryXDR,
	}
	written, err := chstore.InsertEntryChanges(ctx, addr, []chstore.LedgerEntryChangeRow{row}, 0)
	if err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}
	if written != 1 {
		t.Fatalf("InsertEntryChanges wrote %d rows, want 1", written)
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, []chstore.LedgerEntryChangeRow{fhLiveTTLRow(keyXDR, dormantLedger)}, 0); err != nil {
		t.Fatalf("InsertEntryChanges (ttl): %v", err)
	}
	// The live ledger_entries_current_mv mirrors the row we just inserted
	// (a fresh test schema always has the MV in place before any insert —
	// unlike r1, where it was created after ~62M-worth of ch-backfilled
	// history already existed). Synchronously delete the mirrored copy to
	// reproduce the floor's actual end state.
	fhSuppressFromCurrentState(t, ctx, addr, keyXDR)

	watched := map[string]string{sac: asset}

	// (1) The default current-state-backed reader sees NOTHING — the
	// mirrored row was suppressed above, reproducing "this Balance entry
	// is absent from ledger_entries_current".
	var currentStateFound int
	if err := chstore.StreamSACBalanceSeeds(ctx, addr, watched, func(seed chstore.SACBalanceSeed) error {
		if seed.Holder == poolAddr {
			currentStateFound++
		}
		return nil
	}); err != nil {
		t.Fatalf("StreamSACBalanceSeeds: %v", err)
	}
	if currentStateFound != 0 {
		t.Errorf("StreamSACBalanceSeeds (current-state) found %d rows for the dormant pool holder, want 0 (test fixture didn't touch ledger_entries_current — if this fires, the fixture itself is wrong, not the reader)", currentStateFound)
	}

	// (2) The full-history reader recovers it directly from the append-log.
	var got *chstore.SACBalanceSeed
	if _, err := chstore.StreamSACBalanceSeedsFullHistory(ctx, addr, watched, chstore.SeedWalk{}, func(seed chstore.SACBalanceSeed) error {
		if seed.Holder == poolAddr {
			s := seed
			got = &s
		}
		return nil
	}); err != nil {
		t.Fatalf("StreamSACBalanceSeedsFullHistory: %v", err)
	}
	if got == nil {
		t.Fatal("StreamSACBalanceSeedsFullHistory did not find the dormant pool holder — the fix did not recover it")
	}
	if got.ContractID != sac {
		t.Errorf("ContractID = %q, want %q", got.ContractID, sac)
	}
	if got.AssetKey != asset {
		t.Errorf("AssetKey = %q, want %q", got.AssetKey, asset)
	}
	if got.Balance.Cmp(dormantBalance) != 0 {
		t.Errorf("Balance = %s, want %s (i128 truncated?)", got.Balance, dormantBalance)
	}
	if got.LedgerSeq != dormantLedger {
		t.Errorf("LedgerSeq = %d, want %d", got.LedgerSeq, dormantLedger)
	}
}

// TestSACFullHistorySeed_LatestWriteWins proves the server-side
// `ORDER BY key_xdr, ledger_seq DESC LIMIT 1 BY key_xdr` reduction picks
// the HIGHEST-ledger write per storage key, not an arbitrary one — the
// same "latest wins" guarantee ledger_entries_current's
// ReplacingMergeTree(ledger_seq) provides, reproduced over the raw
// append-log which can (and does, under ch-backfill re-derive / live
// capture) hold multiple historical writes to the same key.
func TestSACFullHistorySeed_LatestWriteWins(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		sac   = "CD25MNVTZDL4Y3XBCPCJXGXATV5WUHHOWMYFF4YBEGU5FCPGMYTVG5JY" // BLND SAC wrapper (real mainnet id)
		asset = "BLND:GDJEHTBE6ZHUXSWFI642DCGLUOECLHPF3KSXHPXTSTJ7E3JF6MQ5EZYY"
	)
	closeTimeOld := time.Date(2021, 6, 1, 0, 0, 0, 0, time.UTC)
	closeTimeNew := time.Date(2023, 6, 1, 0, 0, 0, 0, time.UTC)

	sacContract := fhContractScAddr(t, sac)
	holder, holderAddr := fhSyntheticAccountAddr(t, 0xB2)
	holderKey := fhBalanceKey(t, holderAddr)
	keyXDR := fhKeyXDR(t, sacContract, holderKey)

	oldBal := big.NewInt(1_000_000)
	newBal := big.NewInt(2_000_000)
	rows := []chstore.LedgerEntryChangeRow{
		{
			LedgerSeq: 30_000_000, CloseTime: closeTimeOld, OpIndex: -1, ChangeIndex: 1,
			ChangeType: "created", EntryType: "contract_data", KeyXDR: keyXDR,
			EntryXDR: fhEntryXDR(t, sacContract, holderKey, fhI128Val(oldBal), 30_000_000),
		},
		{
			LedgerSeq: 45_000_000, CloseTime: closeTimeNew, OpIndex: -1, ChangeIndex: 1,
			ChangeType: "updated", EntryType: "contract_data", KeyXDR: keyXDR,
			EntryXDR: fhEntryXDR(t, sacContract, holderKey, fhI128Val(newBal), 45_000_000),
		},
		fhLiveTTLRow(keyXDR, 45_000_000),
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	watched := map[string]string{sac: asset}
	var got *chstore.SACBalanceSeed
	if _, err := chstore.StreamSACBalanceSeedsFullHistory(ctx, addr, watched, chstore.SeedWalk{}, func(seed chstore.SACBalanceSeed) error {
		if seed.Holder == holder {
			s := seed
			got = &s
		}
		return nil
	}); err != nil {
		t.Fatalf("StreamSACBalanceSeedsFullHistory: %v", err)
	}
	if got == nil {
		t.Fatal("StreamSACBalanceSeedsFullHistory found no row for the test holder")
	}
	if got.Balance.Cmp(newBal) != 0 {
		t.Errorf("Balance = %s, want %s (the LOWER-ledger write won — latest-wins reduction is broken)", got.Balance, newBal)
	}
	if got.LedgerSeq != 45_000_000 {
		t.Errorf("LedgerSeq = %d, want 45000000", got.LedgerSeq)
	}
}

// TestSACFullHistorySeed_SameLedgerRemovalCoherence proves: when one
// storage key is BOTH present and removed within the SAME
// ledger, the full-history seed must resolve the single genuine latest change
// coherently (every projected column from that one row) so the removed-entry
// skip fires and the deleted balance is NOT resurrected.
//
// change_index is only a per-transaction counter (extract_entry_changes.go),
// so ledger_seq does not order intra-ledger changes — the intra-ledger order
// is (op_index, change_index). The prior query took four INDEPENDENT
// argMax(col, ledger_seq) aggregates, which ClickHouse resolves per-column on
// a ledger_seq tie: it could pair the present row's entry_xdr with the removed
// row's change_type (or vice versa), so the removed skip below missed and the
// pre-removal before-image balance leaked into the SAC supply seed. The tuple
// argMax (ledger_seq, tx_hash, op_index, change_index) forces all columns from
// the one latest row, here the op_index=1 'removed' change → holder skipped.
func TestSACFullHistorySeed_SameLedgerRemovalCoherence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		sac   = "CBZ7M5B3Y4WWBZ5XK5UZCAFOEZ23KSSZXYECYX3IXM6E2JOLQC52DK32"
		asset = "PHO:GAX5TXB5RYJNLBUR477PEXM4X75APK2PGMTN6KEFQSESGWFXEAKFSXJO"
	)
	const ledger = uint32(50_000_000)
	closeTime := time.Date(2022, 6, 1, 0, 0, 0, 0, time.UTC)

	sacContract := fhContractScAddr(t, sac)
	holder, holderAddr := fhSyntheticAccountAddr(t, 0xC3)
	holderKey := fhBalanceKey(t, holderAddr)
	keyXDR := fhKeyXDR(t, sacContract, holderKey)

	// A non-zero before-image balance on BOTH rows: if the query resolves
	// change_type incoherently and misses the removal, this is exactly the
	// value that would be resurrected into the supply seed.
	beforeImage := big.NewInt(100_000_000)
	rows := []chstore.LedgerEntryChangeRow{
		{ // present earlier in the ledger (op 0)
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: "aa", OpIndex: 0, ChangeIndex: 1,
			ChangeType: "updated", EntryType: "contract_data", KeyXDR: keyXDR,
			EntryXDR: fhEntryXDR(t, sacContract, holderKey, fhI128Val(beforeImage), ledger),
		},
		{ // removed later in the SAME ledger (op 1) — the genuine latest state
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: "aa", OpIndex: 1, ChangeIndex: 1,
			ChangeType: "removed", EntryType: "contract_data", KeyXDR: keyXDR,
			EntryXDR: fhEntryXDR(t, sacContract, holderKey, fhI128Val(beforeImage), ledger),
		},
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	watched := map[string]string{sac: asset}
	// A retraction tombstone (IsRemoval=true, Balance=0) for this holder is
	// the CORRECT outcome: the removal is the genuine latest
	// intra-ledger state and must overwrite any prior observation. Only a
	// live, nonzero-balance emission would mean the deleted balance was
	// RESURRECTED — that's what this test guards against.
	var resurrected, tombstones int
	if _, err := chstore.StreamSACBalanceSeedsFullHistory(ctx, addr, watched, chstore.SeedWalk{}, func(seed chstore.SACBalanceSeed) error {
		if seed.Holder != holder {
			return nil
		}
		if seed.IsRemoval && seed.Balance.Sign() == 0 && seed.LedgerSeq == ledger {
			tombstones++
		} else {
			resurrected++
		}
		return nil
	}); err != nil {
		t.Fatalf("StreamSACBalanceSeedsFullHistory: %v", err)
	}
	if resurrected != 0 {
		t.Errorf("removed-in-same-ledger holder was emitted %d time(s) with a live balance — the deleted balance was RESURRECTED (column-incoherent argMax tie): the latest intra-ledger change is 'removed'", resurrected)
	}
	if tombstones != 1 {
		t.Errorf("emitted %d removal tombstones at ledger %d for the removed holder, want exactly 1", tombstones, ledger)
	}
}

// TestSACFullHistorySeed_SameLedgerRecreateWins is the positive complement:
// removed early then RE-CREATED later in the same ledger → the holder IS
// emitted, with the re-created balance (the latest intra-ledger change wins,
// resolved by op_index within the tied ledger_seq).
func TestSACFullHistorySeed_SameLedgerRecreateWins(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		sac   = "CD25MNVTZDL4Y3XBCPCJXGXATV5WUHHOWMYFF4YBEGU5FCPGMYTVG5JY"
		asset = "BLND:GDJEHTBE6ZHUXSWFI642DCGLUOECLHPF3KSXHPXTSTJ7E3JF6MQ5EZYY"
	)
	const ledger = uint32(55_000_000)
	closeTime := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)

	sacContract := fhContractScAddr(t, sac)
	holder, holderAddr := fhSyntheticAccountAddr(t, 0xD4)
	holderKey := fhBalanceKey(t, holderAddr)
	keyXDR := fhKeyXDR(t, sacContract, holderKey)

	recreated := big.NewInt(777_000_000)
	rows := []chstore.LedgerEntryChangeRow{
		{ // removed early (op 0)
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: "bb", OpIndex: 0, ChangeIndex: 1,
			ChangeType: "removed", EntryType: "contract_data", KeyXDR: keyXDR,
			EntryXDR: fhEntryXDR(t, sacContract, holderKey, fhI128Val(big.NewInt(1)), ledger),
		},
		{ // re-created later in the SAME ledger (op 2) — the genuine latest state
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: "bb", OpIndex: 2, ChangeIndex: 1,
			ChangeType: "created", EntryType: "contract_data", KeyXDR: keyXDR,
			EntryXDR: fhEntryXDR(t, sacContract, holderKey, fhI128Val(recreated), ledger),
		},
		fhLiveTTLRow(keyXDR, ledger),
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	watched := map[string]string{sac: asset}
	var got *chstore.SACBalanceSeed
	if _, err := chstore.StreamSACBalanceSeedsFullHistory(ctx, addr, watched, chstore.SeedWalk{}, func(seed chstore.SACBalanceSeed) error {
		if seed.Holder == holder {
			s := seed
			got = &s
		}
		return nil
	}); err != nil {
		t.Fatalf("StreamSACBalanceSeedsFullHistory: %v", err)
	}
	if got == nil {
		t.Fatal("re-created-in-same-ledger holder was NOT emitted — the removal at op 0 incoherently won over the re-create at op 2")
	}
	if got.Balance.Cmp(recreated) != 0 {
		t.Errorf("Balance = %s, want %s (a stale intra-ledger change won)", got.Balance, recreated)
	}
}

// fhSuppressFromCurrentState synchronously deletes the row matching
// keyXDR from stellar.ledger_entries_current — used to reproduce, in a
// fresh test schema, the end state of the real current-state coverage
// floor (a row present in ledger_entry_changes but absent from the
// current-state projection). mutations_sync=2 makes the ALTER TABLE
// DELETE block until the mutation (and any dependent replica/merge work)
// completes, so the row is guaranteed gone before the test reads it.
func fhSuppressFromCurrentState(t *testing.T, ctx context.Context, addr, keyXDR string) {
	t.Helper()
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr:     []string{addr},
		Auth:     clickhouse.Auth{Database: "stellar"},
		Settings: clickhouse.Settings{"mutations_sync": "2"},
	})
	if err != nil {
		t.Fatalf("open clickhouse for suppress: %v", err)
	}
	defer func() { _ = conn.Close() }()
	const q = `ALTER TABLE stellar.ledger_entries_current DELETE WHERE entry_type = 'contract_data' AND key_xdr = $1`
	if err := conn.Exec(ctx, q, keyXDR); err != nil {
		t.Fatalf("suppress mirrored row from ledger_entries_current: %v", err)
	}
}

// ─── XDR fixture helpers (mirror internal/storage/clickhouse's
// sac_balance_seed_test.go — duplicated here because that package's test
// helpers aren't exported across the package boundary) ────────────────

func fhContractScAddr(t *testing.T, cAddr string) xdr.ScAddress {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteContract, cAddr)
	if err != nil {
		t.Fatalf("strkey.Decode(%q): %v", cAddr, err)
	}
	var cid [32]byte
	copy(cid[:], raw)
	return xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: (*xdr.ContractId)(&cid)}
}

// fhBalanceKey builds the `Vec(Symbol("Balance"), Address(holder))` key
// for a CONTRACT holder (a pool address) — the shape a Phoenix/Blend pool
// contract's own SAC balance entry uses.
func fhBalanceKey(t *testing.T, holder xdr.ScAddress) xdr.ScVal {
	t.Helper()
	sym := xdr.ScSymbol("Balance")
	symSV := xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym}
	addrSV := xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &holder}
	vec := xdr.ScVec{symSV, addrSV}
	vp := &vec
	return xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &vp}
}

// fhSyntheticContractAddr builds a structurally-valid, deterministic
// C-strkey + matching xdr.ScAddress from a single tag byte (via
// strkey.Encode, so it's always checksum-valid — no hand-typed strkeys
// to get wrong). Used for stand-in pool/holder addresses whose specific
// identity doesn't matter to the test.
func fhSyntheticContractAddr(t *testing.T, tag byte) (string, xdr.ScAddress) {
	t.Helper()
	var raw [32]byte
	raw[0] = tag
	s, err := strkey.Encode(strkey.VersionByteContract, raw[:])
	if err != nil {
		t.Fatalf("strkey.Encode(contract, tag=%#x): %v", tag, err)
	}
	cid := xdr.ContractId(raw)
	return s, xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &cid}
}

// fhSyntheticAccountAddr is the G-address analogue of
// [fhSyntheticContractAddr].
func fhSyntheticAccountAddr(t *testing.T, tag byte) (string, xdr.ScAddress) {
	t.Helper()
	var raw [32]byte
	raw[0] = tag
	s, err := strkey.Encode(strkey.VersionByteAccountID, raw[:])
	if err != nil {
		t.Fatalf("strkey.Encode(account, tag=%#x): %v", tag, err)
	}
	pk := xdr.Uint256(raw)
	aid := xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: &pk}
	return s, xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &aid}
}

func fhI128Val(amount *big.Int) xdr.ScVal {
	lo := new(big.Int).And(amount, new(big.Int).SetUint64(^uint64(0)))
	hi := new(big.Int).Rsh(amount, 64)
	return xdr.ScVal{
		Type: xdr.ScValTypeScvI128,
		I128: &xdr.Int128Parts{Hi: xdr.Int64(hi.Int64()), Lo: xdr.Uint64(lo.Uint64())},
	}
}

// fhLiveTTLRow is the TTL change keeping a fixture Balance entry live past any
// lake tip another test can raise: the SAC seed refuses a watched key with no
// TTL row. The tx hash is per key so two fixtures' TTL rows never collapse.
func fhLiveTTLRow(keyXDR string, ledger uint32) chstore.LedgerEntryChangeRow {
	row := ttlChangeRow(keyXDR, ledger, 1, 4_000_000_000, 48)
	row.TxHash = "fh-ttl-" + keyXDR[len(keyXDR)-24:]
	return row
}

func fhKeyXDR(t *testing.T, contract xdr.ScAddress, key xdr.ScVal) string {
	t.Helper()
	lk := xdr.LedgerKey{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.LedgerKeyContractData{
			Contract:   contract,
			Key:        key,
			Durability: xdr.ContractDataDurabilityPersistent,
		},
	}
	b64, err := xdr.MarshalBase64(lk)
	if err != nil {
		t.Fatalf("MarshalBase64 key: %v", err)
	}
	return b64
}

func fhEntryXDR(t *testing.T, contract xdr.ScAddress, key, val xdr.ScVal, lastMod uint32) string {
	t.Helper()
	le := xdr.LedgerEntry{
		LastModifiedLedgerSeq: xdr.Uint32(lastMod),
		Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeContractData,
			ContractData: &xdr.ContractDataEntry{
				Contract:   contract,
				Key:        key,
				Durability: xdr.ContractDataDurabilityPersistent,
				Val:        val,
			},
		},
	}
	b64, err := xdr.MarshalBase64(le)
	if err != nil {
		t.Fatalf("MarshalBase64 entry: %v", err)
	}
	return b64
}

// TestSACSeed_ArchivedHolderRetractedAtArchivalLedger drives both SAC seed
// readers against a real ClickHouse: a watched Balance entry written at
// lastWrite whose TTL lapsed at liveUntil must come back as a tombstone at
// liveUntil+1 carrying that ledger's stellar.ledgers close time — not at
// lastWrite, where the seed's top-of-ledger upsert would overwrite the genuine
// observation and zero the holder across [lastWrite, archival).
func TestSACSeed_ArchivedHolderRetractedAtArchivalLedger(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	raw := dialClickHouse(t, ctx, "stellar")

	const (
		lastWrite = uint32(31_100_000)
		liveUntil = uint32(31_200_000)
		tipLedger = uint32(31_300_000)
		asset     = "ARCH:GAX5TXB5RYJNLBUR477PEXM4X75APK2PGMTN6KEFQSESGWFXEAKFSXJO"
	)
	archivalClose := time.Date(2025, 3, 1, 12, 0, 5, 0, time.UTC)

	sac, sacContract := fhSyntheticContractAddr(t, 0xE5)
	holder, holderAddr := fhSyntheticAccountAddr(t, 0xE6)
	balanceKey := fhBalanceKey(t, holderAddr)
	keyXDR := fhKeyXDR(t, sacContract, balanceKey)

	rows := []chstore.LedgerEntryChangeRow{
		{
			LedgerSeq: lastWrite, CloseTime: time.Date(2024, 11, 1, 0, 0, 0, 0, time.UTC),
			TxHash: "arch-seed-it", OpIndex: 0, ChangeIndex: 0,
			ChangeType: "created", EntryType: "contract_data", KeyXDR: keyXDR,
			EntryXDR: fhEntryXDR(t, sacContract, balanceKey, fhI128Val(big.NewInt(5_000_000)), lastWrite),
		},
		ttlChangeRow(keyXDR, lastWrite, 1, liveUntil, 48),
		// Lifts the lake tip (the seed's as-of ledger) past liveUntil.
		ttlChangeRow(ttlGovernedKeyXDR("arch-seed-tip"), tipLedger, 1, tipLedger+1_000_000, 48),
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}
	lb, err := raw.PrepareBatch(ctx, `INSERT INTO stellar.ledgers (ledger_seq, close_time, ledger_hash, prev_hash, protocol_version)`)
	if err != nil {
		t.Fatalf("prepare ledgers: %v", err)
	}
	if err := lb.Append(liveUntil+1, archivalClose, fmt.Sprintf("%064d", liveUntil+1), "00", uint32(22)); err != nil {
		t.Fatalf("append ledger: %v", err)
	}
	if err := lb.Send(); err != nil {
		t.Fatalf("send ledgers: %v", err)
	}

	watched := map[string]string{sac: asset}
	readers := map[string]func(context.Context, string, map[string]string, func(chstore.SACBalanceSeed) error) error{
		"current-state": chstore.StreamSACBalanceSeeds,
		"full-history":  sacFullHistoryUnverified,
	}
	for name, stream := range readers {
		var got []chstore.SACBalanceSeed
		if err := stream(ctx, addr, watched, func(s chstore.SACBalanceSeed) error {
			if s.Holder == holder {
				got = append(got, s)
			}
			return nil
		}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(got) != 1 {
			t.Fatalf("%s: emitted %d seeds for the archived holder, want 1 tombstone: %+v", name, len(got), got)
		}
		s := got[0]
		if !s.IsRemoval || s.Balance.Sign() != 0 {
			t.Errorf("%s: emitted %+v, want an IsRemoval zero-balance tombstone", name, s)
		}
		if s.LedgerSeq != liveUntil+1 {
			t.Errorf("%s: tombstone LedgerSeq = %d, want archival ledger %d (not last write %d)", name, s.LedgerSeq, liveUntil+1, lastWrite)
		}
		if !s.CloseTime.Equal(archivalClose) {
			t.Errorf("%s: tombstone CloseTime = %v, want %v from stellar.ledgers", name, s.CloseTime, archivalClose)
		}
	}
}

// TestSACSeed_UncoveredTTLRefuses: a watched Balance entry with no
// stellar.ttl_live_until row (an unbackfilled projection) must fail both
// readers — never be emitted as a live holder under a clean-looking pass.
func TestSACSeed_UncoveredTTLRefuses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		written = uint32(31_400_000)
		asset   = "NOTTL:GAX5TXB5RYJNLBUR477PEXM4X75APK2PGMTN6KEFQSESGWFXEAKFSXJO"
	)
	sac, sacContract := fhSyntheticContractAddr(t, 0xF1)
	holder, holderAddr := fhSyntheticAccountAddr(t, 0xF2)
	balanceKey := fhBalanceKey(t, holderAddr)
	keyXDR := fhKeyXDR(t, sacContract, balanceKey)

	rows := []chstore.LedgerEntryChangeRow{{
		LedgerSeq: written, CloseTime: time.Date(2024, 12, 1, 0, 0, 0, 0, time.UTC),
		TxHash: "nottl-seed-it", OpIndex: 0, ChangeIndex: 0,
		ChangeType: "created", EntryType: "contract_data", KeyXDR: keyXDR,
		EntryXDR: fhEntryXDR(t, sacContract, balanceKey, fhI128Val(big.NewInt(9_000_000)), written),
	}}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	watched := map[string]string{sac: asset}
	readers := map[string]func(context.Context, string, map[string]string, func(chstore.SACBalanceSeed) error) error{
		"current-state": chstore.StreamSACBalanceSeeds,
		"full-history":  sacFullHistoryUnverified,
	}
	for name, stream := range readers {
		var emitted int
		err := stream(ctx, addr, watched, func(s chstore.SACBalanceSeed) error {
			if s.Holder == holder {
				emitted++
			}
			return nil
		})
		if err == nil || !strings.Contains(err.Error(), "no stellar.ttl_live_until row") {
			t.Errorf("%s: err = %v, want the TTL-coverage refusal", name, err)
		}
		if emitted != 0 {
			t.Errorf("%s: emitted %d seeds for the uncovered holder, want 0", name, emitted)
		}
	}
}

// sacFullHistoryUnverified is the full-history reader over the synthetic test
// lake, which carries stellar.ledgers rows only where a fixture needs them.
func sacFullHistoryUnverified(ctx context.Context, addr string, watched map[string]string, fn func(chstore.SACBalanceSeed) error) error {
	_, err := chstore.StreamSACBalanceSeedsFullHistory(ctx, addr, watched, chstore.SeedWalk{}, fn)
	return err
}

// TestSupplyCrossCheckConvergesAfterPoolBalanceRecovery is the
// acceptance test for the BLND/EURC/KALE/PHO supply_cross_check_divergence
// residual ("PHO/BLND VERDICT" incident). It
// can't run against live r1 data from here, so it reproduces the
// documented divergence SHAPE with synthetic rows through the REAL
// Algorithm-2 pipeline (StorageClassicSupplyReader.ClassicSupplyAt →
// ClassicComputer.Compute, exactly what the aggregator's per-asset
// Refresher runs) against a real TimescaleDB:
//
//  1. Insert only the classic-side components a classic-only system would
//     have observed (trustlines + a small "already-visible" SAC
//     balance) — Algorithm 2's total under-counts, exactly like the
//     documented incident.
//  2. Cross-check that under-count against a fixed Algorithm-3 total
//     (the SAC's verified-correct lifetime supply — for PHO/BLND these
//     are the incident's real on-chain-verified figures; EURC/KALE use
//     representative round numbers reproducing the same shape) via
//     supply.CrossCheckForClass(..., WrapClassPartial) — the exact
//     function CrossCheckRefresher uses. Assert it reports `over`
//     (WithinTolerance=false), reproducing the alert firing.
//  3. Insert the "recovered" pool-held SAC balance — the row
//     `supply seed-sac-balances -full-history` would have written via
//     clickhouse.StreamSACBalanceSeedsFullHistory + Store
//     .InsertSACBalanceObservation (test/integration/
//     seed_test.go covers that extraction step
//     against ClickHouse; this test covers what happens to Algorithm 2
//     once the row lands in Postgres, which is the same table either
//     seed source writes to — sac_balance_observations. No new
//     aggregation code was needed: SumSACBalancesAtOrBefore already
//     sums every holder regardless of type).
//  4. Re-run the cross-check. Assert it now converges
//     (WithinTolerance=true) — the acceptance criterion.
func TestSupplyCrossCheckConvergesAfterPoolBalanceRecovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	reader := supply.NewStorageClassicSupplyReader(store, supply.ClassicSupplyReaderOptions{})
	computer, err := supply.NewClassicComputer(supply.Policy{}, reader)
	if err != nil {
		t.Fatalf("NewClassicComputer: %v", err)
	}

	const asOfLedger = 70_000_000
	observedAt := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name string
		// asset is the classic side (Algorithm 2).
		asset canonical.Asset
		// sacContract is the SAC wrapper's contract id (any structurally
		// distinct string — sac_balance_observations keys on it as an
		// opaque string, no strkey validation at this layer).
		sacContract string
		// classicHolder is an ordinary trustline holder representing the
		// classic-only "visible" classic supply.
		classicHolder string
		classicAmount int64
		// poolHolder is the dormant Phoenix/Blend pool contract's
		// address — the holder the -full-history seed recovers.
		poolHolder string
		poolAmount string // decimal string; some of these exceed int64
		// sacTotal is Algorithm 3's verified lifetime SAC supply — fixed,
		// not affected by anything this test inserts. PHO/BLND use the
		// incident's real on-chain-verified figures (docs/architecture/
		// supply-pipeline.md); EURC/KALE use representative round
		// numbers reproducing the same "sac_total > classic_total"
		// shape documented for the residual four.
		sacTotal string
	}{
		{
			name:          "PHO",
			asset:         canonical.Asset{Type: canonical.AssetClassic, Code: "PHO", Issuer: "GAX5TXB5RYJNLBUR477PEXM4X75APK2PGMTN6KEFQSESGWFXEAKFSXJO"},
			sacContract:   "CBZ7M5B3Y4WWBZ5XK5UZCAFOEZ23KSSZXYECYX3IXM6E2JOLQC52DK32",
			classicHolder: "GHOLDER_PHO_1",
			classicAmount: 200_000_000_000_000, // representative classic-only Alg-2 reading — well under sacTotal
			poolHolder:    "CPOOL_PHO_PHOENIX_1",
			poolAmount:    "1900000000000000", // recovers the dormant pool balance; classic+pool > sacTotal
			sacTotal:      "1999999993050277", // PHO lifetime SAC supply (exact real figure)
		},
		{
			name:          "BLND",
			asset:         canonical.Asset{Type: canonical.AssetClassic, Code: "BLND", Issuer: "GDJEHTBE6ZHUXSWFI642DCGLUOECLHPF3KSXHPXTSTJ7E3JF6MQ5EZYY"},
			sacContract:   "CD25MNVTZDL4Y3XBCPCJXGXATV5WUHHOWMYFF4YBEGU5FCPGMYTVG5JY",
			classicHolder: "GHOLDER_BLND_1",
			classicAmount: 1_100_000_000_000_000, // representative classic-only reading — under sacTotal by ~12%, matching the incident's ~12.4%-under BLND finding
			poolHolder:    "CPOOL_BLND_BACKSTOP_1",
			poolAmount:    "200000000000000",
			sacTotal:      "1236670485295609", // BLND lifetime SAC supply (exact real figure)
		},
		{
			name:          "EURC",
			asset:         canonical.Asset{Type: canonical.AssetClassic, Code: "EURC", Issuer: "GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2"},
			sacContract:   "CDTKPWPLOURQA2SGTKTUQOWRCBZEORB4BWBOMJ3D3ZTQQSGE5F6JBQLV",
			classicHolder: "GHOLDER_EURC_1",
			classicAmount: 3_000_000_000_000_000, // representative classic-only reading, same shape (exact live figure not captured in this investigation)
			poolHolder:    "CPOOL_EURC_PHOENIX_1",
			poolAmount:    "6000000000000000",
			sacTotal:      "7900000000000000", // representative, > classicAmount alone
		},
		{
			name:          "KALE",
			asset:         canonical.Asset{Type: canonical.AssetClassic, Code: "KALE", Issuer: "GBDVX4VELCDSQ54KQJYTNHXAHFLBCA77ZY2USQBM4CSHTTV7DME7KALE"},
			sacContract:   "CB23WRDQWGSP6YPMY4UV5C4OW5CBTXKYN3XEATG7KJEZCXMJBYEHOUOV",
			classicHolder: "GHOLDER_KALE_1",
			classicAmount: 1_000_000_000_000_000, // representative classic-only reading, same shape (exact live figure not captured in this investigation)
			poolHolder:    "CPOOL_KALE_DEFINDEX_1",
			poolAmount:    "2500000000000000",
			sacTotal:      "3010000000000000", // representative, matches the post-2×-fix KALE served-supply magnitude
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assetKey, err := supply.AssetKey(tc.asset)
			if err != nil {
				t.Fatalf("AssetKey: %v", err)
			}

			sacTotal, ok := new(big.Int).SetString(tc.sacTotal, 10)
			if !ok {
				t.Fatalf("bad fixture: sacTotal %q", tc.sacTotal)
			}
			sacSupply := supply.Supply{AssetKey: tc.sacContract, TotalSupply: sacTotal}

			// (1) Classic-side visibility only: an ordinary trustline
			// holder only — no pool-held balance yet.
			insertTrustline(t, ctx, store, tc.classicHolder, assetKey, 1000, tc.classicAmount, observedAt, false)

			before, err := computer.Compute(ctx, tc.asset, asOfLedger, observedAt)
			if err != nil {
				t.Fatalf("Compute (before): %v", err)
			}

			// (2) Cross-check BEFORE recovery: the over-mint direction
			// (sac_total > classic_total) is DIAGNOSTIC-ONLY since the
			// leg-1 demotion (c4184ddd — BLND retires supply classically
			// with no SAC burn and PHO minted its whole supply through
			// the SAC once, so this direction paged for a week on
			// correct accounting). The new contract this step pins:
			// WithinTolerance stays TRUE (no page) while OverMintStroops
			// still reports the excess so the diagnostic isn't lost.
			resultBefore, err := supply.CrossCheckForClass(before, sacSupply, supply.WrapClassPartial)
			if err != nil {
				t.Fatalf("CrossCheckForClass (before): %v", err)
			}
			if !resultBefore.WithinTolerance {
				t.Fatalf("%s: over-mint direction paged (WithinTolerance=false) — leg-1 demotion contract violated (classic=%s sac=%s)",
					tc.name, before.TotalSupply, sacTotal)
			}
			wantOverMint := new(big.Int).Sub(sacTotal, before.TotalSupply)
			if resultBefore.OverMintStroops == nil || resultBefore.OverMintStroops.Cmp(wantOverMint) != 0 {
				t.Fatalf("%s: OverMintStroops = %v, want %s (the demoted leg must still REPORT the excess)",
					tc.name, resultBefore.OverMintStroops, wantOverMint)
			}

			// (3) Recover the dormant pool-held SAC balance — the exact
			// row a `supply seed-sac-balances -full-history` pass would
			// write via Store.InsertSACBalanceObservation.
			poolAmount, ok := new(big.Int).SetString(tc.poolAmount, 10)
			if !ok {
				t.Fatalf("bad fixture: poolAmount %q", tc.poolAmount)
			}
			if err := store.InsertSACBalanceObservation(ctx, timescale.SACBalanceObservation{
				ContractID: tc.sacContract,
				AssetKey:   assetKey,
				Holder:     tc.poolHolder,
				Ledger:     41_500_000, // the pool's own dormant last-modified ledger, well below asOfLedger
				ObservedAt: observedAt.Add(-time.Hour),
				Balance:    poolAmount,
			}); err != nil {
				t.Fatalf("InsertSACBalanceObservation (recovered pool balance): %v", err)
			}

			after, err := computer.Compute(ctx, tc.asset, asOfLedger, observedAt)
			if err != nil {
				t.Fatalf("Compute (after): %v", err)
			}

			// (4) Acceptance criterion: recovery registers — the
			// cross-check stays within tolerance AND the over-mint
			// diagnostic clears (classic+pool now covers the SAC total,
			// so the reported excess drops to zero).
			resultAfter, err := supply.CrossCheckForClass(after, sacSupply, supply.WrapClassPartial)
			if err != nil {
				t.Fatalf("CrossCheckForClass (after): %v", err)
			}
			if !resultAfter.WithinTolerance {
				t.Errorf("%s: cross-check OVER TOLERANCE after pool-balance recovery — classic_total=%s sac_total=%s divergence=%s",
					tc.name, after.TotalSupply, sacTotal, resultAfter.DivergenceStroops)
			}
			if resultAfter.OverMintStroops != nil && resultAfter.OverMintStroops.Sign() > 0 {
				t.Errorf("%s: OverMintStroops = %s after recovery, want 0 (classic+pool must cover the SAC total)",
					tc.name, resultAfter.OverMintStroops)
			}
			if after.TotalSupply.Cmp(before.TotalSupply) <= 0 {
				t.Errorf("%s: classic total did not increase after recovering the pool balance (before=%s after=%s) — SumSACBalancesAtOrBefore did not pick up the new holder",
					tc.name, before.TotalSupply, after.TotalSupply)
			}
		})
	}
}

// TestSeedWalk_VerifyLakeRefusesAHole pins the coverage leg on both
// full-history seed readers: a walk asked to verify the lake must refuse,
// before emitting anything, a range whose stellar.ledgers rows are missing.
// The shared test lake carries entry changes at ledgers no fixture wrote a
// ledgers row for, so the hole here is real. Without the check both readers
// emit the holder / balance below as current state, which is what let a
// LiveSink drop elect a pre-hole value and stamp it full_history.
func TestSeedWalk_VerifyLakeRefusesAHole(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		sac   = "CD25MNVTZDL4Y3XBCPCJXGXATV5WUHHOWMYFF4YBEGU5FCPGMYTVG5JY" // BLND SAC wrapper (real mainnet id)
		asset = "BLND:GDJEHTBE6ZHUXSWFI642DCGLUOECLHPF3KSXHPXTSTJ7E3JF6MQ5EZYY"
		at    = uint32(31_000_000)
	)
	closeTime := time.Date(2021, 7, 1, 0, 0, 0, 0, time.UTC)
	sacContract := fhContractScAddr(t, sac)
	holder, holderAddr := fhSyntheticAccountAddr(t, 0xC7)
	holderKey := fhBalanceKey(t, holderAddr)
	keyXDR := fhKeyXDR(t, sacContract, holderKey)
	cbID := cbsID(t, 0x71)

	rows := []chstore.LedgerEntryChangeRow{
		{
			LedgerSeq: at, CloseTime: closeTime, OpIndex: -1, ChangeIndex: 1,
			ChangeType: "created", EntryType: "contract_data", KeyXDR: keyXDR,
			EntryXDR: fhEntryXDR(t, sacContract, holderKey, fhI128Val(big.NewInt(7_000_000)), at),
		},
		fhLiveTTLRow(keyXDR, at),
		{
			LedgerSeq: at, CloseTime: closeTime, TxHash: "slv01", IntraLedgerSeq: 1,
			ChangeType: "created", EntryType: "claimable_balance",
			KeyXDR:   cbsKeyXDR(t, cbID),
			EntryXDR: cbsEntryXDR(t, cbID, cbsAsset(t, "AQUA"), 5_000, at),
		},
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}
	walk := chstore.SeedWalk{VerifyLake: true}

	var sacEmitted int
	ev, err := chstore.StreamSACBalanceSeedsFullHistory(ctx, addr, map[string]string{sac: asset}, walk, func(s chstore.SACBalanceSeed) error {
		if s.Holder == holder {
			sacEmitted++
		}
		return nil
	})
	if !errors.Is(err, chstore.ErrSeedLakeIncomplete) {
		t.Errorf("SAC full-history: err = %v, want ErrSeedLakeIncomplete", err)
	}
	if sacEmitted != 0 || ev.LakeVerifiedThrough != 0 {
		t.Errorf("SAC full-history: emitted %d seeds, LakeVerifiedThrough=%d over an unverified lake, want 0 and 0", sacEmitted, ev.LakeVerifiedThrough)
	}

	var cbEmitted int
	ev, err = chstore.StreamClaimableBalanceSeeds(ctx, addr, nil, nil, walk, func(s chstore.ClaimableBalanceSeed) error {
		if s.ClaimableID == cbsHex(cbID) {
			cbEmitted++
		}
		return nil
	})
	if !errors.Is(err, chstore.ErrSeedLakeIncomplete) {
		t.Errorf("claimable: err = %v, want ErrSeedLakeIncomplete", err)
	}
	if cbEmitted != 0 || ev.LakeVerifiedThrough != 0 {
		t.Errorf("claimable: emitted %d seeds, LakeVerifiedThrough=%d over an unverified lake, want 0 and 0", cbEmitted, ev.LakeVerifiedThrough)
	}
}

// These tests prove the v0.21.4 TTL-liveness path end-to-end against a real
// ClickHouse: rows inserted into stellar.ledger_entry_changes flow through the
// ttl_live_until_mv materialized view (applied from tier1_schema.sql by the
// harness) into the slim stellar.ttl_live_until projection, and
// ClassifyTTLLiveness resolves verdicts from THAT table — the
// ledger_entries_current scan path is not consulted.

// ttlAsOf is the reference ledger for verdicts. Fixture ledgers sit far above
// any other test's ranges to keep the shared container's keys disjoint.
const ttlAsOf = uint32(72_500_000)

// ttlGovernedKeyXDR builds a distinctive base64 LedgerKey for the governed
// (e.g. contract_data) entry — the input shape callers hand to
// ClassifyTTLLiveness.
func ttlGovernedKeyXDR(tag string) string {
	return base64.StdEncoding.EncodeToString([]byte("ttl-liveness-it-governed-" + tag))
}

// ttlChangeRow renders the lake's TTL change for the entry governed by
// governedXDR: KeyXDR is the 36-byte TTL LedgerKey (type=00000009 |
// sha256(decoded governed key)), EntryXDR the TTLEntry
// (lastModified(4) | data.type(4) | keyHash(32) | liveUntil(4) | ext(4)),
// truncated/padded to entryLen so malformed shapes can be seeded too.
func ttlChangeRow(governedXDR string, ledger, intra, liveUntil uint32, entryLen int) chstore.LedgerEntryChangeRow {
	rawKey, err := base64.StdEncoding.DecodeString(governedXDR)
	if err != nil {
		panic(fmt.Sprintf("fixture governed key must be valid base64: %v", err))
	}
	keyHash := sha256.Sum256(rawKey)

	ttlKey := make([]byte, 0, 36)
	ttlKey = append(ttlKey, 0x00, 0x00, 0x00, 0x09) // LedgerEntryType TTL = 9
	ttlKey = append(ttlKey, keyHash[:]...)

	full := make([]byte, 48)
	binary.BigEndian.PutUint32(full[0:4], ledger) // lastModifiedLedgerSeq
	binary.BigEndian.PutUint32(full[4:8], 9)      // data.type = TTL
	copy(full[8:40], keyHash[:])
	binary.BigEndian.PutUint32(full[40:44], liveUntil) // XDR big-endian
	// full[44:48] = ext.v 0
	entry := full
	if entryLen != len(full) {
		entry = make([]byte, entryLen)
		copy(entry, full)
	}

	return chstore.LedgerEntryChangeRow{
		LedgerSeq:      ledger,
		CloseTime:      time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		TxHash:         "ttl-liveness-it-" + governedXDR[:8],
		OpIndex:        0,
		ChangeIndex:    0,
		ChangeType:     "updated",
		EntryType:      "ttl",
		KeyXDR:         base64.StdEncoding.EncodeToString(ttlKey),
		EntryXDR:       base64.StdEncoding.EncodeToString(entry),
		IntraLedgerSeq: intra,
	}
}

func TestClassifyTTLLiveness_SlimProjectionEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")

	var (
		archived  = ttlGovernedKeyXDR("archived")   // lapsed long ago
		boundary  = ttlGovernedKeyXDR("boundary")   // live_until == asOf → LIVE (not yet lapsed)
		extended  = ttlGovernedKeyXDR("extended")   // lapsed then bumped in a later ledger → LIVE
		sameLedgr = ttlGovernedKeyXDR("sameledger") // bumped twice in ONE ledger → intra tie-break
		malformed = ttlGovernedKeyXDR("malformed")  // 47-byte entry → MV skips → UNKNOWN
		absent    = ttlGovernedKeyXDR("absent")     // no TTL row at all → UNKNOWN
	)

	rows := []chstore.LedgerEntryChangeRow{
		ttlChangeRow(archived, 72_000_001, 1, ttlAsOf-1_000_000, 48),
		ttlChangeRow(boundary, 72_000_002, 1, ttlAsOf, 48),
		// extended: the LATER (winning) change is handed to the writer FIRST —
		// adversarial insert order; argMax(live_until, version) must not care.
		ttlChangeRow(extended, 72_000_100, 1, ttlAsOf+5_000_000, 48),
		ttlChangeRow(extended, 72_000_003, 1, ttlAsOf-2_000_000, 48),
		// sameLedgr: two changes in ONE ledger. Only intra_ledger_seq (the low
		// 32 bits of the RMT version) separates them; the later one extends.
		ttlChangeRow(sameLedgr, 72_000_004, 7, ttlAsOf+3_000_000, 48),
		ttlChangeRow(sameLedgr, 72_000_004, 6, ttlAsOf-3_000_000, 48),
		ttlChangeRow(malformed, 72_000_005, 1, ttlAsOf+1_000_000, 47),
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	keys := []string{archived, boundary, extended, sameLedgr, malformed, absent, "not!valid!base64"}
	got, err := chstore.ClassifyTTLLiveness(ctx, conn, keys, ttlAsOf)
	if err != nil {
		t.Fatalf("ClassifyTTLLiveness: %v", err)
	}

	want := map[string]chstore.TTLLiveness{
		archived:           chstore.TTLArchived,
		boundary:           chstore.TTLLive,
		extended:           chstore.TTLLive,
		sameLedgr:          chstore.TTLLive,
		malformed:          chstore.TTLUnknown, // fail-open: unrecognised shape never proves archived
		absent:             chstore.TTLUnknown,
		"not!valid!base64": chstore.TTLUnknown, // undecodable input key stays kept, no error
	}
	if len(got) != len(want) {
		t.Errorf("got %d verdicts, want %d", len(got), len(want))
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("verdict[%s] = %v, want %v", k, got[k], w)
		}
	}
}

// TestClassifyTTLLiveness_MissingProjectionFailsLoud proves the no-fallback
// contract on a real server: with stellar.ttl_live_until absent, the
// classifier returns an error naming the DDL artifact instead of silently
// degrading every key to TTLUnknown (which would read as "nothing archived").
// The table is renamed away and restored — integration tests in this package
// run sequentially, and no ledger_entry_changes insert happens in the window
// (the MV would otherwise error on its missing target).
func TestClassifyTTLLiveness_MissingProjectionFailsLoud(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_ = clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")

	if err := conn.Exec(ctx, "RENAME TABLE stellar.ttl_live_until TO stellar.ttl_live_until_it_hidden"); err != nil {
		t.Fatalf("hide ttl_live_until: %v", err)
	}
	restored := false
	restore := func() {
		if restored {
			return
		}
		restored = true
		if err := conn.Exec(ctx, "RENAME TABLE stellar.ttl_live_until_it_hidden TO stellar.ttl_live_until"); err != nil {
			t.Fatalf("restore ttl_live_until (container state now broken for later tests): %v", err)
		}
	}
	defer restore()

	_, err := chstore.ClassifyTTLLiveness(ctx, conn, []string{ttlGovernedKeyXDR("missing-table")}, ttlAsOf)
	if err == nil {
		t.Fatal("ClassifyTTLLiveness succeeded without the projection; the deleted scan path must not have a silent fallback")
	}
	if !strings.Contains(err.Error(), "deploy/clickhouse/ttl_live_until.sql") {
		t.Errorf("error does not point the operator at the DDL artifact: %v", err)
	}

	// Restore, then prove the guard clears: absent keys resolve UNKNOWN, no error.
	restore()
	got, err := chstore.ClassifyTTLLiveness(ctx, conn, []string{ttlGovernedKeyXDR("missing-table")}, ttlAsOf)
	if err != nil {
		t.Fatalf("ClassifyTTLLiveness after restore: %v", err)
	}
	if got[ttlGovernedKeyXDR("missing-table")] != chstore.TTLUnknown {
		t.Errorf("expected TTLUnknown for a key with no TTL row, got %v", got[ttlGovernedKeyXDR("missing-table")])
	}
}

// The instance backfill reads ledger_entry_changes, a
// ReplacingMergeTree(ingested_at), and a corrected re-ingest leaves the stale
// part beside the fix until a merge. Without FINAL both rows ride one INSERT
// into contract_instance_changes, tie on its DEFAULT now() version, and the
// stale wasm_hash can be the one code-history serves. The stale part is
// written LAST so an unmerged read hands it to the target last.
func TestContractInstanceBackfill_ReadsCorrectedSourceRowOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")

	const ledger = uint32(72_719_001)
	var cidRaw xdr.Hash
	copy(cidRaw[:], []byte("gh719-instance-backfill-final-c1"))
	cid := xdr.ContractId(cidRaw)
	contractHash := hex.EncodeToString(cidRaw[:])
	stale, corrected := t356Hash(0x20), t356Hash(0x21)
	instKey, correctedEntry := t356InstanceKeyAndEntry(t, cid, corrected)
	_, staleEntry := t356InstanceKeyAndEntry(t, cid, stale)

	row := chstore.LedgerEntryChangeRow{
		LedgerSeq: ledger, CloseTime: time.Date(2025, 4, 1, 0, 0, 0, 0, time.UTC), TxHash: "gh719-upgrade",
		OpIndex: 0, ChangeIndex: 0, IntraLedgerSeq: 1, ChangeType: "updated", EntryType: "contract_data",
		KeyXDR: instKey, EntryXDR: correctedEntry,
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, []chstore.LedgerEntryChangeRow{row}, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}
	// The same source key, one hour OLDER by version: the stale row a
	// merge would discard, still sitting in its own part.
	if err := conn.Exec(ctx, `INSERT INTO stellar.ledger_entry_changes
		SELECT * REPLACE (? AS entry_xdr, ingested_at - INTERVAL 1 HOUR AS ingested_at)
		FROM stellar.ledger_entry_changes WHERE ledger_seq = ? AND tx_hash = ?`,
		staleEntry, ledger, row.TxHash); err != nil {
		t.Fatalf("insert stale part: %v", err)
	}
	var parts uint64
	if err := conn.QueryRow(ctx, `SELECT count() FROM stellar.ledger_entry_changes
		WHERE ledger_seq = ? AND tx_hash = ?`, ledger, row.TxHash).Scan(&parts); err != nil || parts != 2 {
		t.Fatalf("unmerged source rows = %d (%v), want 2 (the fix and its stale twin)", parts, err)
	}

	// Drop what the MV wrote at insert time, so the target holds only the
	// backfill's output.
	if err := conn.Exec(ctx, `ALTER TABLE stellar.contract_instance_changes DELETE
		WHERE contract_hash = ? SETTINGS mutations_sync = 2`, contractHash); err != nil {
		t.Fatalf("clear MV rows: %v", err)
	}
	if err := chstore.BackfillContractInstanceChanges(ctx, addr, ledger, ledger, 10, t.Logf); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	var got []string
	q, err := conn.Query(ctx, `SELECT wasm_hash FROM stellar.contract_instance_changes FINAL
		WHERE contract_hash = ? AND ledger_seq = ?`, contractHash, ledger)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	for q.Next() {
		var h string
		if err := q.Scan(&h); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, h)
	}
	_ = q.Close()
	want := hex.EncodeToString(corrected[:])
	if len(got) != 1 || got[0] != want {
		t.Fatalf("backfilled wasm_hash = %v, want exactly the corrected [%s] (stale %s must not survive)",
			got, want, hex.EncodeToString(stale[:]))
	}
}

// contract_instance_changes keyed (contract_hash, ledger_seq,
// change_index), and change_index restarts per TRANSACTION. Two transactions
// writing one contract's instance in the same ledger — both at change_index 0
// — collapsed to one row on merge, so a same-ledger upgrade vanished from the
// code history and the survivor was the last INSERTED, not the ledger-final
// write. This drives the shipped DDL (tier1 + the r1 migration's v2), its MV,
// the backfill INSERT and both indexed reader queries on a real ClickHouse.
//
// Red against the old key: after OPTIMIZE … FINAL only one ledger-L row
// survives, so the count assertions fail and the history loses a version.

const (
	t356Ledger0 = uint32(72_356_001) // deploy: executable h0
	t356Ledger  = uint32(72_356_050) // tx A installs h1, then tx B installs h2
)

func t356Hash(b byte) xdr.Hash {
	var h xdr.Hash
	for i := range h {
		h[i] = b
	}
	return h
}

func t356InstanceKeyAndEntry(t *testing.T, cid xdr.ContractId, wasm xdr.Hash) (string, string) {
	t.Helper()
	addr := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &cid}
	key := xdr.LedgerKey{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.LedgerKeyContractData{
			Contract:   addr,
			Key:        xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance},
			Durability: xdr.ContractDataDurabilityPersistent,
		},
	}
	inst := xdr.ScContractInstance{Executable: xdr.ContractExecutable{
		Type: xdr.ContractExecutableTypeContractExecutableWasm, WasmHash: &wasm,
	}}
	entry := xdr.LedgerEntry{Data: xdr.LedgerEntryData{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.ContractDataEntry{
			Contract:   addr,
			Key:        xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance},
			Durability: xdr.ContractDataDurabilityPersistent,
			Val:        xdr.ScVal{Type: xdr.ScValTypeScvContractInstance, Instance: &inst},
		},
	}}
	k, err := xdr.MarshalBase64(key)
	if err != nil {
		t.Fatalf("marshal instance key: %v", err)
	}
	e, err := xdr.MarshalBase64(entry)
	if err != nil {
		t.Fatalf("marshal instance entry: %v", err)
	}
	return k, e
}

func t356CodeKeyAndEntry(t *testing.T, wasm xdr.Hash) (string, string) {
	t.Helper()
	var key xdr.LedgerKey
	if err := key.SetContractCode(wasm); err != nil {
		t.Fatalf("contract_code key: %v", err)
	}
	entry := xdr.LedgerEntry{Data: xdr.LedgerEntryData{
		Type:         xdr.LedgerEntryTypeContractCode,
		ContractCode: &xdr.ContractCodeEntry{Hash: wasm, Code: []byte("\x00asm\x01\x00\x00\x00")},
	}}
	k, err := xdr.MarshalBase64(key)
	if err != nil {
		t.Fatalf("marshal code key: %v", err)
	}
	e, err := xdr.MarshalBase64(entry)
	if err != nil {
		t.Fatalf("marshal code entry: %v", err)
	}
	return k, e
}

// applyDeployStatements executes every statement of one deploy/clickhouse
// artifact — for the migration, that is its Step-1 CREATEs only; the later
// steps are operator-run comments.
func applyDeployStatements(t *testing.T, ctx context.Context, conn driver.Conn, name string) {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "..", "..", "deploy", "clickhouse", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	for _, s := range splitSQLStatements(string(raw)) {
		if err := conn.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %.80q: %v", name, s, err)
		}
	}
}

func t356CountRows(t *testing.T, ctx context.Context, conn driver.Conn, table, contractHash string, ledger uint32) uint64 {
	t.Helper()
	if err := conn.Exec(ctx, "OPTIMIZE TABLE stellar."+table+" FINAL"); err != nil {
		t.Fatalf("optimize %s: %v", table, err)
	}
	var n uint64
	if err := conn.QueryRow(ctx, "SELECT count() FROM stellar."+table+
		" FINAL WHERE contract_hash = ? AND ledger_seq = ?", contractHash, ledger).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestContractInstanceChanges_SameLedgerTransactionsBothSurvive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")

	applyDeployStatements(t, ctx, conn, "contract_instance_changes_tx_key.sql")
	t.Cleanup(func() {
		_ = conn.Exec(context.Background(), "DROP VIEW IF EXISTS stellar.contract_instance_changes_v2_mv")
		_ = conn.Exec(context.Background(), "DROP TABLE IF EXISTS stellar.contract_instance_changes_v2")
	})

	var cidRaw xdr.Hash
	copy(cidRaw[:], []byte("t356-two-tx-same-ledger-contract"))
	cid := xdr.ContractId(cidRaw)
	contractHash := hex.EncodeToString(cidRaw[:])
	contractStrkey, err := strkey.Encode(strkey.VersionByteContract, cidRaw[:])
	if err != nil {
		t.Fatalf("strkey: %v", err)
	}
	h0, h1, h2 := t356Hash(0x10), t356Hash(0x11), t356Hash(0x12)
	instKey, entry0 := t356InstanceKeyAndEntry(t, cid, h0)
	_, entry1 := t356InstanceKeyAndEntry(t, cid, h1)
	_, entry2 := t356InstanceKeyAndEntry(t, cid, h2)
	codeKey, codeEntry := t356CodeKeyAndEntry(t, h2)
	closeTime := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)

	rows := []chstore.LedgerEntryChangeRow{
		{
			LedgerSeq: t356Ledger0, CloseTime: closeTime, TxHash: "t356-deploy", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 3, ChangeType: "created", EntryType: "contract_data", KeyXDR: instKey, EntryXDR: entry0,
		},
		{
			LedgerSeq: t356Ledger0, CloseTime: closeTime, TxHash: "t356-deploy", OpIndex: 0, ChangeIndex: 1,
			IntraLedgerSeq: 4, ChangeType: "created", EntryType: "contract_code", KeyXDR: codeKey, EntryXDR: codeEntry,
		},
		// Tx B is ledger-final (intra 9) and is handed to the writer FIRST,
		// so an insertion-order survivor would be tx A's h1.
		{
			LedgerSeq: t356Ledger, CloseTime: closeTime, TxHash: "t356-tx-b", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 9, ChangeType: "updated", EntryType: "contract_data", KeyXDR: instKey, EntryXDR: entry2,
		},
		{
			LedgerSeq: t356Ledger, CloseTime: closeTime, TxHash: "t356-tx-a", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 4, ChangeType: "updated", EntryType: "contract_data", KeyXDR: instKey, EntryXDR: entry1,
		},
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	// The MVs (canonical and the migration's v2) keep both same-ledger writes.
	for _, table := range []string{chstore.ContractInstanceChangesTable, chstore.ContractInstanceChangesV2Table} {
		if n := t356CountRows(t, ctx, conn, table, contractHash, t356Ledger); n != 2 {
			t.Fatalf("%s via MV: %d rows at ledger %d, want 2 (one per transaction)", table, n, t356Ledger)
		}
	}

	// The backfill INSERT, into the migration's target, re-derives the same.
	if err := conn.Exec(ctx, "TRUNCATE TABLE stellar.contract_instance_changes_v2"); err != nil {
		t.Fatalf("truncate v2: %v", err)
	}
	if err := chstore.BackfillContractInstanceChangesInto(ctx, addr, chstore.ContractInstanceChangesV2Table,
		t356Ledger0, t356Ledger, 1000, t.Logf); err != nil {
		t.Fatalf("backfill v2: %v", err)
	}
	if n := t356CountRows(t, ctx, conn, chstore.ContractInstanceChangesV2Table, contractHash, t356Ledger); n != 2 {
		t.Fatalf("backfill: %d rows at ledger %d, want 2", n, t356Ledger)
	}
	var ils []uint32
	var txs []string
	q, err := conn.Query(ctx, `SELECT intra_ledger_seq, tx_hash FROM stellar.contract_instance_changes_v2 FINAL
		WHERE contract_hash = ? AND ledger_seq = ? ORDER BY intra_ledger_seq`, contractHash, t356Ledger)
	if err != nil {
		t.Fatalf("read v2: %v", err)
	}
	for q.Next() {
		var s uint32
		var h string
		if err := q.Scan(&s, &h); err != nil {
			t.Fatalf("scan v2: %v", err)
		}
		ils, txs = append(ils, s), append(txs, h)
	}
	_ = q.Close()
	if len(ils) != 2 || ils[0] != 4 || ils[1] != 9 || txs[0] != "t356-tx-a" || txs[1] != "t356-tx-b" {
		t.Fatalf("backfilled (intra_ledger_seq, tx_hash) = %v %v, want [4 9] [t356-tx-a t356-tx-b]", ils, txs)
	}

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })

	hist, err := er.ContractCodeHistory(ctx, contractStrkey)
	if err != nil {
		t.Fatalf("ContractCodeHistory: %v", err)
	}
	want := []string{hex.EncodeToString(h0[:]), hex.EncodeToString(h1[:]), hex.EncodeToString(h2[:])}
	if len(hist) != len(want) {
		t.Fatalf("code history = %+v, want hashes %v (the same-ledger h1 must not vanish)", hist, want)
	}
	for i, v := range hist {
		if v.WasmHash != want[i] {
			t.Fatalf("code history[%d] = %s, want %s (ledger-final h2 last): %+v", i, v.WasmHash, want[i], hist)
		}
	}

	info, err := er.ContractWasm(ctx, contractStrkey)
	if err != nil {
		t.Fatalf("ContractWasm: %v", err)
	}
	if info.WasmHash != want[2] {
		t.Fatalf("ContractWasm hash = %s, want the ledger-final %s", info.WasmHash, want[2])
	}
}

// The genesis watermark is written by Set...Watermark (ordering-guarded) and
// consumed by ContractCodeHistory: an index miss skips the ledger_entry_changes
// scan only under a covering mark. Runs the real INSERT and SELECT.
func TestContractInstanceGenesisWatermark_RoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")
	const name = chstore.ContractInstanceChangesTable
	clearMark := func() {
		cctx, ccancel := context.WithTimeout(context.Background(), time.Minute)
		defer ccancel()
		if err := conn.Exec(cctx, `ALTER TABLE stellar.entry_history_watermark DELETE WHERE name = ? SETTINGS mutations_sync = 2`, name); err != nil {
			t.Fatalf("clear mark: %v", err)
		}
	}
	clearMark()
	t.Cleanup(clearMark)

	const ledger = uint32(72_720_001)
	var cidRaw xdr.Hash
	copy(cidRaw[:], []byte("inv2188-watermark-roundtrip-c001"))
	cid := xdr.ContractId(cidRaw)
	strk, err := strkey.Encode(strkey.VersionByteContract, cidRaw[:])
	if err != nil {
		t.Fatal(err)
	}
	instKey, entry := t356InstanceKeyAndEntry(t, cid, t356Hash(0x31))
	if _, err := chstore.InsertEntryChanges(ctx, addr, []chstore.LedgerEntryChangeRow{{
		LedgerSeq: ledger, CloseTime: time.Date(2025, 4, 2, 0, 0, 0, 0, time.UTC), TxHash: "inv2188-tx",
		IntraLedgerSeq: 1, ChangeType: "created", EntryType: "contract_data", KeyXDR: instKey, EntryXDR: entry,
	}}, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}
	// Leave the index non-empty (so it is "available") but without this
	// contract: an index miss the writer's mark alone can authorise.
	if err := conn.Exec(ctx, `ALTER TABLE stellar.contract_instance_changes DELETE WHERE contract_hash = ? SETTINGS mutations_sync = 2`,
		hex.EncodeToString(cidRaw[:])); err != nil {
		t.Fatalf("clear MV row: %v", err)
	}
	other, h33 := t356Hash(0x32), t356Hash(0x33)
	if err := conn.Exec(ctx, `INSERT INTO stellar.contract_instance_changes
		(contract_hash, ledger_seq, tx_hash, change_index, intra_ledger_seq, close_time, is_sac, wasm_hash)
		VALUES (?, 1, 'inv2188-other', 0, 1, now(), 0, ?)`, hex.EncodeToString(h33[:]), hex.EncodeToString(other[:])); err != nil {
		t.Fatalf("seed index row: %v", err)
	}

	history := func() int {
		er, err := chstore.NewExplorerReader(ctx, addr)
		if err != nil {
			t.Fatalf("NewExplorerReader: %v", err)
		}
		defer func() { _ = er.Close() }()
		h, err := er.ContractCodeHistory(ctx, strk)
		if err != nil {
			t.Fatalf("ContractCodeHistory: %v", err)
		}
		return len(h)
	}
	if n := history(); n != 1 {
		t.Fatalf("no mark: history = %d versions, want 1 (the scan must run)", n)
	}
	if err := chstore.SetContractInstanceChangesGenesisWatermark(ctx, addr, name, 500); err != nil {
		t.Fatal(err)
	}
	if n := history(); n != 0 {
		t.Fatalf("covering mark: history = %d versions, want 0 (scan skipped)", n)
	}
	// A later, lower mark must not replace the higher one.
	if err := chstore.SetContractInstanceChangesGenesisWatermark(ctx, addr, name, 300); err != nil {
		t.Fatal(err)
	}
	var rows uint64
	if err := conn.QueryRow(ctx, `SELECT count() FROM stellar.entry_history_watermark WHERE name = ?`, name).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("mark rows = %d (%v), want 1: a lower thru must record nothing", rows, err)
	}
}

// The mark is gated on the view's identity and age from system.tables; run
// that lookup for real against a table with a view and one without.
func TestContractInstanceGenesisWatermark_StartState(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	st, err := chstore.ReadInstanceBackfillStart(ctx, addr, chstore.ContractInstanceChangesTable)
	if err != nil {
		t.Fatalf("ReadInstanceBackfillStart: %v", err)
	}
	if !st.MVExists || st.MVUUID == "" || st.MVModified.IsZero() || st.MVAge < 0 {
		t.Fatalf("start state = %+v, want the canonical view's identity and age", st)
	}
	again, err := chstore.ReadInstanceBackfillStart(ctx, addr, chstore.ContractInstanceChangesTable)
	if err != nil || again.MVUUID != st.MVUUID || !again.MVModified.Equal(st.MVModified) {
		t.Fatalf("re-read = %+v (%v), want the same view identity as %+v", again, err, st)
	}
	v2, err := chstore.ReadInstanceBackfillStart(ctx, addr, chstore.ContractInstanceChangesV2Table)
	if err != nil || v2.MVExists {
		t.Fatalf("v2 start state = %+v (%v), want no view", v2, err)
	}
}

// instanceFixture returns a deterministic C-strkey and the base64 LedgerKey of
// its persistent ScvLedgerKeyContractInstance entry — the key the explorer's
// contract routes classify.
func instanceFixture(t *testing.T, tag string) (contractStrkey, instanceKeyXDR string) {
	t.Helper()
	sum := sha256.Sum256([]byte("contract-instance-state-it-" + tag))
	contractStrkey, err := strkey.Encode(strkey.VersionByteContract, sum[:])
	if err != nil {
		t.Fatalf("encode contract strkey: %v", err)
	}
	cid := xdr.ContractId(sum)
	key := xdr.LedgerKey{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.LedgerKeyContractData{
			Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &cid},
			Key:        xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance},
			Durability: xdr.ContractDataDurabilityPersistent,
		},
	}
	instanceKeyXDR, err = xdr.MarshalBase64(key)
	if err != nil {
		t.Fatalf("marshal instance key: %v", err)
	}
	return contractStrkey, instanceKeyXDR
}

// TestContractInstanceState_TTLRowAndAbsence runs the explorer's instance read
// against a real ClickHouse: a TTL row for the instance key yields Known plus
// the newest live_until (so an archived instance is judged archived), and a
// contract with no lake evidence at all yields Known=false without error.
func TestContractInstanceState_TTLRowAndAbsence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	archivedContract, archivedKey := instanceFixture(t, "archived")
	neverDeployed, _ := instanceFixture(t, "never-deployed")
	lapsedAt := ttlAsOf - 1_000_000

	rows := []chstore.LedgerEntryChangeRow{
		ttlChangeRow(archivedKey, 72_100_001, 1, lapsedAt-5, 48),
		ttlChangeRow(archivedKey, 72_100_002, 1, lapsedAt, 48),
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	defer func() { _ = er.Close() }()

	st, err := er.ContractInstanceState(ctx, archivedContract)
	if err != nil {
		t.Fatalf("ContractInstanceState(archived): %v", err)
	}
	if !st.Known || st.LiveUntil != lapsedAt {
		t.Fatalf("archived instance state = %+v, want Known with LiveUntil %d", st, lapsedAt)
	}
	if v := chstore.TTLVerdictAt(st.LiveUntil, ttlAsOf); v != chstore.TTLArchived {
		t.Errorf("verdict = %v, want TTLArchived", v)
	}

	st, err = er.ContractInstanceState(ctx, neverDeployed)
	if err != nil {
		t.Fatalf("ContractInstanceState(never deployed): %v", err)
	}
	if st.Known || st.LiveUntil != 0 {
		t.Errorf("never-deployed state = %+v, want zero", st)
	}
}
