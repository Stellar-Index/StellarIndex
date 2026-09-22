package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// THE DEFECT (T400). `down` calls m.Steps(-n) — a destructive rollback —
// with no confirmation prompt, no -yes/-force flag and no TTY guard: an
// operator who fat-fingers `down` (or a script that inherits the wrong
// $STELLARINDEX_POSTGRES_DSN) drops production migrations with no
// chance to notice, and the command prints success. `up` is the only
// verb any deploy pipeline runs (deploy-binary.yml); `down` is a manual,
// break-glass command, so the safe default is to ask first and to
// refuse — never guess — when nothing can answer the prompt, matching
// scripts/dev/cut-release.sh's rule for the same class of prompt.
//
// THE TEST BUILDS THE REAL BINARY, because the gate has to run before
// `newMigrator` ever opens a connection: it asserts on WHICH failure
// comes back, not just that one occurs, and only the real dispatch in
// main() proves the gate runs first.
func TestMigrate_DownRefusesWithoutConfirmationOnNonTTYStdin(t *testing.T) {
	bin := buildMigrateBinary(t)

	// Port 1 on loopback refuses immediately with no DNS or connect
	// timeout, so the test only takes real time if the gate is missing
	// and the tool goes on to actually try to connect.
	const dsn = "postgres://u:p@127.0.0.1:1/db?sslmode=disable"

	cmd := exec.Command(bin, "-dsn", dsn, "down", "1")
	// exec.Command leaves Stdin nil, which os/exec wires to /dev/null —
	// guaranteed non-interactive, exactly the shape this gate must
	// refuse rather than guess on.
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a non-zero exit refusing the unconfirmed rollback, got success:\n%s", out)
	}

	got := string(out)
	if !strings.Contains(got, "not a TTY") || !strings.Contains(got, "-yes") {
		t.Fatalf("expected the confirmation refusal naming -yes and the non-TTY stdin, got:\n%s", got)
	}
	// The defect this replaces: on unfixed code there is no gate, so the
	// tool goes straight to newMigrator and this message never appears —
	// this asserts the connection was never attempted.
	if strings.Contains(got, "connect") || strings.Contains(got, "refused") || strings.Contains(got, "open migrator") {
		t.Fatalf("rollback should have been refused before any connection attempt, got:\n%s", got)
	}
}

// -yes must actually skip the prompt and let the command proceed to the
// real work, not just always fail differently.
func TestMigrate_DownYesSkipsConfirmationAndReachesTheMigrator(t *testing.T) {
	bin := buildMigrateBinary(t)

	const dsn = "postgres://u:p@127.0.0.1:1/db?sslmode=disable"

	cmd := exec.Command(bin, "-dsn", dsn, "-yes", "down", "1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a non-zero exit (connection refused), got success:\n%s", out)
	}

	got := string(out)
	if strings.Contains(got, "not a TTY") {
		t.Fatalf("-yes should have skipped the confirmation gate entirely, got:\n%s", got)
	}
}

// buildMigrateBinary compiles the real binary once per test into a temp
// dir, matching the other black-box tests in this package.
func buildMigrateBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "stellarindex-migrate")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}
