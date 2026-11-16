package clickhouse

import (
	"context"
	"errors"
	"math/big"
	"reflect"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
)

// The Blend reserve reader matches lake rows by `key_xdr` verbatim, so a wrong key silently matches nothing.
// Fixtures are a live mainnet pool and USDC SAC with keys confirmed in the lake, not invented values. One
// reserve key matches one ledger_entries_current row but tens of thousands of raw changes.
const (
	blendTestPool     = "CAJJZSGMMM3PD7N33TAPHGBUGTB43OC73HVIK2L2G6BNGGGYOSSYBXBD"
	blendTestAssetSAC = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75" // USDC SAC

	// PoolDataKey::ResData(USDC) under blendTestPool, persistent durability.
	blendTestResDataKey = "AAAABgAAAAESnMjMYzbx/bvcwPOYNDTDzbhf2eqFaXo3gtMY2HSlgAAAABAAAAABAAAAAgAAAA8AAAAHUmVzRGF0YQAAAAASAAAAAa3vzlmu5Slo92Bh1JTCUlt1ZZ+kKWpl9JnvKeVkd+SWAAAAAQ=="
	// The pool's contract-instance entry, persistent then temporary. These are public on-chain Soroban
	// LedgerKeys, not credentials, differing only in the durability byte; keep them intact, as a wrong storage
	// key silently matches nothing.
	blendTestInstanceKeyPersistent = "AAAABgAAAAESnMjMYzbx/bvcwPOYNDTDzbhf2eqFaXo3gtMY2HSlgAAAABQAAAAB" // gitleaks:allow
	blendTestInstanceKeyTemporary  = "AAAABgAAAAESnMjMYzbx/bvcwPOYNDTDzbhf2eqFaXo3gtMY2HSlgAAAABQAAAAA" // gitleaks:allow
)

// resDataEntryFixture builds a minimal, genuinely decodable Blend ResData contract_data LedgerEntry, so a
// test can tell "row decoded" from "row skipped" instead of asserting over two indistinguishable failures.
func resDataEntryFixture(t *testing.T) string {
	t.Helper()
	i128 := func(v uint64) xdr.ScVal {
		return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &xdr.Int128Parts{Hi: 0, Lo: xdr.Uint64(v)}}
	}
	sym := func(s string) xdr.ScVal {
		ss := xdr.ScSymbol(s)
		return xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &ss}
	}
	u64 := func(v uint64) xdr.ScVal {
		u := xdr.Uint64(v)
		return xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: &u}
	}
	mp := &xdr.ScMap{
		{Key: sym("b_rate"), Val: i128(1_000_000_000_000)},
		{Key: sym("b_supply"), Val: i128(0)},
		{Key: sym("backstop_credit"), Val: i128(0)},
		{Key: sym("d_rate"), Val: i128(1_000_000_000_000)},
		{Key: sym("d_supply"), Val: i128(0)},
		{Key: sym("ir_mod"), Val: i128(10_000_000)},
		{Key: sym("last_time"), Val: u64(0)},
	}
	pid := mustContractID(t, blendTestPool)
	keyVec := &xdr.ScVec{sym("ResData")}
	b64, err := xdr.MarshalBase64(xdr.LedgerEntry{
		Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeContractData,
			ContractData: &xdr.ContractDataEntry{
				Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &pid},
				Key:        xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &keyVec},
				Durability: xdr.ContractDataDurabilityPersistent,
				Val:        xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &mp},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal ResData entry fixture: %v", err)
	}
	return b64
}

func mustContractID(t *testing.T, c string) xdr.ContractId {
	t.Helper()
	id, err := contractIDFromStrkey(c)
	if err != nil {
		t.Fatalf("contractIDFromStrkey(%s): %v", c, err)
	}
	return id
}

