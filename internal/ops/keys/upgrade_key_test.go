package keys

import (
	"errors"
	"flag"
	"io"
	"os"
	"strings"
	"testing"
)

// TestUpgradeKey_HelpMatchesParser — the parser rejects a negative
// -rate-limit-per-min and treats 0 as "reset to tier default", so the
// help text must not tell the operator to pass -1.
func TestUpgradeKey_HelpMatchesParser(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	helpErr := Upgrade([]string{"-h"})
	os.Stderr = orig
	_ = w.Close()
	usage, _ := io.ReadAll(r)
	if !errors.Is(helpErr, flag.ErrHelp) {
		t.Fatalf("Upgrade(-h) = %v, want flag.ErrHelp", helpErr)
	}
	if strings.Contains(string(usage), "-1") {
		t.Errorf("help advertises -1, which the parser rejects:\n%s", usage)
	}

	negErr := Upgrade([]string{"-config", "/nonexistent.toml", "-key-id", "kid_x", "-rate-limit-per-min", "-1"})
	if negErr == nil || !strings.Contains(negErr.Error(), "must be >= 0") {
		t.Errorf("negative -rate-limit-per-min must be refused, got: %v", negErr)
	}
}

func TestUpgradeKey_WritesOnlyWithWriteFlag(t *testing.T) {
	base := []string{"-config", "c.toml", "-key-id", "kid_x", "-rate-limit-per-min", "5000", "-reason", "r", "-actor", "alice"}
	for _, tc := range []struct {
		extra []string
		write bool
	}{{nil, false}, {[]string{"-dry-run"}, false}, {[]string{"-write"}, true}} {
		opts, err := parseUpgradeKeyFlags(append(append([]string{}, base...), tc.extra...))
		if err != nil {
			t.Fatalf("%v: %v", tc.extra, err)
		}
		if opts.write != tc.write {
			t.Errorf("%v: write = %v, want %v", tc.extra, opts.write, tc.write)
		}
	}
}
