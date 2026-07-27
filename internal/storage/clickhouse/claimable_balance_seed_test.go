package clickhouse

import (
	"bytes"
	"encoding/hex"
	"math/big"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	claimable "github.com/Stellar-Index/StellarIndex/internal/sources/claimable_balances"
)

const (
	// Real AQUA — the asset the 2026-07-27 Horizon cross-check measured the
	// 13.2% claimable-component gap on.
	cbIssuer   = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
	cbAssetKey = "AQUA:" + cbIssuer
	cbLedger   = uint32(30_500_000)
)

// cbID builds a deterministic 32-byte ClaimableBalanceId from a tag byte.
func cbID(tag byte) [32]byte {
	var id [32]byte
	id[0], id[31] = tag, tag
	return id
}

func cbAsset4(t *testing.T, code string) xdr.Asset {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteAccountID, cbIssuer)
	if err != nil {
		t.Fatalf("strkey.Decode(%q): %v", cbIssuer, err)
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

func cbAsset12(t *testing.T, code string) xdr.Asset {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteAccountID, cbIssuer)
	if err != nil {
		t.Fatalf("strkey.Decode(%q): %v", cbIssuer, err)
	}
	var pk [32]byte
	copy(pk[:], raw)
	var ac xdr.AssetCode12
	copy(ac[:], code)
	return xdr.Asset{
		Type: xdr.AssetTypeAssetTypeCreditAlphanum12,
		AlphaNum12: &xdr.AlphaNum12{
			AssetCode: ac,
			Issuer:    xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: (*xdr.Uint256)(&pk)},
		},
	}
}

// cbEntry builds the ClaimableBalanceEntry the lake stores in entry_xdr.
func cbEntry(id [32]byte, asset xdr.Asset, amount int64) xdr.LedgerEntry {
	h := xdr.Hash(id)
	return xdr.LedgerEntry{
		LastModifiedLedgerSeq: xdr.Uint32(cbLedger),
		Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeClaimableBalance,
			ClaimableBalance: &xdr.ClaimableBalanceEntry{
				BalanceId: xdr.ClaimableBalanceId{Type: xdr.ClaimableBalanceIdTypeClaimableBalanceIdTypeV0, V0: &h},
				Claimants: []xdr.Claimant{},
				Asset:     asset,
				Amount:    xdr.Int64(amount),
			},
		},
	}
}

func cbEntryXDR(t *testing.T, id [32]byte, asset xdr.Asset, amount int64) string {
	t.Helper()
	b64, err := xdr.MarshalBase64(cbEntry(id, asset, amount))
	if err != nil {
		t.Fatalf("MarshalBase64 entry: %v", err)
	}
	return b64
}

func cbKeyXDR(t *testing.T, id [32]byte) string {
	t.Helper()
	h := xdr.Hash(id)
	lk := xdr.LedgerKey{
		Type: xdr.LedgerEntryTypeClaimableBalance,
		ClaimableBalance: &xdr.LedgerKeyClaimableBalance{
			BalanceId: xdr.ClaimableBalanceId{Type: xdr.ClaimableBalanceIdTypeClaimableBalanceIdTypeV0, V0: &h},
		},
	}
	b64, err := xdr.MarshalBase64(lk)
	if err != nil {
		t.Fatalf("MarshalBase64 key: %v", err)
	}
	return b64
}

func cbOrd(ledger, intra uint32, tx string, op int32, changeIdx uint32) lakeEntryChangeOrder {
	return lakeEntryChangeOrder{ledgerSeq: ledger, intraLedgerSeq: intra, txHash: tx, opIndex: op, changeIndex: changeIdx}
}

func collectClaimableSeeds(t *testing.T, r *claimableSeedReducer) []ClaimableBalanceSeed {
	t.Helper()
	var got []ClaimableBalanceSeed
	if err := r.emit(func(s ClaimableBalanceSeed) error { got = append(got, s); return nil }); err != nil {
		t.Fatalf("emit: %v", err)
	}
	return got
}

// newCBReducer builds a reducer already positioned at cbLedger's window, so a
// test that never calls startWindow itself still exercises the same path the
// walker uses.
func newCBReducer(t *testing.T, assets map[string]struct{}) *claimableSeedReducer {
	t.Helper()
	r := newClaimableSeedReducer(assets)
	if err := r.startWindow(0); err != nil {
		t.Fatalf("startWindow(0): %v", err)
	}
	return r
}

// ─── asset-key derivation ───────────────────────────────────────────────

