package scval

import (
	"encoding/base64"
	"testing"
)

// TestEncodeDecodeArgs_RoundTrip is the op-args plumbing the
// soroban_events landing zone (ADR-0029) + projector-replay depend
// on: InvokeContract args are stored as a marshalled ScVal::Vec and
// later reconstructed to the per-arg base64 blobs events.Event.OpArgs
// expects. A regression here silently corrupts the args Band /
// Redstone decode their feed IDs + relay values from. XDR marshalling
// is canonical, so an exact byte-for-byte round-trip must hold.
func TestEncodeDecodeArgs_RoundTrip(t *testing.T) {
	args := []string{
		MustEncodeSymbol("relay"),
		MustEncodeString("BTC"),
		MustEncodeSymbol("force_relay"),
	}

	encoded, err := EncodeArgsAsScVec(args)
	if err != nil {
		t.Fatalf("EncodeArgsAsScVec: %v", err)
	}
	if len(encoded) == 0 {
		t.Fatal("EncodeArgsAsScVec returned empty bytes for non-empty args")
	}

	decoded, err := DecodeScVecToArgs(encoded)
	if err != nil {
		t.Fatalf("DecodeScVecToArgs: %v", err)
	}
	if len(decoded) != len(args) {
		t.Fatalf("decoded %d args, want %d", len(decoded), len(args))
	}
	for i := range args {
		if decoded[i] != args[i] {
			t.Errorf("arg[%d] = %q, want %q (order/value must round-trip)", i, decoded[i], args[i])
		}
	}
}

// TestEncodeDecodeArgs_Empty — empty in, empty out on BOTH sides.
// EncodeArgsAsScVec(nil) must return (nil, nil) so the sink stores
// SQL NULL, and DecodeScVecToArgs(nil) must return (nil, nil) so a
// NULL op_args column reconstructs to "no args", never an error.
func TestEncodeDecodeArgs_Empty(t *testing.T) {
	for _, in := range [][]string{nil, {}} {
		b, err := EncodeArgsAsScVec(in)
		if err != nil {
			t.Errorf("EncodeArgsAsScVec(%v): unexpected err %v", in, err)
		}
		if b != nil {
			t.Errorf("EncodeArgsAsScVec(%v) = %v, want nil bytes", in, b)
		}
	}
	for _, in := range [][]byte{nil, {}} {
		out, err := DecodeScVecToArgs(in)
		if err != nil {
			t.Errorf("DecodeScVecToArgs(%v): unexpected err %v", in, err)
		}
		if out != nil {
			t.Errorf("DecodeScVecToArgs(%v) = %v, want nil", in, out)
		}
	}
}

// TestEncodeArgs_InvalidBase64 — a non-decodable arg blob is a wire
// corruption; encode must fail loudly (wrapped with the arg index),
// not silently drop the arg from the vec.
func TestEncodeArgs_InvalidBase64(t *testing.T) {
	_, err := EncodeArgsAsScVec([]string{MustEncodeSymbol("ok"), "!!!not-base64!!!"})
	if err == nil {
		t.Fatal("EncodeArgsAsScVec with a malformed arg: want error, got nil")
	}
}

// TestDecodeScVec_WrongShape — the column commits to ScVal::Vec.
// A blob that unmarshals to some OTHER ScVal type (here a bare
// Symbol) must error rather than being interpreted as a zero-arg
// list, so a schema/producer drift is caught instead of silently
// dropping every arg.
func TestDecodeScVec_WrongShape(t *testing.T) {
	// MustEncodeSymbol yields a base64 ScVal::Symbol; its raw bytes
	// are a valid ScVal but NOT a Vec.
	notAVec, err := base64.StdEncoding.DecodeString(MustEncodeSymbol("swap"))
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if _, err := DecodeScVecToArgs(notAVec); err == nil {
		t.Error("DecodeScVecToArgs on a non-Vec ScVal: want error, got nil")
	}

	// Outright garbage must also error (unmarshal failure), never panic.
	if _, err := DecodeScVecToArgs([]byte{0xde, 0xad, 0xbe, 0xef}); err == nil {
		t.Error("DecodeScVecToArgs on garbage bytes: want error, got nil")
	}
}
