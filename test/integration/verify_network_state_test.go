//go:build integration

package integration_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/historyarchive"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/ops/chops"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// cap76Checkpoint is the first checkpoint after the CAP-0076 upgrade ledger.
const cap76Checkpoint = 59_501_311

// resetNetworkStateLake empties the tables these tests read, before and after,
// so other tests' fixture rows cannot enter the lumen sum or the lake tip.
func resetNetworkStateLake(t *testing.T, ctx context.Context) {
	t.Helper()
	raw := dialClickHouse(t, ctx, "stellar")
	truncate := func() {
		for _, table := range []string{"ledgers", "ledger_entry_changes", "ledger_entries_current"} {
			if err := raw.Exec(context.Background(), "TRUNCATE TABLE stellar."+table); err != nil {
				t.Errorf("truncate %s: %v", table, err)
			}
		}
	}
	truncate()
	t.Cleanup(truncate)
}

func insertNetworkStateLedger(t *testing.T, ctx context.Context, seq uint32, totalCoins, feePool int64) {
	t.Helper()
	raw := dialClickHouse(t, ctx, "stellar")
	if err := raw.Exec(ctx, `INSERT INTO stellar.ledgers
		(ledger_seq, close_time, ledger_hash, prev_hash, protocol_version, total_coins, fee_pool)
		VALUES (?, ?, ?, '00', 24, ?, ?)`,
		seq, time.Date(2025, 9, 1, 0, 0, 0, 0, time.UTC), fmt.Sprintf("%064d", seq), totalCoins, feePool); err != nil {
		t.Fatalf("insert ledger %d: %v", seq, err)
	}
}

// entryChange builds a ledger_entry_changes row in the extractor's encoding.
func entryChange(t *testing.T, seq, intra uint32, changeType string, e xdr.LedgerEntry) chstore.LedgerEntryChangeRow {
	t.Helper()
	k, err := e.LedgerKey()
	if err != nil {
		t.Fatalf("ledger key: %v", err)
	}
	entryType, key, err := chstore.LedgerKeyColumns(k)
	if err != nil {
		t.Fatal(err)
	}
	row := chstore.LedgerEntryChangeRow{
		LedgerSeq: seq, CloseTime: time.Date(2025, 9, 1, 0, 0, 0, 0, time.UTC), TxHash: "network-state",
		ChangeIndex: intra, IntraLedgerSeq: intra, ChangeType: changeType, EntryType: entryType, KeyXDR: key,
	}
	if changeType == "removed" {
		return row
	}
	if row.EntryXDR, err = xdr.MarshalBase64(e); err != nil {
		t.Fatal(err)
	}
	if a, ok := e.Data.GetAccount(); ok {
		row.Balance = int64(a.Balance)
	}
	return row
}

func insertEntryChanges(t *testing.T, ctx context.Context, rows ...chstore.LedgerEntryChangeRow) {
	t.Helper()
	if _, err := chstore.InsertEntryChanges(ctx, clickhouseAddr(t), rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}
}

func cap76Data(name string, val uint64) xdr.LedgerEntry {
	id := xdr.ContractId{0xca, 0x76, 0x1e}
	sym := xdr.ScSymbol(name)
	v := xdr.Uint64(val)
	return xdr.LedgerEntry{
		LastModifiedLedgerSeq: 59_000_000,
		Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeContractData,
			ContractData: &xdr.ContractDataEntry{
				Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &id},
				Key:        xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym},
				Durability: xdr.ContractDataDurabilityPersistent,
				Val:        xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: &v},
			},
		},
	}
}

