package clickhouse

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
)

// The Blend reserve reader matches lake rows by their `key_xdr` VERBATIM, so a
// wrong key does not error — it simply matches nothing, and the pool reports
// "no reserves captured". These fixtures are therefore not invented: the pool
// is a live mainnet Blend pool (the busiest by `supply` events on r1,
// 2026-09-03) and the asset is the mainnet USDC SAC, and both derived keys
// below were confirmed present in r1's lake before being pinned here:
//
//	ledger_entry_changes, last 250k ledgers, ResData key → 74,834 rows
//	ledger_entries_current, instance key (persistent)    → ledger 61,962,028
//
// So a regression in the derivation fails this test with a value that is
// provably not what the chain stores, rather than with a self-consistent
// nonsense both sides agree on.
//
// Those two figures are also the #504 measurement: ONE reserve's key matches
// 74,834 rows in the window the reader used to fold over, and exactly one row
// in the projection it reads now. The instance key resolving in
// ledger_entries_current is what proves contract_data is projected there.
const (
	blendTestPool     = "CAJJZSGMMM3PD7N33TAPHGBUGTB43OC73HVIK2L2G6BNGGGYOSSYBXBD"
	blendTestAssetSAC = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75" // USDC SAC

	// PoolDataKey::ResData(USDC) under blendTestPool, persistent durability.
	blendTestResDataKey = "AAAABgAAAAESnMjMYzbx/bvcwPOYNDTDzbhf2eqFaXo3gtMY2HSlgAAAABAAAAABAAAAAgAAAA8AAAAHUmVzRGF0YQAAAAASAAAAAa3vzlmu5Slo92Bh1JTCUlt1ZZ+kKWpl9JnvKeVkd+SWAAAAAQ=="
	// The pool's own contract-instance entry, persistent then temporary.
	//
	// These two are base64 Soroban LedgerKeys — PUBLIC on-chain addresses,
	// not credentials. They differ only in their final durability byte,
	// which is exactly why they trip gitleaks' generic-api-key rule: a
	// long high-entropy base64 string assigned to a name ending in "Key".
	// The annotation is per-line and deliberate; do not delete it, and do
	// not "fix" the finding by shortening or mangling the fixtures — a
	// wrong storage key here does not error, it silently matches nothing,
	// which is the failure these goldens exist to catch.
	blendTestInstanceKeyPersistent = "AAAABgAAAAESnMjMYzbx/bvcwPOYNDTDzbhf2eqFaXo3gtMY2HSlgAAAABQAAAAB" // gitleaks:allow
	blendTestInstanceKeyTemporary  = "AAAABgAAAAESnMjMYzbx/bvcwPOYNDTDzbhf2eqFaXo3gtMY2HSlgAAAABQAAAAA" // gitleaks:allow
)

// resDataEntryFixture builds a minimal, genuinely-decodable Blend ResData
// contract_data LedgerEntry (the ScMap blend.DecodeReserveData expects), so a
// test can distinguish "row decoded" from "row skipped" rather than asserting
// over two indistinguishable failures.
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

// TestPoolDataKeyXDR_MatchesTheOnChainKey pins the storage-key encoding for a
// Blend `PoolDataKey::ResData(asset)` entry: a `#[contracttype]` enum variant
// encodes as Vec[Symbol(variant), Address(asset)] under the POOL contract at
// PERSISTENT durability. Every one of those four choices is silently fatal if
// wrong — the key just never matches a lake row.
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

