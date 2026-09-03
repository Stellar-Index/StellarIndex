package clickhouse

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/stellar/go-stellar-sdk/xdr"
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
const (
	blendTestPool     = "CAJJZSGMMM3PD7N33TAPHGBUGTB43OC73HVIK2L2G6BNGGGYOSSYBXBD"
	blendTestAssetSAC = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75" // USDC SAC

	// PoolDataKey::ResData(USDC) under blendTestPool, persistent durability.
	blendTestResDataKey = "AAAABgAAAAESnMjMYzbx/bvcwPOYNDTDzbhf2eqFaXo3gtMY2HSlgAAAABAAAAABAAAAAgAAAA8AAAAHUmVzRGF0YQAAAAASAAAAAa3vzlmu5Slo92Bh1JTCUlt1ZZ+kKWpl9JnvKeVkd+SWAAAAAQ=="
	// The pool's own contract-instance entry, persistent then temporary.
	blendTestInstanceKeyPersistent = "AAAABgAAAAESnMjMYzbx/bvcwPOYNDTDzbhf2eqFaXo3gtMY2HSlgAAAABQAAAAB"
	blendTestInstanceKeyTemporary  = "AAAABgAAAAESnMjMYzbx/bvcwPOYNDTDzbhf2eqFaXo3gtMY2HSlgAAAABQAAAAA"
)

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
// territory rather than errors.
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

	// The COMPOSITE version key. A Blend ResData entry is commonly rewritten
	// several times inside ONE ledger, so argMax on ledger_seq alone ties and
	// serves an arbitrary MID-ledger reserve state (audit C2-4c).
	if !strings.Contains(q, "argMax(entry_xdr, (ledger_seq, intra_ledger_seq))") {
		t.Errorf("reserve lookup does not resolve versions on the composite (ledger_seq, intra_ledger_seq):\n%s", q)
	}
	// The removed-key drop must be a HAVING on the WINNING row, not a
	// pre-aggregation filter: filtering `entry_xdr != ''` first EXCLUDES a
	// same-ledger removal from the argMax and thus RESURRECTS a key deleted
	// later in that ledger.
	if !strings.Contains(q, "HAVING argMax(change_type, (ledger_seq, intra_ledger_seq)) != 'removed'") {
		t.Errorf("removed-key drop is not a HAVING on the winning row (a deleted reserve would resurrect):\n%s", q)
	}
	if strings.Contains(q, "WHERE") && strings.Contains(q, "entry_xdr != ''") {
		t.Errorf("reserve lookup reintroduced the pre-aggregation entry_xdr filter:\n%s", q)
	}
	for _, s := range []string{
		"entry_type = 'contract_data'",
		"key_xdr IN (?)",
		"GROUP BY key_xdr",
	} {
		if !strings.Contains(q, s) {
			t.Errorf("reserve lookup missing %q:\n%s", s, q)
		}
	}

	// Two bound args, in clause order: the window width, then the key list.
	args := conn.args[0]
	if len(args) != 2 {
		t.Fatalf("bound %d args, want 2 (window width, key list)", len(args))
	}
	if _, ok := args[0].(uint32); !ok {
		t.Errorf("arg 0 is %T, want the uint32 window width", args[0])
	}
	keys, ok := args[1].([]string)
	if !ok {
		t.Fatalf("arg 1 is %T, want []string of key_xdr", args[1])
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
	byAsset, bstop, err := scanBlendReserveParts(rows, refByKey)
	if err != nil {
		t.Fatalf("scanBlendReserveParts: %v", err)
	}
	if len(byAsset) != 0 {
		t.Errorf("unrequested key produced %v, want nothing", byAsset)
	}
	if bstop != 0 {
		t.Errorf("bstop = %d, want 0", bstop)
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
	byAsset, _, err := scanBlendReserveParts(rows, refByKey)
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
	if _, _, err := scanBlendReserveParts(rows, nil); !errors.Is(err, truncated) {
		t.Fatalf("err = %v, want it to wrap %v", err, truncated)
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