// writeHotArchiveFixture lays out a file:// history archive whose checkpoint
// HAS carries one hot-archive bucket holding entries.
func writeHotArchiveFixture(t *testing.T, entries ...xdr.LedgerEntry) string {
	t.Helper()
	dir := t.TempDir()
	listType := xdr.BucketListTypeHotArchive
	var raw bytes.Buffer
	if err := xdr.MarshalFramed(&raw, xdr.HotArchiveBucketEntry{
		Type:      xdr.HotArchiveBucketEntryTypeHotArchiveMetaentry,
		MetaEntry: &xdr.BucketMetadata{LedgerVersion: 24, Ext: xdr.BucketMetadataExt{V: 1, BucketListType: &listType}},
	}); err != nil {
		t.Fatal(err)
	}
	for i := range entries {
		if err := xdr.MarshalFramed(&raw, xdr.HotArchiveBucketEntry{
			Type: xdr.HotArchiveBucketEntryTypeHotArchiveArchived, ArchivedEntry: &entries[i],
		}); err != nil {
			t.Fatal(err)
		}
	}
	hash := historyarchive.Hash(sha256.Sum256(raw.Bytes()))
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(dir, historyarchive.BucketPath(hash)), gz.Bytes())

	has := historyarchive.HistoryArchiveState{Version: 2, CurrentLedger: cap76Checkpoint, NetworkPassphrase: network.PublicNetworkPassphrase}
	zero := strings.Repeat("0", 64)
	for i := range has.HotArchiveBuckets {
		has.CurrentBuckets[i].Curr, has.CurrentBuckets[i].Snap = zero, zero
		has.HotArchiveBuckets[i].Curr, has.HotArchiveBuckets[i].Snap = zero, zero
	}
	has.HotArchiveBuckets[0].Curr = hex.EncodeToString(hash[:])
	body, err := json.Marshal(has)
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(dir, historyarchive.CategoryCheckpointPath("history", cap76Checkpoint)), body)
	writeFixtureFile(t, filepath.Join(dir, ".well-known", "stellar-history.json"), body)
	return "file://" + dir
}

func writeFixtureFile(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestVerifyNetworkState_CAP76AmendmentFailsOnAmendedKeys replays the
// CAP-0076 case end to end: the lake holds archived entries as last seen in
// ledger meta, the history archive's hot-archive bucket holds two of them
// amended. The verb must exit with exactly the two mismatches; an archive
// matching the lake must pass.
func TestVerifyNetworkState_CAP76AmendmentFailsOnAmendedKeys(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	resetNetworkStateLake(t, ctx)
	insertNetworkStateLedger(t, ctx, cap76Checkpoint, 0, 0)
	const evicted = 59_000_000
	insertEntryChanges(t, ctx,
		entryChange(t, evicted, 1, "updated", cap76Data("amended_a", 1)),
		entryChange(t, evicted, 2, "updated", cap76Data("amended_b", 2)),
		entryChange(t, evicted, 3, "updated", cap76Data("untouched", 3)),
	)

	cfgPath := filepath.Join(t.TempDir(), "stellarindex.toml")
	writeFixtureFile(t, cfgPath, []byte("[stellar]\nnetwork = \"pubnet\"\n"))
	run := func(archiveURL, textfile string) error {
		return chops.Run([]string{
			"verify-network-state", "-config", cfgPath, "-ch-addr", addr, "-archive", archiveURL,
			"-checks", "hotarchive", "-checkpoint", fmt.Sprint(cap76Checkpoint), "-textfile", textfile,
		})
	}

	amended := writeHotArchiveFixture(t, cap76Data("amended_a", 101), cap76Data("amended_b", 102), cap76Data("untouched", 3))
	prom := filepath.Join(t.TempDir(), "network_state_verify.prom")
	var exit *opsutil.ExitCodeError
	if err := run(amended, prom); !errors.As(err, &exit) || exit.Code != 2 {
		t.Fatalf("amended archive: err = %v, want exit code 2 (the two amended keys)", err)
	}
	body, err := os.ReadFile(prom)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`stellarindex_network_state_verify_failures{check="hot_archive"} 2`,
		`stellarindex_network_state_verify_hot_archive_entries{class="matched"} 1`,
		`stellarindex_network_state_verify_hot_archive_entries{class="mismatched"} 2`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("textfile missing %q:\n%s", want, body)
		}
	}

	clean := writeHotArchiveFixture(t, cap76Data("amended_a", 1), cap76Data("amended_b", 2), cap76Data("untouched", 3))
	if err := run(clean, filepath.Join(t.TempDir(), "clean.prom")); err != nil {
		t.Fatalf("archive matching the lake: %v", err)
	}
}

