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

// TestLendingPoolAuctions_ReadsLiveAuctionKeysOfThePoolOnly: the prefix read returns the pool's live Auction keys and
// not a removed one, another pool's, or a Positions key.
func TestLendingPoolAuctions_ReadsLiveAuctionKeysOfThePoolOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	raw := dialClickHouse(t, ctx, "stellar")
	pool, other := xdr.ContractId{0xA7, 0x51, 0x0E}, xdr.ContractId{0xA7, 0x51, 0x0F}
	u1, u2, u3 := xdr.ContractId{0xC1}, xdr.ContractId{0xC2}, xdr.ContractId{0xC3}
	const ledger = uint32(91_000_003)
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

	posVec := &xdr.ScVec{positionsSym("Positions"), positionsContractVal(u1)}
	rows := []struct {
		pool    xdr.ContractId
		key     xdr.ScVal
		removed bool
	}{
		{pool, auctionsKey(0, u1), false},
		{pool, auctionsKey(1, u2), false},
		{pool, auctionsKey(2, u3), true},
		{other, auctionsKey(0, u1), false},
		{pool, xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &posVec}, false},
	}
	b, err := raw.PrepareBatch(ctx, `INSERT INTO stellar.ledger_entries_current
		(entry_type, key_xdr, account_id, asset, balance, change_type, ledger_seq, close_time, entry_xdr)`)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	for _, r := range rows {
		change, entry := "updated", positionsEntryB64(t, r.pool, r.key, positionsIndexMap(nil), ledger)
		if r.removed {
			change, entry = "removed", ""
		}
		if err := b.Append("contract_data", lendingKeyB64(t, r.pool, r.key), "", "", int64(0), change, ledger, at, entry); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := b.Send(); err != nil {
		t.Fatalf("send: %v", err)
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
	want := []chstore.LendingAuctionUser{{AuctionType: 0, User: positionsStrkey(t, u1)}, {AuctionType: 1, User: positionsStrkey(t, u2)}}
	if got.Truncated || got.Unrecognised != 0 || len(got.Auctions) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i, w := range want {
		if got.Auctions[i] != w {
			t.Fatalf("auction %d = %+v, want %+v", i, got.Auctions[i], w)
		}
	}
}
