package clickhouse

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

const (
	// Valid C-strkey (zero contract id) + G-strkey holder, generated at
	// test-design time so the fixtures don't depend on encoding helpers
	// (matches the sac_balances dispatcher-adapter test constants).
	seedSAC    = "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABSC4"
	seedHolder = "GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAWHF"
	seedAsset  = "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	otherSAC   = "CBZ7M5B3Y4WWBZ5XK5UZCAFOEZ23KSSZXYECYX3IXM6E2JOLQC52DK32"
	seedLedger = uint32(62_400_123)
)

func seedWatched() map[string]string { return map[string]string{seedSAC: seedAsset} }

func mustContractScAddr(t *testing.T, cAddr string) xdr.ScAddress {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteContract, cAddr)
	if err != nil {
		t.Fatalf("strkey.Decode(%q): %v", cAddr, err)
	}
	var cid [32]byte
	copy(cid[:], raw)
	return xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: (*xdr.ContractId)(&cid)}
}

func seedBalanceKey(t *testing.T, holder string) xdr.ScVal {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteAccountID, holder)
	if err != nil {
		t.Fatalf("strkey.Decode(%q): %v", holder, err)
	}
	var pk [32]byte
	copy(pk[:], raw)
	aid := xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: (*xdr.Uint256)(&pk)}
	addr := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &aid}
	addrSV := xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &addr}
	sym := xdr.ScSymbol("Balance")
	symSV := xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym}
	vec := xdr.ScVec{symSV, addrSV}
	vp := &vec
	return xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &vp}
}

func seedI128Val(amount *big.Int) xdr.ScVal {
	// Split a (possibly > 2^63) non-negative big.Int into hi/lo i128 parts.
	lo := new(big.Int).And(amount, new(big.Int).SetUint64(^uint64(0)))
	hi := new(big.Int).Rsh(amount, 64)
	return xdr.ScVal{
		Type: xdr.ScValTypeScvI128,
		I128: &xdr.Int128Parts{Hi: xdr.Int64(hi.Int64()), Lo: xdr.Uint64(lo.Uint64())},
	}
}

func seedMapVal(amount int64) xdr.ScVal {
	amtSV := seedI128Val(big.NewInt(amount))
	amtSym := xdr.ScSymbol("amount")
	authSym := xdr.ScSymbol("authorized")
	trueB := true
	m := xdr.ScMap{
		{Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &amtSym}, Val: amtSV},
		{Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &authSym}, Val: xdr.ScVal{Type: xdr.ScValTypeScvBool, B: &trueB}},
	}
	mp := &m
	return xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &mp}
}

