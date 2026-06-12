package main

import (
	"os"
	"testing"

	sdkxdr "github.com/stellar/go-stellar-sdk/xdr"
)

// TestRecordWasmTransition_FirstSeen confirms an initial deploy
// opens an entry whose ToLedger is unset (open range).
func TestRecordWasmTransition_FirstSeen(t *testing.T) {
	state := map[sdkxdr.Hash]*wasmContractState{}
	var contract sdkxdr.Hash
	contract[0] = 0x01
	recordWasmTransition(state, contract, "abc", 100, nil)

	if got := len(state[contract].ranges); got != 1 {
		t.Fatalf("ranges len = %d, want 1", got)
	}
	r := state[contract].ranges[0]
	if r.WasmHash != "abc" || r.FromLedger != 100 || r.ToLedger != 0 {
		t.Errorf("range = %+v, want {WasmHash:abc FromLedger:100 ToLedger:0}", r)
	}
	if state[contract].current != "abc" {
		t.Errorf("current = %q, want abc", state[contract].current)
	}
}

// TestRecordWasmTransition_RepeatSameHash is a no-op — the same
// hash seen at successive ledgers must NOT split into ranges.
func TestRecordWasmTransition_RepeatSameHash(t *testing.T) {
	state := map[sdkxdr.Hash]*wasmContractState{}
	var contract sdkxdr.Hash
	recordWasmTransition(state, contract, "abc", 100, nil)
	recordWasmTransition(state, contract, "abc", 101, nil)
	recordWasmTransition(state, contract, "abc", 200, nil)

	if got := len(state[contract].ranges); got != 1 {
		t.Fatalf("ranges len = %d, want 1 (idempotent on same hash)", got)
	}
}

// TestRecordWasmTransition_Upgrade closes the prior range and
// opens a new one when the hash flips.
func TestRecordWasmTransition_Upgrade(t *testing.T) {
	state := map[sdkxdr.Hash]*wasmContractState{}
	var contract sdkxdr.Hash
	recordWasmTransition(state, contract, "v1hash", 100, nil)
	recordWasmTransition(state, contract, "v1hash", 150, nil) // no-op
	recordWasmTransition(state, contract, "v2hash", 200, nil) // upgrade!
	recordWasmTransition(state, contract, "v3hash", 300, nil) // upgrade again

	ranges := state[contract].ranges
	if len(ranges) != 3 {
		t.Fatalf("ranges len = %d, want 3", len(ranges))
	}
	want := []wasmRange{
		{WasmHash: "v1hash", FromLedger: 100, ToLedger: 199},
		{WasmHash: "v2hash", FromLedger: 200, ToLedger: 299},
		{WasmHash: "v3hash", FromLedger: 300, ToLedger: 0},
	}
	for i, w := range want {
		if ranges[i] != w {
			t.Errorf("range[%d] = %+v, want %+v", i, ranges[i], w)
		}
	}
}

// TestScanLedgerEntryChange_IgnoresUnrelatedContract — a watched
// contract update should NOT trigger when an unrelated contract
// changes.
func TestScanLedgerEntryChange_IgnoresUnrelatedContract(t *testing.T) {
	var watched, other sdkxdr.Hash
	watched[0] = 0xAA
	other[0] = 0xBB

	watch := map[sdkxdr.Hash]string{watched: "CDLZ_watched"}
	state := map[sdkxdr.Hash]*wasmContractState{}

	change := makeUpdateChange(t, other, [32]byte{1, 2, 3})
	scanLedgerEntryChange(&change, watch, state, 100, nil)

	if len(state) != 0 {
		t.Errorf("state populated for unwatched contract: %v", state)
	}
}

