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
	kinds := map[string]int{}
	for _, k := range keys {
		kinds[refs[k].kind]++
	}
	if kinds[backstopKindBalance] != 3 || kinds[backstopKindConfig] != 1 || kinds[backstopKindInstance] == 0 {
		t.Fatalf("key kinds = %v, want 3 balances, the config and the instance", kinds)
	}
	vals := map[string]xdr.ScVal{}
	for k, ref := range refs {
		if ref.kind == backstopKindConfig {
			assertConfigKey(t, k)
		}
		if ref.kind != backstopKindBalance {
			continue
		}
		key := assertBalanceKey(t, k, ref.asset)
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
	token, tiers := assembleBackstop(vals, nil, refs, true) // no configuration read: all three tiers, a missing entry unavailable
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

// backstopConfigVal is a PoolBackstopConfig value: Vec<BackstopTierConfig{asset, take_rate_weight}>.
func backstopConfigVal(tiers ...blntTierConfig) xdr.ScVal {
	vec := xdr.ScVec{}
	for _, c := range tiers {
		asset := &xdr.ScVec{symVal(c.asset)}
		m := &xdr.ScMap{
			{Key: symVal("asset"), Val: xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &asset}},
			{Key: symVal("take_rate_weight"), Val: scval.NewU32(c.weight)},
		}
		vec = append(vec, xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &m})
	}
	p := &vec
	return xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &p}
}

// TestAssembleBackstop_ServesOnlyConfiguredTiers — with a PoolBackstopConfig of two tiers: FirstLoss's live entry is served
// exactly with its asset, token and weight; SecondLoss with no entry is zero (the contract's get_persistent_default); ThirdLoss is
// unconfigured and not served even though an entry exists. An archived configured entry stays unavailable.
func TestAssembleBackstop_ServesOnlyConfiguredTiers(t *testing.T) {
	backstopID, poolID := xdr.ContractId{1, 2, 3}, xdr.ContractId{9, 8, 7}
	_, refs, err := backstopKeys(backstopID, poolID, true)
	if err != nil {
		t.Fatal(err)
	}
	usdc, bToken := xdr.ContractId{4}, xdr.ContractId{5}
	storage := &xdr.ScMap{
		{Key: symVal("BToken"), Val: contractAddressVal(bToken)},
		{Key: symVal("USDCTkn"), Val: contractAddressVal(usdc)},
	}
	instance := xdr.ScVal{Type: xdr.ScValTypeScvContractInstance, Instance: &xdr.ScContractInstance{Storage: storage}}
	balance := func(key string) xdr.ScVal {
		_, k, err := scval.ParseContractDataKey(key)
		if err != nil {
			t.Fatal(err)
		}
		v, ok := contractDataValue(poolBalanceEntry(t, backstopID, k, "9223372036854775817", "18446744073709551621", "7"))
		if !ok {
			t.Fatal("entry did not decode")
		}
		return v
	}
	vals := map[string]xdr.ScVal{}
	var firstLossKey string
	for k, ref := range refs {
		switch {
		case ref.kind == backstopKindInstance:
			vals[k] = instance
		case ref.kind == backstopKindConfig:
			vals[k] = backstopConfigVal(blntTierConfig{"Usdc", 40}, blntTierConfig{"BlntUsdc", 60})
		case ref.asset == "FirstLoss":
			vals[k], firstLossKey = balance(k), k
		case ref.asset == "ThirdLoss":
			vals[k] = balance(k)
		}
	}
	_, tiers := assembleBackstop(vals, nil, refs, true)
	if len(tiers) != 2 {
		t.Fatalf("tiers = %+v, want the two configured", tiers)
	}
	fl, sl := tiers[0], tiers[1]
	usdcStr, _ := scval.AsAddressStrkey(contractAddressVal(usdc))
	bTokenStr, _ := scval.AsAddressStrkey(contractAddressVal(bToken))
	if fl.Tier != "FirstLoss" || fl.Balance == nil || fl.Balance.Shares.String() != "9223372036854775817" || fl.Asset != "Usdc" ||
		fl.Token != usdcStr || fl.TakeRateWeight == nil || *fl.TakeRateWeight != 40 {
		t.Fatalf("FirstLoss = %+v", fl)
	}
	if sl.Tier != "SecondLoss" || sl.Balance == nil || sl.Balance.Shares.Sign() != 0 || sl.Balance.Tokens.Sign() != 0 ||
		sl.Balance.Q4W.Sign() != 0 || sl.Asset != "BlntUsdc" || sl.Token != bTokenStr || *sl.TakeRateWeight != 60 {
		t.Fatalf("SecondLoss = %+v, want zero (no entry)", sl)
	}
	_, tiers = assembleBackstop(vals, map[string]bool{firstLossKey: true}, refs, true)
	if tiers[0].Balance != nil || tiers[0].Asset != "Usdc" {
		t.Fatalf("archived FirstLoss = %+v, want unavailable", tiers[0])
	}
}

func TestDecodeBLNTBackstopConfig_RejectsUnknownShapes(t *testing.T) {
	for name, v := range map[string]xdr.ScVal{
		"void":          {Type: xdr.ScValTypeScvVoid},
		"empty":         backstopConfigVal(),
		"four tiers":    backstopConfigVal(blntTierConfig{"Usdc", 1}, blntTierConfig{"Xlm", 1}, blntTierConfig{"BlntXlm", 1}, blntTierConfig{"BlntUsdc", 1}),
		"unknown asset": backstopConfigVal(blntTierConfig{"Eth", 1}),
	} {
		if _, err := decodeBLNTBackstopConfig(v); err == nil {
			t.Errorf("%s decoded", name)
		}
	}
}

// assertConfigKey checks the BackstopDataKey::PoolBackstopConfig(pool) shape: Vec[Symbol, Address].
func assertConfigKey(t *testing.T, k string) {
	t.Helper()
	_, key, err := scval.ParseContractDataKey(k)
	if err != nil {
		t.Fatal(err)
	}
	vec, _ := key.GetVec()
	if vec == nil || len(*vec) != 2 || string(*(*vec)[0].Sym) != "PoolBackstopConfig" || (*vec)[1].Type != xdr.ScValTypeScvAddress {
		t.Fatalf("config key = %s", scval.Display(key))
	}
}

// assertBalanceKey checks a tier's key: SecondLoss on the pre-tier PoolBalance(pool), the others on TierPoolBalance{pool, tier}.
func assertBalanceKey(t *testing.T, k, tier string) xdr.ScVal {
	t.Helper()
	_, key, err := scval.ParseContractDataKey(k)
	if err != nil {
		t.Fatal(err)
	}
	vec, _ := key.GetVec()
	variant := string(*(*vec)[0].Sym)
	if tier == "SecondLoss" {
		if variant != "PoolBalance" || (*vec)[1].Type != xdr.ScValTypeScvAddress {
			t.Fatalf("SecondLoss key = %s", scval.Display(key))
		}
		return key
	}
	m, _ := (*vec)[1].GetMap()
	if variant != "TierPoolBalance" || m == nil || len(*m) != 2 || string(*(*m)[0].Key.Sym) != "pool" ||
		string(*(*m)[1].Key.Sym) != "tier" || tierName((*m)[1].Val) != tier {
		t.Fatalf("%s key = %s", tier, scval.Display(key))
	}
	return key
}
