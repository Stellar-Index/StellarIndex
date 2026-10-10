package defindex

import (
	"encoding/base64"
	"errors"
	"math/big"
	"testing"

	sdkxdr "github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

type classifyCase struct {
	name      string
	topic     []string
	wantClass string
}

func runClassify(t *testing.T, fn func(*events.Event) string, cases []classifyCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := fn(&events.Event{Topic: tc.topic}); got != tc.wantClass {
				t.Errorf("class = %q, want %q", got, tc.wantClass)
			}
		})
	}
}

// Pins the topic-byte equality path: prefix is a String, not a Symbol.
func TestClassify_depositWithdraw(t *testing.T) {
	t.Parallel()
	runClassify(t, classify, []classifyCase{
		{"deposit", []string{TopicPrefixStrategy, TopicSymbolDeposit}, EventDeposit},
		{"withdraw", []string{TopicPrefixStrategy, TopicSymbolWithdraw}, EventWithdraw},
		{"wrong prefix (SoroswapPair)", []string{mustB64String(t, "SoroswapPair"), TopicSymbolDeposit}, ""},
		{"prefix as Symbol not String", []string{mustB64Symbol(t, "BlendStrategy"), TopicSymbolDeposit}, ""},
		{"harvest (classification-only)", []string{TopicPrefixStrategy, TopicSymbolHarvest}, EventHarvest},
		{"single-element topic", []string{TopicPrefixStrategy}, ""},
	})
}

const (
	strategyContract = "CDB2WMKQQNVZMEBY7Q7GZ5C7E7IAFSNMZ7GGVD6WKTCEWK7XOIAVZSAP"
	vaultContract    = "CCA2ZJP5BVRXYTQH4FAGHCAUMRYCXVC4CRYC2NXHWMR7TIVX36U7F5HR"
)

// Covers deposit (account `from`), withdraw (contract `from`, as seen on
// mainnet) and harvest (extra price_per_share ignored: decode-by-name).
func TestDecodeFlow(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		kind       string
		topic1     string
		from       sdkxdr.ScAddress
		amount     int64
		extra      []sdkxdr.ScMapEntry
		wantDir    Direction
		wantPrefix byte
		wantAmount string
	}{
		{"deposit from account", EventDeposit, TopicSymbolDeposit, makeAccountAddress(t, 0xAA), 123_456_789_000, nil, DirectionDeposit, 'G', "123456789000"},
		{"withdraw from contract", EventWithdraw, TopicSymbolWithdraw, makeContractAddress(t, 0xBB), 29_999_999, nil, DirectionWithdraw, 'C', "29999999"},
		{"harvest", EventHarvest, TopicSymbolHarvest, makeAccountAddress(t, 0xBB), 915_806, []sdkxdr.ScMapEntry{mapEntry(t, "price_per_share", i128SCVal(big.NewInt(1_002_345)))}, DirectionHarvest, 'G', "915806"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			entries := append([]sdkxdr.ScMapEntry{
				mapEntry(t, "from", addrSCVal(tc.from)),
				mapEntry(t, "amount", i128SCVal(big.NewInt(tc.amount))),
			}, tc.extra...)
			ev := &events.Event{
				Type: "contract", Ledger: 60_000_000, LedgerClosedAt: "2026-05-14T10:30:00Z",
				ContractID: strategyContract, OperationIndex: 2, TxHash: "abc123",
				Topic: []string{TopicPrefixStrategy, tc.topic1},
				Value: mustB64(t, mapSCVal(t, entries...)),
			}
			flow, err := decodeFlow(ev, tc.kind)
			if err != nil {
				t.Fatalf("decodeFlow: %v", err)
			}
			if flow.Source != SourceName {
				t.Errorf("Source = %q, want %q", flow.Source, SourceName)
			}
			if flow.Direction != tc.wantDir {
				t.Errorf("Direction = %q, want %q", flow.Direction, tc.wantDir)
			}
			if flow.From == "" || flow.From[0] != tc.wantPrefix {
				t.Errorf("From = %q, want prefix %q", flow.From, string(tc.wantPrefix))
			}
			if got := flow.Amount.String(); got != tc.wantAmount {
				t.Errorf("Amount = %q, want %q (no truncation)", got, tc.wantAmount)
			}
			if flow.Ledger != 60_000_000 || flow.OpIndex != 2 || flow.TxHash != "abc123" {
				t.Errorf("header fields not preserved: %+v", flow)
			}
		})
	}
}

