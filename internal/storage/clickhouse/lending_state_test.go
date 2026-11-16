package clickhouse

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

const (
	testUSDCSAC = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	testHolder  = "GAAZI4TCR3TY5OJHCTJC2A4QSY6CJWJH5IAJTGKIN2ER7LBNVKOCCWN7"
)

func testScAddress(t *testing.T, s string) xdr.ScAddress {
	t.Helper()
	if s[0] == 'G' {
		raw := strkey.MustDecode(strkey.VersionByteAccountID, s)
		var pk xdr.Uint256
		copy(pk[:], raw)
		acct := xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: &pk}
		return xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &acct}
	}
	id, err := contractIDFromStrkey(s)
	if err != nil {
		t.Fatal(err)
	}
	return xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &id}
}

func testAddrVal(t *testing.T, s string) xdr.ScVal {
	a := testScAddress(t, s)
	return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &a}
}

func testSym(s string) xdr.ScVal {
	sym := xdr.ScSymbol(s)
	return xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym}
}

func testU32(v uint32) xdr.ScVal {
	u := xdr.Uint32(v)
	return xdr.ScVal{Type: xdr.ScValTypeScvU32, U32: &u}
}

// testStruct is a #[contracttype] struct: a map keyed by field name.
func testStruct(fields ...any) xdr.ScVal {
	m := &xdr.ScMap{}
	for i := 0; i < len(fields); i += 2 {
		*m = append(*m, xdr.ScMapEntry{Key: testSym(fields[i].(string)), Val: fields[i+1].(xdr.ScVal)})
	}
	return xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &m}
}

// testDataKey is the key_xdr of contract storage entry `key` (persistent unless temporary is set).
func testDataKey(t *testing.T, contract string, key xdr.ScVal) string {
	t.Helper()
	lk := xdr.LedgerKey{Type: xdr.LedgerEntryTypeContractData, ContractData: &xdr.LedgerKeyContractData{
		Contract: testScAddress(t, contract), Key: key, Durability: xdr.ContractDataDurabilityPersistent,
	}}
	s, err := xdr.MarshalBase64(lk)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func testEnumKey(sym string, arg xdr.ScVal) xdr.ScVal {
	vec := &xdr.ScVec{testSym(sym), arg}
	return xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &vec}
}

func matchingKeys(ps []LendingPrefix, key string) []string {
	var out []string
	for _, p := range ps {
		if strings.HasPrefix(key, p.Prefix) {
			out = append(out, p.Contract+"/"+p.Key)
		}
	}
	return out
}