// TestPoolDataKeyXDR_MatchesTheOnChainKey pins the Blend `PoolDataKey::ResData(asset)` encoding:
// Vec[Symbol(variant), Address(asset)] under the POOL contract, PERSISTENT. Any error silently misses lake
// rows.
func TestPoolDataKeyXDR_MatchesTheOnChainKey(t *testing.T) {
	got, err := poolDataKeyXDR(mustContractID(t, blendTestPool), "ResData", mustContractID(t, blendTestAssetSAC))
	if err != nil {
		t.Fatalf("poolDataKeyXDR: %v", err)
	}
	if got != blendTestResDataKey {
		t.Fatalf("ResData key_xdr =\n  %s\nwant (verified present in r1's lake)\n  %s", got, blendTestResDataKey)
	}

	// Decode it back and assert the structure, so a future encoding change
	// that happens to produce the same bytes for a different reason still has
	// to satisfy the shape.
	var lk xdr.LedgerKey
	if err := xdr.SafeUnmarshalBase64(got, &lk); err != nil {
		t.Fatalf("derived key does not decode: %v", err)
	}
	if lk.Type != xdr.LedgerEntryTypeContractData {
		t.Fatalf("key type = %v, want ContractData", lk.Type)
	}
	cd := lk.ContractData
	if cd.Durability != xdr.ContractDataDurabilityPersistent {
		t.Errorf("durability = %v, want Persistent — a temporary key matches nothing", cd.Durability)
	}
	if cd.Contract.Type != xdr.ScAddressTypeScAddressTypeContract || *cd.Contract.ContractId != mustContractID(t, blendTestPool) {
		t.Errorf("key is not scoped to the POOL contract: %+v", cd.Contract)
	}
	if cd.Key.Type != xdr.ScValTypeScvVec || cd.Key.Vec == nil || len(**cd.Key.Vec) != 2 {
		t.Fatalf("key ScVal is not a 2-element Vec: %+v", cd.Key)
	}
	elems := **cd.Key.Vec
	if elems[0].Type != xdr.ScValTypeScvSymbol || string(*elems[0].Sym) != "ResData" {
		t.Errorf("Vec[0] = %+v, want Symbol(\"ResData\")", elems[0])
	}
	if elems[1].Type != xdr.ScValTypeScvAddress || *elems[1].Address.ContractId != mustContractID(t, blendTestAssetSAC) {
		t.Errorf("Vec[1] = %+v, want Address(asset)", elems[1])
	}

	// A different variant must produce a DIFFERENT key — proving the variant
	// name is really part of the encoding and not decoration.
	other, err := poolDataKeyXDR(mustContractID(t, blendTestPool), "ResConfig", mustContractID(t, blendTestAssetSAC))
	if err != nil {
		t.Fatalf("poolDataKeyXDR(ResConfig): %v", err)
	}
	if other == got {
		t.Error("ResData and ResConfig derive the SAME key — the variant name is not encoded")
	}
}

// TestBlendReserveKeys_CoversInstanceAndEveryResolvableAsset pins the key set and its reverse index. An
// asset whose strkey does not decode is skipped, so one bad asset cannot blank the other reserves.
func TestBlendReserveKeys_CoversInstanceAndEveryResolvableAsset(t *testing.T) {
	assets := []string{
		blendTestAssetSAC,
		"not-a-contract-strkey", // skipped
		"GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAWHF", // a G-address is not a contract: skipped
	}
	keys, refByKey, err := blendReserveKeys(mustContractID(t, blendTestPool), assets)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{blendTestInstanceKeyPersistent, blendTestInstanceKeyTemporary, blendTestResDataKey}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("keys =\n  %v\nwant\n  %v", keys, want)
	}
	// Both instance durabilities are probed: a contract's instance entry can
	// legitimately sit under either, and probing only one silently loses the
	// backstop rate for pools that use the other.
	if refByKey[blendTestInstanceKeyPersistent].kind != "Instance" ||
		refByKey[blendTestInstanceKeyTemporary].kind != "Instance" {
		t.Errorf("instance keys are not both indexed as Instance: %+v", refByKey)
	}
	ref := refByKey[blendTestResDataKey]
	if ref.kind != "ResData" || ref.asset != blendTestAssetSAC {
		t.Errorf("ResData key maps to %+v, want kind=ResData asset=%s", ref, blendTestAssetSAC)
	}
	// The reverse index is what maps a returned row back to its asset; an
	// entry per key and no more.
	if len(refByKey) != len(keys) {
		t.Errorf("reverse index holds %d entries for %d keys", len(refByKey), len(keys))
	}
}

