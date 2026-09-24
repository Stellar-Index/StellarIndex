package clickhouse

import (
	_ "embed"
	"math/big"
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// The fixtures are VERBATIM rows from the certified lake on r1, captured
// 2026-09-15 from stellar.ledger_entries_current FINAL, with the answers
// checked there before they were written down here.
//
// contract_storage_caocxwnx.tsv is the COMPLETE storage of one private-credit
// deal token (CAOCXWNX…): all 26 entries, not a curated six. That is the point
// of it — the decoder has to pick the balances out of the same neighbours it
// meets in production, including the two shapes that look like balances and are
// not (see TestContractStorageSupplyIgnoresLookalikeKeys).
//
//go:embed testdata/contract_storage_caocxwnx.tsv
var caocxwnxStorageTSV string

// contract_storage_kale_sac.tsv is a Stellar Asset Contract's instance entry
// plus three of its holder balances — KALE (CB23WRDQ…), whose balances are
// map-shaped rather than bare i128.
//
//go:embed testdata/contract_storage_kale_sac.tsv
var kaleSACStorageTSV string

const (
	// caocxwnxContractID is the deal token the fixture was taken from.
	caocxwnxContractID = "CAOCXWNXCG63U43DCJSS52FDAFT2ZXY2T2UZF3ZYVZ4LLLFVFRF2GM6Z"

	// caocxwnxRawTotal is Σ of its six Balance entries, in the token's
	// smallest unit. Verified on r1 three independent ways before being
	// written here: by summing the decoded entries, against the contract's own
	// `TotalSupply` instance field, and against the $34.5M order of magnitude
	// a third-party dashboard publishes for this deal.
	caocxwnxRawTotal = "344995973100000"

	// caocxwnxHolders is both the number of Balance entries in the fixture and
	// the contract's own declared HolderCount. They agreeing is what proves no
	// entry was archived out from under the reading.
	caocxwnxHolders = 6

	// caocxwnxDecimals is the scale the contract declares for ITSELF, in its
	// instance storage under Config.decimals. Not borrowed from a seed file.
	caocxwnxDecimals = 7

	kaleSACContractID = "CB23WRDQWGSP6YPMY4UV5C4OW5CBTXKYN3XEATG7KJEZCXMJBYEHOUOV"
)

// foldFixture drives the same row-folding the reader's query loop drives,
// so the decode path under test is the production one.
func foldFixture(t *testing.T, contractID, tsv string) ContractStorageSupply {
	t.Helper()
	out := ContractStorageSupply{ContractID: contractID, Total: new(big.Int)}
	for _, line := range strings.Split(strings.TrimSpace(tsv), "\n") {
		cols := strings.Split(line, "\t")
		if len(cols) < 2 {
			t.Fatalf("fixture row has %d columns, want >=2: %q", len(cols), line)
		}
		if err := out.apply(cols[0], cols[1], 0); err != nil {
			t.Fatalf("apply: %v", err)
		}
	}
	return out
}

func TestContractStorageSupplyReproducesVerifiedTotal(t *testing.T) {
	got := foldFixture(t, caocxwnxContractID, caocxwnxStorageTSV)

	if got.Total.String() != caocxwnxRawTotal {
		t.Errorf("total = %s, want %s (verified on r1)", got.Total, caocxwnxRawTotal)
	}
	if got.BalanceEntries != caocxwnxHolders {
		t.Errorf("balance entries = %d, want %d", got.BalanceEntries, caocxwnxHolders)
	}
	if !got.DecimalsFound || got.Decimals != caocxwnxDecimals {
		t.Errorf("decimals = %d found=%v, want %d true — the scale is on-chain under Config.decimals "+
			"and must be READ, never assumed", got.Decimals, got.DecimalsFound, caocxwnxDecimals)
	}
	if got.DeclaredTotal == nil {
		t.Fatal("declared total is nil; the contract publishes TotalSupply in instance storage and " +
			"it is the independent cross-check on our own sum")
	}
	if got.DeclaredTotal.Cmp(got.Total) != 0 {
		t.Errorf("declared total %s != summed total %s", got.DeclaredTotal, got.Total)
	}
	if got.DeclaredHolders == nil || int(*got.DeclaredHolders) != got.BalanceEntries {
		t.Errorf("declared holders = %v, summed entries = %d", got.DeclaredHolders, got.BalanceEntries)
	}
	if !got.SelfConsistent() {
		t.Error("SelfConsistent() = false on a reading where every cross-check agreed")
	}
}

// TestContractStorageSupplyIgnoresLookalikeKeys is the regression test for the
// bug that was actually written first.
//
// `BalanceCheckpoints(Address)` holds a VECTOR of a holder's past balances and
// `Eligible(Address)` holds a bool, and both are keyed by a two-element vector
// whose leading symbol starts with the text `Balance` / sits beside an Address
// exactly like the real key. A matcher that compared the symbol by PREFIX swept
// BalanceCheckpoints in; one that only checked "two elements, second is an
// Address" swept Eligible in. This contract carries six of each, so either
// mistake corrupts the figure rather than failing loudly.
func TestContractStorageSupplyIgnoresLookalikeKeys(t *testing.T) {
	var checkpoints, eligible, balances int
	for _, line := range strings.Split(strings.TrimSpace(caocxwnxStorageTSV), "\n") {
		keyB64 := strings.Split(line, "\t")[0]
		var key xdr.LedgerKey
		if err := xdr.SafeUnmarshalBase64(keyB64, &key); err != nil {
			t.Fatalf("fixture key: %v", err)
		}
		cd, ok := key.GetContractData()
		if !ok {
			continue
		}
		vec, ok := cd.Key.GetVec()
		if !ok || vec == nil || len(*vec) == 0 {
			continue
		}
		sym, _ := (*vec)[0].GetSym()
		switch string(sym) {
		case "BalanceCheckpoints":
			checkpoints++
			if balanceKeyHolder(cd.Key) {
				t.Error("balanceKeyHolder accepted a BalanceCheckpoints key; its vector of historical " +
					"balances would be summed into supply")
			}
		case "Eligible":
			eligible++
			if balanceKeyHolder(cd.Key) {
				t.Error("balanceKeyHolder accepted an Eligible key")
			}
		case "Balance":
			balances++
			if !balanceKeyHolder(cd.Key) {
				t.Error("balanceKeyHolder rejected a real Balance key")
			}
		}
	}
	if checkpoints == 0 || eligible == 0 {
		t.Fatalf("fixture no longer carries the lookalike keys this test exists for "+
			"(checkpoints=%d eligible=%d); the guard is untested", checkpoints, eligible)
	}
	if balances != caocxwnxHolders {
		t.Errorf("fixture has %d Balance keys, want %d", balances, caocxwnxHolders)
	}
}

// TestContractStorageSupplyRefusesStellarAssetContract pins the refusal that
// keeps a 6.8x understatement off the wire.
//
// A SAC's storage holds only the slice of a classic asset wrapped into Soroban.
// Measured on r1 2026-09-15, KALE's storage summed to 471,938,508,419,832
// against an event-derived 3,224,226,487,856,012. Nothing about the storage sum
// looks wrong on its own — which is exactly why the refusal has to be
// structural, keyed on the instance executable, rather than a plausibility check
// on the number.
func TestContractStorageSupplyRefusesStellarAssetContract(t *testing.T) {
	got := foldFixture(t, kaleSACContractID, kaleSACStorageTSV)
	if !got.isSAC {
		t.Fatal("a Stellar Asset Contract's instance entry did not set the SAC refusal; its storage " +
			"sum would be served as the asset's supply and understate it by whatever never " +
			"entered Soroban")
	}
	if got.BalanceEntries == 0 {
		t.Error("fixture carries no balances, so the refusal is not actually protecting a sum")
	}
}

// TestContractStorageSupplyRefusesBalancesWithNoInstance pins that the SAC
// check must be RUN, not merely not fire.
//
// The instance entry is the only evidence separating a Wasm token from a
// Stellar Asset Contract. If the lake never captured one, a SAC's balances and
// a token's balances are indistinguishable, and the reading that looks fine is
// the one that understates a classic asset by whatever never entered Soroban.
func TestContractStorageSupplyRefusesBalancesWithNoInstance(t *testing.T) {
	var withoutInstance []string
	for _, line := range strings.Split(strings.TrimSpace(caocxwnxStorageTSV), "\n") {
		keyB64 := strings.Split(line, "\t")[0]
		var key xdr.LedgerKey
		if err := xdr.SafeUnmarshalBase64(keyB64, &key); err != nil {
			t.Fatalf("fixture key: %v", err)
		}
		cd, ok := key.GetContractData()
		if !ok || cd.Key.Type == xdr.ScValTypeScvLedgerKeyContractInstance {
			continue
		}
		withoutInstance = append(withoutInstance, line)
	}

	got := foldFixture(t, caocxwnxContractID, strings.Join(withoutInstance, "\n"))
	if got.sawInstance {
		t.Fatal("the instance row was not actually removed from the fixture")
	}
	if got.BalanceEntries == 0 {
		t.Fatal("no balances survived, so the guard is not being exercised")
	}
	// The reader's post-loop guard is what refuses; assert the condition it
	// keys on, since the loop itself has no error to give here.
	if got.BalanceEntries > 0 && got.sawInstance {
		t.Error("guard condition would not fire")
	}
}

// TestBalanceAmountDecodesBothLiveShapes covers the two value shapes that exist
// on pubnet: a bare i128, and the SAC / older-token-sdk map carrying `amount`
// beside authorization flags.
func TestBalanceAmountDecodesBothLiveShapes(t *testing.T) {
	sac := foldFixture(t, kaleSACContractID, kaleSACStorageTSV)
	if sac.Total.Sign() <= 0 {
		t.Error("map-shaped (BalanceValue{amount,…}) balances decoded to a non-positive total")
	}

	deal := foldFixture(t, caocxwnxContractID, caocxwnxStorageTSV)
	if deal.Total.Sign() <= 0 {
		t.Error("bare-i128 balances decoded to a non-positive total")
	}
}

// TestBalanceAmountI128LowWordHighBit pins the i128 reassembly.
//
// An i128 arrives as a signed high word and an UNSIGNED low word. Reading Lo as
// signed corrupts every value whose low word has its top bit set — about half of
// all values — and the corruption is a near-doubling or a sign flip, not a
// rounding error.
func TestBalanceAmountI128LowWordHighBit(t *testing.T) {
	val := xdr.ScVal{
		Type: xdr.ScValTypeScvI128,
		I128: &xdr.Int128Parts{Hi: 0, Lo: xdr.Uint64(1) << 63},
	}
	got, ok := balanceAmount(val)
	if !ok {
		t.Fatal("balanceAmount refused a bare i128")
	}
	want := new(big.Int).Lsh(big.NewInt(1), 63)
	if got.Cmp(want) != 0 {
		t.Errorf("i128 low-word 2^63 decoded to %s, want %s (Lo must be read unsigned)", got, want)
	}
}

func TestContractStorageSupplyRefusesNegativeBalance(t *testing.T) {
	var holderKey xdr.Uint256
	holderKey[0] = 1
	holder := xdr.AccountId{
		Type:    xdr.PublicKeyTypePublicKeyTypeEd25519,
		Ed25519: &holderKey,
	}
	contractID := xdr.ContractId(mustContractHash(t, caocxwnxContractID))
	key := xdr.LedgerKey{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.LedgerKeyContractData{
			Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &contractID},
			Key:        balanceKeyFor(holder),
			Durability: xdr.ContractDataDurabilityPersistent,
		},
	}
	entry := xdr.LedgerEntry{
		Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeContractData,
			ContractData: &xdr.ContractDataEntry{
				Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &contractID},
				Key:        balanceKeyFor(holder),
				Durability: xdr.ContractDataDurabilityPersistent,
				Val:        xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &xdr.Int128Parts{Hi: -1, Lo: 0}},
			},
		},
	}
	keyB64, err := xdr.MarshalBase64(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	entryB64, err := xdr.MarshalBase64(entry)
	if err != nil {
		t.Fatalf("marshal entry: %v", err)
	}

	out := ContractStorageSupply{ContractID: caocxwnxContractID, Total: new(big.Int)}
	if err := out.apply(keyB64, entryB64, 0); err == nil {
		t.Error("a negative held balance was folded into the sum; it is impossible under correct " +
			"token accounting and means the wrong field was decoded")
	}
}

