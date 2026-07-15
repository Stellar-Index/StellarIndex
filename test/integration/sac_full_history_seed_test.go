//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestSACFullHistorySeed_RecoversDormantPoolHolder reproduces the exact
// PHO/BLND VERDICT shape (incident 2026-07-06, docs/architecture/
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
	if err := chstore.StreamSACBalanceSeedsFullHistory(ctx, addr, watched, func(seed chstore.SACBalanceSeed) error {
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
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	watched := map[string]string{sac: asset}
	var got *chstore.SACBalanceSeed
	if err := chstore.StreamSACBalanceSeedsFullHistory(ctx, addr, watched, func(seed chstore.SACBalanceSeed) error {
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
