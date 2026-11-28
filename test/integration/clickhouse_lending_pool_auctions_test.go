//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

func auctionsKey(auctType uint32, user xdr.ContractId) xdr.ScVal {
	t := xdr.Uint32(auctType)
	m := xdr.ScMap{
		{Key: positionsSym("auct_type"), Val: xdr.ScVal{Type: xdr.ScValTypeScvU32, U32: &t}},
		{Key: positionsSym("user"), Val: positionsContractVal(user)},
	}
	mp := &m
	vec := &xdr.ScVec{positionsSym("Auction"), xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &mp}}
	return xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &vec}
}

// TestLendingPoolAuctions_ReadsLiveAuctionKeysOfThePoolOnly: the prefix read returns the pool's present Auction keys
// with their last-modified ledgers, reports a removed one as gone at its removal ledger, keeps a present entry whose TTL
// has lapsed live, and reads no other pool's key or a Positions key.
func TestLendingPoolAuctions_ReadsLiveAuctionKeysOfThePoolOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	raw := dialClickHouse(t, ctx, "stellar")
	pool, other := xdr.ContractId{0xA7, 0x51, 0x0E}, xdr.ContractId{0xA7, 0x51, 0x0F}
	u1, u2, u3, u4 := xdr.ContractId{0xC1}, xdr.ContractId{0xC2}, xdr.ContractId{0xC3}, xdr.ContractId{0xC4}
	const ledger = uint32(91_000_003)
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

	posVec := &xdr.ScVec{positionsSym("Positions"), positionsContractVal(u1)}
	rows := []struct {
		pool    xdr.ContractId
		key     xdr.ScVal
		ledger  uint32
		removed bool
	}{
		{pool, auctionsKey(0, u1), ledger, false},
		{pool, auctionsKey(1, u2), ledger + 1, false},
		{pool, auctionsKey(2, u3), ledger + 5, true},
		{pool, auctionsKey(0, u4), ledger, false},
		{other, auctionsKey(0, u1), ledger, false},
		{other, auctionsKey(1, u3), ledger + 6, true},
		{pool, xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &posVec}, ledger, false},
	}
	b, err := raw.PrepareBatch(ctx, `INSERT INTO stellar.ledger_entries_current
		(entry_type, key_xdr, account_id, asset, balance, change_type, ledger_seq, close_time, entry_xdr)`)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	for _, r := range rows {
		change, entry := "updated", positionsEntryB64(t, r.pool, r.key, positionsIndexMap(nil), r.ledger)
		if r.removed {
			change, entry = "removed", ""
		}
		if err := b.Append("contract_data", lendingKeyB64(t, r.pool, r.key), "", "", int64(0), change, r.ledger, at, entry); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := b.Send(); err != nil {
		t.Fatalf("send: %v", err)
	}

	// u4's auction entry is present but its TTL lapsed before the lake tip.
	expiredKey := lendingKeyB64(t, pool, auctionsKey(0, u4))
	if _, err := chstore.InsertEntryChanges(ctx, addr, []chstore.LedgerEntryChangeRow{ttlChangeRow(expiredKey, ledger, 1, ledger-1, 48)}, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}
	verdicts, err := chstore.ClassifyTTLLiveness(ctx, raw, []string{expiredKey}, ledger+6)
	if err != nil || verdicts[expiredKey] != chstore.TTLArchived {
		t.Fatalf("fixture TTL verdict = %v, %v; want TTLArchived", verdicts[expiredKey], err)
	}

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })
	got, err := er.LiveAuctionUsers(ctx, positionsStrkey(t, pool))
	if err != nil {
		t.Fatal(err)
	}
	want := []chstore.LendingAuctionUser{
		{AuctionType: 0, User: positionsStrkey(t, u1), Ledger: ledger},
		{AuctionType: 1, User: positionsStrkey(t, u2), Ledger: ledger + 1},
		{AuctionType: 0, User: positionsStrkey(t, u4), Ledger: ledger},
	}
	wantGone := []chstore.LendingAuctionUser{{AuctionType: 2, User: positionsStrkey(t, u3), Ledger: ledger + 5}}
	if got.Truncated || got.Unrecognised != 0 || len(got.Auctions) != len(want) || len(got.Gone) != len(wantGone) {
		t.Fatalf("got %+v, want auctions %+v gone %+v", got, want, wantGone)
	}
	for i, w := range want {
		if got.Auctions[i] != w {
			t.Fatalf("auction %d = %+v, want %+v", i, got.Auctions[i], w)
		}
	}
	for i, w := range wantGone {
		if got.Gone[i] != w {
			t.Fatalf("gone %d = %+v, want %+v", i, got.Gone[i], w)
		}
	}
}