// TestBlendReserveKeys_EmptyAssetListStillProbesTheInstance: an empty asset list is not an empty key list,
// since the pool config is read from the instance whether or not the pool has reserves.
func TestBlendReserveKeys_EmptyAssetListStillProbesTheInstance(t *testing.T) {
	keys, _, err := blendReserveKeys(mustContractID(t, blendTestPool), nil)
	if err != nil || len(keys) != 2 {
		t.Fatalf("keys = %v, want the two instance-durability keys", keys)
	}
}

// TestBlendPoolReserves_RejectsANonContractPool — the pool id comes from a
// request path. A G-address or garbage must be rejected before any query, not
// silently turned into a zero contract id that reads another contract's keys.
func TestBlendPoolReserves_RejectsANonContractPool(t *testing.T) {
	conn := &stubConn{respond: func(q string) (driver.Rows, error) {
		t.Fatalf("an invalid pool id must not reach ClickHouse; query was: %s", q)
		return nil, nil
	}}
	r := &ExplorerReader{conn: conn}
	if _, err := r.BlendPoolReserves(t.Context(), "GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAWHF", blend.PoolV2, nil, nil); err == nil {
		t.Fatal("BlendPoolReserves accepted a non-contract pool id")
	}
}

// TestBlendPoolReserves_QueryShape pins the reader's version-resolution and pruning (silent-wrong-answer
// territory) and that the read is a PK-prefix probe on the current-state projection, not a windowed
// ledger_entry_changes scan.
func TestBlendPoolReserves_QueryShape(t *testing.T) {
	conn := &stubConn{respond: func(string) (driver.Rows, error) { return &stubRows{}, nil }}
	r := &ExplorerReader{conn: conn}
	if _, err := r.BlendPoolReserves(t.Context(), blendTestPool, blend.PoolV2, []string{blendTestAssetSAC}, nil); err != nil {
		t.Fatalf("BlendPoolReserves: %v", err)
	}
	if len(conn.queries) != 1 {
		t.Fatalf("issued %d queries, want ONE batched lookup", len(conn.queries))
	}
	q := conn.queries[0]

	// The current-state projection's sort key IS (entry_type, key_xdr), so the probe reads
	// ~one row per requested key. Folding the latest entry out of the CHANGES table instead
	// costs a window scan that tracks pool ACTIVITY, a floor under every pool.
	if !strings.Contains(q, "stellar.ledger_entries_current FINAL") {
		t.Errorf("reserve lookup does not read the current-state projection:\n%s", q)
	}
	if strings.Contains(q, "stellar.ledger_entry_changes") {
		t.Errorf("reserve lookup scans the raw changes table again (#504 regression):\n%s", q)
	}
	for _, banned := range []string{"GROUP BY", "argMax(", "ledger_seq >"} {
		if strings.Contains(q, banned) {
			t.Errorf("reserve lookup reintroduced the windowed fold (%q) — FINAL over the projection already resolves versions:\n%s", banned, q)
		}
	}
	// Version resolution follows the projection: FINAL keeps the last change in a ledger. The removed-key filter
	// must not run before that collapse, or moving `entry_xdr != ''` into PREWHERE resurrects a deleted key.
	if !strings.Contains(q, "optimize_move_to_prewhere_if_final = 0") {
		t.Errorf("reserve lookup does not pin optimize_move_to_prewhere_if_final=0; a removed reserve could resurrect:\n%s", q)
	}
	for _, s := range []string{
		"entry_type = 'contract_data'",
		"key_xdr IN (?)",
		"entry_xdr != ''",
		"max_threads = 4",
		"max_memory_usage = 8000000000",
	} {
		if !strings.Contains(q, s) {
			t.Errorf("reserve lookup missing %q:\n%s", s, q)
		}
	}

	// One bound arg: the key list. The window width is gone with the scan.
	args := conn.args[0]
	if len(args) != 1 {
		t.Fatalf("bound %d args, want 1 (the key list)", len(args))
	}
	keys, ok := args[0].([]string)
	if !ok {
		t.Fatalf("arg 0 is %T, want []string of key_xdr", args[0])
	}
	if len(keys) != 3 {
		t.Errorf("bound %d keys, want 3 (two instance durabilities + one ResData)", len(keys))
	}
}