func TestLendingPrefixes_MatchCapturedKeysOnly(t *testing.T) {
	pool, bstop := "CACSQWCMKG45TUHCQ3LXHOB7YEEJH4W376ZHQPXH2IZ3TE4NJHFZMCY7", "CAHCUKBPTW5DR5RNXOHCTJD5U76A6EEZXYZAQFRQEEQRXQ3CRUZ6Z3MO"
	reg := []LendingContract{{ID: pool, Protocol: LendingBLNT}, {ID: bstop, Protocol: LendingBLNT, Backstop: true}}
	ps, err := LendingPrefixes(reg)
	if err != nil {
		t.Fatal(err)
	}
	instance := testDataKey(t, pool, xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance})
	poolTier := testStruct("pool", testAddrVal(t, pool), "tier", testU32(1))
	captured := map[string]string{
		testDataKey(t, pool, testEnumKey("ResData", testAddrVal(t, testUSDCSAC))):   pool + "/ResData",
		testDataKey(t, pool, testEnumKey("ResConfig", testAddrVal(t, testUSDCSAC))): pool + "/ResConfig",
		instance: pool + "/Instance",
		testDataKey(t, bstop, testEnumKey("PoolBalance", testAddrVal(t, pool))): bstop + "/PoolBalance",
		testDataKey(t, bstop, testEnumKey("TierPoolBalance", poolTier)):         bstop + "/TierPoolBalance",
	}
	for key, want := range captured {
		if got := matchingKeys(ps, key); len(got) != 1 || got[0] != want {
			t.Errorf("key for %s matched %v", want, got)
		}
	}
	userReserve := testStruct("reserve_id", testU32(0), "user", testAddrVal(t, testHolder))
	ignored := map[string]string{
		"Positions(G)":     testDataKey(t, pool, testEnumKey("Positions", testAddrVal(t, testHolder))),
		"Positions(C)":     testDataKey(t, pool, testEnumKey("Positions", testAddrVal(t, testUSDCSAC))),
		"UserEmis":         testDataKey(t, pool, testEnumKey("UserEmis", userReserve)),
		"Auction":          testDataKey(t, pool, testEnumKey("Auction", testStruct("auct_type", testU32(0), "user", testAddrVal(t, testHolder)))),
		"ResInit":          testDataKey(t, pool, testEnumKey("ResInit", testAddrVal(t, testUSDCSAC))),
		"EmisData":         testDataKey(t, pool, testEnumKey("EmisData", testU32(0))),
		"PoolUSDC":         testDataKey(t, bstop, testEnumKey("PoolUSDC", testAddrVal(t, pool))),
		"UserBalance":      testDataKey(t, bstop, testEnumKey("UserBalance", testStruct("pool", testAddrVal(t, pool), "user", testAddrVal(t, testHolder)))),
		"TierUserBalance":  testDataKey(t, bstop, testEnumKey("TierUserBalance", poolTier)),
		"ResData of other": testDataKey(t, "CAUTPSQDAJEUV5XEU3EXFVYRGDJLEJGVTFJCFZCSPTDCREAMZ573VF4D", testEnumKey("ResData", testAddrVal(t, testUSDCSAC))),
	}
	for name, key := range ignored {
		if got := matchingKeys(ps, key); len(got) != 0 {
			t.Errorf("%s matched %v", name, got)
		}
	}
	for _, p := range ps {
		if p.Key != "Instance" && (len(p.Prefix)%4 != 0 || strings.Contains(p.Prefix, "=")) {
			t.Errorf("%s prefix %q is not an exact base64 run of whole bytes", p.Key, p.Prefix)
		}
	}
	_, _, _, heads := lendingPrefixArgs(ps)
	if len(heads) != 2 {
		t.Errorf("contract heads = %d, want one per contract", len(heads))
	}
}

// Every symbol byte must survive the cut to a 3-byte boundary, or two names differing in their last bytes would collide.
func TestLendingKeyPrefix_KeepsTheWholeSymbol(t *testing.T) {
	id, err := contractIDFromStrkey("CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7")
	if err != nil {
		t.Fatal(err)
	}
	for _, sym := range []string{"ResData", "ResConfig", "PoolBalance", "TierPoolBalance"} {
		p, err := lendingKeyPrefix(id, sym, xdr.ScValTypeScvAddress)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := base64.StdEncoding.DecodeString(p)
		if err != nil {
			t.Fatal(err)
		}
		symEnd := 56 + 4 + len(sym)
		if len(raw) < symEnd || string(raw[60:symEnd]) != sym {
			t.Errorf("%s: prefix holds %d bytes, symbol ends at %d", sym, len(raw), symEnd)
		}
	}
}

func TestLendingSQL_Shape(t *testing.T) {
	for name, tc := range map[string]struct {
		q     string
		binds int
		must  []string
	}{
		"insert": {lendingStateInsert, 7, []string{
			"INSERT INTO stellar.lending_state_hourly",
			"change_type != 'state'",
			"toStartOfHour(close_time)",
			"(ledger_seq, intra_ledger_seq, tx_hash, op_index, change_index)",
			"GROUP BY key_xdr, i, hour",
		}},
		"proof": {lendingProofQuery, 5, []string{
			"stellar.ledger_entries_current FINAL",
			"stellar.lending_state_hourly FINAL",
			"WHERE ledger_seq <= ? AND entry_type = 'contract_data' AND change_type != 'state'",
		}},
	} {
		if got := strings.Count(tc.q, "?"); got != tc.binds {
			t.Errorf("%s: %d binds, want %d", name, got, tc.binds)
		}
		for _, m := range tc.must {
			if !strings.Contains(tc.q, m) {
				t.Errorf("%s: missing %q", name, m)
			}
		}
	}
	if !strings.Contains(lendingStateInsert, "substring(key_xdr, 1, 52)") || lendingContractHeadLen != 52 {
		t.Error("the SQL contract-head length drifted from lendingContractHeadLen")
	}
}