// TestScanLedgerEntryChange_CapturesWatchedUpgrade — a watched
// contract update emits a transition.
func TestScanLedgerEntryChange_CapturesWatchedUpgrade(t *testing.T) {
	var watched sdkxdr.Hash
	watched[0] = 0xAA

	watch := map[sdkxdr.Hash]string{watched: "CDLZ_watched"}
	state := map[sdkxdr.Hash]*wasmContractState{}

	wasmHash := [32]byte{0xDE, 0xAD, 0xBE, 0xEF}
	change := makeUpdateChange(t, watched, wasmHash)
	scanLedgerEntryChange(&change, watch, state, 12345, nil)

	if len(state) != 1 {
		t.Fatalf("state has %d entries, want 1", len(state))
	}
	got := state[watched].ranges
	if len(got) != 1 {
		t.Fatalf("ranges len = %d, want 1", len(got))
	}
	if got[0].FromLedger != 12345 {
		t.Errorf("from = %d, want 12345", got[0].FromLedger)
	}
	wantHashHex := "deadbeef00000000000000000000000000000000000000000000000000000000"
	if got[0].WasmHash != wantHashHex {
		t.Errorf("hash = %s, want %s", got[0].WasmHash, wantHashHex)
	}
}

// makeUpdateChange constructs a synthetic LedgerEntryChange of type
// Updated whose ContractData entry corresponds to the given
// contract's instance row, with the given executable WASM hash.
func makeUpdateChange(t *testing.T, contract sdkxdr.Hash, wasmHash [32]byte) sdkxdr.LedgerEntryChange {
	t.Helper()
	contractID := sdkxdr.ContractId(contract)
	w := sdkxdr.Hash(wasmHash)
	scAddr := sdkxdr.ScAddress{
		Type:       sdkxdr.ScAddressTypeScAddressTypeContract,
		ContractId: &contractID,
	}
	instance := sdkxdr.ScContractInstance{
		Executable: sdkxdr.ContractExecutable{
			Type:     sdkxdr.ContractExecutableTypeContractExecutableWasm,
			WasmHash: &w,
		},
	}
	val := sdkxdr.ScVal{
		Type:     sdkxdr.ScValTypeScvContractInstance,
		Instance: &instance,
	}
	key := sdkxdr.ScVal{Type: sdkxdr.ScValTypeScvLedgerKeyContractInstance}
	cd := sdkxdr.ContractDataEntry{
		Contract:   scAddr,
		Key:        key,
		Durability: sdkxdr.ContractDataDurabilityPersistent,
		Val:        val,
	}
	data := sdkxdr.LedgerEntryData{
		Type:         sdkxdr.LedgerEntryTypeContractData,
		ContractData: &cd,
	}
	entry := &sdkxdr.LedgerEntry{Data: data}
	return sdkxdr.LedgerEntryChange{
		Type:    sdkxdr.LedgerEntryChangeTypeLedgerEntryUpdated,
		Updated: entry,
	}
}

// makeStorageChange builds a synthetic LedgerEntryChange for a
// non-Instance ContractData entry (typical "custom storage"
// shape) that recordStorageChange should pick up when the
// contract is in the watch list.
func makeStorageChange(t *testing.T, contract sdkxdr.Hash, keySymbol string) sdkxdr.LedgerEntryChange {
	t.Helper()
	contractID := sdkxdr.ContractId(contract)
	scAddr := sdkxdr.ScAddress{
		Type:       sdkxdr.ScAddressTypeScAddressTypeContract,
		ContractId: &contractID,
	}
	sym := sdkxdr.ScSymbol(keySymbol)
	key := sdkxdr.ScVal{Type: sdkxdr.ScValTypeScvSymbol, Sym: &sym}
	val := sdkxdr.ScVal{Type: sdkxdr.ScValTypeScvU32, U32: func() *sdkxdr.Uint32 { v := sdkxdr.Uint32(42); return &v }()}
	cd := sdkxdr.ContractDataEntry{
		Contract:   scAddr,
		Key:        key,
		Durability: sdkxdr.ContractDataDurabilityPersistent,
		Val:        val,
	}
	entry := &sdkxdr.LedgerEntry{Data: sdkxdr.LedgerEntryData{
		Type:         sdkxdr.LedgerEntryTypeContractData,
		ContractData: &cd,
	}}
	return sdkxdr.LedgerEntryChange{
		Type:    sdkxdr.LedgerEntryChangeTypeLedgerEntryUpdated,
		Updated: entry,
	}
}