// TestScanBlendReserveParts_IgnoresUnrequestedKeys: the reverse index alone ties a row to an asset, so a row
// whose key is not in it must be dropped, not attributed to the empty-string asset.
func TestScanBlendReserveParts_IgnoresUnrequestedKeys(t *testing.T) {
	refByKey := map[string]keyRef{blendTestResDataKey: {asset: blendTestAssetSAC, kind: "ResData"}}
	rows := &stubRows{data: [][]any{
		{"some-other-key", "AAAA"},
	}}
	byAsset, bstop, matched, err := scanReserveParts(rows, refByKey, blendModel(blend.PoolV2).decode)
	if err != nil {
		t.Fatalf("scanReserveParts: %v", err)
	}
	if len(byAsset) != 0 {
		t.Errorf("unrequested key produced %v, want nothing", byAsset)
	}
	if bstop != nil {
		t.Errorf("pool instance = %+v, want nil", *bstop)
	}
	// `matched` scopes the TTL-liveness filter. A key we did not ask about
	// must not enter it: classifying it could only ever DROP reserves on the
	// strength of a row the reader never attributed to anything.
	if len(matched) != 0 {
		t.Errorf("matched = %v, want empty — an unrequested key must not be handed to the TTL filter", matched)
	}
}

// TestScanBlendReserveParts_MatchedTracksOnlyReportedEntries: `matched` feeds the TTL filter, so it must
// list exactly the keys this read reports (decoded ResData rows plus the instance row), never keys with no
// usable or undecodable row.
func TestScanBlendReserveParts_MatchedTracksOnlyReportedEntries(t *testing.T) {
	const otherAsset = "CB7777777777777777777777777777777777777777777777777777777"
	refByKey := map[string]keyRef{
		blendTestResDataKey:            {asset: blendTestAssetSAC, kind: "ResData"},
		"resdata-undecodable":          {asset: otherAsset, kind: "ResData"},
		blendTestInstanceKeyPersistent: {kind: "Instance"},
	}
	rows := &stubRows{data: [][]any{
		{blendTestResDataKey, resDataEntryFixture(t)},
		{"resdata-undecodable", "!!!not-base64!!!"},
	}}
	byAsset, _, matched, err := scanReserveParts(rows, refByKey, blendModel(blend.PoolV2).decode)
	if err != nil {
		t.Fatalf("scanReserveParts: %v", err)
	}
	if len(byAsset) != 1 {
		t.Fatalf("byAsset = %v, want only the decodable reserve", byAsset)
	}
	want := []string{blendTestResDataKey}
	if !reflect.DeepEqual(matched, want) {
		t.Errorf("matched = %v, want %v — only entries actually reported may be TTL-classified", matched, want)
	}
}