func TestDecodeFlow_errors(t *testing.T) {
	t.Parallel()
	from := mapEntry(t, "from", addrSCVal(makeAccountAddress(t, 0xAA)))
	cases := []struct {
		name string
		kind string
		body sdkxdr.ScVal
		want error
	}{
		{"missing amount is ErrMalformedPayload, not a nil-deref", EventDeposit, mapSCVal(t, from), ErrMalformedPayload},
		{"kind classify() would never return", "rebalance", mapSCVal(t), ErrUnknownEvent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := &events.Event{
				ContractID: strategyContract, LedgerClosedAt: "2026-05-14T10:30:00Z",
				Topic: []string{TopicPrefixStrategy, TopicSymbolDeposit}, Value: mustB64(t, tc.body),
			}
			if _, err := decodeFlow(ev, tc.kind); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// Vault topic[1] symbols are shared with the strategy layer, so the reject
// paths mostly cover topic[0] discrimination.
func TestClassifyVault_depositWithdraw(t *testing.T) {
	t.Parallel()
	v := func(sym string) []string { return []string{TopicPrefixVault, sym} }
	runClassify(t, classifyVault, []classifyCase{
		{"vault deposit", v(TopicSymbolDeposit), EventDeposit},
		{"vault withdraw", v(TopicSymbolWithdraw), EventWithdraw},
		{"strategy prefix routes to classify(), not classifyVault()", []string{TopicPrefixStrategy, TopicSymbolDeposit}, ""},
		{"vault prefix encoded as Symbol not String", []string{mustB64Symbol(t, "DeFindexVault"), TopicSymbolDeposit}, ""},
		// Classification-only admin topics (no decoder); n_wasm has no real body sample.
		{"vault rescue", v(TopicSymbolRescue), EventRescue},
		{"vault paused", v(TopicSymbolPaused), EventPaused},
		{"vault unpaused", v(TopicSymbolUnpaused), EventUnpaused},
		{"vault nreceiver", v(TopicSymbolNReceiver), EventNReceiver},
		{"vault nmanager", v(TopicSymbolNManager), EventNManager},
		{"vault nemanager", v(TopicSymbolNEManager), EventNEManager},
		{"vault rbmanager", v(TopicSymbolRBManager), EventRBManager},
		{"vault dfees", v(TopicSymbolDFees), EventDFees},
		{"vault rebalance (multiplexed body)", v(TopicSymbolRebalance), EventRebalance},
		{"vault n_wasm", v(TopicSymbolNWasm), EventNWasm},
		{"single-element topic", []string{TopicPrefixVault}, ""},
	})
}

// Factory events are classified-only: Decode returns (nil, nil) so the
// dispatcher doesn't count them as unmatched.
func TestClassifyFactory_createNfee(t *testing.T) {
	t.Parallel()
	runClassify(t, classifyFactory, []classifyCase{
		{"factory create", []string{TopicPrefixFactory, TopicSymbolCreate}, EventCreate},
		{"factory n_fee", []string{TopicPrefixFactory, TopicSymbolNFee}, EventNFee},
		{"strategy prefix routes to classify(), not classifyFactory()", []string{TopicPrefixStrategy, TopicSymbolCreate}, ""},
		{"vault prefix routes to classifyVault(), not classifyFactory()", []string{TopicPrefixVault, TopicSymbolCreate}, ""},
		{"factory prefix encoded as Symbol not String", []string{mustB64Symbol(t, "DeFindexFactory"), TopicSymbolCreate}, ""},
		{"factory with deposit symbol (wrong topic[1])", []string{TopicPrefixFactory, TopicSymbolDeposit}, ""},
		{"single-element topic", []string{TopicPrefixFactory}, ""},
	})
}

// Real lake bytes of ("DeFindexFactory","create") bodies (r1 raw lake).
const (
	// Ledger 57,057,068: one asset, TWO strategies, both in MainnetStrategies.
	createBodyTwoStrategies = "AAAAEQAAAAEAAAADAAAADwAAAAZhc3NldHMAAAAAABAAAAABAAAAAQAAABEAAAABAAAAAgAAAA8AAAAHYWRkcmVzcwAAAAASAAAAAa3vzlmu5Slo92Bh1JTCUlt1ZZ+kKWpl9JnvKeVkd+SWAAAADwAAAApzdHJhdGVnaWVzAAAAAAAQAAAAAQAAAAIAAAARAAAAAQAAAAMAAAAPAAAAB2FkZHJlc3MAAAAAEgAAAAHDqzFQg2uWEDj8Pmz0XyfQAsmsz8xqj9ZUxEsr93IBXAAAAA8AAAAEbmFtZQAAAA4AAAAYYmxlbmRfYXV0b2NvbXBvdW5kX2ZpeGVkAAAADwAAAAZwYXVzZWQAAAAAAAAAAAAAAAAAEQAAAAEAAAADAAAADwAAAAdhZGRyZXNzAAAAABIAAAABpRv0nN7/BgmC2p24jLhHfz63Ne3Du252JykhAkoy+0gAAAAPAAAABG5hbWUAAAAOAAAAHGJsZW5kX2F1dG9jb21wb3VuZF95aWVsZGJsb3gAAAAPAAAABnBhdXNlZAAAAAAAAAAAAAAAAAAPAAAABXJvbGVzAAAAAAAAEQAAAAEAAAAEAAAAAwAAAAAAAAASAAAAAAAAAAA/yG0JmrdjpOcWUQkJHLRLd1OhvkvZDYFcHc7gVBDUmQAAAAMAAAABAAAAEgAAAAAAAAAAixJCFtWLc+peA9dQXbhNguV6nHi4456Q+b2VWsg3JIUAAAADAAAAAgAAABIAAAAAAAAAAJ8DBa6Ko1Zw7Uo5qB28HTW2ZtZrKsggNIY4eX8/F0FiAAAAAwAAAAMAAAASAAAAAAAAAAANx5WIC2/uT2FiHgSp7KMg/li5+cX+rFbIaNgZKoQ7ygAAAA8AAAAJdmF1bHRfZmVlAAAAAAAAAwAAB9A="
	// Ledger 57,147,588: one asset with an EMPTY strategies Vec (legitimate, observed).
	createBodyZeroStrategies = "AAAAEQAAAAEAAAADAAAADwAAAAZhc3NldHMAAAAAABAAAAABAAAAAQAAABEAAAABAAAAAgAAAA8AAAAHYWRkcmVzcwAAAAASAAAAASAi1W4KumRRb25iYE0pYjK+hk/9+4TVhhPnQjys4CsoAAAADwAAAApzdHJhdGVnaWVzAAAAAAAQAAAAAQAAAAAAAAAPAAAABXJvbGVzAAAAAAAAEQAAAAEAAAAEAAAAAwAAAAAAAAASAAAAAAAAAABuCGdDDiAqa8Ozjwj2jTBN1K57+trQBkkwYN0L5b4o6AAAAAMAAAABAAAAEgAAAAAAAAAAbghnQw4gKmvDs48I9o0wTdSue/ra0AZJMGDdC+W+KOgAAAADAAAAAgAAABIAAAAAAAAAAG4IZ0MOICprw7OPCPaNME3Urnv62tAGSTBg3QvlvijoAAAAAwAAAAMAAAASAAAAAAAAAABuCGdDDiAqa8Ozjwj2jTBN1K57+trQBkkwYN0L5b4o6AAAAA8AAAAJdmF1bHRfZmVlAAAAAAAAAwAAB9A="
	// Ledger 55,484,403, earliest factory: one asset, one strategy ("Blend Strategy").
	createBodyEarliestFactory = "AAAAEQAAAAEAAAADAAAADwAAAAZhc3NldHMAAAAAABAAAAABAAAAAQAAABEAAAABAAAAAgAAAA8AAAAHYWRkcmVzcwAAAAASAAAAASW0/NhZrsL6Y0hDjEibPDwQyYttIb5P08swy2iVPvl3AAAADwAAAApzdHJhdGVnaWVzAAAAAAAQAAAAAQAAAAEAAAARAAAAAQAAAAMAAAAPAAAAB2FkZHJlc3MAAAAAEgAAAAFnf2w30jxNVNqTCwuM6wPG4DaWDNl4NsGuiz8NDKweKQAAAA8AAAAEbmFtZQAAAA4AAAAOQmxlbmQgU3RyYXRlZ3kAAAAAAA8AAAAGcGF1c2VkAAAAAAAAAAAAAAAAAA8AAAAFcm9sZXMAAAAAAAARAAAAAQAAAAQAAAADAAAAAAAAABIAAAAAAAAAAI/sKanankkaQEGC08WiRi97yjWn3C73URmgU+eSxFGvAAAAAwAAAAEAAAASAAAAAAAAAACP7Cmp2p5JGkBBgtPFokYve8o1p9wu91EZoFPnksRRrwAAAAMAAAACAAAAEgAAAAAAAAAAj+wpqdqeSRpAQYLTxaJGL3vKNafcLvdRGaBT55LEUa8AAAADAAAAAwAAABIAAAAAAAAAAI/sKanankkaQEGC08WiRi97yjWn3C73URmgU+eSxFGvAAAADwAAAAl2YXVsdF9mZWUAAAAAAAADAAAAZA=="
)

// The factory is PERMISSIONLESS, so a `create` body's named strategies are
// attacker-controlled: factory events are recognised (Matches true, no
// error, no events) but never seed the registry and never decode the body.
// The registry is bare (factory roots only, curated children withheld), so
// any Has() hit could only come from a body seed.
func TestDecode_factoryEvents_recognisedNeverSeed(t *testing.T) {
	t.Parallel()
	const current = "CDKFHFJIET3A73A2YN4KV7NSV32S6YGQMUFH3DNJXLBWL4SKEGVRNFKI"
	cases := []struct {
		name    string
		factory string
		sym     string
		body    string
		named   []string // strategy addresses the body NAMES
	}{
		{"n_fee", MainnetFactories[0], TopicSymbolNFee, "", nil},
		{"create with non-Map body is not decoded", MainnetFactories[0], TopicSymbolCreate, mustB64(t, i128SCVal(big.NewInt(1))), nil},
		{"two strategies (current factory)", current, TopicSymbolCreate, createBodyTwoStrategies, []string{
			strategyContract,
			"CCSRX5E4337QMCMC3KO3RDFYI57T5NZV5XB3W3TWE4USCASKGL5URKJL",
		}},
		{"zero strategies", current, TopicSymbolCreate, createBodyZeroStrategies, nil},
		{
			"earliest factory", "CAVP2QLPIG7FQNHI57KXF7KS6NIAAUQKHZZDM3AGVADE64WHFBC5YURX", TopicSymbolCreate, createBodyEarliestFactory,
			[]string{"CBTX63BX2I6E2VG2SMFQXDHLAPDOANUWBTMXQNWBV2FT6DIMVQPCSOBW"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := NewDecoder()
			d.reg = contractid.New(contractid.WithFactories(MainnetFactories))
			ev := events.Event{
				Ledger: 60_000_000, ContractID: tc.factory,
				Topic: []string{TopicPrefixFactory, tc.sym}, Value: tc.body,
			}
			if !d.Matches(ev) {
				t.Fatal("Matches = false, want true (canonical factory)")
			}
			out, err := d.Decode(ev)
			if err != nil {
				t.Fatalf("Decode err = %v, want nil (recognised, body untrusted)", err)
			}
			if len(out) != 0 {
				t.Errorf("Decode emitted %d events, want 0", len(out))
			}
			if got := d.reg.Len(); got != 0 {
				t.Errorf("registry grew to %d child(ren); want 0 — bodies must not seed", got)
			}
			for _, named := range tc.named {
				if d.reg.Has(named) {
					t.Errorf("strategy %s was seeded from the create body — permissionless-poisoning path open", named)
				}
				if d.Matches(events.Event{ContractID: named, Topic: []string{TopicPrefixStrategy, TopicSymbolDeposit}}) {
					t.Errorf("named-but-unseeded strategy %s matches its own deposit topic", named)
				}
			}
		})
	}
}

// Legitimate flows survive: curated strategies are seeded by NewDecoder;
// anything else stays fail-closed.
func TestDecode_curatedStrategy_stillRecognised(t *testing.T) {
	t.Parallel()
	d := NewDecoder()
	strategy := MainnetStrategies[0]
	if !d.reg.Has(strategy) {
		t.Fatalf("curated strategy %s not seeded by NewDecoder", strategy)
	}
	if !d.Matches(events.Event{ContractID: strategy, Topic: []string{TopicPrefixStrategy, TopicSymbolDeposit}}) {
		t.Errorf("curated strategy %s deposit topic does not match", strategy)
	}
	attacker := events.Event{
		ContractID: "CATTACKERSTRATEGY00000000000000000000000000000000000000000",
		Topic:      []string{TopicPrefixStrategy, TopicSymbolDeposit},
	}
	if d.Matches(attacker) {
		t.Error("unregistered contract matched a BlendStrategy topic — gate is not fail-closed")
	}
}

// Deposit uses `depositor`/`amounts`/`df_tokens_minted`; withdraw swaps in
// `withdrawer`/`amounts_withdrawn`/`df_tokens_burned`.
func TestDecodeVaultFlow(t *testing.T) {
	t.Parallel()
	amts := func(ns ...int64) sdkxdr.ScVal {
		vals := make([]sdkxdr.ScVal, len(ns))
		for i, n := range ns {
			vals[i] = i128SCVal(big.NewInt(n))
		}
		return vecSCVal(t, vals...)
	}
	cases := []struct {
		name        string
		kind        string
		topic1      string
		userKey     string
		amountsKey  string
		dfKey       string
		user        sdkxdr.ScAddress
		amounts     sdkxdr.ScVal
		df          int64
		wantDir     Direction
		wantPrefix  byte
		wantAmounts []string
		wantDf      string
	}{
		{
			"deposit", EventDeposit, TopicSymbolDeposit, "depositor", "amounts", "df_tokens_minted",
			makeAccountAddress(t, 0xCC), amts(10_000_000), 9_876_543, DirectionDeposit, 'G',
			[]string{"10000000"},
			"9876543",
		},
		{
			"withdraw, two-asset basket", EventWithdraw, TopicSymbolWithdraw, "withdrawer", "amounts_withdrawn", "df_tokens_burned",
			makeAccountAddress(t, 0xDD), amts(5_000_000, 2_500_000), 7_400_000, DirectionWithdraw, 'G',
			[]string{"5000000", "2500000"},
			"7400000",
		},
		{
			"router/aggregator depositor is a C-strkey", EventDeposit, TopicSymbolDeposit, "depositor", "amounts", "df_tokens_minted",
			makeContractAddress(t, 0xEE), amts(1_111_111), 1_000_000, DirectionDeposit, 'C',
			[]string{"1111111"},
			"1000000",
		},
		{
			"empty amounts Vec is legal", EventDeposit, TopicSymbolDeposit, "depositor", "amounts", "df_tokens_minted",
			makeAccountAddress(t, 0xCC), amts(), 0, DirectionDeposit, 'G', nil, "0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ev := &events.Event{
				Type: "contract", Ledger: 60_500_000, LedgerClosedAt: "2026-05-15T08:00:00Z",
				ContractID: vaultContract, OperationIndex: 1, TxHash: "vault-abc",
				Topic: []string{TopicPrefixVault, tc.topic1},
				Value: mustB64(t, mapSCVal(t,
					mapEntry(t, tc.amountsKey, tc.amounts),
					mapEntry(t, tc.dfKey, i128SCVal(big.NewInt(tc.df))),
					mapEntry(t, tc.userKey, addrSCVal(tc.user)),
				)),
			}
			flow, err := decodeVaultFlow(ev, tc.kind)
			if err != nil {
				t.Fatalf("decodeVaultFlow: %v", err)
			}
			if flow.Source != SourceName {
				t.Errorf("Source = %q, want %q", flow.Source, SourceName)
			}
			if flow.Direction != tc.wantDir {
				t.Errorf("Direction = %q, want %q", flow.Direction, tc.wantDir)
			}
			if flow.User == "" || flow.User[0] != tc.wantPrefix {
				t.Errorf("User = %q, want prefix %q", flow.User, string(tc.wantPrefix))
			}
			if len(flow.Amounts) != len(tc.wantAmounts) {
				t.Fatalf("len(Amounts) = %d, want %d", len(flow.Amounts), len(tc.wantAmounts))
			}
			for i, want := range tc.wantAmounts {
				if got := flow.Amounts[i].String(); got != want {
					t.Errorf("Amounts[%d] = %q, want %q", i, got, want)
				}
			}
			if got := flow.DfTokens.String(); got != tc.wantDf {
				t.Errorf("DfTokens = %q, want %q", got, tc.wantDf)
			}
			if flow.Ledger != 60_500_000 || flow.OpIndex != 1 || flow.TxHash != "vault-abc" {
				t.Errorf("header fields not preserved: %+v", flow)
			}
		})
	}
}

func TestDecodeVaultFlow_missingField(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		ContractID:     vaultContract,
		LedgerClosedAt: "2026-05-15T08:00:00Z",
		Topic:          []string{TopicPrefixVault, TopicSymbolDeposit},
		Value:          mustB64(t, mapSCVal(t, mapEntry(t, "depositor", addrSCVal(makeAccountAddress(t, 0xCC))))),
	}
	if _, err := decodeVaultFlow(ev, EventDeposit); !errors.Is(err, ErrMalformedPayload) {
		t.Errorf("err = %v, want ErrMalformedPayload", err)
	}
}