// mustKeyXDR builds the base64 LedgerKey (the shape ledger_entries_current
// stores in key_xdr) for a ContractData Balance entry.
func mustKeyXDR(t *testing.T, contract xdr.ScAddress, key xdr.ScVal) string {
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

// mustEntryXDR builds the base64 LedgerEntry (the shape stored in
// entry_xdr) for a ContractData Balance entry with the supplied value.
func mustEntryXDR(t *testing.T, contract xdr.ScAddress, key, val xdr.ScVal, lastMod uint32) string {
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

// TestSACBalanceSeedFromRow_I128Val — the common shape: a watched SAC
// wrapper's Balance(Address) entry with a bare i128 value decodes to the
// right (holder, amount, ledger). Uses a value > 2^63 to prove the i128
// hi bits survive (ADR-0003 — never truncated to int64).
func TestSACBalanceSeedFromRow_I128Val(t *testing.T) {
	contract := mustContractScAddr(t, seedSAC)
	key := seedBalanceKey(t, seedHolder)
	// 12_345_678_901_234_567_890 > math.MaxInt64 (9.22e18).
	want, _ := new(big.Int).SetString("12345678901234567890", 10)
	entryXDR := mustEntryXDR(t, contract, key, seedI128Val(want), seedLedger)
	keyXDR := mustKeyXDR(t, contract, key)
	closeTime := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)

	seed, matched, err := sacBalanceSeedFromRow(keyXDR, entryXDR, "updated", seedLedger, closeTime, seedWatched())
	if err != nil {
		t.Fatalf("sacBalanceSeedFromRow: %v", err)
	}
	if !matched {
		t.Fatal("matched=false, want true for a watched SAC Balance entry")
	}
	if seed.ContractID != seedSAC {
		t.Errorf("ContractID=%q want %q", seed.ContractID, seedSAC)
	}
	if seed.AssetKey != seedAsset {
		t.Errorf("AssetKey=%q want %q", seed.AssetKey, seedAsset)
	}
	if seed.Holder != seedHolder {
		t.Errorf("Holder=%q want %q", seed.Holder, seedHolder)
	}
	if seed.Balance.Cmp(want) != 0 {
		t.Errorf("Balance=%s want %s (i128 hi bits truncated?)", seed.Balance, want)
	}
	if seed.LedgerSeq != seedLedger {
		t.Errorf("LedgerSeq=%d want %d", seed.LedgerSeq, seedLedger)
	}
	if !seed.CloseTime.Equal(closeTime) {
		t.Errorf("CloseTime=%v want %v", seed.CloseTime, closeTime)
	}
}

// TestSACBalanceSeedFromRow_MapVal — the native SAC BalanceValue map
// shape ({amount, authorized, ...}) decodes its `amount` field.
func TestSACBalanceSeedFromRow_MapVal(t *testing.T) {
	contract := mustContractScAddr(t, seedSAC)
	key := seedBalanceKey(t, seedHolder)
	entryXDR := mustEntryXDR(t, contract, key, seedMapVal(599_880_000_000_000), seedLedger)
	keyXDR := mustKeyXDR(t, contract, key)

	seed, matched, err := sacBalanceSeedFromRow(keyXDR, entryXDR, "updated", seedLedger, time.Now().UTC(), seedWatched())
	if err != nil {
		t.Fatalf("sacBalanceSeedFromRow: %v", err)
	}
	if !matched {
		t.Fatal("matched=false, want true for a map-shaped BalanceValue")
	}
	if seed.Balance.Cmp(big.NewInt(599_880_000_000_000)) != 0 {
		t.Errorf("Balance=%s want 599880000000000 (from BalanceValue map)", seed.Balance)
	}
}

// TestSACBalanceSeedFromRow_NonBalanceKeySkipped — a watched contract's
// non-Balance storage key (e.g. Allowance / metadata) is skipped with no
// error, not mis-seeded.
func TestSACBalanceSeedFromRow_NonBalanceKeySkipped(t *testing.T) {
	contract := mustContractScAddr(t, seedSAC)
	wrongSym := xdr.ScSymbol("Allowance")
	wrongVec := xdr.ScVec{{Type: xdr.ScValTypeScvSymbol, Sym: &wrongSym}}
	wp := &wrongVec
	wrongKey := xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &wp}
	keyXDR := mustKeyXDR(t, contract, wrongKey)
	entryXDR := mustEntryXDR(t, contract, wrongKey, seedI128Val(big.NewInt(1)), seedLedger)

	seed, matched, err := sacBalanceSeedFromRow(keyXDR, entryXDR, "updated", seedLedger, time.Now().UTC(), seedWatched())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if matched {
		t.Errorf("matched=true for an Allowance key; want skip (seed=%+v)", seed)
	}
}

// TestSACBalanceSeedFromRow_WrongContractSkipped — a Balance entry of a
// contract NOT in the watched set is skipped (the watched-set filter
// that SQL can't apply because there's no contract_id column).
func TestSACBalanceSeedFromRow_WrongContractSkipped(t *testing.T) {
	contract := mustContractScAddr(t, otherSAC) // not in seedWatched()
	key := seedBalanceKey(t, seedHolder)
	keyXDR := mustKeyXDR(t, contract, key)
	entryXDR := mustEntryXDR(t, contract, key, seedI128Val(big.NewInt(42)), seedLedger)

	_, matched, err := sacBalanceSeedFromRow(keyXDR, entryXDR, "updated", seedLedger, time.Now().UTC(), seedWatched())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if matched {
		t.Error("matched=true for an unwatched contract; want skip")
	}
}

