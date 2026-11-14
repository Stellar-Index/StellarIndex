package clickhouse

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// TestEffectiveEventName covers the topic name-recovery priority behind the protocol event-breakdown pane,
// including recovering a non-Symbol topic[0] action name so phoenix-style events do not land in "untyped".
func TestEffectiveEventName(t *testing.T) {
	var (
		symPOOL   = scval.MustEncodeSymbol("POOL")
		symSwap   = scval.MustEncodeSymbol("swap")
		strPrefix = scval.MustEncodeString("SoroswapPair")
		strSwap   = scval.MustEncodeString("swap") // Phoenix action topic[0]
		strSender = scval.MustEncodeString("sender")
		strSpaces = scval.MustEncodeString("actual received amount")
	)

	cases := []struct {
		name      string
		topic0Sym string
		topic1XDR string
		topic0XDR string
		want      string
	}{
		{
			// topic[0] is a Symbol (comet/aquarius) — denormalized column wins.
			name:      "symbol-topic0",
			topic0Sym: "POOL",
			topic1XDR: symSwap, // ignored — topic_0_sym present
			topic0XDR: symPOOL,
			want:      "POOL",
		},
		{
			// Soroswap: [String("SoroswapPair"), Symbol("swap")] — name from topic[1].
			name:      "soroswap-topic1-symbol",
			topic0Sym: "",
			topic1XDR: symSwap,
			topic0XDR: strPrefix,
			want:      "swap",
		},
		{
			// Soroswap must NOT regress to the "SoroswapPair" prefix label.
			name:      "soroswap-not-prefix",
			topic0Sym: "",
			topic1XDR: scval.MustEncodeSymbol("sync"),
			topic0XDR: strPrefix,
			want:      "sync",
		},
		{
			// Phoenix: [String("swap"), String("sender")] — topic[1] is a
			// String field name (Symbol decode fails) so recover topic[0].
			name:      "phoenix-topic0-string-action",
			topic0Sym: "",
			topic1XDR: strSender,
			topic0XDR: strSwap,
			want:      "swap",
		},
		{
			// Phoenix field with spaces on topic[1] — still recovers topic[0].
			name:      "phoenix-spaced-field",
			topic0Sym: "",
			topic1XDR: strSpaces,
			topic0XDR: strSwap,
			want:      "swap",
		},
		{
			// Nothing decodable → untyped (empty).
			name:      "untyped",
			topic0Sym: "",
			topic1XDR: "",
			topic0XDR: "",
			want:      "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectiveEventName(tc.topic0Sym, tc.topic1XDR, tc.topic0XDR); got != tc.want {
				t.Errorf("effectiveEventName(%q,%q,%q) = %q, want %q",
					tc.topic0Sym, tc.topic1XDR, tc.topic0XDR, got, tc.want)
			}
		})
	}
}

// TestDecodeTopicName asserts the Symbol-or-String recovery that
// distinguishes decodeTopicName from decodeTopicSymbol.
func TestDecodeTopicName(t *testing.T) {
	if got, ok := decodeTopicName(scval.MustEncodeSymbol("swap")); !ok || got != "swap" {
		t.Errorf("Symbol: got (%q,%v), want (swap,true)", got, ok)
	}
	if got, ok := decodeTopicName(scval.MustEncodeString("swap")); !ok || got != "swap" {
		t.Errorf("String: got (%q,%v), want (swap,true)", got, ok)
	}
	// decodeTopicSymbol stays Symbol-only so Phoenix String fields fall
	// through to the topic[0] arm rather than fragmenting the breakdown.
	if _, ok := decodeTopicSymbol(scval.MustEncodeString("sender")); ok {
		t.Error("decodeTopicSymbol must reject a String topic")
	}
	if _, ok := decodeTopicName(""); ok {
		t.Error("empty input must be ok=false")
	}
}

// TestFoldEventBreakdown_CutGroupsAreCountedAsOther: groups past the read limit stay in the composition as one
// labelled bucket, and nameless groups are counted as untyped by the reader.
func TestFoldEventBreakdown_CutGroupsAreCountedAsOther(t *testing.T) {
	groups := []eventBreakdownGroup{
		{sym: "swap", count: 70},
		{sym: "", count: 20},
	}
	got := map[string]uint64{}
	for _, r := range foldEventBreakdown(groups, 100) {
		got[r.EventType] = r.Count
	}
	if got["swap"] != 70 || got[UntypedEventType] != 20 || got[OtherEventType] != 10 || len(got) != 3 {
		t.Errorf("cut read = %v, want swap 70, untyped 20, other 10", got)
	}
	for _, r := range foldEventBreakdown(groups, 90) {
		if r.EventType == OtherEventType {
			t.Errorf("complete read grew an %q bucket: %+v", OtherEventType, r)
		}
	}
}