// TestScanBlendReserveParts_UndecodableEntryIsSkippedNotFatal: a reserve whose entry does not
// decode is reported ABSENT (the API omits it); erroring would blank the whole pool page for
// one bad row.
func TestScanBlendReserveParts_UndecodableEntryIsSkippedNotFatal(t *testing.T) {
	refByKey := map[string]keyRef{blendTestResDataKey: {asset: blendTestAssetSAC, kind: "ResData"}}
	rows := &stubRows{data: [][]any{{blendTestResDataKey, "!!!not-base64!!!"}}}
	byAsset, _, _, err := scanReserveParts(rows, refByKey, blendModel(blend.PoolV2).decode)
	if err != nil {
		t.Fatalf("one undecodable entry aborted the pool read: %v", err)
	}
	if len(byAsset) != 0 {
		t.Errorf("byAsset = %v, want the reserve reported absent", byAsset)
	}
}

// TestScanBlendReserveParts_TruncatedStreamIsAnError: unlike a single bad row, a truncated stream may be
// missing reserves for reasons unrelated to capture. Reporting them absent would present a partially read
// pool as fully read.
func TestScanBlendReserveParts_TruncatedStreamIsAnError(t *testing.T) {
	truncated := errors.New("stream truncated")
	rows := &stubRows{streamErr: truncated}
	if _, _, _, err := scanReserveParts(rows, nil, blendModel(blend.PoolV2).decode); !errors.Is(err, truncated) {
		t.Fatalf("err = %v, want it to wrap %v", err, truncated)
	}
}

// blendDropFixture is the (dataByAsset, refByKey, matched) triple
// dropArchivedBlendReserves operates on: a two-reserve pool whose instance
// entry also came back.
func blendDropFixture() (map[string]*blend.ReserveData, map[string]keyRef, []string) {
	const (
		keyA = "resdata-key-a"
		keyB = "resdata-key-b"
		inst = "instance-key"
	)
	data := map[string]*blend.ReserveData{"assetA": {}, "assetB": {}}
	refs := map[string]keyRef{
		keyA: {asset: "assetA", kind: "ResData"},
		keyB: {asset: "assetB", kind: "ResData"},
		inst: {kind: "Instance"},
	}
	return data, refs, []string{inst, keyA, keyB}
}

// fixedTTLCache is a verdict cache wired to a canned answer, so the drop
// RULES can be tested without a ClickHouse server.
func fixedTTLCache(verdicts map[string]TTLLiveness) *ttlLivenessCache {
	c := newTTLLivenessCache(func(_ context.Context, keys []string) (map[string]TTLLiveness, error) {
		out := make(map[string]TTLLiveness, len(keys))
		for _, k := range keys {
			out[k] = verdicts[k] // absent → TTLUnknown (the zero value)
		}
		return out, nil
	})
	return c
}

// TestDropArchivedBlendReserves_DropsOnlyPositivelyArchived pins fail-open: a reserve is dropped only
// on a positively lapsed TTL; TTLUnknown keeps it, as under-reporting liquidity is as wrong as phantom.
func TestDropArchivedBlendReserves_DropsOnlyPositivelyArchived(t *testing.T) {
	data, refs, matched := blendDropFixture()
	cache := fixedTTLCache(map[string]TTLLiveness{
		"resdata-key-a": TTLArchived,
		"resdata-key-b": TTLUnknown, // unresolved — must be KEPT
		"instance-key":  TTLLive,
	})
	if _, err := dropArchivedBlendReserves(t.Context(), cache, data, refs, matched); err != nil {
		t.Fatalf("dropArchivedBlendReserves: %v", err)
	}
	if _, present := data["assetA"]; present {
		t.Error("assetA survived a positively-ARCHIVED verdict — its last-known reserves would be priced as current liquidity")
	}
	if _, present := data["assetB"]; !present {
		t.Error("assetB was dropped on a TTLUnknown verdict — the filter must fail OPEN; only a parsed, lapsed liveUntilLedgerSeq justifies exclusion")
	}
}

