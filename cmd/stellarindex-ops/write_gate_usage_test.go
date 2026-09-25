package main

import (
	"bytes"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
)

var usageVerbRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// usageEntryVerbs returns the verb path of a usageBody entry line ("  supply
// seed-sac-balances -config PATH …" → [supply seed-sac-balances]), or nil
// when the line is not an entry for a dispatchable subcommand.
func usageEntryVerbs(line string) []string {
	if !strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "   ") {
		return nil
	}
	var verbs []string
	for _, tok := range strings.Fields(line) {
		if !usageVerbRe.MatchString(tok) {
			break
		}
		verbs = append(verbs, tok)
	}
	if len(verbs) == 0 {
		return nil
	}
	if _, ok := subcommands[verbs[0]]; !ok {
		return nil
	}
	return verbs
}

// helpOutput runs one subcommand with -h and returns what it printed to
// stderr, where flag.FlagSet writes its defaults.
func helpOutput(t *testing.T, verbs []string) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = devNull.Close() }()
	oldErr, oldOut := os.Stderr, os.Stdout
	os.Stderr, os.Stdout = w, devNull
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = subcommands[verbs[0]](append(append([]string{}, verbs...), "-h"))
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Errorf("%s -h did not return within 10s", strings.Join(verbs, " "))
	}
	os.Stderr, os.Stdout = oldErr, oldOut
	_ = w.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	_ = r.Close()
	return buf.String()
}

// TestUsageBodyNamesWriteForEveryWriteGatedCommand pins the write gate's
// help contract: dry run is the default and -dry-run is a no-op alias, so a
// --help synopsis for a write-gated command that shows [-dry-run] and never
// -write documents an invocation that cannot write. Write-gated commands are
// found mechanically — the ones whose -h output carries the gate's own -write
// help text — so a new gated command is covered without editing this test.
func TestUsageBodyNamesWriteForEveryWriteGatedCommand(t *testing.T) {
	var missing []string
	gated := 0
	for _, line := range strings.Split(usageBody, "\n") {
		verbs := usageEntryVerbs(line)
		if verbs == nil {
			continue
		}
		if !strings.Contains(helpOutput(t, verbs), opsutil.WriteFlagUsage) {
			continue
		}
		gated++
		if !strings.Contains(line, "-write") {
			missing = append(missing, strings.Join(verbs, " "))
		}
	}
	sort.Strings(missing)
	if gated == 0 {
		t.Fatal("found no write-gated command in usageBody; the detector is broken, not the help text")
	}
	if len(missing) > 0 {
		t.Fatalf("%d write-gated command(s) whose --help synopsis never names -write (dry run is the default, so the documented invocation writes nothing): %s",
			len(missing), strings.Join(missing, ", "))
	}
}
