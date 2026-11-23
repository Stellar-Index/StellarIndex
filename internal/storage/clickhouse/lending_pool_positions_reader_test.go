package clickhouse

import (
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"
)

func positionsKey(t *testing.T, pool xdr.ContractId, account string) string {
	t.Helper()
	user, err := addressScVal(account)
	if err != nil {
		t.Fatal(err)
	}
	k, err := contractDataKeyXDR(pool, "Positions", user)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// TestPositionsKeyPrefix_MatchesEveryAccountOfThePoolOnly: the range prefix covers G- and C-address Positions keys of the
// pool, and neither its ResList key nor another pool's Positions key.
func TestPositionsKeyPrefix_MatchesEveryAccountOfThePoolOnly(t *testing.T) {
	pool, other := xdr.ContractId{7}, xdr.ContractId{8}
	prefix, err := positionsKeyPrefix(pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, acct := range []string{"GAAZI4TCR3TY5OJHCTJC2A4QSY6CJWJH5IAJTGKIN2ER7LBNVKOCCWN7", strkeyOf(t, xdr.ContractId{9})} {
		k := positionsKey(t, pool, acct)
		if !strings.HasPrefix(k, prefix) {
			t.Fatalf("Positions(%s) key %s lacks prefix %s", acct, k, prefix)
		}
		if got, ok := positionsKeyAccount(k); !ok || got != acct {
			t.Fatalf("positionsKeyAccount = %q, %v; want %s", got, ok, acct)
		}
		if strings.HasPrefix(positionsKey(t, other, acct), prefix) {
			t.Fatal("another pool's Positions key matches the prefix")
		}
	}
	list, err := symbolKeyXDR(pool, "ResList")
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(list, prefix) {
		t.Fatal("ResList key matches the Positions prefix")
	}
	if _, ok := positionsKeyAccount(list); ok {
		t.Fatal("ResList key read as a Positions key")
	}
}

// TestAssemblePoolPositions_DecodesEachAccountAndCountsUnreadable: each live entry maps through ResList exactly above 2^63;
// an archived or undecodable entry is counted unreadable, never read as zero; an unreadable ResList makes the read unreadable.
func TestAssemblePoolPositions_DecodesEachAccountAndCountsUnreadable(t *testing.T) {
	pool, a0 := xdr.ContractId{7}, xdr.ContractId{1}
	s0 := strkeyOf(t, a0)
	g1, g2, g3 := "GAAZI4TCR3TY5OJHCTJC2A4QSY6CJWJH5IAJTGKIN2ER7LBNVKOCCWN7", strkeyOf(t, xdr.ContractId{9}), strkeyOf(t, xdr.ContractId{10})
	pos := func(supply, borrow string) xdr.ScVal {
		return mapVal(
			xdr.ScMapEntry{Key: symVal("collateral"), Val: indexMap(t, map[uint32]string{0: supply})},
			xdr.ScMapEntry{Key: symVal("liabilities"), Val: indexMap(t, map[uint32]string{0: borrow})},
			xdr.ScMapEntry{Key: symVal("supply"), Val: indexMap(t, nil)},
		)
	}
	k1, k2, k3 := positionsKey(t, pool, g1), positionsKey(t, pool, g2), positionsKey(t, pool, g3)
	ce := contractEntries{
		vals:     map[string]xdr.ScVal{k1: pos("18446744073709551616", "3"), k2: pos("5", "0"), k3: symVal("junk")},
		archived: map[string]bool{k2: true},
		ledger:   map[string]uint32{k1: 100, k2: 101, k3: 102},
	}
	got := assemblePoolPositions(ce, reserveListVal(t, a0), true)
	if !got.Readable || got.Unreadable != 2 || len(got.Accounts) != 1 || len(got.Reserves) != 1 || got.Reserves[0] != s0 {
		t.Fatalf("got %+v", got)
	}
	if a := got.Accounts[0]; a.Account != g1 || a.Supply[s0].String() != "18446744073709551616" || a.Borrow[s0].String() != "3" || a.Ledger != 100 {
		t.Fatalf("account = %+v", a)
	}
	if got := assemblePoolPositions(ce, xdr.ScVal{}, false); got.Readable || len(got.Accounts) != 0 {
		t.Fatalf("missing ResList read as %+v", got)
	}
}
