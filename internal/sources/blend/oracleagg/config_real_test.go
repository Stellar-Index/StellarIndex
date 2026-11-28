package oracleagg

import (
	"bufio"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

const fixtureDir = "../../../../test/fixtures/lending_oracle/"

type fixtureEntry struct {
	contract string
	key      xdr.ScVal
	val      xdr.ScVal
	wasm     string // instance entries only
}

// readEntries decodes every LedgerEntry in a fixture TSV whose entry column is entryCol.
func readEntries(t *testing.T, file string, entryCol int) []fixtureEntry {
	t.Helper()
	f, err := os.Open(fixtureDir + file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out []fixtureEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		cols := strings.Split(sc.Text(), "\t")
		if len(cols) <= entryCol || len(cols[entryCol]) < 40 {
			continue // instances.tsv's capture-summary lines
		}
		var le xdr.LedgerEntry
		if err := xdr.SafeUnmarshalBase64(cols[entryCol], &le); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		cd := le.Data.MustContractData()
		id := cd.Contract.MustContractId()
		e := fixtureEntry{contract: strkey.MustEncode(strkey.VersionByteContract, id[:]), key: cd.Key, val: cd.Val}
		if inst, ok := cd.Val.GetInstance(); ok && inst.Executable.WasmHash != nil {
			e.wasm = hexHash(*inst.Executable.WasmHash)
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func hexHash(h xdr.Hash) string {
	const digits = "0123456789abcdef"
	b := make([]byte, 0, 64)
	for _, x := range h {
		b = append(b, digits[x>>4], digits[x&0xf])
	}
	return string(b)
}

func instanceOf(t *testing.T, contract string) fixtureEntry {
	t.Helper()
	for _, e := range readEntries(t, "instances.tsv", 2) {
		if e.contract == contract {
			return e
		}
	}
	t.Fatalf("no instance for %s", contract)
	return fixtureEntry{}
}

func storageOf(t *testing.T, e fixtureEntry) []xdr.ScMapEntry {
	t.Helper()
	inst := e.val.MustInstance()
	if inst.Storage == nil {
		t.Fatal("instance has no storage")
	}
	return *inst.Storage
}

// v1Lookup serves persistent entries from v1-entries.tsv by exact key, as the lake read does.
func v1Lookup(t *testing.T, contract string) EntryLookup {
	t.Helper()
	byKey := map[string]xdr.ScVal{}
	for _, e := range readEntries(t, "v1-entries.tsv", 3) {
		if e.contract != contract || e.key.Type == xdr.ScValTypeScvLedgerKeyContractInstance {
			continue
		}
		k, err := xdr.MarshalBase64(e.key)
		if err != nil {
			t.Fatal(err)
		}
		byKey[k] = e.val
	}
	return func(key xdr.ScVal) (xdr.ScVal, bool, error) {
		k, err := xdr.MarshalBase64(key)
		if err != nil {
			return xdr.ScVal{}, false, err
		}
		v, ok := byKey[k]
		return v, ok, nil
	}
}

const feedDEX = "CALI2BYU2JE6WVRUFYTS6MSBNEHGJ35P4AVCZYF3B6QOE3QKOB2PLE6M"

func cexRule(sym string, maxDev uint32) AssetRule {
	return AssetRule{Feed: feedCEX, FeedAsset: Asset{Symbol: sym}, FeedDecimals: 14, ResolutionSeconds: 300, MaxDev: maxDev}
}

func dexRule(addr string, maxDev uint32) AssetRule {
	return AssetRule{Feed: feedDEX, FeedAsset: Stellar(addr), FeedDecimals: 14, ResolutionSeconds: 300, MaxDev: maxDev}
}

// Expected values are the audit's decoded configurations (oracle-aggregator.md "Decoded configurations").
func TestDecodeV2Instance_RealEntries(t *testing.T) {
	cdn := map[string]uint32{
		"CAL6ER2TI6CTRAY6BFXWNWA7WTYXUXTQCHUBCIBU5O6KM3HJFG6Z6VXV": 3, xlmSAC: 0,
		"CAUIKL3IYGMERDRUN6YSCLWVAKIFG5Q4YJHUKM4S4NJZQIA3BAS6OJPK": 10, "CBIJBDNZNF4X35BJ4FFZWCDBSCKOP5NB4PLG4SNENRMLAPYG4P5FM6VN": 10,
		"CBLV4ATSIWU67CFSQU2NVRKINQIKUZ2ODSZBUJTJ43VJVRSBTZYOPNUR": 2, "CBSJZEIO5C7KC2SF3MKSNXXJSW5G3VTNBX4ATMKUI3B2MR4JKM4R26YF": 2,
		"CB226ZOEYXTBPD3QEGABTJYSKZVBP2PASEISLG3SBMTN5CE4QZUVZ3CE": 2, "CCCRWH6Q3FNP3I2I57BDLM5AFAT7O6OF6GKQOC6SSJNDAVRZ57SPHGU2": 2,
		eurcSAC: 10,
	}
	cdnAssets := map[Asset]AssetRule{}
	for a, d := range cdn {
		cdnAssets[Stellar(a)] = dexRule(a, d)
	}
	cases := []struct {
		adapter string
		want    Config
	}{
		{"CCVTVW2CVA7JLH4ROQGP3CU4T3EXVCK66AZGSM4MUQPXAI4QHCZPOATS", Config{
			Program: ProgramV2, Base: Asset{Symbol: "USD"}, Decimals: 7, MaxAge: 900,
			Assets: map[Asset]AssetRule{Stellar(xlmSAC): cexRule("XLM", 60), Stellar(usdcSAC): cexRule("USDC", 20), Stellar(eurcSAC): cexRule("EURC", 20)},
		}},
		{"CDN7JD6SP32RZHB3DHK4S4AWCCQF6GYIUO455UCR4RGRZILZDRD24N36", Config{
			Program: ProgramV2, Base: Stellar(usdcSAC), Decimals: 7, MaxAge: 900, Assets: cdnAssets,
		}},
	}
	for _, c := range cases {
		t.Run(c.adapter[:8], func(t *testing.T) {
			e := instanceOf(t, c.adapter)
			if ProgramForHash(e.wasm) != ProgramV2 {
				t.Fatalf("wasm %s is not the audited v2 build", e.wasm)
			}
			got, err := DecodeV2Instance(storageOf(t, e))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got  %+v\nwant %+v", got, c.want)
			}
		})
	}
}

func TestDecodeV1_RealEntries(t *testing.T) {
	cex := func(sym string, blocked bool) AssetRule {
		r := cexRule(sym, 0)
		r.Blocked = blocked
		return r
	}
	cases := []struct {
		adapter string
		want    Config
	}{
		// Blocked(XLM) = false is present; Blocked(USDC) is absent and reads false.
		{"CATKK5ZNJCKQQWTUWIUFZMY6V6MOQUGSTFSXMNQZHVJHYF7GVV36FB3Y", Config{
			Program: ProgramV1, Base: Asset{Symbol: "USD"}, Decimals: 7,
			Assets: map[Asset]AssetRule{Stellar(xlmSAC): cex("XLM", false), Stellar(usdcSAC): cex("USDC", false)},
		}},
		{"CCPXGHMVZROX7G6OIP4VBN7FJO6KJAMASNQXLISC3QI52EQVX3VKIWO6", Config{
			Program: ProgramV1, Base: Asset{Symbol: "USD"}, Decimals: 7,
			Assets: map[Asset]AssetRule{
				Stellar(xlmSAC): cex("XLM", false), Stellar(usdcSAC): cex("USDC", false),
				Stellar("CAAV3AE3VKD2P4TY7LWTQMMJHIJ4WOCZ5ANCIJPC3NRSERKVXNHBU2W7"): cex("XRP", false),
			},
		}},
	}
	for _, c := range cases {
		t.Run(c.adapter[:8], func(t *testing.T) {
			e := instanceOf(t, c.adapter)
			if ProgramForHash(e.wasm) != ProgramV1 {
				t.Fatalf("wasm %s is not the audited v1 build", e.wasm)
			}
			got, err := DecodeV1(storageOf(t, e), v1Lookup(t, c.adapter))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got  %+v\nwant %+v", got, c.want)
			}
		})
	}
}

func extraSymbolEntry(name string) xdr.ScMapEntry {
	return xdr.ScMapEntry{Key: symbolKey(name), Val: scval.NewU32(1)}
}

func TestDecode_RejectsUnknownKeys(t *testing.T) {
	v2 := storageOf(t, instanceOf(t, "CCVTVW2CVA7JLH4ROQGP3CU4T3EXVCK66AZGSM4MUQPXAI4QHCZPOATS"))
	if _, err := DecodeV2Instance(append(append([]xdr.ScMapEntry{}, v2...), extraSymbolEntry("Paused"))); !errors.Is(err, ErrConfig) {
		t.Fatalf("v2 extra instance key: err = %v, want ErrConfig", err)
	}
	v1Adapter := "CATKK5ZNJCKQQWTUWIUFZMY6V6MOQUGSTFSXMNQZHVJHYF7GVV36FB3Y"
	v1 := storageOf(t, instanceOf(t, v1Adapter))
	if _, err := DecodeV1(append(append([]xdr.ScMapEntry{}, v1...), extraSymbolEntry("Paused")), v1Lookup(t, v1Adapter)); !errors.Is(err, ErrConfig) {
		t.Fatalf("v1 extra instance key: err = %v, want ErrConfig", err)
	}
	// An unknown field inside a struct (OracleConfig) is rejected too.
	var withField []xdr.ScMapEntry
	for _, e := range v2 {
		if s, _ := scval.AsSymbol(e.Key); s == "Oracles" {
			vec := *e.Val.MustVec()
			m := append(append(xdr.ScMap{}, *vec[0].MustMap()...), extraSymbolEntry("weight"))
			mp := &m
			nv := xdr.ScVec{{Type: xdr.ScValTypeScvMap, Map: &mp}}
			pv := &nv
			e.Val = xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &pv}
		}
		withField = append(withField, e)
	}
	if _, err := DecodeV2Instance(withField); !errors.Is(err, ErrConfig) {
		t.Fatalf("v2 extra struct field: err = %v, want ErrConfig", err)
	}
}

func TestProgramForHash_RejectsForeign(t *testing.T) {
	// The Blend V2 pool WASM: a real contract, not an audited adapter.
	if p := ProgramForHash("a41fc53d6753b6c04eb15b021c55052366a4c8e0e21bc72700f461264ec1350e"); p != ProgramUnknown {
		t.Fatalf("foreign hash mapped to %s", p)
	}
	if ProgramForHash(HashV1) != ProgramV1 || ProgramForHash(HashV2) != ProgramV2 {
		t.Fatal("audited hashes must map")
	}
}