// TestClaimableSeedAssetKey pins the CODE:ISSUER derivation against the shapes
// the live observer accepts and declines. Getting the code padding wrong here
// would seed rows under an asset_key the served reader never queries — a
// silent no-op that still reports "seeded N balances".
func TestClaimableSeedAssetKey(t *testing.T) {
	nonEd := cbAsset4(t, "AQUA")
	nonEd.AlphaNum4.Issuer = xdr.AccountId{Type: xdr.PublicKeyType(99)}

	cases := []struct {
		name  string
		asset xdr.Asset
		want  string
		ok    bool
	}{
		{"alphanum4 trims null padding", cbAsset4(t, "AQUA"), cbAssetKey, true},
		{"alphanum4 short code", cbAsset4(t, "yXLM"), "yXLM:" + cbIssuer, true},
		{"alphanum12 trims null padding", cbAsset12(t, "SOMETHINGXL"), "SOMETHINGXL:" + cbIssuer, true},
		{"native is not seeded", xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}, "", false},
		{"nil alphanum4 body", xdr.Asset{Type: xdr.AssetTypeAssetTypeCreditAlphanum4}, "", false},
		{"nil alphanum12 body", xdr.Asset{Type: xdr.AssetTypeAssetTypeCreditAlphanum12}, "", false},
		{"non-ed25519 issuer", nonEd, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := claimableSeedAssetKey(c.asset)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v (got %q)", ok, c.ok, got)
			}
			if got != c.want {
				t.Errorf("assetKey = %q, want %q", got, c.want)
			}
		})
	}
}

// TestClaimableSeedMatchesLiveObserver is the parity check the whole design
// rests on: a seeded row must be INDISTINGUISHABLE from one the live
// LedgerEntryChange observer writes, or the two populations of
// claimable_observations disagree about identity and the served
// `DISTINCT ON (claimable_id)` sum double-counts (or silently drops) the
// overlap. It runs the same entry through both paths and compares the three
// fields that make up that identity: claimable_id, asset_key, balance.
func TestClaimableSeedMatchesLiveObserver(t *testing.T) {
	obs, err := claimable.NewObserver([]string{cbAssetKey})
	if err != nil {
		t.Fatalf("NewObserver: %v", err)
	}
	id := cbID(0x5A)
	entry := cbEntry(id, cbAsset4(t, "AQUA"), 4_500_000_000_000)
	change := xdr.LedgerEntryChange{Type: xdr.LedgerEntryChangeTypeLedgerEntryCreated, Created: &entry}
	if !obs.Matches(change) {
		t.Fatal("live observer does not match the fixture — the parity check is vacuous")
	}
	events, err := obs.Decode(dispatcher.LedgerEntryChangeContext{
		Ledger: cbLedger, ClosedAt: time.Unix(1_600_000_000, 0).UTC(), Change: change,
	})
	if err != nil {
		t.Fatalf("observer Decode: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("observer emitted %d events, want 1", len(events))
	}
	live, ok := events[0].(claimable.Observation)
	if !ok {
		t.Fatalf("observer emitted %T, want claimable_balances.Observation", events[0])
	}

	r := newCBReducer(t, nil)
	if err := r.offer(cbKeyXDR(t, id), cbEntryXDR(t, id, cbAsset4(t, "AQUA"), 4_500_000_000_000),
		"created", time.Unix(1_600_000_000, 0).UTC(), cbOrd(cbLedger, 1, "aa", 0, 0)); err != nil {
		t.Fatalf("offer: %v", err)
	}
	seeds := collectClaimableSeeds(t, r)
	if len(seeds) != 1 {
		t.Fatalf("seed emitted %d rows, want 1", len(seeds))
	}

	if seeds[0].ClaimableID != live.ClaimableID {
		t.Errorf("claimable_id: seed %q vs live observer %q", seeds[0].ClaimableID, live.ClaimableID)
	}
	if seeds[0].AssetKey != live.AssetKey {
		t.Errorf("asset_key: seed %q vs live observer %q", seeds[0].AssetKey, live.AssetKey)
	}
	if seeds[0].Balance.Cmp(live.Balance) != 0 {
		t.Errorf("balance: seed %s vs live observer %s", seeds[0].Balance, live.Balance)
	}
	if seeds[0].LedgerSeq != live.Ledger {
		t.Errorf("ledger: seed %d vs live observer %d", seeds[0].LedgerSeq, live.Ledger)
	}
	var _ consumer.Event = live // the observer's row shape is the ingest contract
}

// ─── decode ─────────────────────────────────────────────────────────────

// TestClaimableSeedDecodesAmount — the ordinary case, asserted as *big.Int
// (ADR-0003): the persisted column is NUMERIC and the live observer emits
// *big.Int, so the seed must too, at full stroop precision.
func TestClaimableSeedDecodesAmount(t *testing.T) {
	id := cbID(0x11)
	want := big.NewInt(92_233_720_368_547_758) // ~9.2e16 stroops
	r := newCBReducer(t, nil)
	closeTime := time.Date(2020, 11, 23, 12, 0, 0, 0, time.UTC)
	if err := r.offer(cbKeyXDR(t, id), cbEntryXDR(t, id, cbAsset4(t, "AQUA"), want.Int64()),
		"created", closeTime, cbOrd(cbLedger, 3, "aa", 0, 0)); err != nil {
		t.Fatalf("offer: %v", err)
	}
	got := collectClaimableSeeds(t, r)
	if len(got) != 1 {
		t.Fatalf("emitted %d seeds, want 1", len(got))
	}
	if got[0].Balance.Cmp(want) != 0 {
		t.Errorf("Balance = %s, want %s", got[0].Balance, want)
	}
	if got[0].ClaimableID != hex.EncodeToString(id[:]) {
		t.Errorf("ClaimableID = %q, want %q", got[0].ClaimableID, hex.EncodeToString(id[:]))
	}
	if got[0].LedgerSeq != cbLedger {
		t.Errorf("LedgerSeq = %d, want %d", got[0].LedgerSeq, cbLedger)
	}
	if !got[0].CloseTime.Equal(closeTime) {
		t.Errorf("CloseTime = %v, want %v", got[0].CloseTime, closeTime)
	}
}

// TestClaimableSeedSkipsNative — native XLM claimable balances belong to
// Algorithm 1, whose reader never consumes claimable_observations, and the
// live observer declines them (ErrUnsupportedClaimableAsset). Seeding them
// would inflate nothing today but would put rows in a table whose only
// consumer sums by classic asset_key — and, more importantly, they are the
// bulk of the network's claimable balances, so retaining them is what would
// blow the reducer's memory bound.
func TestClaimableSeedSkipsNative(t *testing.T) {
	id := cbID(0x22)
	r := newCBReducer(t, nil)
	if err := r.offer(cbKeyXDR(t, id), cbEntryXDR(t, id, xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}, 100),
		"created", time.Now().UTC(), cbOrd(cbLedger, 1, "aa", 0, 0)); err != nil {
		t.Fatalf("offer: %v", err)
	}
	if n := len(r.live); n != 0 {
		t.Errorf("reducer retained %d native balance(s); they must be rejected on sight", n)
	}
	if got := collectClaimableSeeds(t, r); len(got) != 0 {
		t.Errorf("emitted %d native seed(s): %+v", len(got), got)
	}
}

