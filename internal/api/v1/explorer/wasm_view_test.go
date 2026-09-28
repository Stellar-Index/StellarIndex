package explorer

import "testing"

// TestWasmCacheControl_NotDayLong pins #1070: the wasm view is served at
// /v1/contracts/{id}/wasm, keyed on contract_id, not on the wasm hash it
// actually returns. An in-place upgrade changes the bytes behind that same
// URL (see the sibling /code-history endpoint), so treating the route as
// content-addressed-immutable let a CDN/browser serve pre-upgrade
// bytecode, exports and decompile for up to a day. Without a bound, the
// no-TTL branch previously returned "public, max-age=86400".
func TestWasmCacheControl_NotDayLong(t *testing.T) {
	got := wasmCacheControl(ContractWasmView{})
	if got == "public, max-age=86400" {
		t.Fatalf("wasmCacheControl(no ttl) = %q, a day-long cache on a contract_id-keyed URL serves stale post-upgrade bytecode (#1070)", got)
	}
	want := "public, max-age=60, s-maxage=300"
	if got != want {
		t.Errorf("wasmCacheControl(no ttl) = %q, want %q", got, want)
	}
}

// TestWasmCacheControl_TTLVerdictStaysShort guards the existing archival/
// restore branch: it must stay bounded regardless of the no-TTL band above.
func TestWasmCacheControl_TTLVerdictStaysShort(t *testing.T) {
	got := wasmCacheControl(ContractWasmView{TTL: &ContractTTLV{State: ttlStateArchived}})
	if want := "public, max-age=300"; got != want {
		t.Errorf("wasmCacheControl(archived ttl) = %q, want %q", got, want)
	}
}