// TestSACBalanceSeedFromRow_RemovedSkipped — a removed current-state row
// (holder's balance entry deleted) is skipped: it holds nothing.
func TestSACBalanceSeedFromRow_RemovedSkipped(t *testing.T) {
	contract := mustContractScAddr(t, seedSAC)
	key := seedBalanceKey(t, seedHolder)
	keyXDR := mustKeyXDR(t, contract, key)

	_, matched, err := sacBalanceSeedFromRow(keyXDR, "", "removed", seedLedger, time.Now().UTC(), seedWatched())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if matched {
		t.Error("matched=true for a removed entry; want skip")
	}
}

// TestSACBalanceSeedFromRow_CorruptEntryErrors — a watched Balance key
// whose entry_xdr is corrupt is a HARD error (the caller is about to
// persist into the served tier; silently dropping it would masquerade as
// "holder holds nothing" — the exact under-count this seed fixes).
func TestSACBalanceSeedFromRow_CorruptEntryErrors(t *testing.T) {
	contract := mustContractScAddr(t, seedSAC)
	key := seedBalanceKey(t, seedHolder)
	keyXDR := mustKeyXDR(t, contract, key)

	if _, _, err := sacBalanceSeedFromRow(keyXDR, "not-base64-xdr!", "updated", seedLedger, time.Now().UTC(), seedWatched()); err == nil {
		t.Fatal("expected error for corrupt entry_xdr on a watched Balance entry")
	}
}

// ─── full-history windowed reduction (incident 2026-07-27) ───────────────
//
// StreamSACBalanceSeedsFullHistory now bounds the ClickHouse GROUP BY to one
// ledger window at a time and finishes the latest-write-wins reduction in Go.
// These exercise that Go half: it must reproduce, ACROSS windows, exactly the
// ordering the server-side argMax applies WITHIN one — in particular the
// audit-2026-07-16 C2-4 within-ledger tie-break, which is what stops a deleted
// balance being resurrected into the supply seed.

func seedOrd(ledger, intra uint32, tx string, op int32, changeIdx uint32) lakeEntryChangeOrder {
	return lakeEntryChangeOrder{ledgerSeq: ledger, intraLedgerSeq: intra, txHash: tx, opIndex: op, changeIndex: changeIdx}
}

// seedEntryFor builds the entry_xdr for the standard test holder's Balance
// entry at the given amount.
func seedEntryFor(t *testing.T, amount int64, lastMod uint32) string {
	t.Helper()
	contract := mustContractScAddr(t, seedSAC)
	return mustEntryXDR(t, contract, seedBalanceKey(t, seedHolder), seedI128Val(big.NewInt(amount)), lastMod)
}

// seedKeyFor builds the key_xdr for the standard test holder's Balance entry.
func seedKeyFor(t *testing.T) string {
	t.Helper()
	contract := mustContractScAddr(t, seedSAC)
	return mustKeyXDR(t, contract, seedBalanceKey(t, seedHolder))
}

func collectSeeds(t *testing.T, r *sacSeedReducer) []SACBalanceSeed {
	t.Helper()
	var got []SACBalanceSeed
	if err := r.emit(func(s SACBalanceSeed) error { got = append(got, s); return nil }); err != nil {
		t.Fatalf("emit: %v", err)
	}
	return got
}

