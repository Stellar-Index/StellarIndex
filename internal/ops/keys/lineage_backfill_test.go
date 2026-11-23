package keys

import (
	"errors"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
)

func TestLineageBackfillFlags_RequireAStatedMode(t *testing.T) {
	if _, _, _, err := parseLineageBackfillFlags([]string{"-config", "x.toml"}); !errors.Is(err, opsutil.ErrWriteModeUnstated) {
		t.Fatalf("no -write or -dry-run: err = %v, want ErrWriteModeUnstated", err)
	}
	for _, mode := range []string{"-dry-run", "-write"} {
		if _, _, _, err := parseLineageBackfillFlags([]string{"-config", "x.toml", mode}); err != nil {
			t.Errorf("%s: %v", mode, err)
		}
	}
	if _, _, _, err := parseLineageBackfillFlags([]string{"-dry-run"}); err == nil {
		t.Error("missing -config accepted")
	}
}

func TestPrintLineageBackfill_NamesKeysByIDAndModeVerb(t *testing.T) {
	rep := auth.LineageBackfillReport{Scanned: 5, Delegated: 3, Complete: 1, Resolved: []string{"k-a"}, Orphaned: []string{"k-b"}, MaxDepth: 2}
	out := lineageBackfillSummary(rep, false)
	for _, want := range []string{"scanned 5 records: 3 delegated, 1 already complete, max depth 2", "would record the chain of 1 keys: k-a", "orphaned", "1 keys: k-b"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output lacks %q:\n%s", want, out)
		}
	}
}