// TestRecordStorageChange_CapturesNonInstanceKeys is the storage-
// rotation scanner's positive path: a watched contract has a
// non-Instance ContractData change → one storageChange recorded.
func TestRecordStorageChange_CapturesNonInstanceKeys(t *testing.T) {
	var watched sdkxdr.Hash
	watched[0] = 0xa1
	watch := map[sdkxdr.Hash]string{watched: "C..."}
	out := map[sdkxdr.Hash][]storageChange{}

	change := makeStorageChange(t, watched, "ADMIN")
	recordStorageChange(&change, watch, out, 12345)

	got := out[watched]
	if len(got) != 1 {
		t.Fatalf("storage changes len = %d, want 1", len(got))
	}
	if got[0].Ledger != 12345 {
		t.Errorf("ledger = %d, want 12345", got[0].Ledger)
	}
	if got[0].ChangeType != "updated" {
		t.Errorf("change_type = %q, want updated", got[0].ChangeType)
	}
	if got[0].Durability != "persistent" {
		t.Errorf("durability = %q, want persistent", got[0].Durability)
	}
	if got[0].KeyHint != `symbol("ADMIN")` {
		t.Errorf("key_hint = %q, want symbol(\"ADMIN\")", got[0].KeyHint)
	}
}

// TestRecordStorageChange_SkipsInstanceKey ensures the storage-
// rotation scanner ignores the LedgerKeyContractInstance row
// (already covered by the wasm-history tracker; including it would
// double-count and pollute the storage log).
func TestRecordStorageChange_SkipsInstanceKey(t *testing.T) {
	var watched sdkxdr.Hash
	watched[0] = 0xa2
	var w [32]byte
	w[0] = 0xde
	w[1] = 0xad
	change := makeUpdateChange(t, watched, w)
	watch := map[sdkxdr.Hash]string{watched: "C..."}
	out := map[sdkxdr.Hash][]storageChange{}

	recordStorageChange(&change, watch, out, 12345)

	if len(out) != 0 {
		t.Fatalf("expected 0 entries (Instance key should be skipped), got %d", len(out))
	}
}

// TestRecordStorageChange_SkipsUnwatchedContract ensures the
// scanner short-circuits on contracts not in the watch list.
func TestRecordStorageChange_SkipsUnwatchedContract(t *testing.T) {
	var unrelated sdkxdr.Hash
	unrelated[0] = 0xff
	change := makeStorageChange(t, unrelated, "ADMIN")

	var watched sdkxdr.Hash
	watched[0] = 0xa3
	watch := map[sdkxdr.Hash]string{watched: "C..."}
	out := map[sdkxdr.Hash][]storageChange{}

	recordStorageChange(&change, watch, out, 12345)

	if len(out) != 0 {
		t.Fatalf("expected 0 entries (unrelated contract), got %d", len(out))
	}
}

// makeCodeUploadChange builds a synthetic ContractCode Created
// LedgerEntryChange — the entry type the code-upload scanner is
// watching for.
func makeCodeUploadChange(t *testing.T, hash [32]byte, code []byte) sdkxdr.LedgerEntryChange {
	t.Helper()
	cc := sdkxdr.ContractCodeEntry{
		Hash: sdkxdr.Hash(hash),
		Code: code,
	}
	entry := &sdkxdr.LedgerEntry{Data: sdkxdr.LedgerEntryData{
		Type:         sdkxdr.LedgerEntryTypeContractCode,
		ContractCode: &cc,
	}}
	return sdkxdr.LedgerEntryChange{
		Type:    sdkxdr.LedgerEntryChangeTypeLedgerEntryCreated,
		Created: entry,
	}
}