// TestSACSeedOrderAfter compares the SAME five-element identity tuple the
// server-side argMax orders on, element by element. If Go and ClickHouse
// disagree on ANY element, a cross-window winner differs from a within-window
// one and the reduction is silently wrong.
func TestSACSeedOrderAfter(t *testing.T) {
	base := seedOrd(50_000_000, 7, "bb", 2, 3)
	cases := []struct {
		name string
		a, b lakeEntryChangeOrder
		want bool
	}{
		{"identical is not after", base, base, false},
		{"higher ledger wins", seedOrd(50_000_001, 0, "aa", 0, 0), base, true},
		{"lower ledger loses", seedOrd(49_999_999, 9999, "zz", 99, 99), base, false},
		// C2-4: ledger_seq alone does NOT order intra-ledger changes.
		{"same ledger, higher intra_ledger_seq wins", seedOrd(50_000_000, 8, "aa", 0, 0), base, true},
		{"same ledger, lower intra_ledger_seq loses", seedOrd(50_000_000, 6, "zz", 99, 99), base, false},
		// Legacy / pre-C2-4c rows carry intra_ledger_seq = 0, so the tuple
		// must fall through to (tx_hash, op_index, change_index).
		{"intra tie, higher tx_hash wins", seedOrd(50_000_000, 0, "bb", 0, 0), seedOrd(50_000_000, 0, "aa", 9, 9), true},
		{"intra+tx tie, higher op_index wins", seedOrd(50_000_000, 0, "aa", 1, 0), seedOrd(50_000_000, 0, "aa", 0, 9), true},
		{"intra+tx+op tie, higher change_index wins", seedOrd(50_000_000, 0, "aa", 0, 2), seedOrd(50_000_000, 0, "aa", 0, 1), true},
		{"intra+tx+op tie, lower change_index loses", seedOrd(50_000_000, 0, "aa", 0, 1), seedOrd(50_000_000, 0, "aa", 0, 2), false},
		// op_index is signed on both sides: -1 is the fee-meta / tx-level
		// sentinel and must order BELOW op 0, not above it as a uint would.
		{"fee-meta op -1 loses to op 0", seedOrd(50_000_000, 0, "aa", -1, 9), seedOrd(50_000_000, 0, "aa", 0, 0), false},
		{"op 0 beats fee-meta op -1", seedOrd(50_000_000, 0, "aa", 0, 0), seedOrd(50_000_000, 0, "aa", -1, 9), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.a.after(c.b); got != c.want {
				t.Errorf("after() = %v, want %v (a=%+v b=%+v)", got, c.want, c.a, c.b)
			}
		})
	}
}