func vecSCVal(t *testing.T, elts ...sdkxdr.ScVal) sdkxdr.ScVal {
	t.Helper()
	vec := sdkxdr.ScVec(elts)
	pv := &vec
	return sdkxdr.ScVal{Type: sdkxdr.ScValTypeScvVec, Vec: &pv}
}

// SCVal builders (per-package; the production package exports none).

func i128SCVal(n *big.Int) sdkxdr.ScVal {
	abs := new(big.Int).Set(n)
	if abs.Sign() < 0 {
		abs.Neg(abs)
	}
	bytes := abs.Bytes()
	for len(bytes) < 16 {
		bytes = append([]byte{0}, bytes...)
	}
	hi := int64(0)
	for i := 0; i < 8; i++ {
		hi = (hi << 8) | int64(bytes[i])
	}
	lo := uint64(0)
	for i := 8; i < 16; i++ {
		lo = (lo << 8) | uint64(bytes[i])
	}
	if n.Sign() < 0 {
		hi = ^hi
		lo = ^lo + 1
		if lo == 0 {
			hi++
		}
	}
	return sdkxdr.ScVal{
		Type: sdkxdr.ScValTypeScvI128,
		I128: &sdkxdr.Int128Parts{
			Hi: sdkxdr.Int64(hi),
			Lo: sdkxdr.Uint64(lo),
		},
	}
}