// TestMaybeAppendCodeUpload_CapturesCreatedAndRestored covers the
// two change types the code-upload scanner accepts.
func TestMaybeAppendCodeUpload_CapturesCreatedAndRestored(t *testing.T) {
	var hash [32]byte
	hash[0] = 0xca
	hash[1] = 0xfe
	body := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00, 0xde, 0xad}

	// Created
	created := makeCodeUploadChange(t, hash, body)
	got := maybeAppendCodeUpload(&created, nil, 100)
	if len(got) != 1 || got[0].ChangeType != "created" || got[0].Ledger != 100 || got[0].SizeBytes != len(body) {
		t.Fatalf("Created not captured cleanly: %+v", got)
	}

	// Restored
	restored := sdkxdr.LedgerEntryChange{
		Type: sdkxdr.LedgerEntryChangeTypeLedgerEntryRestored,
		Restored: &sdkxdr.LedgerEntry{Data: sdkxdr.LedgerEntryData{
			Type: sdkxdr.LedgerEntryTypeContractCode,
			ContractCode: &sdkxdr.ContractCodeEntry{
				Hash: sdkxdr.Hash(hash),
				Code: body,
			},
		}},
	}
	got = maybeAppendCodeUpload(&restored, got, 200)
	if len(got) != 2 || got[1].ChangeType != "restored" || got[1].Ledger != 200 {
		t.Fatalf("Restored not captured cleanly: %+v", got)
	}

	// Updated should be ignored — ContractCode bytes are immutable
	// in Soroban; Updated only adjusts TTL.
	updated := sdkxdr.LedgerEntryChange{
		Type:    sdkxdr.LedgerEntryChangeTypeLedgerEntryUpdated,
		Updated: created.Created,
	}
	got2 := maybeAppendCodeUpload(&updated, got, 300)
	if len(got2) != len(got) {
		t.Errorf("Updated should be ignored, but uploads grew from %d to %d", len(got), len(got2))
	}
}

// TestMaybeAppendCodeUpload_IgnoresOtherEntryTypes ensures
// non-ContractCode entries (Account, Trustline, ContractData) are
// not picked up by the code-upload scanner.
func TestMaybeAppendCodeUpload_IgnoresOtherEntryTypes(t *testing.T) {
	var contract sdkxdr.Hash
	var w [32]byte
	change := makeUpdateChange(t, contract, w) // ContractData entry
	got := maybeAppendCodeUpload(&change, nil, 100)
	if len(got) != 0 {
		t.Errorf("ContractData entry should not be captured by code-upload scanner; got %d uploads", len(got))
	}
}

