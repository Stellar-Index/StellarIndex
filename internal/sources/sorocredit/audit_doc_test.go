package sorocredit

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestAuditDoc_TracksEveryClassifiedSymbol guards the Q075 class of drift:
// docs/operations/wasm-audits/sorocredit.md backs the `BackfillSafe` flag
// with a hardcoded tracked-symbol count and inventory in prose, and
// classify() grew an 8th arm (TreasuryUpdated, commit 8f4569d94) without
// the doc being updated. Fail loudly if the doc's stated count or symbol
// inventory ever falls behind EventSymbols() again.
func TestAuditDoc_TracksEveryClassifiedSymbol(t *testing.T) {
	const docPath = "../../../docs/operations/wasm-audits/sorocredit.md"
	b, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("read %s: %v", docPath, err)
	}
	doc := string(b)

	symbols := EventSymbols()
	wantPhrase := fmt.Sprintf("one of %d symbols", len(symbols))
	if !strings.Contains(doc, wantPhrase) {
		t.Errorf("audit doc does not contain %q — it hardcodes a tracked-symbol count that no longer matches classify(), which now routes %d symbols (%v)", wantPhrase, len(symbols), symbols)
	}

	for _, sym := range symbols {
		if !strings.Contains(doc, sym) {
			t.Errorf("audit doc never mentions tracked symbol %q — classify() routes it to an EventType but the doc's inventory doesn't cover it", sym)
		}
	}
}