func addrSCVal(addr sdkxdr.ScAddress) sdkxdr.ScVal {
	return sdkxdr.ScVal{Type: sdkxdr.ScValTypeScvAddress, Address: &addr}
}

func makeAccountAddress(t *testing.T, fillByte byte) sdkxdr.ScAddress {
	t.Helper()
	var ed25519 sdkxdr.Uint256
	for i := range ed25519 {
		ed25519[i] = fillByte
	}
	acct := sdkxdr.AccountId{
		Type:    sdkxdr.PublicKeyTypePublicKeyTypeEd25519,
		Ed25519: &ed25519,
	}
	return sdkxdr.ScAddress{Type: sdkxdr.ScAddressTypeScAddressTypeAccount, AccountId: &acct}
}

func makeContractAddress(t *testing.T, fillByte byte) sdkxdr.ScAddress {
	t.Helper()
	var cid sdkxdr.ContractId
	for i := range cid {
		cid[i] = fillByte
	}
	return sdkxdr.ScAddress{Type: sdkxdr.ScAddressTypeScAddressTypeContract, ContractId: &cid}
}

func mapEntry(t *testing.T, key string, val sdkxdr.ScVal) sdkxdr.ScMapEntry {
	t.Helper()
	sym := sdkxdr.ScSymbol(key)
	keySv := sdkxdr.ScVal{Type: sdkxdr.ScValTypeScvSymbol, Sym: &sym}
	return sdkxdr.ScMapEntry{Key: keySv, Val: val}
}