// TestStorageKeyHint covers the common SCVal key shapes the hint
// helper recognises. Best-effort summaries; doesn't need to cover
// every SCVal variant.
func TestStorageKeyHint(t *testing.T) {
	mkSym := func(s string) sdkxdr.ScVal {
		sym := sdkxdr.ScSymbol(s)
		return sdkxdr.ScVal{Type: sdkxdr.ScValTypeScvSymbol, Sym: &sym}
	}
	cases := []struct {
		name string
		key  sdkxdr.ScVal
		want string
	}{
		{"symbol", mkSym("ADMIN"), `symbol("ADMIN")`},
		{"u32", sdkxdr.ScVal{Type: sdkxdr.ScValTypeScvU32, U32: func() *sdkxdr.Uint32 { v := sdkxdr.Uint32(7); return &v }()}, "u32(7)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := storageKeyHint(tc.key)
			if got != tc.want {
				t.Errorf("storageKeyHint(%s) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

// TestBuildRangesFromTransitions covers the merge tool's core
// reconstruction logic — JSONL transitions back to the wasmRange
// shape `wasmHistory` would have produced at end-of-run.
func TestBuildRangesFromTransitions(t *testing.T) {
	t.Run("single transition closes at -to", func(t *testing.T) {
		trs := []transitionRecord{{Contract: "C...", WasmHash: "a", AtLedger: 100}}
		got := buildRangesFromTransitions(trs, 1000)
		want := []wasmRange{{WasmHash: "a", FromLedger: 100, ToLedger: 1000}}
		if len(got) != 1 || got[0] != want[0] {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})

	t.Run("two transitions chain through hash boundaries", func(t *testing.T) {
		trs := []transitionRecord{
			{Contract: "C...", WasmHash: "a", AtLedger: 100},
			{Contract: "C...", WasmHash: "b", AtLedger: 500},
		}
		got := buildRangesFromTransitions(trs, 1000)
		want := []wasmRange{
			{WasmHash: "a", FromLedger: 100, ToLedger: 499},
			{WasmHash: "b", FromLedger: 500, ToLedger: 1000},
		}
		if len(got) != 2 {
			t.Fatalf("len=%d, want 2", len(got))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("range[%d] = %+v, want %+v", i, got[i], want[i])
			}
		}
	})

	t.Run("collapses adjacent same-hash transitions across worker boundary", func(t *testing.T) {
		// Worker 0 sees hash a at ledger 100; worker 1 starts fresh
		// and re-observes hash a at its lower bound 600 (its first
		// observation). The merge tool must collapse — no real
		// transition happened between the workers.
		trs := []transitionRecord{
			{Contract: "C...", WasmHash: "a", AtLedger: 100},
			{Contract: "C...", WasmHash: "a", AtLedger: 600}, // worker boundary
			{Contract: "C...", WasmHash: "b", AtLedger: 800},
		}
		got := buildRangesFromTransitions(trs, 1000)
		want := []wasmRange{
			{WasmHash: "a", FromLedger: 100, ToLedger: 799},
			{WasmHash: "b", FromLedger: 800, ToLedger: 1000},
		}
		if len(got) != 2 {
			t.Fatalf("len=%d, want 2 (got %+v)", len(got), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("range[%d] = %+v, want %+v", i, got[i], want[i])
			}
		}
	})

	t.Run("empty input yields empty output", func(t *testing.T) {
		got := buildRangesFromTransitions(nil, 1000)
		if got != nil {
			t.Errorf("got %+v, want nil", got)
		}
	})
}

// TestReadTransitionJSONL_RoundTrip writes a synthetic JSONL file
// (matching the shape `transitionLog.append` produces) and confirms
// the merge tool's reader consumes it correctly.
func TestReadTransitionJSONL_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/wasm-history-w0.jsonl"
	contents := `{"contract":"C1","wasm_hash":"a","at_ledger":100}
{"contract":"C1","wasm_hash":"b","at_ledger":500}
{"contract":"C2","wasm_hash":"x","at_ledger":300}
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	transitions := make(map[string][]transitionRecord)
	n, err := readTransitionJSONL(path, transitions)
	if err != nil {
		t.Fatalf("readTransitionJSONL: %v", err)
	}
	if n != 3 {
		t.Errorf("count = %d, want 3", n)
	}
	if len(transitions["C1"]) != 2 {
		t.Errorf("C1 transitions = %d, want 2", len(transitions["C1"]))
	}
	if len(transitions["C2"]) != 1 {
		t.Errorf("C2 transitions = %d, want 1", len(transitions["C2"]))
	}
	if transitions["C1"][0].WasmHash != "a" || transitions["C1"][1].WasmHash != "b" {
		t.Errorf("C1 hashes = %v, want [a b]", transitions["C1"])
	}
}

// TestReadTransitionJSONL_TruncatedTail proves the merge tool
// recovers from a half-written trailing line — exactly the failure
// shape a crashed walker would produce. The good lines must still
// land in the transitions map.
func TestReadTransitionJSONL_TruncatedTail(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/wasm-history-w0.jsonl"
	contents := `{"contract":"C1","wasm_hash":"a","at_ledger":100}
{"contract":"C1","wasm_hash":"b","at_ledger":500}
{"contract":"C1","wasm_hash":"c","at_le` // truncated mid-line
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	transitions := make(map[string][]transitionRecord)
	n, err := readTransitionJSONL(path, transitions)
	if err != nil {
		t.Fatalf("readTransitionJSONL: %v", err)
	}
	if n != 2 {
		t.Errorf("count = %d, want 2 (good lines before truncation)", n)
	}
	if len(transitions["C1"]) != 2 {
		t.Errorf("C1 transitions = %d, want 2", len(transitions["C1"]))
	}
}