// TestDropArchivedBlendReserves_ArchivedInstanceSinksThePool — the pool
// contract itself is no longer live ledger state, so no reserve under it can
// be current liquidity, whatever the individual ResData TTLs say.
func TestDropArchivedBlendReserves_ArchivedInstanceSinksThePool(t *testing.T) {
	data, refs, matched := blendDropFixture()
	cache := fixedTTLCache(map[string]TTLLiveness{
		"instance-key":  TTLArchived,
		"resdata-key-a": TTLLive, // individually live, but the POOL is dead
		"resdata-key-b": TTLLive,
	})
	archived, err := dropArchivedBlendReserves(t.Context(), cache, data, refs, matched)
	if err != nil || !archived {
		t.Fatalf("dropArchivedBlendReserves: archived=%v err=%v, want the instance reported archived", archived, err)
	}
	if len(data) != 0 {
		t.Errorf("reserves %v survived an ARCHIVED pool instance — a dead pool's reserves are not current liquidity", data)
	}
}

// TestDropArchivedBlendReserves_NilCacheKeepsEverything — a reader built
// without a verdict cache (every test-constructed ExplorerReader) must not
// silently blank every pool. Nil degrades to TTLUnknown, i.e. keep.
func TestDropArchivedBlendReserves_NilCacheKeepsEverything(t *testing.T) {
	data, refs, matched := blendDropFixture()
	if _, err := dropArchivedBlendReserves(t.Context(), nil, data, refs, matched); err != nil {
		t.Fatalf("dropArchivedBlendReserves(nil cache): %v", err)
	}
	if len(data) != 2 {
		t.Errorf("data = %v, want both reserves kept — a reader with no verdict cache must fail OPEN, not drop everything", data)
	}
}

// TestPoolInstanceFromStorage_MissesReportNotOK — a miss is not a 0%
// backstop take: callers must withhold the APR, and the miss paths must not
// panic on a non-instance ScVal or an instance with no Config entry.
func TestPoolInstanceFromStorage_MissesReportNotOK(t *testing.T) {
	// Not an instance at all.
	i := xdr.Int64(5)
	if got, ok := poolInstanceFromStorage(xdr.ScVal{Type: xdr.ScValTypeScvI64, I64: &i}); ok || got != (PoolInstance{}) {
		t.Errorf("non-instance ScVal gave (%+v, %v), want (zero, false)", got, ok)
	}
	// An instance with no storage map.
	if got, ok := poolInstanceFromStorage(xdr.ScVal{
		Type:     xdr.ScValTypeScvContractInstance,
		Instance: &xdr.ScContractInstance{Executable: xdr.ContractExecutable{Type: xdr.ContractExecutableTypeContractExecutableStellarAsset}},
	}); ok || got != (PoolInstance{}) {
		t.Errorf("instance without storage gave (%+v, %v), want (zero, false)", got, ok)
	}
	// An instance whose storage holds no "Config" key.
	other := xdr.ScSymbol("Something")
	v := xdr.Int64(1)
	storage := xdr.ScMap{{Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &other}, Val: xdr.ScVal{Type: xdr.ScValTypeScvI64, I64: &v}}}
	if got, ok := poolInstanceFromStorage(xdr.ScVal{
		Type: xdr.ScValTypeScvContractInstance,
		Instance: &xdr.ScContractInstance{
			Executable: xdr.ContractExecutable{Type: xdr.ContractExecutableTypeContractExecutableStellarAsset},
			Storage:    &storage,
		},
	}); ok || got != (PoolInstance{}) {
		t.Errorf("instance without a Config entry gave (%+v, %v), want (zero, false)", got, ok)
	}
}

