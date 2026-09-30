package redstone

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestReadmeFeedCountMatchesRegistry stops README.md's "all N mainnet
// feeds" prose from drifting away from feedRegistry, the same failure
// mode TestFeedRegistry_CountMatchesItsDocComment already guards for
// feeds.go's doc comment (Q101: the README said 30 while the registry
// had grown to 32 via USDT0 and earnUSDC_FUNDAMENTAL).
func TestReadmeFeedCountMatchesRegistry(t *testing.T) {
	raw, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}

	re := regexp.MustCompile(`all (\d+) mainnet feeds`)
	m := re.FindSubmatch(raw)
	if m == nil {
		t.Fatal(`README.md no longer contains "all N mainnet feeds" — update this test's pattern`)
	}
	documented, err := strconv.Atoi(string(m[1]))
	if err != nil {
		t.Fatalf("parse documented feed count: %v", err)
	}

	if got := len(feedRegistry); got != documented {
		t.Errorf("README.md says %d mainnet feeds, feedRegistry has %d — update README.md", documented, got)
	}
}

// TestReadmeFilesTableListsEverySourceFile keeps README.md's Files table
// from silently falling behind the package as files are added.
func TestReadmeFilesTableListsEverySourceFile(t *testing.T) {
	raw, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		if row := fmt.Sprintf("| [`%s`](%s) |", f, f); !bytes.Contains(raw, []byte(row)) {
			t.Errorf("README.md Files table has no row for %s — add one", f)
		}
	}
}