// TestSACSeedReducer_LatestLedgerWins — the ordinary cross-window case: the
// same storage key written in two different ledger windows resolves to the
// higher ledger, whichever order the windows are offered in.
func TestSACSeedReducer_LatestLedgerWins(t *testing.T) {
	keyXDR := seedKeyFor(t)
	old := time.Date(2021, 6, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2023, 6, 1, 0, 0, 0, 0, time.UTC)

	for _, order := range []string{"ascending", "descending"} {
		t.Run(order, func(t *testing.T) {
			r := newSACSeedReducer(seedWatched())
			offers := []func() error{
				func() error {
					return r.offer(keyXDR, seedEntryFor(t, 1_000_000, 30_000_000), "created", old, seedOrd(30_000_000, 1, "aa", 0, 1))
				},
				func() error {
					return r.offer(keyXDR, seedEntryFor(t, 2_000_000, 45_000_000), "updated", recent, seedOrd(45_000_000, 1, "bb", 0, 1))
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
			got := collectSeeds(t, r)
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
				t.Errorf("CloseTime = %v, want %v (winner's columns must travel together)", got[0].CloseTime, recent)
			}
		})
	}
}

// TestSACSeedReducer_SameLedgerRemovalWins is the C2-4 case, now on the Go
// side: one key changed TWICE in ONE ledger — live at the lower
// intra_ledger_seq, removed at the higher. The removal is the genuine latest
// state, so nothing may be emitted. Getting this backwards resurrects a
// deleted balance into the SAC supply seed (the exact 2026-07-16 finding).
func TestSACSeedReducer_SameLedgerRemovalWins(t *testing.T) {
	keyXDR := seedKeyFor(t)
	ct := time.Date(2022, 6, 1, 0, 0, 0, 0, time.UTC)
	const ledger = uint32(50_000_000)

	// Offer the removal FIRST so a naive "last offer wins" implementation
	// would emit the stale live row and fail this test.
	r := newSACSeedReducer(seedWatched())
	if err := r.offer(keyXDR, "", "removed", ct, seedOrd(ledger, 42, "aa", 1, 1)); err != nil {
		t.Fatalf("offer removal: %v", err)
	}
	if err := r.offer(keyXDR, seedEntryFor(t, 100_000_000, ledger), "updated", ct, seedOrd(ledger, 41, "aa", 0, 1)); err != nil {
		t.Fatalf("offer live: %v", err)
	}
	if got := collectSeeds(t, r); len(got) != 0 {
		t.Errorf("emitted %d seed(s) for a key whose latest same-ledger change is 'removed' — the deleted balance was RESURRECTED: %+v", len(got), got)
	}
}

// TestSACSeedReducer_SameLedgerRemovalWinsOnChangeIndex is the same tie one
// element deeper: legacy rows carry intra_ledger_seq = 0 and both changes can
// share a tx and an op, so change_index is the only discriminator left.
func TestSACSeedReducer_SameLedgerRemovalWinsOnChangeIndex(t *testing.T) {
	keyXDR := seedKeyFor(t)
	ct := time.Date(2022, 6, 1, 0, 0, 0, 0, time.UTC)
	const ledger = uint32(50_000_000)

	r := newSACSeedReducer(seedWatched())
	if err := r.offer(keyXDR, seedEntryFor(t, 100_000_000, ledger), "updated", ct, seedOrd(ledger, 0, "aa", 0, 1)); err != nil {
		t.Fatalf("offer live: %v", err)
	}
	if err := r.offer(keyXDR, "", "removed", ct, seedOrd(ledger, 0, "aa", 0, 2)); err != nil {
		t.Fatalf("offer removal: %v", err)
	}
	if got := collectSeeds(t, r); len(got) != 0 {
		t.Errorf("emitted %d seed(s); the change_index=2 removal is the latest change: %+v", len(got), got)
	}
}

// TestSACSeedReducer_RecreateAfterRemovalWins is the positive complement, and
// the case the pre-windowing reader could not express at all: a removal in an
// EARLIER window must not suppress a re-create in a LATER one.
func TestSACSeedReducer_RecreateAfterRemovalWins(t *testing.T) {
	keyXDR := seedKeyFor(t)
	r := newSACSeedReducer(seedWatched())
	if err := r.offer(keyXDR, "", "removed", time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC), seedOrd(40_000_000, 3, "aa", 0, 1)); err != nil {
		t.Fatalf("offer removal: %v", err)
	}
	if err := r.offer(keyXDR, seedEntryFor(t, 777_000_000, 45_000_000), "created",
		time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC), seedOrd(45_000_000, 4, "bb", 0, 1)); err != nil {
		t.Fatalf("offer recreate: %v", err)
	}
	got := collectSeeds(t, r)
	if len(got) != 1 {
		t.Fatalf("emitted %d seeds, want 1 (an earlier-window removal suppressed a later re-create)", len(got))
	}
	if got[0].Balance.Cmp(big.NewInt(777_000_000)) != 0 {
		t.Errorf("Balance = %s, want 777000000", got[0].Balance)
	}
}

// TestSACSeedReducer_RepeatedOfferIsIdempotent — a bisected retry re-reads a
// window whose stream already delivered part of its output, so the same rows
// are offered twice (and superseded rows can arrive after their successor).
// The reduction must be a pure maximum.
func TestSACSeedReducer_RepeatedOfferIsIdempotent(t *testing.T) {
	keyXDR := seedKeyFor(t)
	ct := time.Date(2023, 6, 1, 0, 0, 0, 0, time.UTC)
	win := func(r *sacSeedReducer) error {
		return r.offer(keyXDR, seedEntryFor(t, 2_000_000, 45_000_000), "updated", ct, seedOrd(45_000_000, 1, "bb", 0, 1))
	}
	lose := func(r *sacSeedReducer) error {
		return r.offer(keyXDR, seedEntryFor(t, 1_000_000, 30_000_000), "created", ct, seedOrd(30_000_000, 1, "aa", 0, 1))
	}
	r := newSACSeedReducer(seedWatched())
	for _, o := range []func(*sacSeedReducer) error{win, lose, win, lose, win} {
		if err := o(r); err != nil {
			t.Fatalf("offer: %v", err)
		}
	}
	got := collectSeeds(t, r)
	if len(got) != 1 {
		t.Fatalf("emitted %d seeds, want exactly 1 per key", len(got))
	}
	if got[0].Balance.Cmp(big.NewInt(2_000_000)) != 0 {
		t.Errorf("Balance = %s, want 2000000 (a re-offered superseded row displaced the winner)", got[0].Balance)
	}
}