func mapSCVal(t *testing.T, entries ...sdkxdr.ScMapEntry) sdkxdr.ScVal {
	t.Helper()
	m := sdkxdr.ScMap(entries)
	pm := &m
	return sdkxdr.ScVal{Type: sdkxdr.ScValTypeScvMap, Map: &pm}
}

func mustB64(t *testing.T, sv sdkxdr.ScVal) string {
	t.Helper()
	bs, err := sv.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal scval: %v", err)
	}
	return base64.StdEncoding.EncodeToString(bs)
}

func mustB64String(t *testing.T, s string) string {
	t.Helper()
	xs := sdkxdr.ScString(s)
	return mustB64(t, sdkxdr.ScVal{Type: sdkxdr.ScValTypeScvString, Str: &xs})
}

func mustB64Symbol(t *testing.T, s string) string {
	t.Helper()
	return mustB64(t, symSCVal(s))
}

func symSCVal(s string) sdkxdr.ScVal {
	sym := sdkxdr.ScSymbol(s)
	return sdkxdr.ScVal{Type: sdkxdr.ScValTypeScvSymbol, Sym: &sym}
}

// The namespaced topic strings are shared by every pubnet contract, so a
// perfect topic shape from an unregistered contract must NOT be attributed
// to defindex; the same event from a curated vault / strategy / factory must.
func TestDecoder_GateRejectsForeignContract(t *testing.T) {
	t.Parallel()
	d := NewDecoder()

	vaultTopics := []string{TopicPrefixVault, TopicSymbolDeposit}
	strategyTopics := []string{TopicPrefixStrategy, TopicSymbolDeposit}
	factoryTopics := []string{TopicPrefixFactory, TopicSymbolCreate}

	foreign := "CFOREIGNFAKEVAULT000000000000000000000000000000000000000"
	for name, ev := range map[string]events.Event{
		"vault shape":    {ContractID: foreign, Topic: vaultTopics},
		"strategy shape": {ContractID: foreign, Topic: strategyTopics},
		"factory shape":  {ContractID: foreign, Topic: factoryTopics},
	} {
		if d.Matches(ev) {
			t.Fatalf("foreign contract with defindex-shaped topics (%s) matched — the CS-026 injection vector is open", name)
		}
	}

	// Real emitter with the DeFindexVault shape and none of the provenance
	// proofs (docs/protocols/defindex.md): stays excluded until verified.
	flagged := events.Event{
		ContractID: "CBGCGVKHVA4TG6MGQ3XTOEHEJXK4DYLOKTMR4UT4PZFPTQKLYXYRF6KV",
		Topic:      vaultTopics,
	}
	if d.Matches(flagged) {
		t.Fatal("flagged unverified emitter matched — it must fail-close into a recognition gap")
	}

	if !d.Matches(events.Event{ContractID: MainnetVaults[0], Topic: vaultTopics}) {
		t.Fatal("curated vault failed to match — gate is over-closed")
	}
	if !d.Matches(events.Event{ContractID: MainnetStrategies[0], Topic: strategyTopics}) {
		t.Fatal("curated strategy failed to match — gate is over-closed")
	}
	for _, f := range MainnetFactories {
		if !d.Matches(events.Event{ContractID: f, Topic: factoryTopics}) {
			t.Fatalf("canonical factory %s failed to match", f)
		}
	}
	// A factory is a trust root, not a child.
	if d.Matches(events.Event{ContractID: MainnetFactories[0], Topic: vaultTopics}) {
		t.Fatal("factory address matched a vault flow shape — factory and child sets must stay separate")
	}
}

// A newly verified vault is admitted via the protocol_contracts warm with no code change.
func TestDecoder_OperatorSeedAdmitsNewVault(t *testing.T) {
	t.Parallel()
	newVault := "CNEWLYVERIFIEDVAULT0000000000000000000000000000000000000"
	ev := events.Event{ContractID: newVault, Topic: []string{TopicPrefixVault, TopicSymbolDeposit}}

	if NewDecoder().Matches(ev) {
		t.Fatal("unseeded vault matched")
	}
	if !NewDecoder(contractid.WithSeed([]string{newVault})).Matches(ev) {
		t.Fatal("protocol_contracts-seeded vault failed to match")
	}
}