func lumenAccount(seed byte, balance int64) xdr.LedgerEntry {
	var pk xdr.Uint256
	pk[0] = seed
	id := xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: &pk}
	return xdr.LedgerEntry{Data: xdr.LedgerEntryData{
		Type:    xdr.LedgerEntryTypeAccount,
		Account: &xdr.AccountEntry{AccountId: id, Balance: xdr.Int64(balance)},
	}}
}

func lumenCB(seed byte, asset xdr.Asset, amount int64) xdr.LedgerEntry {
	return xdr.LedgerEntry{Data: xdr.LedgerEntryData{
		Type: xdr.LedgerEntryTypeClaimableBalance,
		ClaimableBalance: &xdr.ClaimableBalanceEntry{
			BalanceId: xdr.ClaimableBalanceId{Type: xdr.ClaimableBalanceIdTypeClaimableBalanceIdTypeV0, V0: &xdr.Hash{seed}},
			Asset:     asset,
			Amount:    xdr.Int64(amount),
		},
	}}
}

func lumenLP(native, credit xdr.Asset, reserveNative int64) xdr.LedgerEntry {
	return xdr.LedgerEntry{Data: xdr.LedgerEntryData{
		Type: xdr.LedgerEntryTypeLiquidityPool,
		LiquidityPool: &xdr.LiquidityPoolEntry{
			LiquidityPoolId: xdr.PoolId{0x1f},
			Body: xdr.LiquidityPoolEntryBody{
				Type: xdr.LiquidityPoolTypeLiquidityPoolConstantProduct,
				ConstantProduct: &xdr.LiquidityPoolEntryConstantProduct{
					Params:   xdr.LiquidityPoolConstantProductParameters{AssetA: native, AssetB: credit, Fee: 30},
					ReserveA: xdr.Int64(reserveNative), ReserveB: 999_999,
				},
			},
		},
	}}
}

func lumenSACBalance(sac xdr.ContractId, holder byte, amount int64) xdr.LedgerEntry {
	sym := xdr.ScSymbol("Balance")
	var pk xdr.Uint256
	pk[0] = holder
	acct := xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: &pk}
	vec := xdr.ScVec{
		{Type: xdr.ScValTypeScvSymbol, Sym: &sym},
		{Type: xdr.ScValTypeScvAddress, Address: &xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &acct}},
	}
	pv := &vec
	return xdr.LedgerEntry{Data: xdr.LedgerEntryData{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.ContractDataEntry{
			Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &sac},
			Key:        xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &pv},
			Durability: xdr.ContractDataDurabilityPersistent,
			Val:        xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &xdr.Int128Parts{Lo: xdr.Uint64(amount)}},
		},
	}}
}

