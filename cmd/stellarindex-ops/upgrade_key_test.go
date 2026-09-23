package main

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
	helpErr := upgradeKey([]string{"-h"})
	os.Stderr = orig
	_ = w.Close()
	usage, _ := io.ReadAll(r)
	if !errors.Is(helpErr, flag.ErrHelp) {
		t.Fatalf("upgradeKey(-h) = %v, want flag.ErrHelp", helpErr)
	}
	if strings.Contains(string(usage), "-1") {
		t.Errorf("help advertises -1, which the parser rejects:\n%s", usage)
	}

	negErr := upgradeKey([]string{"-config", "/nonexistent.toml", "-key-id", "kid_x", "-rate-limit-per-min", "-1"})
	if negErr == nil || !strings.Contains(negErr.Error(), "must be >= 0") {
		t.Errorf("negative -rate-limit-per-min must be refused, got: %v", negErr)
	}
}