// TestClaimableSeedAssetFilter — -assets scoping is applied at OFFER time, so
// out-of-scope balances never cost memory. In-scope ones are unaffected.
func TestClaimableSeedAssetFilter(t *testing.T) {
	inScope, outOfScope := cbID(0x31), cbID(0x32)
	r := newCBReducer(t, map[string]struct{}{cbAssetKey: {}})

	if err := r.offer(cbKeyXDR(t, inScope), cbEntryXDR(t, inScope, cbAsset4(t, "AQUA"), 7),
		"created", time.Now().UTC(), cbOrd(cbLedger, 1, "aa", 0, 0)); err != nil {
		t.Fatalf("offer in-scope: %v", err)
	}
	if err := r.offer(cbKeyXDR(t, outOfScope), cbEntryXDR(t, outOfScope, cbAsset4(t, "USDC"), 9),
		"created", time.Now().UTC(), cbOrd(cbLedger, 2, "aa", 0, 0)); err != nil {
		t.Fatalf("offer out-of-scope: %v", err)
	}

	got := collectClaimableSeeds(t, r)
	if len(got) != 1 {
		t.Fatalf("emitted %d seeds, want exactly the in-scope one: %+v", len(got), got)
	}
	if got[0].AssetKey != cbAssetKey {
		t.Errorf("AssetKey = %q, want %q", got[0].AssetKey, cbAssetKey)
	}
}

// ─── ordering / tie-breaks (the C2-4 family, on the Go side) ────────────