// TestNetworkStateReader_LumenConservationAtCommittedTip proves the lumen
// tally against real ClickHouse: it sums every native holding domain as of
// the newest stellar.ledgers row and rolls back changes the sink wrote for a
// ledger whose header is not yet committed.
func TestNetworkStateReader_LumenConservationAtCommittedTip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	resetNetworkStateLake(t, ctx)

	sac := xdr.ContractId{0x5a, 0xc0}
	sacID, err := strkey.Encode(strkey.VersionByteContract, sac[:])
	if err != nil {
		t.Fatal(err)
	}
	native := xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	credit := xdr.MustNewCreditAsset("USDC", "GC3C4AKRBQLHOJ45U4XG35ESVWRDECWO5XLDGYADO6DPR3L7KIDVUMML")

	const tip = 100
	insertNetworkStateLedger(t, ctx, tip-1, 1_000, 200)
	insertNetworkStateLedger(t, ctx, tip, 1_000, 220)
	insertEntryChanges(t, ctx,
		entryChange(t, 90, 1, "created", lumenAccount(1, 600)),
		entryChange(t, 91, 1, "created", lumenAccount(3, 7)),
		entryChange(t, 99, 1, "removed", lumenAccount(3, 0)),
		entryChange(t, 95, 1, "created", lumenCB(1, native, 100)),
		entryChange(t, 95, 2, "created", lumenCB(2, credit, 5_000)),
		entryChange(t, 95, 3, "created", lumenLP(native, credit, 30)),
		entryChange(t, 95, 4, "created", lumenSACBalance(sac, 1, 50)),
		entryChange(t, 95, 5, "created", lumenSACBalance(xdr.ContractId{0x5a, 0xc1}, 1, 9_999)),
		// Ledger 101's changes are in the lake but its header is not.
		entryChange(t, tip+1, 1, "state", lumenAccount(1, 600)),
		entryChange(t, tip+1, 2, "updated", lumenAccount(1, 400)),
		entryChange(t, tip+1, 3, "created", lumenAccount(2, 200)),
		entryChange(t, tip+1, 4, "state", lumenCB(1, native, 100)),
		entryChange(t, tip+1, 5, "removed", lumenCB(1, native, 0)),
	)

	reader, err := chstore.NewNetworkStateReader(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	got, err := reader.LumenConservation(ctx, sacID)
	if err != nil {
		t.Fatalf("LumenConservation: %v", err)
	}
	if got.Ledger != tip || got.TotalCoins != 1_000 || got.FeePool != 220 || got.Preimages != 3 {
		t.Fatalf("header/preimages = (%d, %d, %d, %d), want (%d, 1000, 220, 3)", got.Ledger, got.TotalCoins, got.FeePool, got.Preimages, tip)
	}
	for _, c := range []struct {
		name string
		got  *big.Int
		want int64
	}{
		{"accounts", got.Accounts, 600},
		{"claimable_balances", got.ClaimableBalances, 100},
		{"liquidity_pools", got.LiquidityPools, 30},
		{"contract_balances", got.ContractBalances, 50},
	} {
		if c.got.Cmp(big.NewInt(c.want)) != 0 {
			t.Errorf("%s = %s, want %d", c.name, c.got, c.want)
		}
	}
	if r := got.Residual(); r.Sign() != 0 {
		t.Errorf("residual = %s, want 0", r)
	}

	// The same lake under a header claiming 5 more stroops exist must not conserve.
	insertNetworkStateLedger(t, ctx, tip, 1_005, 220)
	got, err = reader.LumenConservation(ctx, sacID)
	if err != nil {
		t.Fatalf("LumenConservation: %v", err)
	}
	if r := got.Residual(); r.Cmp(big.NewInt(-5)) != 0 {
		t.Fatalf("residual = %s, want -5", r)
	}
}

func TestNetworkStateReader_CurrentEntries(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	resetNetworkStateLake(t, ctx)
	live := entryChange(t, 96, 1, "updated", cap76Data("live", 2))
	gone := entryChange(t, 97, 1, "removed", cap76Data("gone", 0))
	insertEntryChanges(t, ctx,
		entryChange(t, 95, 1, "created", cap76Data("live", 1)),
		live,
		entryChange(t, 90, 1, "created", cap76Data("gone", 1)),
		gone,
	)
	reader, err := chstore.NewNetworkStateReader(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	got, err := reader.CurrentEntries(ctx, "contract_data", []string{live.KeyXDR, gone.KeyXDR, "absent-key"})
	if err != nil {
		t.Fatalf("CurrentEntries: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(got), got)
	}
	if e := got[live.KeyXDR]; e.LedgerSeq != 96 || e.ChangeType != "updated" || e.EntryXDR != live.EntryXDR {
		t.Errorf("live key resolved to %+v, want the ledger-96 update", e)
	}
	if e := got[gone.KeyXDR]; e.LedgerSeq != 97 || e.ChangeType != "removed" {
		t.Errorf("removed key resolved to %+v, want the ledger-97 removal", e)
	}
}