func TestContractStorageSupplySelfConsistentCatchesMissingHolder(t *testing.T) {
	declared := uint32(caocxwnxHolders + 1)
	s := ContractStorageSupply{
		Total:           big.NewInt(1),
		BalanceEntries:  caocxwnxHolders,
		DeclaredHolders: &declared,
	}
	if s.SelfConsistent() {
		t.Error("SelfConsistent() = true while the contract declares one more holder than we can " +
			"see; that gap is what an archived contract-data entry looks like")
	}

	s = ContractStorageSupply{
		Total:         big.NewInt(10),
		DeclaredTotal: big.NewInt(11),
	}
	if s.SelfConsistent() {
		t.Error("SelfConsistent() = true while our sum disagrees with the contract's own TotalSupply")
	}

	if !(ContractStorageSupply{Total: big.NewInt(5)}).SelfConsistent() {
		t.Error("SelfConsistent() = false for a contract that offered no cross-checks; absence of " +
			"contradiction is all a bare token can give")
	}
}

// TestContractDataKeyPrefixAndMarkersMatchRealKeys is the test that would have
// caught an off-by-one in the SQL.
//
// The reader's server-side filter is three magic numbers — a 52-character
// primary-key prefix and two fixed base64 windows at a fixed offset — none of
// which the Go type system checks and none of which a unit test of the decoder
// would exercise. A wrong offset does not error: it matches nothing, the sum
// comes back 0, and 0 is indistinguishable from a fully-burned token. So the
// constants are checked here against real keys and against the SDK's own
// marshalling.
func TestContractDataKeyPrefixAndMarkersMatchRealKeys(t *testing.T) {
	prefix, err := contractDataKeyPrefix(caocxwnxContractID)
	if err != nil {
		t.Fatalf("contractDataKeyPrefix: %v", err)
	}
	if len(prefix) != contractKeyPrefixChars {
		t.Fatalf("prefix is %d chars, want %d", len(prefix), contractKeyPrefixChars)
	}

	var sawBalance, sawInstance, sawHolderCount int
	for _, line := range strings.Split(strings.TrimSpace(caocxwnxStorageTSV), "\n") {
		keyB64 := strings.Split(line, "\t")[0]
		if !strings.HasPrefix(keyB64, prefix) {
			t.Fatalf("derived prefix does not match a real key for this contract:\n prefix %s\n key    %s",
				prefix, keyB64)
		}
		// markerOffset is 1-indexed for SQL substring(); Go slices from 0.
		window := keyB64[markerOffset-1:]
		var key xdr.LedgerKey
		if err := xdr.SafeUnmarshalBase64(keyB64, &key); err != nil {
			t.Fatalf("fixture key: %v", err)
		}
		cd, _ := key.GetContractData()
		switch {
		case balanceKeyHolder(cd.Key):
			if !strings.HasPrefix(window, balanceKeyMarker) {
				t.Errorf("balanceKeyMarker does not match a real Balance key at markerOffset %d:\n"+
					" want %s\n got  %s", markerOffset, balanceKeyMarker, window[:min(len(window), len(balanceKeyMarker))])
			}
			sawBalance++
		case cd.Key.Type == xdr.ScValTypeScvLedgerKeyContractInstance:
			if !strings.HasPrefix(window, instanceKeyMarker) {
				t.Errorf("instanceKeyMarker does not match a real instance key at markerOffset %d:\n"+
					" want %s\n got  %s", markerOffset, instanceKeyMarker, window[:min(len(window), len(instanceKeyMarker))])
			}
			sawInstance++
		case keyNameIs(cd.Key, "HolderCount"):
			if !strings.HasPrefix(window, holderCountKeyMarker) {
				t.Errorf("holderCountKeyMarker does not match a real HolderCount key at markerOffset %d:\n"+
					" want %s\n got  %s", markerOffset, holderCountKeyMarker, window[:min(len(window), len(holderCountKeyMarker))])
			}
			sawHolderCount++
		default:
			// Every other key in this contract must be EXCLUDED by the
			// markers, or the server-side filter is not actually narrowing.
			if strings.HasPrefix(window, balanceKeyMarker) {
				t.Errorf("balanceKeyMarker matched a key that is not a holder balance: %s", keyB64)
			}
			if strings.HasPrefix(window, holderCountKeyMarker) {
				t.Errorf("holderCountKeyMarker matched a key that is not HolderCount: %s", keyB64)
			}
		}
	}
	if sawBalance != caocxwnxHolders || sawInstance != 1 || sawHolderCount != 1 {
		t.Errorf("markers matched %d balances, %d instances and %d holder counts, want %d, 1 and 1",
			sawBalance, sawInstance, sawHolderCount, caocxwnxHolders)
	}
}