// TestClaimableSeedReducer_LatestLedgerWins — the ordinary cross-window case:
// the same balance written in two windows resolves to the higher ledger,
// whichever order the offers arrive in.
func TestClaimableSeedReducer_LatestLedgerWins(t *testing.T) {
	id := cbID(0x41)
	old := time.Date(2021, 6, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2023, 6, 1, 0, 0, 0, 0, time.UTC)

	for _, order := range []string{"ascending", "descending"} {
		t.Run(order, func(t *testing.T) {
			r := newCBReducer(t, nil)
			offers := []func() error{
				func() error {
					return r.offer(cbKeyXDR(t, id), cbEntryXDR(t, id, cbAsset4(t, "AQUA"), 1_000_000),
						"created", old, cbOrd(30_000_000, 1, "aa", 0, 1))
				},
				func() error {
					return r.offer(cbKeyXDR(t, id), cbEntryXDR(t, id, cbAsset4(t, "AQUA"), 2_000_000),
						"updated", recent, cbOrd(45_000_000, 1, "bb", 0, 1))
				},
			}
			if order == "descending" {
				offers[0], offers[1] = offers[1], offers[0]
			}
			for _, o := range offers {
				if err := o(); err != nil {
					t.Fatalf("offer: %v", err)
				}
			}
			got := collectClaimableSeeds(t, r)
			if len(got) != 1 {
				t.Fatalf("emitted %d seeds, want 1", len(got))
			}
			if got[0].Balance.Cmp(big.NewInt(2_000_000)) != 0 {
				t.Errorf("Balance = %s, want 2000000 (the lower-ledger write won)", got[0].Balance)
			}
			if got[0].LedgerSeq != 45_000_000 {
				t.Errorf("LedgerSeq = %d, want 45000000", got[0].LedgerSeq)
			}
			if !got[0].CloseTime.Equal(recent) {
				t.Errorf("CloseTime = %v, want %v (the winner's columns must travel together)", got[0].CloseTime, recent)
			}
		})
	}
}

// TestClaimableSeedReducer_SameLedgerRemovalWins is the C2-4 case. A claimable
// balance created and CLAIMED in the same ledger is an ordinary pattern (one
// transaction can do both), so the tie-break is not hypothetical here: getting
// it backwards seeds a claimed balance and inflates classic supply — the
// over-count direction, in the component this seed exists to correct.
//
// The removal is offered FIRST so a naive "last offer wins" fold fails.
func TestClaimableSeedReducer_SameLedgerRemovalWins(t *testing.T) {
	id := cbID(0x51)
	ct := time.Date(2022, 6, 1, 0, 0, 0, 0, time.UTC)
	const ledger = uint32(50_000_000)

	r := newCBReducer(t, nil)
	if err := r.offer(cbKeyXDR(t, id), "", "removed", ct, cbOrd(ledger, 42, "aa", 1, 1)); err != nil {
		t.Fatalf("offer removal: %v", err)
	}
	if err := r.offer(cbKeyXDR(t, id), cbEntryXDR(t, id, cbAsset4(t, "AQUA"), 100_000_000),
		"created", ct, cbOrd(ledger, 41, "aa", 0, 1)); err != nil {
		t.Fatalf("offer live: %v", err)
	}
	if got := collectClaimableSeeds(t, r); len(got) != 0 {
		t.Errorf("emitted %d seed(s) for a balance whose latest same-ledger change is 'removed' — a CLAIMED balance was resurrected into classic supply: %+v", len(got), got)
	}
}

// TestClaimableSeedReducer_SameLedgerRemovalWinsOnChangeIndex is the same tie
// one element deeper: legacy rows carry intra_ledger_seq = 0 and both changes
// can share a tx and an op, so change_index is the only discriminator left.
func TestClaimableSeedReducer_SameLedgerRemovalWinsOnChangeIndex(t *testing.T) {
	id := cbID(0x52)
	ct := time.Date(2022, 6, 1, 0, 0, 0, 0, time.UTC)
	const ledger = uint32(50_000_000)

	r := newCBReducer(t, nil)
	if err := r.offer(cbKeyXDR(t, id), cbEntryXDR(t, id, cbAsset4(t, "AQUA"), 100_000_000),
		"created", ct, cbOrd(ledger, 0, "aa", 0, 1)); err != nil {
		t.Fatalf("offer live: %v", err)
	}
	if err := r.offer(cbKeyXDR(t, id), "", "removed", ct, cbOrd(ledger, 0, "aa", 0, 2)); err != nil {
		t.Fatalf("offer removal: %v", err)
	}
	if got := collectClaimableSeeds(t, r); len(got) != 0 {
		t.Errorf("emitted %d seed(s); the change_index=2 removal is the latest change: %+v", len(got), got)
	}
}

