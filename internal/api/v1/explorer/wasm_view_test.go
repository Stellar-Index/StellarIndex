package explorer

import "testing"

// TestWasmCacheControl_NotDayLong: the wasm view is keyed on contract_id, not on the wasm hash it
// returns. An in-place upgrade changes the bytes behind the same URL, so treating the route as
// content-addressed-immutable would let a CDN serve pre-upgrade bytecode for up to a day.
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