// The 19 Dune-registry vaults emit deposit/withdraw; gate membership is the
// only thing that could drop them (docs/protocols/defindex.md).
func TestDecoder_DuneRegistryVaultsGated(t *testing.T) {
	t.Parallel()
	d := NewDecoder()
	for _, vault := range []string{
		"CA25XTGHKQ6PUMFJ4SDNRFMUABIFX46U7VAZBFDZKAOX5C3KZXUAR2KQ",
		"CAGERKFCDHHCES64L43EU242KIVQMPYAL37CFYIGMLBGJIQTYWXFRWIT",
		"CAIFV6BSPN2UHGDSOJK7RLOEVBLQX6EAGIVJWVWSEI7ROLUGI3U2XDTP",
		"CAQ6PAG4X6L7LJVGOKSQ6RU2LADWK4EQXRJGMUWL7SECS7LXUEQLM5U7",
		"CAVL4BSHMU5ECWZCB6ETYSBV4EWTRMHAGMVUEJ5PXM3P3E3AOJPX2TLU",
		"CAXRLUOSI7DL3SYNZW5UGRIPVNRKKSZTW35OX5DWKZSJ4PFEVA2VEFCQ",
		"CBDZ2L4HHEPPL4ABHPORQC72E5S2GLNRPJ467XV3CW5FDWICUNH6SF4B",
		"CBDZYJVQJQT7QJ7ZTMGNGZ7RR3DF32LERLZ26A2HLW5FNJ4OOZCLI3OG",
		"CBGE43WF5GBDCHMN2XPKIAC7TYMWCR6FOJTVFMBR6QQM6WKZB7BM23LL",
		"CBHB2G4TMSVWE4YFDTFYRYNCP5KUT6RQVWQGIM4LQO2IKKHVDB7N5JJQ",
		"CCDRFMZ7CH364ATQ5YSVTEJ3G3KPNFVM6TTC6N4T5REHWJS6LGVFP7MY",
		"CCFWKCD52JNSQLN5OS4F7EG6BPDT4IRJV6KODIEIZLWPM35IKHOKT6S2",
		"CCKTLDG6I2MMJCKFWXXBXMA42LJ3XN2IOW6M7TK6EWNPJTS736ETFF2N",
		"CCPKQH3K5XUGP5GXCT6WTABS7TGXRR745BJ4MEFSGNATB7AOBRL4VEOT",
		"CDIHXKZ4PFKAIONK52JAR6ZNMP62F3UP7XTIBSJTQLMLHQ44PQ5Q2H3J",
		"CDKNDBBVLTSO2DSLTZOIF2A4NJWPXTGHD3WYSWBHYBJDKAX4JCKEFMHT",
		"CDONBLOOTYZ7QN62ZLJFHK7CT3JCP3JEZDCRSG3VLGAP73QAXS7HF6HU",
		"CDPJEMZOYZLITC4MRLGJQHPMNCIB3TZ4R42J6M37PWP5Q2FGO4WFIXAD",
		"CDRSZ4OGRVUU5ONTI6C6UNF5QFJ3OGGQCNTC5UXXTZQFVRTILJFSVG5D",
	} {
		for _, sym := range []string{TopicSymbolDeposit, TopicSymbolWithdraw} {
			if !d.Matches(events.Event{ContractID: vault, Topic: []string{TopicPrefixVault, sym}}) {
				t.Errorf("Dune-registry vault %s not gated for %s", vault, sym)
			}
		}
	}
}

// A registered strategy's harvest emits one DirectionHarvest StrategyFlow
// end to end through Decode (the real body also carries price_per_share).
func TestDecode_strategyHarvestDecodes(t *testing.T) {
	t.Parallel()
	d := NewDecoder()
	ev := events.Event{
		ContractID:     MainnetStrategies[0],
		Ledger:         63_783_690,
		LedgerClosedAt: "2026-08-01T10:30:00Z",
		TxHash:         "harvesttx3",
		Topic:          []string{TopicPrefixStrategy, TopicSymbolHarvest},
		Value: mustB64(t, mapSCVal(t,
			mapEntry(t, "amount", i128SCVal(big.NewInt(915_806))),
			mapEntry(t, "from", addrSCVal(makeAccountAddress(t, 0xDD))),
			mapEntry(t, "price_per_share", i128SCVal(big.NewInt(1))),
		)),
	}
	if !d.Matches(ev) {
		t.Fatal("Matches(strategy harvest) = false, want true")
	}
	out, err := d.Decode(ev)
	if err != nil {
		t.Fatalf("Decode(strategy harvest) err = %v, want a decoded flow", err)
	}
	if len(out) != 1 {
		t.Fatalf("Decode(strategy harvest) emitted %d events, want 1", len(out))
	}
	if fe := out[0].(Event); fe.Flow.Direction != DirectionHarvest {
		t.Errorf("Direction = %q, want harvest", fe.Flow.Direction)
	}
}

// Unmodelled vault topics are recognised (Matches true) and emit nothing
// without erroring. dfees and the admin topics graduated out of this set.
func TestDecode_vaultUnmodelledRecognisedEmit0Events(t *testing.T) {
	t.Parallel()
	d := NewDecoder()
	for name, sym := range map[string]string{"rebalance": TopicSymbolRebalance, "n_wasm": TopicSymbolNWasm} {
		t.Run(name, func(t *testing.T) {
			ev := events.Event{ContractID: MainnetVaults[0], Topic: []string{TopicPrefixVault, sym}}
			if !d.Matches(ev) {
				t.Fatalf("Matches(vault %s) = false, want true", name)
			}
			out, err := d.Decode(ev)
			if err != nil {
				t.Errorf("Decode(vault %s) err = %v, want nil (recognised, unmodelled)", name, err)
			}
			if len(out) != 0 {
				t.Errorf("Decode(vault %s) emitted %d events, want 0", name, len(out))
			}
		})
	}
}

// The decoder reads `rebalance_method` verbatim; Known() classifies the four
// documented methods. The per-method payload is unmodelled.
func TestDecodeRebalanceMethod(t *testing.T) {
	t.Parallel()
	body := func(key string, v sdkxdr.ScVal) *events.Event {
		return &events.Event{Value: mustB64(t, mapSCVal(t, mapEntry(t, key, v)))}
	}

	for _, want := range []RebalanceMethod{RebalanceUnwind, RebalanceInvest, RebalanceSwapExactIn, RebalanceSwapExactOut} {
		t.Run("documented/"+string(want), func(t *testing.T) {
			got, err := DecodeRebalanceMethod(body(RebalanceMethodField, symSCVal(string(want))))
			if err != nil {
				t.Fatalf("DecodeRebalanceMethod: %v", err)
			}
			if got != want || !got.Known() {
				t.Errorf("method = %q (Known=%v), want %q known", got, got.Known(), want)
			}
		})
	}

	t.Run("unknown method is read verbatim but not Known", func(t *testing.T) {
		got, err := DecodeRebalanceMethod(body(RebalanceMethodField, symSCVal("some_future_method")))
		if err != nil {
			t.Fatalf("DecodeRebalanceMethod: %v", err)
		}
		if got != RebalanceMethod("some_future_method") || got.Known() {
			t.Errorf("method = %q (Known=%v), want verbatim and unknown", got, got.Known())
		}
	})

	for name, ev := range map[string]*events.Event{
		"missing discriminator field": body("not_the_field", symSCVal("unwind")),
		"discriminator not a Symbol":  body(RebalanceMethodField, i128SCVal(big.NewInt(7))),
	} {
		t.Run(name+" is ErrMalformedPayload", func(t *testing.T) {
			if _, err := DecodeRebalanceMethod(ev); !errors.Is(err, ErrMalformedPayload) {
				t.Errorf("err = %v, want ErrMalformedPayload", err)
			}
		})
	}
}