// TestBlendReserveKeys_CoversInstanceAndEveryResolvableAsset pins the key set
// and its reverse index. The instance key is what carries the backstop rate;
// the ResData key per asset is what carries the reserve state. An asset whose
// strkey does not decode is SKIPPED rather than aborting the whole pool read —
// one bad asset in a caller's list must not blank the other reserves.
func TestBlendReserveKeys_CoversInstanceAndEveryResolvableAsset(t *testing.T) {
	assets := []string{
		blendTestAssetSAC,
		"not-a-contract-strkey", // skipped
		"GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAWHF", // a G-address is not a contract: skipped
	}
	keys, refByKey := blendReserveKeys(mustContractID(t, blendTestPool), assets)

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

// TestBlendReserveKeys_EmptyAssetListStillProbesTheInstance — the caller
// short-circuits on an EMPTY key list, so this documents that an empty asset
// list is not the empty key list: the instance key is still built. (The pool
// read then returns no reserves because none were asked for.)
func TestBlendReserveKeys_EmptyAssetListStillProbesTheInstance(t *testing.T) {
	keys, _ := blendReserveKeys(mustContractID(t, blendTestPool), nil)
	if len(keys) != 2 {
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
	if _, err := r.BlendPoolReserves(t.Context(), "GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAWHF", nil, nil); err == nil {
		t.Fatal("BlendPoolReserves accepted a non-contract pool id")
	}
}

// TestBlendPoolReserves_QueryShape pins the version-resolution and pruning
// decisions documented on the reader — all of which are silent-wrong-answer
// territory rather than errors — plus the #504 requirement that the read is a
// PK-prefix probe on the current-state projection, not a windowed scan of
// stellar.ledger_entry_changes.
func TestBlendPoolReserves_QueryShape(t *testing.T) {
	conn := &stubConn{respond: func(string) (driver.Rows, error) { return &stubRows{}, nil }}
	r := &ExplorerReader{conn: conn}
	if _, err := r.BlendPoolReserves(t.Context(), blendTestPool, []string{blendTestAssetSAC}, nil); err != nil {
		t.Fatalf("BlendPoolReserves: %v", err)
	}
	if len(conn.queries) != 1 {
		t.Fatalf("issued %d queries, want ONE batched lookup", len(conn.queries))
	}
	q := conn.queries[0]

	// #504: the reserve state comes from the current-state projection, whose
	// sort key IS (entry_type, key_xdr) — so the probe reads ~one row per
	// requested key. Folding the latest entry per key out of the CHANGES
	// table instead costs a window scan whose size tracks pool ACTIVITY, and
	// that cost is a floor under every pool regardless of reserve count.
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
	// Version resolution is now the projection's, and it is the SAME composite
	// the old argMax spelled out: ledger_entries_current is
	// ReplacingMergeTree(version) with version = (ledger_seq << 32) |
	// intra_ledger_seq, so FINAL keeps the LAST change in a ledger (audit
	// C2-4c). Nothing to assert in the text; what MUST be asserted is that the
	// removed-key filter cannot run before that collapse — moving
	// `entry_xdr != ''` into PREWHERE would drop the winning removal and
	// RESURRECT a key deleted later in the same ledger, which is exactly the
	// bug the old HAVING existed to avoid.
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

// TestScanBlendReserveParts_IgnoresUnrequestedKeys — the reverse index is the
// only thing tying a returned row to an asset. A row whose key is not in the
// index (an impossible answer, or a future column change) must be dropped, not
// attributed to the empty-string asset.
func TestScanBlendReserveParts_IgnoresUnrequestedKeys(t *testing.T) {
	refByKey := map[string]keyRef{blendTestResDataKey: {asset: blendTestAssetSAC, kind: "ResData"}}
	rows := &stubRows{data: [][]any{
		{"some-other-key", "AAAA"},
	}}
	byAsset, bstop, matched, err := scanBlendReserveParts(rows, refByKey)
	if err != nil {
		t.Fatalf("scanBlendReserveParts: %v", err)
	}
	if len(byAsset) != 0 {
		t.Errorf("unrequested key produced %v, want nothing", byAsset)
	}
	if bstop != 0 {
		t.Errorf("bstop = %d, want 0", bstop)
	}
	// `matched` scopes the TTL-liveness filter. A key we did not ask about
	// must not enter it: classifying it could only ever DROP reserves on the
	// strength of a row the reader never attributed to anything.
	if len(matched) != 0 {
		t.Errorf("matched = %v, want empty — an unrequested key must not be handed to the TTL filter", matched)
	}
}

// TestScanBlendReserveParts_MatchedTracksOnlyReportedEntries — `matched` is
// the TTL filter's input set, so it must list exactly the keys whose state
// this read is about to REPORT: the decoded ResData rows plus the instance
// row. A key that produced no usable row is not in it (nothing to drop), and
// an undecodable ResData row is not either (already absent).
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
	byAsset, _, matched, err := scanBlendReserveParts(rows, refByKey)
	if err != nil {
		t.Fatalf("scanBlendReserveParts: %v", err)
	}
	if len(byAsset) != 1 {
		t.Fatalf("byAsset = %v, want only the decodable reserve", byAsset)
	}
	want := []string{blendTestResDataKey}
	if !reflect.DeepEqual(matched, want) {
		t.Errorf("matched = %v, want %v — only entries actually reported may be TTL-classified", matched, want)
	}
}

// TestScanBlendReserveParts_UndecodableEntryIsSkippedNotFatal — a reserve
// whose entry does not decode is reported ABSENT (the reader's documented
// "reserves with no captured ResData are absent"), which the API renders as an
// omitted reserve. Erroring instead would blank the whole pool page for one
// bad row.
func TestScanBlendReserveParts_UndecodableEntryIsSkippedNotFatal(t *testing.T) {
	refByKey := map[string]keyRef{blendTestResDataKey: {asset: blendTestAssetSAC, kind: "ResData"}}
	rows := &stubRows{data: [][]any{{blendTestResDataKey, "!!!not-base64!!!"}}}
	byAsset, _, _, err := scanBlendReserveParts(rows, refByKey)
	if err != nil {
		t.Fatalf("one undecodable entry aborted the pool read: %v", err)
	}
	if len(byAsset) != 0 {
		t.Errorf("byAsset = %v, want the reserve reported absent", byAsset)
	}
}

// TestScanBlendReserveParts_TruncatedStreamIsAnError — unlike a single bad
// row, a truncated stream means reserves may be missing for reasons that have
// nothing to do with capture. Reporting them as "absent" would present a
// partially-read pool as a fully-read one with fewer reserves.
func TestScanBlendReserveParts_TruncatedStreamIsAnError(t *testing.T) {
	truncated := errors.New("stream truncated")
	rows := &stubRows{streamErr: truncated}
	if _, _, _, err := scanBlendReserveParts(rows, nil); !errors.Is(err, truncated) {
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

// TestDropArchivedBlendReserves_DropsOnlyPositivelyArchived — the fail-open
// contract. A reserve is removed ONLY on a positively-resolved lapsed TTL;
// TTLUnknown (no TTL row, unreadable wire shape) KEEPS it, because
// under-reporting live liquidity is the same class of error as the phantom
// liquidity this filter exists to remove.
func TestDropArchivedBlendReserves_DropsOnlyPositivelyArchived(t *testing.T) {
	data, refs, matched := blendDropFixture()
	cache := fixedTTLCache(map[string]TTLLiveness{
		"resdata-key-a": TTLArchived,
		"resdata-key-b": TTLUnknown, // unresolved — must be KEPT
		"instance-key":  TTLLive,
	})
	if err := dropArchivedBlendReserves(t.Context(), cache, data, refs, matched); err != nil {
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
	if err := dropArchivedBlendReserves(t.Context(), cache, data, refs, matched); err != nil {
		t.Fatalf("dropArchivedBlendReserves: %v", err)
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
	if err := dropArchivedBlendReserves(t.Context(), nil, data, refs, matched); err != nil {
		t.Fatalf("dropArchivedBlendReserves(nil cache): %v", err)
	}
	if len(data) != 2 {
		t.Errorf("data = %v, want both reserves kept — a reader with no verdict cache must fail OPEN, not drop everything", data)
	}
}

// TestBackstopRateFromInstance_MissesDegradeToZero — 0 means "backstop take
// unaccounted", i.e. the supply APR is reported GROSS. That is a deliberate
// degrade, so the miss paths must reach it rather than panicking on a
// non-instance ScVal or an instance with no Config entry.
func TestBackstopRateFromInstance_MissesDegradeToZero(t *testing.T) {
	// Not an instance at all.
	i := xdr.Int64(5)
	if got := backstopRateFromInstance(xdr.ScVal{Type: xdr.ScValTypeScvI64, I64: &i}); got != 0 {
		t.Errorf("non-instance ScVal gave bstop = %d, want 0", got)
	}
	// An instance with no storage map.
	if got := backstopRateFromInstance(xdr.ScVal{
		Type:     xdr.ScValTypeScvContractInstance,
		Instance: &xdr.ScContractInstance{Executable: xdr.ContractExecutable{Type: xdr.ContractExecutableTypeContractExecutableStellarAsset}},
	}); got != 0 {
		t.Errorf("instance without storage gave bstop = %d, want 0", got)
	}
	// An instance whose storage holds no "Config" key.
	other := xdr.ScSymbol("Something")
	v := xdr.Int64(1)
	storage := xdr.ScMap{{Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &other}, Val: xdr.ScVal{Type: xdr.ScValTypeScvI64, I64: &v}}}
	if got := backstopRateFromInstance(xdr.ScVal{
		Type: xdr.ScValTypeScvContractInstance,
		Instance: &xdr.ScContractInstance{
			Executable: xdr.ContractExecutable{Type: xdr.ContractExecutableTypeContractExecutableStellarAsset},
			Storage:    &storage,
		},
	}); got != 0 {
		t.Errorf("instance without a Config entry gave bstop = %d, want 0", got)
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