// TestClaimableSeedReducer_RecreateAfterRemovalWins — a removal in an EARLIER
// window must not suppress a later live change for the same key. (A claimable
// id is derived from the creating operation id and is not expected to recur,
// but the reduction must not DEPEND on that: relying on removal being terminal
// is exactly how a latest-wins fold turns into a first-wins one.)
func TestClaimableSeedReducer_RecreateAfterRemovalWins(t *testing.T) {
	id := cbID(0x53)
	r := newCBReducer(t, nil)
	if err := r.offer(cbKeyXDR(t, id), "", "removed",
		time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC), cbOrd(40_000_000, 3, "aa", 0, 1)); err != nil {
		t.Fatalf("offer removal: %v", err)
	}
	if err := r.offer(cbKeyXDR(t, id), cbEntryXDR(t, id, cbAsset4(t, "AQUA"), 777_000_000), "created",
		time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC), cbOrd(45_000_000, 4, "bb", 0, 1)); err != nil {
		t.Fatalf("offer recreate: %v", err)
	}
	got := collectClaimableSeeds(t, r)
	if len(got) != 1 {
		t.Fatalf("emitted %d seeds, want 1 (an earlier removal suppressed a later live change)", len(got))
	}
	if got[0].Balance.Cmp(big.NewInt(777_000_000)) != 0 {
		t.Errorf("Balance = %s, want 777000000", got[0].Balance)
	}
}

// TestClaimableSeedReducer_RepeatedOfferIsIdempotent — a bisected retry
// re-reads a window whose stream already delivered part of its output, so the
// same rows are offered twice and superseded rows can arrive after their
// successor. The reduction must be a pure maximum.
func TestClaimableSeedReducer_RepeatedOfferIsIdempotent(t *testing.T) {
	id := cbID(0x61)
	ct := time.Date(2023, 6, 1, 0, 0, 0, 0, time.UTC)
	r := newCBReducer(t, nil)
	win := func() error {
		return r.offer(cbKeyXDR(t, id), cbEntryXDR(t, id, cbAsset4(t, "AQUA"), 2_000_000),
			"updated", ct, cbOrd(45_000_000, 1, "bb", 0, 1))
	}
	lose := func() error {
		return r.offer(cbKeyXDR(t, id), cbEntryXDR(t, id, cbAsset4(t, "AQUA"), 1_000_000),
			"created", ct, cbOrd(30_000_000, 1, "aa", 0, 1))
	}
	for _, o := range []func() error{win, lose, win, lose, win} {
		if err := o(); err != nil {
			t.Fatalf("offer: %v", err)
		}
	}
	got := collectClaimableSeeds(t, r)
	if len(got) != 1 {
		t.Fatalf("emitted %d seeds, want exactly 1 per key", len(got))
	}
	if got[0].Balance.Cmp(big.NewInt(2_000_000)) != 0 {
		t.Errorf("Balance = %s, want 2000000 (a re-offered superseded row displaced the winner)", got[0].Balance)
	}
}

// TestClaimableSeedReducer_RemovalPrunesLiveSet — the memory bound. Unlike the
// SAC seed, this reduction has no operator-curated watched set: it sees every
// claimable balance on the network, most of which have long since been
// claimed. A removal must therefore DROP the live record, not merely mark it,
// so the resident set tracks balances live at the walk position — which is the
// seed's own output cardinality.
func TestClaimableSeedReducer_RemovalPrunesLiveSet(t *testing.T) {
	id := cbID(0x71)
	r := newCBReducer(t, nil)
	if err := r.offer(cbKeyXDR(t, id), cbEntryXDR(t, id, cbAsset4(t, "AQUA"), 5),
		"created", time.Now().UTC(), cbOrd(40_000_000, 1, "aa", 0, 0)); err != nil {
		t.Fatalf("offer create: %v", err)
	}
	if len(r.live) != 1 {
		t.Fatalf("live set = %d after create, want 1", len(r.live))
	}
	if err := r.offer(cbKeyXDR(t, id), "", "removed", time.Now().UTC(), cbOrd(41_000_000, 1, "bb", 0, 0)); err != nil {
		t.Fatalf("offer removal: %v", err)
	}
	if len(r.live) != 0 {
		t.Errorf("live set = %d after the claim, want 0 (a claimed balance must not stay resident)", len(r.live))
	}
	if len(r.dead) != 1 {
		t.Errorf("tombstone set = %d, want 1 (the removal must still be able to reject a re-offered earlier create)", len(r.dead))
	}
}

