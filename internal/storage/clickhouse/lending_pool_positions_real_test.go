package clickhouse

import (
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// scShape renders a Positions value's whole structure, so a field or value type the decoder does not read shows up in a pin.
func scShape(v xdr.ScVal) string {
	switch v.Type {
	case xdr.ScValTypeScvMap:
		m, _ := v.GetMap()
		var parts []string
		if m != nil {
			for _, e := range *m {
				parts = append(parts, scShape(e.Key)+":"+scShape(e.Val))
			}
		}
		return "{" + strings.Join(parts, ",") + "}"
	case xdr.ScValTypeScvSymbol:
		return string(*v.Sym)
	case xdr.ScValTypeScvU32:
		return fmt.Sprintf("u32(%d)", *v.U32)
	case xdr.ScValTypeScvI128:
		a, err := scval.AsAmountFromI128(v)
		if err != nil {
			return "i128(?)"
		}
		return "i128(" + a.String() + ")"
	default:
		return v.Type.String()
	}
}

// TestPoolPositions_RealEntries: one stored Positions entry per protocol from the lake runs through the pool read's own
// prefix, key and value decoding, and decodes to exactly the structure and amounts the chain holds.
func TestPoolPositions_RealEntries(t *testing.T) {
	raw, err := os.ReadFile("../../../test/fixtures/lending_storage/positions_entries.json")
	if err != nil {
		t.Fatal(err)
	}
	var entries []struct {
		Protocol string `json:"protocol"`
		Pool     string `json:"pool"`
		Ledger   uint32 `json:"ledger_seq"`
		KeyXDR   string `json:"key_xdr"`
		EntryXDR string `json:"entry_xdr"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		account, shape string
		supply, borrow []string // by reserve index 0..3; "" is absent
		nonZero        bool     // the entry must carry at least one non-zero amount
	}{
		// A closed position: the account repaid and withdrew everything, and the entry keeps its three empty maps.
		"blend@63030578": {
			account: "GD57AJD6CR4QD4HJG3FW6PO76QBHZXVGNRT3NRHGWX2EER3YEXP4VKUC", shape: "{collateral:{},liabilities:{},supply:{}}",
			supply: []string{"", "", "", ""}, borrow: []string{"", "", "", ""},
		},
		"blend@55126867": {
			account: "GDSFASSHZ7Q47B7KVIAXXT4PRT27OZJMOB4XIHPL62BDNO7FXDMJHLXJ", shape: "{collateral:{u32(3):i128(3292845684288)},liabilities:{},supply:{}}",
			supply: []string{"", "", "", "3292845684288"}, borrow: []string{"", "", "", ""}, nonZero: true,
		},
		"blend@65581104": {
			account: "GCTSFIICPYZW2R6HWA25IWDHQYSFBJJUAXVYHGUQ7K64G54ZQ6OOJUF6", shape: "{collateral:{u32(0):i128(6980245630778052)},liabilities:{u32(1):i128(252592852494516)},supply:{}}",
			supply: []string{"6980245630778052", "", "", ""}, borrow: []string{"", "252592852494516", "", ""}, nonZero: true,
		},
		"blnt@65581656": {
			account: "GDJSH2NU2WF6J4P5DL4522DUCABWSTZOKFQ7BHBCFYQ3QKC6FRYWP6OL",
			shape:   "{collateral:{u32(0):i128(5955145417),u32(1):i128(74472245)},liabilities:{u32(2):i128(414147918)},supply:{}}",
			supply:  []string{"5955145417", "74472245", "", ""}, borrow: []string{"", "", "414147918", ""},
		},
	}
	if len(entries) != len(want) {
		t.Fatalf("fixture holds %d entries, want %d", len(entries), len(want))
	}
	reserves := []xdr.ContractId{{1}, {2}, {3}, {4}}
	for _, e := range entries {
		e.Protocol = fmt.Sprintf("%s@%d", e.Protocol, e.Ledger) // blend holds two pool versions; label each by ledger
		w, ok := want[e.Protocol]
		if !ok {
			t.Fatalf("unexpected fixture protocol %q", e.Protocol)
		}
		delete(want, e.Protocol)
		poolID, err := contractIDFromStrkey(e.Pool)
		if err != nil {
			t.Fatal(err)
		}
		prefix, err := positionsKeyPrefix(poolID)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(e.KeyXDR, prefix) {
			t.Errorf("%s: real key %s lacks the pool's read prefix %s", e.Protocol, e.KeyXDR, prefix)
		}
		account, ok := positionsKeyAccount(e.KeyXDR)
		if !ok || !(strkey.IsValidEd25519PublicKey(account) || strkey.IsValidContractAddress(account)) || account != w.account {
			t.Fatalf("%s: key account = %q, %v; want %s", e.Protocol, account, ok, w.account)
		}
		v, modified, ok := contractDataEntry(e.EntryXDR)
		if !ok || modified != e.Ledger {
			t.Fatalf("%s: entry decode ok=%v ledger=%d, want %d", e.Protocol, ok, modified, e.Ledger)
		}
		if got := scShape(v); got != w.shape {
			t.Fatalf("%s: stored shape %s, want %s", e.Protocol, got, w.shape)
		}
		got := assemblePoolPositions(contractEntries{
			vals: map[string]xdr.ScVal{e.KeyXDR: v}, archived: map[string]bool{}, ledger: map[string]uint32{e.KeyXDR: modified},
		}, reserveListVal(t, reserves...), true)
		if got.Unreadable != 0 || len(got.Accounts) != 1 || got.Accounts[0].Account != w.account || got.Accounts[0].Ledger != e.Ledger {
			t.Fatalf("%s: assembled %+v", e.Protocol, got)
		}
		p := got.Accounts[0]
		if w.nonZero {
			nz := false
			for _, a := range append(append([]string{}, w.supply...), w.borrow...) {
				nz = nz || (a != "" && a != "0")
			}
			if !nz {
				t.Fatalf("%s: pinned amounts are all empty or zero", e.Protocol)
			}
		}
		for i, r := range reserves {
			asset := strkeyOf(t, r)
			if s := amountOrEmpty(p.Supply[asset]); s != w.supply[i] {
				t.Errorf("%s: reserve %d supply = %q, want %q", e.Protocol, i, s, w.supply[i])
			}
			if b := amountOrEmpty(p.Borrow[asset]); b != w.borrow[i] {
				t.Errorf("%s: reserve %d borrow = %q, want %q", e.Protocol, i, b, w.borrow[i])
			}
			// Every stored entry has supply:{}, so all its supply is collateral and none is non-collateral.
			if c := amountOrEmpty(p.Collateral[asset]); c != w.supply[i] {
				t.Errorf("%s: reserve %d collateral = %q, want %q", e.Protocol, i, c, w.supply[i])
			}
			if nc := amountOrEmpty(p.NonCollateral[asset]); nc != "" {
				t.Errorf("%s: reserve %d non-collateral = %q, want none", e.Protocol, i, nc)
			}
			sum := new(big.Int).Add(orZero(p.Collateral[asset]), orZero(p.NonCollateral[asset]))
			if got := orZero(p.Supply[asset]); got.Cmp(sum) != 0 {
				t.Errorf("%s: reserve %d supply %s != collateral + non-collateral %s", e.Protocol, i, got, sum)
			}
		}
	}
	if len(want) != 0 {
		t.Fatalf("no real fixture for %v", want)
	}
}

func orZero(v *big.Int) *big.Int {
	if v == nil {
		return new(big.Int)
	}
	return v
}

func amountOrEmpty(v *big.Int) string {
	if v == nil {
		return ""
	}
	return v.String()
}