// TestContractIDFromStrkey_RejectsNonContractStrkeys — the C-strkey check is
// what stops a G-address (or an L/M address) being copied into a 32-byte
// contract id and reading another contract's storage.
func TestContractIDFromStrkey_RejectsNonContractStrkeys(t *testing.T) {
	good, err := contractIDFromStrkey(blendTestPool)
	if err != nil {
		t.Fatalf("contractIDFromStrkey(valid C-strkey): %v", err)
	}
	if good == (xdr.ContractId{}) {
		t.Error("a valid contract strkey decoded to the ZERO contract id")
	}
	for _, bad := range []string{
		"",
		"GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAWHF", // account
		"CAJJZSGMMM3PD7N33TAPHGBUGTB43OC73HVIK2L2G6BNGGGYOSSYBXB",  // truncated
		"not a strkey at all",
	} {
		if id, err := contractIDFromStrkey(bad); err == nil {
			t.Errorf("contractIDFromStrkey(%q) = %x with no error, want a rejection", bad, id)
		}
	}
}

// A reserve with a config but no readable pool Config must not be served a
// supply APR computed as if the backstop took 0%.
func TestAssembleBlendReserveStates_MissingBackstopRateWithholdsAPR(t *testing.T) {
	one := func() *big.Int { return big.NewInt(1_000_000_000_000) }
	rd := blend.ReserveData{
		Version: blend.PoolV2, DRate: one(), BRate: one(), IRMod: big.NewInt(10_000_000),
		BSupply: big.NewInt(100), DSupply: big.NewInt(50), BackstopCredit: big.NewInt(0),
	}
	data := map[string]*blend.ReserveData{blendTestAssetSAC: &rd}
	cfgs := map[string]blend.ReserveConfig{blendTestAssetSAC: {Decimals: 6}}
	assets := []string{blendTestAssetSAC}

	got := assembleBlendReserveStates(blendTestPool, assets, data, cfgs, nil)
	if len(got) != 1 || got[0].Metrics.HasAPR {
		t.Fatalf("unknown backstop rate: HasAPR = %v, want false (got %+v)", got[0].Metrics.HasAPR, got)
	}
	if !got[0].DecimalsFound || got[0].Decimals != 6 {
		t.Errorf("decimals should still come from the reserve config, got %d found=%v", got[0].Decimals, got[0].DecimalsFound)
	}

	got = assembleBlendReserveStates(blendTestPool, assets, data, cfgs, &PoolInstance{})
	if !got[0].Metrics.HasAPR {
		t.Errorf("a known 0%% backstop rate must still serve APR")
	}
}

// TestAssembleBlendReserveStates_CarriesConfig — the decoded config rides on its reserve and a reserve with no config carries nil.
func TestAssembleBlendReserveStates_CarriesConfig(t *testing.T) {
	one := func() *big.Int { return big.NewInt(1_000_000_000_000) }
	rd := blend.ReserveData{
		Version: blend.PoolV2, DRate: one(), BRate: one(), IRMod: big.NewInt(10_000_000),
		BSupply: big.NewInt(100), DSupply: big.NewInt(50), BackstopCredit: big.NewInt(0),
	}
	const other = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
	data := map[string]*blend.ReserveData{blendTestAssetSAC: &rd, other: &rd}
	cfgs := map[string]blend.ReserveConfig{blendTestAssetSAC: {Decimals: 6, CFactor: 9_000_000}}
	inst := &PoolInstance{Config: blend.PoolConfig{BstopRate: 2_000_000, Status: 1}, Admin: blendTestPool}

	got := assembleBlendReserveStates(blendTestPool, []string{blendTestAssetSAC, other}, data, cfgs, inst)
	if len(got) != 2 || got[0].Config == nil || got[0].Config.CFactor != 9_000_000 || got[1].Config != nil {
		t.Fatalf("states = %+v, want the first reserve's config and nil for the second", got)
	}
}