// TestSACSeedReducer_DropsKeysTheBytePrefilterLetsThrough — the SQL prefilter
// is a raw-byte multiSearchAny, so it admits ANY contract_data key that merely
// mentions a watched contract (allowances, another contract's storage). Those
// must be rejected on sight, never retained: the reducer's memory bound is the
// whole point of the windowed rewrite.
func TestSACSeedReducer_DropsKeysTheBytePrefilterLetsThrough(t *testing.T) {
	watchedContract := mustContractScAddr(t, seedSAC)
	allowanceSym := xdr.ScSymbol("Allowance")
	allowanceVec := xdr.ScVec{{Type: xdr.ScValTypeScvSymbol, Sym: &allowanceSym}}
	avp := &allowanceVec
	allowanceKey := xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &avp}

	unwatchedContract := mustContractScAddr(t, otherSAC)
	balanceKey := seedBalanceKey(t, seedHolder)

	r := newSACSeedReducer(seedWatched())
	rows := []struct{ keyXDR, entryXDR string }{
		{mustKeyXDR(t, watchedContract, allowanceKey), mustEntryXDR(t, watchedContract, allowanceKey, seedI128Val(big.NewInt(1)), seedLedger)},
		{mustKeyXDR(t, unwatchedContract, balanceKey), mustEntryXDR(t, unwatchedContract, balanceKey, seedI128Val(big.NewInt(2)), seedLedger)},
	}
	for _, row := range rows {
		if err := r.offer(row.keyXDR, row.entryXDR, "updated", time.Now().UTC(), seedOrd(seedLedger, 1, "aa", 0, 1)); err != nil {
			t.Fatalf("offer: %v", err)
		}
	}
	if n := len(r.best); n != 0 {
		t.Errorf("reducer retained %d non-seed key(s); the prefilter must reject them before they cost memory", n)
	}
	if got := collectSeeds(t, r); len(got) != 0 {
		t.Errorf("emitted %d seed(s) for allowance / unwatched-contract keys: %+v", len(got), got)
	}
}

// TestSACSeedReducer_CorruptKey preserves the pre-windowing error contract: a
// corrupt key on a LIVE change is lake corruption worth failing the seed for,
// while a corrupt key on a REMOVED change identifies no holder and held
// nothing — the old reader returned on change_type before decoding it at all.
func TestSACSeedReducer_CorruptKey(t *testing.T) {
	t.Run("live change errors", func(t *testing.T) {
		r := newSACSeedReducer(seedWatched())
		if err := r.offer("not-base64-xdr!", "", "updated", time.Now().UTC(), seedOrd(seedLedger, 1, "aa", 0, 1)); err == nil {
			t.Fatal("expected error for a corrupt key_xdr on a live change")
		}
	})
	t.Run("removed change is skipped", func(t *testing.T) {
		r := newSACSeedReducer(seedWatched())
		if err := r.offer("not-base64-xdr!", "", "removed", time.Now().UTC(), seedOrd(seedLedger, 1, "aa", 0, 1)); err != nil {
			t.Fatalf("corrupt key on a removed change should be skipped, got: %v", err)
		}
		if n := len(r.best); n != 0 {
			t.Errorf("reducer retained %d key(s) for an undecodable removed change", n)
		}
	})
}

// seedSyntheticHolder builds a checksum-valid, deterministic G-strkey from a
// single tag byte (mirrors the integration suite's fhSyntheticAccountAddr) so
// multi-holder tests don't need hand-typed strkeys.
func seedSyntheticHolder(t *testing.T, tag byte) string {
	t.Helper()
	var raw [32]byte
	raw[0] = tag
	s, err := strkey.Encode(strkey.VersionByteAccountID, raw[:])
	if err != nil {
		t.Fatalf("strkey.Encode(account, tag=%#x): %v", tag, err)
	}
	return s
}