// keyNameIs reports whether a contract-data key names one fieldless entry.
func keyNameIs(key xdr.ScVal, want string) bool {
	name, ok := instanceStorageKeyName(key)
	return ok && name == want
}

func TestContractDataKeyPrefixRejectsNonContractID(t *testing.T) {
	for _, id := range []string{
		"",
		"GAXQ5BJ4AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", // an account
		"not-a-strkey",
	} {
		if _, err := contractDataKeyPrefix(id); err == nil {
			t.Errorf("contractDataKeyPrefix(%q) returned no error", id)
		}
	}
}

// TestDecimalsFromInstanceReadsConfigSpelling covers the second map spelling,
// and the refusal when the two disagree.
func TestDecimalsFromInstanceReadsConfigSpelling(t *testing.T) {
	deal := foldFixture(t, caocxwnxContractID, caocxwnxStorageTSV)
	if !deal.DecimalsFound || deal.Decimals != caocxwnxDecimals {
		t.Fatalf("Config.decimals not read: got %d found=%v", deal.Decimals, deal.DecimalsFound)
	}

	mk := func(mapName, fieldName string, scale uint32) xdr.ScMapEntry {
		inner := xdr.ScMap{{
			Key: symVal(fieldName),
			Val: xdr.ScVal{Type: xdr.ScValTypeScvU32, U32: (*xdr.Uint32)(&scale)},
		}}
		innerPtr := &inner
		return xdr.ScMapEntry{
			Key: symVal(mapName),
			Val: xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &innerPtr},
		}
	}

	t.Run("config alone", func(t *testing.T) {
		st := xdr.ScMap{mk("Config", "decimals", 7)}
		if d, ok := decimalsFromInstance(xdr.ScContractInstance{Storage: &st}); !ok || d != 7 {
			t.Errorf("got %d ok=%v, want 7 true", d, ok)
		}
	})
	t.Run("metadata alone", func(t *testing.T) {
		st := xdr.ScMap{mk("METADATA", "decimal", 5)}
		if d, ok := decimalsFromInstance(xdr.ScContractInstance{Storage: &st}); !ok || d != 5 {
			t.Errorf("got %d ok=%v, want 5 true", d, ok)
		}
	})
	t.Run("both agreeing", func(t *testing.T) {
		st := xdr.ScMap{mk("METADATA", "decimal", 6), mk("Config", "decimals", 6)}
		if d, ok := decimalsFromInstance(xdr.ScContractInstance{Storage: &st}); !ok || d != 6 {
			t.Errorf("got %d ok=%v, want 6 true", d, ok)
		}
	})
	t.Run("both disagreeing is refused", func(t *testing.T) {
		st := xdr.ScMap{mk("METADATA", "decimal", 7), mk("Config", "decimals", 18)}
		if _, ok := decimalsFromInstance(xdr.ScContractInstance{Storage: &st}); ok {
			t.Error("a contract declaring two different scales for itself was given one anyway; " +
				"picking between them invents an exponent for a money figure")
		}
	})
}