// poolInstanceVal is a pool contract instance holding Admin and a Config map with cfg's entries.
func poolInstanceVal(t *testing.T, cfg xdr.ScMap) xdr.ScVal {
	t.Helper()
	cfgPtr := &cfg
	storage := xdr.ScMap{
		{Key: testSym("Admin"), Val: testContractAddr(t, blendTestPool)},
		{Key: testSym("Config"), Val: xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &cfgPtr}},
	}
	return xdr.ScVal{
		Type: xdr.ScValTypeScvContractInstance,
		Instance: &xdr.ScContractInstance{
			Executable: xdr.ContractExecutable{Type: xdr.ContractExecutableTypeContractExecutableStellarAsset},
			Storage:    &storage,
		},
	}
}

func testSym(s string) xdr.ScVal {
	v := xdr.ScSymbol(s)
	return xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &v}
}

func testU32(n uint32) xdr.ScVal {
	v := xdr.Uint32(n)
	return xdr.ScVal{Type: xdr.ScValTypeScvU32, U32: &v}
}

func testContractAddr(t *testing.T, c string) xdr.ScVal {
	t.Helper()
	id := mustContractID(t, c)
	a := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &id}
	return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &a}
}

// v2PoolConfigMap is a V2 PoolConfig: min_collateral 2^64 + 5, status setup (6).
func v2PoolConfigMap(t *testing.T) xdr.ScMap {
	minCol := xdr.Int128Parts{Hi: 1, Lo: 5}
	return xdr.ScMap{
		{Key: testSym("bstop_rate"), Val: testU32(2_000_000)},
		{Key: testSym("max_positions"), Val: testU32(8)},
		{Key: testSym("min_collateral"), Val: xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &minCol}},
		{Key: testSym("oracle"), Val: testContractAddr(t, blendTestAssetSAC)},
		{Key: testSym("status"), Val: testU32(6)},
	}
}

// TestPoolInstanceFromStorage_DecodesConfigAndAdmin — the whole PoolConfig and the admin come from one walk.
func TestPoolInstanceFromStorage_DecodesConfigAndAdmin(t *testing.T) {
	got, ok := poolInstanceFromStorage(poolInstanceVal(t, v2PoolConfigMap(t)))
	if !ok || got.Admin != blendTestPool || got.Config.Oracle != blendTestAssetSAC || got.Config.BstopRate != 2_000_000 ||
		got.Config.MaxPositions != 8 || got.Config.Status != 6 || got.Config.MinCollateral.String() != "18446744073709551621" {
		t.Fatalf("instance = %+v (ok=%v)", got, ok)
	}
}

// TestBlendPoolReserves_ServesInstanceWithoutReserves — a pool with no reserve state (none listed, or none in the lake) still
// reports its instance config; a pool in setup status has exactly this shape.
func TestBlendPoolReserves_ServesInstanceWithoutReserves(t *testing.T) {
	pid := mustContractID(t, blendTestPool)
	entry, err := xdr.MarshalBase64(xdr.LedgerEntry{Data: xdr.LedgerEntryData{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.ContractDataEntry{
			Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &pid},
			Key:        xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance},
			Durability: xdr.ContractDataDurabilityPersistent,
			Val:        poolInstanceVal(t, v2PoolConfigMap(t)),
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, assets := range [][]string{nil, {blendTestAssetSAC}} {
		conn := &stubConn{respond: func(string) (driver.Rows, error) {
			return &stubRows{data: [][]any{{blendTestInstanceKeyPersistent, entry}}}, nil
		}}
		r := &ExplorerReader{conn: conn, ttlVerdicts: fixedTTLCache(nil)}
		got, err := r.BlendPoolReserves(t.Context(), blendTestPool, blend.PoolV2, assets, nil)
		if err != nil {
			t.Fatalf("assets %v: %v", assets, err)
		}
		if len(conn.queries) != 1 || got.Instance == nil || got.Instance.Config.Status != 6 || len(got.Reserves) != 0 {
			t.Errorf("assets %v: queries=%d state=%+v, want the instance and no reserves", assets, len(conn.queries), got)
		}
	}
}
