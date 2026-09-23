package defindex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// HO-007: docs/protocols/defindex.md's "Events decoded" table said the
// DeFindexFactory `create`/`n_fee` topics "register the vault",
// contradicting the page's own header ("NEITHER vaults NOR strategies
// self-register from factory create events") and Decode itself, whose
// classifyFactory branch returns (nil, nil) and never decodes the
// body — vaults/strategies are curated-set only.
func TestDoc_FactoryEventsDoNotRegisterVault_HO007(t *testing.T) {
	t.Parallel()

	doc := readDefindexDoc(t)

	if strings.Contains(doc, "| `DeFindexFactory` | `create`, `n_fee` | registers the vault |") {
		t.Error("defindex.md: the Events-decoded table still says DeFindexFactory " +
			"create/n_fee events register the vault, contradicting the page's own header " +
			"and Decode's classifyFactory branch (dispatcher_adapter.go), which returns " +
			"(nil, nil) and never decodes the body (HO-007)")
	}
}

func readDefindexDoc(t *testing.T) string {
	t.Helper()
	root := findRepoRootForDoc(t)
	b, err := os.ReadFile(filepath.Join(root, "docs/protocols/defindex.md"))
	if err != nil {
		t.Fatalf("read docs/protocols/defindex.md: %v", err)
	}
	return string(b)
}

func findRepoRootForDoc(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repo root (no go.mod found walking up)")
		}
		dir = parent
	}
}