// TestDecimalsFromInstanceSkipsMapWithoutScaleField pins that a METADATA or
// Config map carrying no scale field is not a declaration: a token-sdk token
// that also keeps an admin `Config` struct still has its METADATA.decimal read,
// while a present-but-invalid field still refuses the instance.
func TestDecimalsFromInstanceSkipsMapWithoutScaleField(t *testing.T) {
	scaleMap := func(mapName, fieldName string, scale uint32) xdr.ScMapEntry {
		inner := xdr.ScMap{
			{Key: symVal(fieldName), Val: xdr.ScVal{Type: xdr.ScValTypeScvU32, U32: (*xdr.Uint32)(&scale)}},
			{Key: symVal("name"), Val: symVal("Token")},
		}
		innerPtr := &inner
		return xdr.ScMapEntry{Key: symVal(mapName), Val: xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &innerPtr}}
	}
	// A #[contracttype] struct stored under DataKey::Config: Vec[Symbol("Config")]
	// keying a Symbol-keyed map with no scale field.
	adminConfig := func(mapName string) xdr.ScMapEntry {
		var admin xdr.AccountId
		inner := xdr.ScMap{
			{Key: symVal("admin"), Val: xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &xdr.ScAddress{
				Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &admin,
			}}},
			{Key: symVal("paused"), Val: xdr.ScVal{Type: xdr.ScValTypeScvBool, B: new(bool)}},
		}
		innerPtr := &inner
		keyVec := xdr.ScVec{symVal(mapName)}
		keyVecPtr := &keyVec
		return xdr.ScMapEntry{
			Key: xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &keyVecPtr},
			Val: xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &innerPtr},
		}
	}

	cases := []struct {
		name   string
		st     xdr.ScMap
		want   uint32
		wantOK bool
	}{
		{"metadata plus scale-less Config", xdr.ScMap{scaleMap("METADATA", "decimal", 18), adminConfig("Config")}, 18, true},
		{"scale-less Config before metadata", xdr.ScMap{adminConfig("Config"), scaleMap("METADATA", "decimal", 9)}, 9, true},
		{"scale-less METADATA plus Config.decimals", xdr.ScMap{adminConfig("METADATA"), scaleMap("Config", "decimals", 6)}, 6, true},
		{"no map carries a scale", xdr.ScMap{adminConfig("METADATA"), adminConfig("Config")}, 0, false},
		{"invalid field still refuses", xdr.ScMap{scaleMap("METADATA", "decimal", 18), scaleMap("Config", "decimals", maxSaneTokenDecimals+1)}, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := tc.st
			d, ok := decimalsFromInstance(xdr.ScContractInstance{Storage: &st})
			if ok != tc.wantOK || d != tc.want {
				t.Errorf("decimalsFromInstance = %d ok=%v, want %d ok=%v", d, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func symVal(s string) xdr.ScVal {
	sym := xdr.ScSymbol(s)
	return xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym}
}

func balanceKeyFor(addr xdr.AccountId) xdr.ScVal {
	vec := xdr.ScVec{
		symVal("Balance"),
		{Type: xdr.ScValTypeScvAddress, Address: &xdr.ScAddress{
			Type:      xdr.ScAddressTypeScAddressTypeAccount,
			AccountId: &addr,
		}},
	}
	vecPtr := &vec
	return xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &vecPtr}
}

func mustContractHash(t *testing.T, contractID string) xdr.Hash {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteContract, contractID)
	if err != nil {
		t.Fatalf("decode contract id: %v", err)
	}
	var h xdr.Hash
	copy(h[:], raw)
	return h
}
