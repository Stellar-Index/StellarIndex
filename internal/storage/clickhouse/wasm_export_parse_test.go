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

func TestParseWasmExports_BadMagic(t *testing.T) {
	if _, err := parseWasmExports([]byte("not wasm at all")); err == nil {
		t.Fatal("expected error on non-wasm input")
	}
	if _, err := parseWasmExports(nil); err == nil {
		t.Fatal("expected error on nil input")
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

// TestParseWasmExports_HugeCountDoesNotOOM is the regression proof for
// W6-go-1: a section declaring a preposterous entry count in a tiny body must
// fail cleanly (the read loop runs out of input) rather than attempt a
// multi-GB prealloc from the attacker-influenced LEB128 count. safeCap bounds
// make() by the reader's remaining bytes; reaching the assertion at all (no
// OOM/panic on the prealloc) is itself the proof.
func TestParseWasmExports_HugeCountDoesNotOOM(t *testing.T) {
	body := uleb(1 << 33) // ~8.6e9 declared type-section entries, no entry bytes
	section := append([]byte{secType}, uleb(uint64(len(body)))...)
	section = append(section, body...)
	module := append([]byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}, section...)

	if _, err := parseWasmExports(module); err == nil {
		t.Fatal("expected a truncation error on an oversized declared count, got nil")
	}
}

// TestParseWasmExports_HugeLengthDoesNotPanic pins the signed-overflow
// vector that the existing huge-COUNT test does not reach.
//
// `bytes(n)` guarded with `r.i+n > len(r.b)`. n is decoded from an
// attacker-authored LEB128, and a 9-byte varint yields values near
// MaxInt64 — for which `r.i+n` overflows to a NEGATIVE number, passes the
// guard, and panics in the slice expression. Reproduced from both call
// sites (a section size and a name length) as
// `slice bounds out of range [:-9223372036854775791]`.
//
// stellar-core validates WASM at upload_contract_wasm, so this cannot
// arrive from a successful on-chain upload today. It is reachable from
// any corruption of entry_xdr that preserves XDR framing — and nothing
// downstream verifies sha256(code) against the content-addressed key —
// and from any future caller feeding non-chain bytes (cold audit
// 2026-08-04).
func TestParseWasmExports_HugeLengthDoesNotPanic(t *testing.T) {
	// LEB128 encoding of a value near MaxInt64.
	huge := []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f}

	hdr := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}

	t.Run("section size", func(t *testing.T) {
		mod := append(append([]byte{}, hdr...), 0x07) // export section id
		mod = append(mod, huge...)                    // section size
		if _, err := parseWasmExports(mod); err == nil {
			t.Error("want an error for an oversized section size, got nil")
		}
	})

	t.Run("name length", func(t *testing.T) {
		body := append([]byte{0x01}, huge...) // 1 export, huge name length
		mod := append(append([]byte{}, hdr...), 0x07)
		mod = append(mod, byte(len(body)))
		mod = append(mod, body...)
		if _, err := parseWasmExports(mod); err == nil {
			t.Error("want an error for an oversized name length, got nil")
		}
	})
}
