package main

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// An empty or repeated -dsn must be refused before anything is dialled.
// The environment DSN stands in for production: an explicit `-dsn ""`
// (the quoted expansion of an unset variable) used to fall through to it,
// and a second -dsn silently replaced the first. Both hosts are
// unresolvable, so the host named in the output is the evidence of which
// database the tool would have written to.
func TestMigrate_EmptyOrRepeatedDSNIsRefusedBeforeAnyDial(t *testing.T) {
	bin, migDir := buildMigrate(t)

	const envHost = "env-host-stands-for-production.invalid"
	const hostA = "flag-host-a.invalid"
	const hostB = "flag-host-b.invalid"
	envDSN := "STELLARINDEX_POSTGRES_DSN=postgres://u:p@" + envHost + ":5432/db?sslmode=disable"
	dsnA := "postgres://u:p@" + hostA + ":5432/a?sslmode=disable"
	dsnB := "postgres://u:p@" + hostB + ":5432/b?sslmode=disable"

	run := func(args ...string) (string, int) {
		t.Helper()
		cmd := exec.Command(bin, append([]string{"-migrations", migDir}, args...)...)
		cmd.Env = append(os.Environ(), envDSN)
		out, err := cmd.CombinedOutput()
		var ee *exec.ExitError
		switch {
		case err == nil:
			return string(out), 0
		case errors.As(err, &ee):
			return string(out), ee.ExitCode()
		default:
			t.Fatalf("run %v: %v", args, err)
			return "", 0
		}
	}

	// Controls: the instrument can see each host, so an absent host below
	// means "not dialled", not "not reported".
	if out, _ := run("status"); !strings.Contains(out, envHost) {
		t.Fatalf("no -dsn: expected the env host to be dialled, got:\n%s", out)
	}
	if out, _ := run("-dsn", dsnA, "status"); !strings.Contains(out, hostA) || strings.Contains(out, envHost) {
		t.Fatalf("-dsn once: expected only %s to be dialled, got:\n%s", hostA, out)
	}

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"empty before verb", []string{"-dsn", "", "status"}, "empty"},
		{"empty with equals", []string{"-dsn=", "status"}, "empty"},
		{"empty after verb", []string{"status", "-dsn", ""}, "empty"},
		{"blank value", []string{"-dsn", "  ", "up"}, "empty"},
		{"empty on down -yes", []string{"-dsn", "", "-yes", "-i-know", "down", "2"}, "empty"},
		{"repeated before verb", []string{"-dsn", dsnA, "-dsn", dsnB, "status"}, "more than once"},
		{"repeated across verb", []string{"-dsn", dsnA, "status", "-dsn", dsnB}, "more than once"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, code := run(tc.args...)
			if code != 2 {
				t.Errorf("exit %d, want 2 (usage error)\n%s", code, out)
			}
			for _, h := range []string{envHost, hostA, hostB} {
				if strings.Contains(out, h) {
					t.Errorf("dialled %s — the flag must be refused before any connection:\n%s", h, out)
				}
			}
			if !strings.Contains(out, "-dsn") || !strings.Contains(out, tc.want) {
				t.Errorf("diagnostic must name -dsn and say %q, got:\n%s", tc.want, out)
			}
		})
	}
}
