package explorer

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestTxSummaryView_MemoBase64_NonUTF8 pins GH-1140: a MEMO_TEXT memo is
// opaque XDR bytes, not guaranteed UTF-8. Without memo_base64, marshaling the
// response silently replaces invalid bytes with U+FFFD — this proves the
// wire JSON carries a lossless companion that round-trips the exact bytes.
func TestTxSummaryView_MemoBase64_NonUTF8(t *testing.T) {
	raw := string([]byte{0x01, 0xff, 0xfe})
	v := txSummaryView(clickhouse.TxSummary{MemoType: "MemoTypeMemoText", Memo: raw})

	if v.MemoType != "text" {
		t.Fatalf("memo_type = %q, want text", v.MemoType)
	}

	body, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire struct {
		Memo       string `json:"memo"`
		MemoBase64 string `json:"memo_base64"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// The defect this pins: json.Marshal has already mangled Memo itself
	// (invalid bytes -> U+FFFD), so a plain round-trip of Memo does NOT
	// recover the original. MemoBase64 must.
	if wire.Memo == raw {
		t.Fatalf("wire memo unexpectedly matches raw bytes; test fixture no longer exercises invalid UTF-8")
	}
	decoded, err := base64.StdEncoding.DecodeString(wire.MemoBase64)
	if err != nil {
		t.Fatalf("decode memo_base64: %v", err)
	}
	if string(decoded) != raw {
		t.Fatalf("memo_base64 round-trip = %q, want %q (the original bytes)", decoded, raw)
	}
}

// TestTxSummaryView_MemoBase64_AbsentForNonText guards against noise: a
// memo_type other than "text" (or none) must not carry a memo_base64 field.
func TestTxSummaryView_MemoBase64_AbsentForNonText(t *testing.T) {
	v := txSummaryView(clickhouse.TxSummary{MemoType: "MemoTypeMemoId", Memo: "12345"})
	if v.MemoBase64 != "" {
		t.Fatalf("memo_base64 = %q, want empty for memo_type %q", v.MemoBase64, v.MemoType)
	}
}
