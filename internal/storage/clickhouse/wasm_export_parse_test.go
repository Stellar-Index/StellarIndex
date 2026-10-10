package clickhouse

import (
	_ "embed"
	"testing"
)

// contractRegisterWasm is a real Soroban contract module pulled from the r1
// lake (contract CAP6ZT7JC3ZCNELT4I7OJ6IBACRGRN2CWS5GBPCPYRLF3RLOTX33FAF6,
// wasm hash f89eb3cc…). Used as a golden fixture for the native export parser.
//
//go:embed testdata/contract_register.wasm
var contractRegisterWasm []byte

func TestParseWasmExports_RealContract(t *testing.T) {
	exports, err := parseWasmExports(contractRegisterWasm)
	if err != nil {
		t.Fatalf("parseWasmExports: %v", err)
	}

	// The module exports a memory + two globals + five FUNCTIONS. Only the
	// functions are surfaced (the explorer wants the callable API). These are
	// the contract's real Soroban entry points.
	want := map[string]bool{
		"get": true, "initialize": true, "register": true, "revoke": true, "_": true,
	}
	got := map[string]WasmExport{}
	for _, e := range exports {
		got[e.Name] = e
	}
	if len(got) != len(want) {
		names := make([]string, 0, len(got))
		for n := range got {
			names = append(names, n)
		}
		t.Fatalf("export count = %d %v, want %d %v", len(got), names, len(want), keys(want))
	}
	for n := range want {
		ex, ok := got[n]
		if !ok {
			t.Errorf("missing export %q", n)
			continue
		}
		// Every Soroban entry point resolved to a real signature (non-nil
		// param/result lists prove the type-section walk + import-offset math
		// chased the index correctly). A contract entry point returns the
		// host-value i64; "register"/"revoke" take args.
		if ex.Results == nil {
			t.Errorf("export %q: nil results — index resolution failed", n)
		}
		for _, vt := range append(append([]string{}, ex.Params...), ex.Results...) {
			switch vt {
			case "i32", "i64", "f32", "f64":
			default:
				t.Errorf("export %q: unexpected value type %q", n, vt)
			}
		}
	}

	// Spot-check a known signature: a Soroban contract function returns one
	// host value (i64).
	if reg := got["register"]; len(reg.Results) != 1 || reg.Results[0] != "i64" {
		t.Errorf("register results = %v, want [i64]", reg.Results)
	}
}

func TestParseWasmExports_MinimalModule(t *testing.T) {
	// A bare valid module header (magic + version) with no sections: valid,
	// zero exported functions.
	header := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	exports, err := parseWasmExports(header)
	if err != nil {
		t.Fatalf("parseWasmExports(minimal): %v", err)
	}
	if len(exports) != 0 {
		t.Fatalf("minimal module exports = %d, want 0", len(exports))
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// uleb encodes v as unsigned LEB128 (test helper for hand-built modules).
func uleb(v uint64) []byte {
	var out []byte
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			b |= 0x80
		}
		out = append(out, b)
		if v == 0 {
			return out
		}
	}
}

// TestParseWasmExports_RejectsMalformed: every input must fail cleanly, never
// OOM or panic. The huge count would drive a multi-GB prealloc from an
// attacker-influenced LEB128 (safeCap bounds make() by the remaining bytes).
// The huge lengths are a 9-byte varint near MaxInt64, for which `r.i+n`
// overflows negative, passes the bounds guard and panicked in the slice
// expression. stellar-core validates WASM at upload, but any corruption of
// entry_xdr that preserves XDR framing reaches this parser.
func TestParseWasmExports_RejectsMalformed(t *testing.T) {
	hdr := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	module := func(parts ...[]byte) []byte {
		out := append([]byte{}, hdr...)
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	}
	hugeCount := uleb(1 << 33) // ~8.6e9 declared type-section entries, no entry bytes
	huge := []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f}
	nameBody := append([]byte{0x01}, huge...) // 1 export, huge name length

	cases := []struct {
		name  string
		input []byte
	}{
		{"non-wasm input", []byte("not wasm at all")},
		{"nil input", nil},
		{"oversized declared count", module([]byte{secType}, uleb(uint64(len(hugeCount))), hugeCount)},
		{"oversized section size", module([]byte{0x07}, huge)},
		{"oversized name length", module([]byte{0x07, byte(len(nameBody))}, nameBody)},
	}
	for _, tc := range cases {
		if _, err := parseWasmExports(tc.input); err == nil {
			t.Errorf("want an error for %s, got nil", tc.name)
		}
	}
}
