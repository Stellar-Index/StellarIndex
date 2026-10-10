package migrations

import (
	"os"
	"strings"
	"testing"
)

func extractRow(t *testing.T, raw, id string) string {
	t.Helper()
	marker := "| " + id + " |"
	idx := strings.Index(raw, marker)
	if idx == -1 {
		t.Fatalf("README.md table row for migration %s not found", id)
	}
	end := strings.Index(raw[idx:], "\n")
	if end == -1 {
		end = len(raw) - idx
	}
	return raw[idx : idx+end]
}

func readReadme(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	return string(raw)
}