// TestClaimableSeedReducer_StartWindowCompactsTombstones — the second half of
// the memory bound. A tombstone only exists to reject a change EARLIER than
// it; once the walk has moved past its ledger, no future offer can be earlier,
// so it is provably dead weight. Without the compaction the tombstone set
// would grow with every claim in chain history.
func TestClaimableSeedReducer_StartWindowCompactsTombstones(t *testing.T) {
	oldID, recentID := cbID(0x81), cbID(0x82)
	r := newCBReducer(t, nil)
	if err := r.offer(cbKeyXDR(t, oldID), "", "removed", time.Now().UTC(), cbOrd(10_000_000, 1, "aa", 0, 0)); err != nil {
		t.Fatalf("offer old removal: %v", err)
	}
	if err := r.offer(cbKeyXDR(t, recentID), "", "removed", time.Now().UTC(), cbOrd(20_000_000, 1, "aa", 0, 0)); err != nil {
		t.Fatalf("offer recent removal: %v", err)
	}
	if err := r.startWindow(15_000_000); err != nil {
		t.Fatalf("startWindow: %v", err)
	}
	if len(r.dead) != 1 {
		t.Fatalf("tombstones after compaction = %d, want 1 (only the one at/above the window start)", len(r.dead))
	}
	if _, kept := r.dead[recentID]; !kept {
		t.Error("compaction dropped the tombstone at/above the window start — a re-offered earlier create would resurrect a claimed balance")
	}

	// A compacted tombstone cannot change any outcome: anything offered from
	// here on is at ledger >= the window start and therefore strictly later.
	if err := r.offer(cbKeyXDR(t, oldID), cbEntryXDR(t, oldID, cbAsset4(t, "AQUA"), 3),
		"created", time.Now().UTC(), cbOrd(16_000_000, 1, "aa", 0, 0)); err != nil {
		t.Fatalf("offer post-compaction: %v", err)
	}
	if got := collectClaimableSeeds(t, r); len(got) != 1 {
		t.Fatalf("emitted %d seeds, want 1 (a later create must win over a compacted earlier removal)", len(got))
	}
}

// TestClaimableSeedReducer_StartWindowRejectsBacktracking — the compaction
// above is only sound while windows are walked in non-decreasing ledger order
// (the bisection retries the SAME start, which is allowed). Make the
// precondition machine-checked rather than a comment: a caller that ever walks
// windows out of order gets an error instead of silently resurrected balances.
func TestClaimableSeedReducer_StartWindowRejectsBacktracking(t *testing.T) {
	r := newClaimableSeedReducer(nil)
	if err := r.startWindow(20_000_000); err != nil {
		t.Fatalf("startWindow(20M): %v", err)
	}
	if err := r.startWindow(20_000_000); err != nil {
		t.Fatalf("startWindow re-declaring the same start (the bisection retry) must be allowed: %v", err)
	}
	if err := r.startWindow(19_999_999); err == nil {
		t.Fatal("startWindow going backwards must be rejected — tombstone compaction is only sound in non-decreasing ledger order")
	}
}

// ─── error contract ─────────────────────────────────────────────────────

// TestClaimableSeedReducer_CorruptKey preserves the SAC seed's error contract:
// a corrupt key on a LIVE change is lake corruption worth failing the seed
// for, while a corrupt key on a REMOVED change identifies no balance and held
// nothing.
func TestClaimableSeedReducer_CorruptKey(t *testing.T) {
	t.Run("live change errors", func(t *testing.T) {
		r := newCBReducer(t, nil)
		if err := r.offer("not-base64-xdr!", "", "updated", time.Now().UTC(), cbOrd(cbLedger, 1, "aa", 0, 1)); err == nil {
			t.Fatal("expected an error for a corrupt key_xdr on a live change")
		}
	})
	t.Run("removed change is skipped", func(t *testing.T) {
		r := newCBReducer(t, nil)
		if err := r.offer("not-base64-xdr!", "", "removed", time.Now().UTC(), cbOrd(cbLedger, 1, "aa", 0, 1)); err != nil {
			t.Fatalf("corrupt key on a removed change should be skipped, got: %v", err)
		}
		if n := len(r.live) + len(r.dead); n != 0 {
			t.Errorf("reducer retained %d entry/ies for an undecodable removed change", n)
		}
	})
}