// dfees bodies are real lake bytes (r1 ClickHouse, 12,785 events on 27 vaults):
//
//	Map{ distributed_fees: Vec[ (token Address<contract>, amount i128) ] }
//
// PER-ASSET, not per-recipient.
const (
	// (CD6M4R23…BCIS, 37).
	dfeesBodyOneEntry37 = "AAAAEQAAAAEAAAABAAAADwAAABBkaXN0cmlidXRlZF9mZWVzAAAAEAAAAAEAAAABAAAAEAAAAAEAAAACAAAAEgAAAAH8zkdb1oOBY0ttmf48gYAh7cgbHRK0bXzWu05molskIAAAAAoAAAAAAAAAAAAAAAAAAAAl"
	// (CDTKPWPL…BQLV, 64).
	dfeesBodyOneEntry64 = "AAAAEQAAAAEAAAABAAAADwAAABBkaXN0cmlidXRlZF9mZWVzAAAAEAAAAAEAAAABAAAAEAAAAAEAAAACAAAAEgAAAAHmp9nrdSMAakaap0g60RByR0Q8DYLmJ2PeZwhIxOl8kAAAAAoAAAAAAAAAAAAAAAAAAABA"
	// (CCW67TSZ…MI75, USDC's SAC, 7686).
	dfeesBodyUSDC = "AAAAEQAAAAEAAAABAAAADwAAABBkaXN0cmlidXRlZF9mZWVzAAAAEAAAAAEAAAABAAAAEAAAAAEAAAACAAAAEgAAAAGt785ZruUpaPdgYdSUwlJbdWWfpClqZfSZ7ynlZHfklgAAAAoAAAAAAAAAAAAAAAAAAB4G"
	// EMPTY distributed_fees Vec: a real distribution with nothing to distribute.
	dfeesBodyEmptyVec = "AAAAEQAAAAEAAAABAAAADwAAABBkaXN0cmlidXRlZF9mZWVzAAAAEAAAAAEAAAAA"
)

// Real captured bodies through the production seams (Matches gate + Decode),
// pinning token, amount, kind and indices.
func TestDecode_dfeesRealLakeBytes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		body       string
		wantToken  string
		wantAmount string
	}{
		{"one entry, amount 37", dfeesBodyOneEntry37, "CD6M4R2322BYCY2LNWM74PEBQAQ63SA3DUJLI3L4225U4ZVCLMSCBCIS", "37"},
		{"one entry, amount 64", dfeesBodyOneEntry64, "CDTKPWPLOURQA2SGTKTUQOWRCBZEORB4BWBOMJ3D3ZTQQSGE5F6JBQLV", "64"},
		{"USDC SAC, amount 7686", dfeesBodyUSDC, "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75", "7686"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := NewDecoder()
			ev := events.Event{
				Type: "contract", ContractID: MainnetVaults[0], Ledger: 60_903_337,
				LedgerClosedAt: "2026-08-01T00:00:00Z", TxHash: "dfeestx", OperationIndex: 0,
				EventIndex: 5,
				Topic:      []string{TopicPrefixVault, TopicSymbolDFees},
				Value:      tc.body,
			}
			if !d.Matches(ev) {
				t.Fatal("Matches(vault dfees) = false, want true (curated vault)")
			}
			out, err := d.Decode(ev)
			if err != nil {
				t.Fatalf("Decode(dfees) err = %v, want decoded fee entries", err)
			}
			if len(out) != 1 {
				t.Fatalf("Decode(dfees) emitted %d events, want 1 (one per distributed_fees entry)", len(out))
			}
			fe, ok := out[0].(DFeesEvent)
			if !ok {
				t.Fatalf("emitted %T, want defindex.DFeesEvent", out[0])
			}
			if got, want := fe.EventKind(), "defindex.vault.dfees"; got != want {
				t.Errorf("EventKind = %q, want %q", got, want)
			}
			if fe.Fee.Token != tc.wantToken {
				t.Errorf("Token = %q, want %q", fe.Fee.Token, tc.wantToken)
			}
			if got := fe.Fee.Amount.String(); got != tc.wantAmount {
				t.Errorf("Amount = %q, want %q (no truncation, ADR-0003)", got, tc.wantAmount)
			}
			if fe.Fee.FeeIndex != 0 {
				t.Errorf("FeeIndex = %d, want 0", fe.Fee.FeeIndex)
			}
			if fe.Fee.EventIndex != 5 {
				t.Errorf("EventIndex = %d, want 5 (propagated from the event)", fe.Fee.EventIndex)
			}
			if fe.Fee.Vault != MainnetVaults[0] || fe.Fee.Ledger != 60_903_337 ||
				fe.Fee.OpIndex != 0 || fe.Fee.TxHash != "dfeestx" {
				t.Errorf("header fields not preserved: %+v", fe.Fee)
			}
		})
	}
}

// The real empty-Vec body is recognised, emits 0 events and no error (not
// ErrMalformedPayload), keeping live decode count-consistent with the
// ADR-0033 completeness re-derive.
func TestDecode_dfeesEmptyVecEmitsZeroEventsNoError(t *testing.T) {
	t.Parallel()
	d := NewDecoder()
	ev := events.Event{
		Type:           "contract",
		ContractID:     MainnetVaults[0],
		Ledger:         60_903_337,
		LedgerClosedAt: "2026-08-01T00:00:00Z",
		TxHash:         "dfeestx-empty",
		Topic:          []string{TopicPrefixVault, TopicSymbolDFees},
		Value:          dfeesBodyEmptyVec,
	}
	if !d.Matches(ev) {
		t.Fatal("Matches(vault dfees) = false, want true")
	}
	out, err := d.Decode(ev)
	if err != nil {
		t.Errorf("Decode(empty dfees) err = %v, want nil (real observed shape)", err)
	}
	if len(out) != 0 {
		t.Errorf("Decode(empty dfees) emitted %d events, want 0", len(out))
	}
}

