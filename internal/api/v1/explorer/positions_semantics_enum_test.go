package explorer

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestAmountSemanticsMatchOpenAPIEnum keeps the AmountSemantics*
// constants and the spec's amount_semantics enum in lockstep: a value
// the handler emits but the spec omits breaks every generated client.
func TestAmountSemanticsMatchOpenAPIEnum(t *testing.T) {
	src, err := os.ReadFile("positions.go")
	if err != nil {
		t.Fatal(err)
	}
	var consts []string
	for _, m := range regexp.MustCompile(`(?m)^\s*AmountSemantics\w+\s*=\s*"([^"]+)"`).FindAllSubmatch(src, -1) {
		consts = append(consts, string(m[1]))
	}

	spec, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "openapi", "stellar-index.v1.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^\s*enum: \[(net_underlying_at_event_time[^\]]*)\]`).FindSubmatch(spec)
	if m == nil {
		t.Fatal("amount_semantics enum not found in openapi/stellar-index.v1.yaml")
	}
	var enum []string
	for _, v := range strings.Split(string(m[1]), ",") {
		enum = append(enum, strings.TrimSpace(v))
	}

	slices.Sort(consts)
	slices.Sort(enum)
	if len(consts) == 0 || !slices.Equal(consts, enum) {
		t.Errorf("AmountSemantics constants %v, spec enum %v", consts, enum)
	}
}