// TestClaimableSeedReducer_CorruptEntryDeferredToEmit — corrupt XDR on a
// SUPERSEDED change is not the seed's problem; on the SURVIVING change it is a
// hard error. Dropping the latter silently would report "this balance holds
// nothing", which is the exact under-count this seed exists to fix.
func TestClaimableSeedReducer_CorruptEntryDeferredToEmit(t *testing.T) {
	t.Run("superseded corruption is tolerated", func(t *testing.T) {
		id := cbID(0x91)
		r := newCBReducer(t, nil)
		if err := r.offer(cbKeyXDR(t, id), "not-base64-xdr!", "created", time.Now().UTC(), cbOrd(40_000_000, 1, "aa", 0, 0)); err != nil {
			t.Fatalf("offer must defer the decode error, got: %v", err)
		}
		if err := r.offer(cbKeyXDR(t, id), cbEntryXDR(t, id, cbAsset4(t, "AQUA"), 11), "updated",
			time.Now().UTC(), cbOrd(41_000_000, 1, "bb", 0, 0)); err != nil {
			t.Fatalf("offer: %v", err)
		}
		got := collectClaimableSeeds(t, r)
		if len(got) != 1 || got[0].Balance.Cmp(big.NewInt(11)) != 0 {
			t.Fatalf("got %+v, want the single later good row (balance 11)", got)
		}
	})
	t.Run("surviving corruption fails the seed", func(t *testing.T) {
		id := cbID(0x92)
		r := newCBReducer(t, nil)
		if err := r.offer(cbKeyXDR(t, id), "not-base64-xdr!", "created", time.Now().UTC(), cbOrd(40_000_000, 1, "aa", 0, 0)); err != nil {
			t.Fatalf("offer must defer the decode error, got: %v", err)
		}
		if err := r.emit(func(ClaimableBalanceSeed) error { return nil }); err == nil {
			t.Fatal("expected emit to fail on a surviving winner whose entry_xdr is corrupt")
		}
	})
	t.Run("live change with no entry_xdr fails the seed", func(t *testing.T) {
		id := cbID(0x93)
		r := newCBReducer(t, nil)
		if err := r.offer(cbKeyXDR(t, id), "", "created", time.Now().UTC(), cbOrd(40_000_000, 1, "aa", 0, 0)); err != nil {
			t.Fatalf("offer must defer, got: %v", err)
		}
		if err := r.emit(func(ClaimableBalanceSeed) error { return nil }); err == nil {
			t.Fatal("expected emit to fail: a created/updated row with no entry_xdr is a lake inconsistency, not an empty balance")
		}
	})
}

// ─── emit ───────────────────────────────────────────────────────────────

// TestClaimableSeedReducer_EmitIsSortedAndOncePerKey — one callback per
// surviving balance, in ascending claimable-id order, so a run's output (and
// any log tail of it) is reproducible even though the reduction happens in a
// Go map.
func TestClaimableSeedReducer_EmitIsSortedAndOncePerKey(t *testing.T) {
	r := newCBReducer(t, nil)
	tags := []byte{0xE1, 0x02, 0x7F, 0xB0}
	for i, tag := range tags {
		id := cbID(tag)
		// Offer each key twice, at different ledgers, to prove emit is
		// per-BALANCE and not per-offer.
		for _, ledger := range []uint32{cbLedger - 1, cbLedger} {
			if err := r.offer(cbKeyXDR(t, id), cbEntryXDR(t, id, cbAsset4(t, "AQUA"), int64(i+1)),
				"updated", time.Now().UTC(), cbOrd(ledger, 1, "aa", 0, 1)); err != nil {
				t.Fatalf("offer: %v", err)
			}
		}
	}
	got := collectClaimableSeeds(t, r)
	if len(got) != len(tags) {
		t.Fatalf("emitted %d seeds for %d distinct balances — want exactly one per balance", len(got), len(tags))
	}
	var last string
	for _, s := range got {
		if last != "" && s.ClaimableID <= last {
			t.Errorf("emit order not strictly ascending by claimable_id: %q after %q", s.ClaimableID, last)
		}
		last = s.ClaimableID
	}
}

// TestClaimableIDFromKeyXDR — the map key must be the balance id itself, and
// its hex form must round-trip to what the live observer writes.
func TestClaimableIDFromKeyXDR(t *testing.T) {
	want := cbID(0xA5)
	got, ok, err := claimableIDFromKeyXDR(cbKeyXDR(t, want))
	if err != nil || !ok {
		t.Fatalf("claimableIDFromKeyXDR: ok=%v err=%v", ok, err)
	}
	if !bytes.Equal(got[:], want[:]) {
		t.Errorf("id = %x, want %x", got, want)
	}

	// A non-claimable key is skipped, not an error — defensive only, since the
	// SQL already scopes to entry_type='claimable_balance'.
	other, err := xdr.MarshalBase64(xdr.LedgerKey{
		Type:    xdr.LedgerEntryTypeAccount,
		Account: &xdr.LedgerKeyAccount{AccountId: xdr.MustAddress(cbIssuer)},
	})
	if err != nil {
		t.Fatalf("MarshalBase64 account key: %v", err)
	}
	if _, ok, err := claimableIDFromKeyXDR(other); err != nil || ok {
		t.Errorf("non-claimable key: ok=%v err=%v, want ok=false err=nil", ok, err)
	}
}