// Synthetic fan-out: entry i becomes the event with FeeIndex = i; a future
// vault upgrade appending a tuple element must not error the event.
func TestDecode_dfeesFanOut(t *testing.T) {
	t.Parallel()
	pair := func(b byte, amount int64, extra ...sdkxdr.ScVal) sdkxdr.ScVal {
		return vecSCVal(t, append([]sdkxdr.ScVal{addrSCVal(makeContractAddress(t, b)), i128SCVal(big.NewInt(amount))}, extra...)...)
	}
	cases := []struct {
		name    string
		entries []sdkxdr.ScVal
		want    []string
	}{
		{"two entries keep Vec order", []sdkxdr.ScVal{pair(0xA1, 11), pair(0xB2, 22)}, []string{"11", "22"}},
		{"additive third tuple element is ignored", []sdkxdr.ScVal{pair(0xA1, 33, i128SCVal(big.NewInt(99)))}, []string{"33"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ev := events.Event{
				ContractID: MainnetVaults[0], Ledger: 61_000_000, LedgerClosedAt: "2026-08-02T00:00:00Z",
				TxHash: "dfeestx-two", EventIndex: 5,
				Topic: []string{TopicPrefixVault, TopicSymbolDFees},
				Value: mustB64(t, mapSCVal(t, mapEntry(t, "distributed_fees", vecSCVal(t, tc.entries...)))),
			}
			out, err := NewDecoder().Decode(ev)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if len(out) != len(tc.want) {
				t.Fatalf("emitted %d events, want %d (one per entry)", len(out), len(tc.want))
			}
			tokens := map[string]bool{}
			for i, want := range tc.want {
				fe, ok := out[i].(DFeesEvent)
				if !ok {
					t.Fatalf("out[%d] is %T, want defindex.DFeesEvent", i, out[i])
				}
				if fe.Fee.FeeIndex != i {
					t.Errorf("out[%d].FeeIndex = %d, want %d (Vec position)", i, fe.Fee.FeeIndex, i)
				}
				if got := fe.Fee.Amount.String(); got != want {
					t.Errorf("out[%d].Amount = %q, want %q", i, got, want)
				}
				if fe.Fee.Token == "" || fe.Fee.Token[0] != 'C' {
					t.Errorf("out[%d].Token = %q, want a C-strkey token contract", i, fe.Fee.Token)
				}
				if fe.Fee.EventIndex != 5 {
					t.Errorf("out[%d].EventIndex = %d, want 5", i, fe.Fee.EventIndex)
				}
				tokens[fe.Fee.Token] = true
			}
			if len(tokens) != len(tc.want) {
				t.Errorf("decoded %d distinct tokens for %d entries — per-entry pairing broken", len(tokens), len(tc.want))
			}
		})
	}
}

// A broken dfees body is a genuine decode error, not a silent drop.
func TestDecode_dfeesMalformedBodyErrors(t *testing.T) {
	t.Parallel()
	fees := func(v ...sdkxdr.ScVal) sdkxdr.ScVal {
		return mapSCVal(t, mapEntry(t, "distributed_fees", vecSCVal(t, v...)))
	}
	one, two := i128SCVal(big.NewInt(1)), i128SCVal(big.NewInt(2))
	cases := []struct {
		name string
		body sdkxdr.ScVal
	}{
		{"map missing distributed_fees", mapSCVal(t, mapEntry(t, "not_the_field", vecSCVal(t)))},
		{"distributed_fees not a Vec", mapSCVal(t, mapEntry(t, "distributed_fees", i128SCVal(big.NewInt(7))))},
		{"entry not a 2-tuple", fees(vecSCVal(t, one))},
		{"entry token not an Address", fees(vecSCVal(t, one, two))},
		{"body not a Map at all", one},
	}
	d := NewDecoder()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := events.Event{
				ContractID:     MainnetVaults[0],
				LedgerClosedAt: "2026-08-02T00:00:00Z",
				Topic:          []string{TopicPrefixVault, TopicSymbolDFees},
				Value:          mustB64(t, tc.body),
			}
			if _, err := d.Decode(ev); !errors.Is(err, ErrMalformedPayload) {
				t.Errorf("err = %v, want ErrMalformedPayload", err)
			}
		})
	}
}

// TestDecode_vaultAdminMalformed: a modelled admin topic whose body lacks
// or mistypes a required field is a decode error, never a partial row.
func TestDecode_vaultAdminMalformed(t *testing.T) {
	t.Parallel()
	d := NewDecoder()
	g := addrSCVal(makeAccountAddress(t, 0xAA))
	c := addrSCVal(makeContractAddress(t, 0xBB))
	cases := map[string]struct {
		sym  string
		body string
	}{
		"nmanager missing new_manager": {TopicSymbolNManager, mustB64(t, mapSCVal(t, mapEntry(t, "manager", g)))},
		"nreceiver missing caller":     {TopicSymbolNReceiver, mustB64(t, mapSCVal(t, mapEntry(t, "new_fee_receiver", g)))},
		"paused strategy not address":  {TopicSymbolPaused, mustB64(t, mapSCVal(t, mapEntry(t, "caller", g), mapEntry(t, "strategy_address", symSCVal("x"))))},
		"rescue missing amount": {TopicSymbolRescue, mustB64(t, mapSCVal(t,
			mapEntry(t, "caller", g), mapEntry(t, "strategy_address", c)))},
		"rescue negative amount": {TopicSymbolRescue, mustB64(t, mapSCVal(t,
			mapEntry(t, "amount_withdrawn", i128SCVal(big.NewInt(-1))),
			mapEntry(t, "caller", g), mapEntry(t, "strategy_address", c)))},
		"body not a map": {TopicSymbolRBManager, mustB64(t, g)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ev := events.Event{
				ContractID:     MainnetVaults[0],
				Ledger:         61_000_000,
				LedgerClosedAt: "2026-01-01T00:00:00Z",
				TxHash:         "admintx",
				Topic:          []string{TopicPrefixVault, tc.sym},
				Value:          tc.body,
			}
			out, err := d.Decode(ev)
			if !errors.Is(err, ErrMalformedPayload) {
				t.Errorf("err = %v, want ErrMalformedPayload", err)
			}
			if len(out) != 0 {
				t.Errorf("emitted %d events, want 0", len(out))
			}
		})
	}
}
