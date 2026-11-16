package clickhouse

import (
	"math/big"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

func i128Val(t *testing.T, s string) xdr.ScVal {
	t.Helper()
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		t.Fatalf("bad int %q", s)
	}
	return seedI128Val(n)
}

func tierName(v xdr.ScVal) string {
	vec, ok := v.GetVec()
	if !ok || vec == nil || len(*vec) != 1 || (*vec)[0].Sym == nil {
		return ""
	}
	return string(*(*vec)[0].Sym)
}

// poolBalanceEntry is a backstop PoolBalance ledger entry as the lake stores it, in base64.
func poolBalanceEntry(t *testing.T, backstop xdr.ContractId, key xdr.ScVal, shares, tokens, q4w string) string {
	t.Helper()
	sym := func(s string) xdr.ScVal {
		v := xdr.ScSymbol(s)
		return xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &v}
	}
	m := &xdr.ScMap{
		{Key: sym("q4w"), Val: i128Val(t, q4w)},
		{Key: sym("shares"), Val: i128Val(t, shares)},
		{Key: sym("tokens"), Val: i128Val(t, tokens)},
	}
	entry := xdr.LedgerEntry{Data: xdr.LedgerEntryData{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.ContractDataEntry{
			Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &backstop},
			Key:        key,
			Durability: xdr.ContractDataDurabilityPersistent,
			Val:        xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &m},
		},
	}}
	b64, err := xdr.MarshalBase64(entry)
	if err != nil {
		t.Fatal(err)
	}
	return b64
}

// TestBackstopKeysAndBalances_BLNTTiers builds the three BLNT tier keys, checks their shape (SecondLoss on the pre-tier
// PoolBalance key, the others on TierPoolBalance{pool, tier}), and decodes real-shaped entries exactly above 2^63.
func TestBackstopKeysAndBalances_BLNTTiers(t *testing.T) {
	backstopID, poolID := xdr.ContractId{1, 2, 3}, xdr.ContractId{9, 8, 7}
	keys, refs, err := backstopKeys(backstopID, poolID, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 3 {
		t.Fatalf("keys = %d, want 3 (no instance read for BLNT)", len(keys))
	}
	vals := map[string]xdr.ScVal{}
	for k, ref := range refs {
		_, key, err := scval.ParseContractDataKey(k)
		if err != nil {
			t.Fatal(err)
		}
		vec, _ := key.GetVec()
		variant := string(*(*vec)[0].Sym)
		switch ref.asset {
		case "SecondLoss":
			if variant != "PoolBalance" || (*vec)[1].Type != xdr.ScValTypeScvAddress {
				t.Fatalf("SecondLoss key = %s", scval.Display(key))
			}
		default:
			m, _ := (*vec)[1].GetMap()
			if variant != "TierPoolBalance" || m == nil || len(*m) != 2 || string(*(*m)[0].Key.Sym) != "pool" ||
				string(*(*m)[1].Key.Sym) != "tier" || tierName((*m)[1].Val) != ref.asset {
				t.Fatalf("%s key = %s", ref.asset, scval.Display(key))
			}
		}
		if ref.asset == "ThirdLoss" {
			continue // no entry: unavailable, never zero
		}
		b64 := poolBalanceEntry(t, backstopID, key, "9223372036854775817", "18446744073709551621", "7")
		v, ok := contractDataValue(b64)
		if !ok {
			t.Fatal("entry did not decode")
		}
		vals[k] = v
	}
	token, tiers := assembleBackstop(vals, refs, true)
	if token != "" || len(tiers) != 3 || tiers[0].Tier != "FirstLoss" || tiers[2].Tier != "ThirdLoss" || tiers[2].Balance != nil {
		t.Fatalf("token=%q tiers=%+v", token, tiers)
	}
	for _, tr := range tiers[:2] {
		b := tr.Balance
		if b == nil || b.Shares.String() != "9223372036854775817" || b.Tokens.String() != "18446744073709551621" || b.Q4W.String() != "7" {
			t.Fatalf("%s balance = %+v", tr.Tier, b)
		}
	}
}

// TestBackstopKeys_BlendIsPoolBalanceAndInstance — a Blend backstop reads PoolBalance(pool) and its instance (for BToken).
func TestBackstopKeys_BlendIsPoolBalanceAndInstance(t *testing.T) {
	keys, refs, err := backstopKeys(xdr.ContractId{1}, xdr.ContractId{2}, false)
	if err != nil {
		t.Fatal(err)
	}
	var balances, instances int
	for _, k := range keys {
		switch refs[k].kind {
		case backstopKindBalance:
			balances++
			if refs[k].asset != "" {
				t.Errorf("blend tier = %q, want unnamed", refs[k].asset)
			}
		case backstopKindInstance:
			instances++
		}
	}
	if balances != 1 || instances == 0 {
		t.Fatalf("balances=%d instances=%d", balances, instances)
	}
	if _, err := decodeBackstopBalance(xdr.ScVal{Type: xdr.ScValTypeScvVoid}); err == nil {
		t.Error("void decoded as a balance")
	}
}