// TestClaimableSeedWindowConstants pins the window-adaptation policy that the
// first r1 dry-run (2026-07-27) proved wrong. That run bisected from 250,000
// all the way to the then-floor of 15,625 ledgers and STILL exceeded the
// ClickHouse memory ceiling at [40,484,378, 40,500,002] — an airdrop-era range
// where a few thousand ledgers mint millions of distinct claimable balances.
// The floor's original premise ("below this the window holds a few thousand
// keys") does not hold anywhere on this chain, so the floor must be low enough
// to survive the densest range, and the walk must be able to recover its width
// afterwards instead of crawling the remaining ~23M ledgers at the floor.
func TestClaimableSeedWindowConstants(t *testing.T) {
	if claimableSeedMinLedgerWindow >= 15_625 {
		t.Fatalf("bisection floor %d is at or above the width that demonstrably OOM'd on r1 (15,625) — a dense range would surface an error instead of narrowing",
			claimableSeedMinLedgerWindow)
	}
	if claimableSeedMinLedgerWindow < 1 {
		t.Fatalf("bisection floor must be positive, got %d", claimableSeedMinLedgerWindow)
	}
	if claimableSeedMinLedgerWindow > claimableSeedLedgerWindow {
		t.Fatalf("floor %d exceeds the initial window %d", claimableSeedMinLedgerWindow, claimableSeedLedgerWindow)
	}
	if claimableSeedWidenAfter < 1 {
		t.Fatalf("widen-after must be positive or the window can only ever narrow, got %d", claimableSeedWidenAfter)
	}
	// Halving from the initial width must actually reach the floor, otherwise
	// the bisection stalls above it and the dense-range failure returns.
	w := uint32(claimableSeedLedgerWindow)
	for w > claimableSeedMinLedgerWindow {
		w /= 2
	}
	if w < 1 {
		t.Fatalf("repeated halving of %d underflows before reaching the floor %d",
			claimableSeedLedgerWindow, claimableSeedMinLedgerWindow)
	}
}

// TestClaimableSeedWindowPolicy exercises the narrow/widen state machine
// directly: it must reach the floor from the initial width, must not narrow
// below it, and must recover toward the initial width after sustained success
// so one dense range cannot pin the rest of the walk at the floor.
func TestClaimableSeedWindowPolicy(t *testing.T) {
	var w claimableSeedWindow
	w.reset()
	if w.width != claimableSeedLedgerWindow {
		t.Fatalf("reset width = %d, want %d", w.width, claimableSeedLedgerWindow)
	}

	// Narrow until it refuses: must land exactly on the floor.
	for w.canNarrow() {
		w.narrow()
	}
	if w.width != claimableSeedMinLedgerWindow {
		t.Fatalf("narrowed to %d, want the floor %d", w.width, claimableSeedMinLedgerWindow)
	}
	w.narrow() // a narrow past the floor must clamp, never underflow
	if w.width != claimableSeedMinLedgerWindow {
		t.Fatalf("narrow past the floor gave %d, want it clamped at %d", w.width, claimableSeedMinLedgerWindow)
	}

	// A single clean window must NOT widen (that would oscillate straight
	// back into the range that just failed).
	w.succeeded()
	if w.width != claimableSeedMinLedgerWindow {
		t.Fatalf("widened after 1 clean window (%d) — widening must require %d", w.width, claimableSeedWidenAfter)
	}

	// Sustained success recovers the width, capped at the initial value.
	for i := 0; i < claimableSeedWidenAfter*64; i++ {
		w.succeeded()
	}
	if w.width != claimableSeedLedgerWindow {
		t.Fatalf("after sustained success width = %d, want it recovered to %d", w.width, claimableSeedLedgerWindow)
	}
	w.succeeded() // must stay capped
	if w.width != claimableSeedLedgerWindow {
		t.Fatalf("width %d exceeded the initial cap %d", w.width, claimableSeedLedgerWindow)
	}
}