// TestSACSeedReducer_EmitIsSortedAndOncePerKey — one callback per surviving
// key, in ascending key_xdr order, so a run's output is reproducible even
// though the reduction happens in a Go map.
func TestSACSeedReducer_EmitIsSortedAndOncePerKey(t *testing.T) {
	contract := mustContractScAddr(t, seedSAC)
	holders := []string{
		seedSyntheticHolder(t, 0xE1),
		seedSyntheticHolder(t, 0x02),
		seedHolder,
		seedSyntheticHolder(t, 0x7F),
	}
	r := newSACSeedReducer(seedWatched())
	wantKeys := make(map[string]bool, len(holders))
	for i, h := range holders {
		key := seedBalanceKey(t, h)
		keyXDR := mustKeyXDR(t, contract, key)
		wantKeys[keyXDR] = true
		entryXDR := mustEntryXDR(t, contract, key, seedI128Val(big.NewInt(int64(i+1))), seedLedger)
		// Offer each key twice, at different ledgers, to prove emit is
		// per-KEY and not per-offer.
		for _, ledger := range []uint32{seedLedger - 1, seedLedger} {
			if err := r.offer(keyXDR, entryXDR, "updated", time.Now().UTC(), seedOrd(ledger, 1, "aa", 0, 1)); err != nil {
				t.Fatalf("offer: %v", err)
			}
		}
	}

	got := collectSeeds(t, r)
	if len(got) != len(wantKeys) {
		t.Fatalf("emitted %d seeds for %d distinct keys — want exactly one per key", len(got), len(wantKeys))
	}
	var last string
	for _, s := range got {
		k := mustKeyXDR(t, contract, seedBalanceKey(t, s.Holder))
		if !wantKeys[k] {
			t.Errorf("emitted an unexpected key for holder %s", s.Holder)
		}
		if last != "" && k <= last {
			t.Errorf("emit order not strictly ascending by key_xdr: %q after %q", k, last)
		}
		last = k
	}
}

// TestSACWatchedContractNeedles renders one raw-byte literal per watched
// wrapper, sorted, so the generated SQL is deterministic run to run.
func TestSACWatchedContractNeedles(t *testing.T) {
	needles, err := sacWatchedContractNeedles(map[string]string{seedSAC: seedAsset, otherSAC: seedAsset})
	if err != nil {
		t.Fatalf("sacWatchedContractNeedles: %v", err)
	}
	if len(needles) != 2 {
		t.Fatalf("got %d needles, want 2: %v", len(needles), needles)
	}
	if needles[0] >= needles[1] {
		t.Errorf("needles not sorted: %v", needles)
	}
	for _, n := range needles {
		if !strings.HasPrefix(n, "unhex('") || !strings.HasSuffix(n, "')") {
			t.Errorf("needle %q is not a raw-byte unhex literal", n)
		}
	}
	if _, err := sacWatchedContractNeedles(map[string]string{"not-a-strkey": seedAsset}); err == nil {
		t.Error("expected an error for a watched key that is not a C-strkey")
	}
}

// TestIsMemoryLimitExceeded — only 241 may trigger a window bisection.
// Treating any exception as "just narrow the window" would silently retry
// through a genuine fault (the asymmetry isSchemaAbsent documents).
func TestIsMemoryLimitExceeded(t *testing.T) {
	if !isMemoryLimitExceeded(fmt.Errorf("wrapped: %w", &clickhouse.Exception{Code: 241, Name: "MEMORY_LIMIT_EXCEEDED"})) {
		t.Error("241 MEMORY_LIMIT_EXCEEDED not recognised through a wrapping error")
	}
	for _, code := range []int32{159, 202, 209, 241 + 1, 60} {
		if code == 241 {
			continue
		}
		if isMemoryLimitExceeded(&clickhouse.Exception{Code: code}) {
			t.Errorf("code %d must NOT trigger a window bisection", code)
		}
	}
	if isMemoryLimitExceeded(errors.New("plain error")) {
		t.Error("a non-ClickHouse error must not trigger a window bisection")
	}
}
