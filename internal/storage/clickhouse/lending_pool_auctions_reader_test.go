package clickhouse

import (
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"
)

func auctionKey(t *testing.T, pool xdr.ContractId, auctType uint32, account string) string {
	t.Helper()
	user, err := addressScVal(account)
	if err != nil {
		t.Fatal(err)
	}
	k, err := contractDataKeyXDR(pool, "Auction", auctionKeyVal(auctType, user))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// TestAuctionKeyPrefix_MatchesEveryAuctionOfThePoolOnly: the range prefix covers G- and C-user Auction keys of every type of
// the pool, and neither its Positions keys nor another pool's Auction keys.
func TestAuctionKeyPrefix_MatchesEveryAuctionOfThePoolOnly(t *testing.T) {
	pool, other := xdr.ContractId{7}, xdr.ContractId{8}
	prefix, err := auctionKeyPrefix(pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, acct := range []string{"GAAZI4TCR3TY5OJHCTJC2A4QSY6CJWJH5IAJTGKIN2ER7LBNVKOCCWN7", strkeyOf(t, xdr.ContractId{9})} {
		for _, typ := range []uint32{0, 1, 2} {
			k := auctionKey(t, pool, typ, acct)
			if !strings.HasPrefix(k, prefix) {
				t.Fatalf("Auction(%d, %s) key %s lacks prefix %s", typ, acct, k, prefix)
			}
			got, ok := auctionKeyUser(k)
			if !ok || got.User != acct || got.AuctionType != typ {
				t.Fatalf("auctionKeyUser = %+v, %v; want %d %s", got, ok, typ, acct)
			}
			if strings.HasPrefix(auctionKey(t, other, typ, acct), prefix) {
				t.Fatal("another pool's Auction key matches the prefix")
			}
		}
		pk := positionsKey(t, pool, acct)
		if strings.HasPrefix(pk, prefix) {
			t.Fatal("a Positions key matches the Auction prefix")
		}
		if _, ok := auctionKeyUser(pk); ok {
			t.Fatal("a Positions key read as an Auction key")
		}
	}
}

// TestAuctionKeyUser_RefusesOtherShapes: a key under the Auction symbol whose argument is not exactly
// {auct_type: u32, user: Address} is refused, not guessed at.
func TestAuctionKeyUser_RefusesOtherShapes(t *testing.T) {
	pool := xdr.ContractId{7}
	user := contractAddressVal(xdr.ContractId{9})
	entry := func(name string, v xdr.ScVal) xdr.ScMapEntry { return xdr.ScMapEntry{Key: symVal(name), Val: v} }
	for name, arg := range map[string]xdr.ScVal{
		"bare address":    user,
		"extra field":     mapVal(entry("auct_type", u32Val(0)), entry("user", user), entry("x", u32Val(1))),
		"missing user":    mapVal(entry("auct_type", u32Val(0)), entry("nope", user)),
		"type not u32":    mapVal(entry("auct_type", symVal("0")), entry("user", user)),
		"user not addr":   mapVal(entry("auct_type", u32Val(0)), entry("user", u32Val(1))),
		"wrong user name": mapVal(entry("auct_type", u32Val(0)), entry("account", user)),
	} {
		k, err := contractDataKeyXDR(pool, "Auction", arg)
		if err != nil {
			t.Fatal(err)
		}
		if got, ok := auctionKeyUser(k); ok {
			t.Errorf("%s: read as %+v", name, got)
		}
	}
}

// TestAssemblePoolAuctions_KeepsArchivedAndCountsUnrecognised: archived keys are still the pool's auctions; keys that are not
// AuctionKeys are counted, not read; order is by user then type.
func TestAssemblePoolAuctions_KeepsArchivedAndCountsUnrecognised(t *testing.T) {
	pool := xdr.ContractId{7}
	g1, g2 := "GAAZI4TCR3TY5OJHCTJC2A4QSY6CJWJH5IAJTGKIN2ER7LBNVKOCCWN7", strkeyOf(t, xdr.ContractId{9})
	k1, k2, k3, bad := auctionKey(t, pool, 1, g1), auctionKey(t, pool, 0, g1), auctionKey(t, pool, 2, g2), positionsKey(t, pool, g1)
	got := assemblePoolAuctions(contractEntries{
		vals:     map[string]xdr.ScVal{k1: symVal("a"), k3: symVal("b"), bad: symVal("c")},
		archived: map[string]bool{k2: true},
	})
	if got.Unrecognised != 1 || len(got.Auctions) != 3 {
		t.Fatalf("got %+v", got)
	}
	want := []LendingAuctionUser{{2, g2}, {0, g1}, {1, g1}} // the C-address user sorts before the G-address one
	for i, w := range want {
		if got.Auctions[i] != w {
			t.Fatalf("auction %d = %+v, want %+v", i, got.Auctions[i], w)
		}
	}
}
