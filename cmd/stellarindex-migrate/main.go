// Binary stellarindex-migrate applies and rolls back TimescaleDB
// schema migrations under migrations/. Thin wrapper over
// golang-migrate/migrate with our project's env-based DSN resolution
// and safety rails.
//
// Subcommands:
//
//	stellarindex-migrate up              Apply every pending migration.
//	stellarindex-migrate down [N]        Roll back last N migrations (default 1).
//	stellarindex-migrate status          Show current + target version.
//	stellarindex-migrate version         Build version.
//	stellarindex-migrate help            Print usage.
//
// DSN resolution order: --dsn flag, then STELLARINDEX_POSTGRES_DSN env,
// then fail. We intentionally do NOT fall back to defaults here —
// running migrations against "whatever DB happens to be local" is
// how people wipe production.
//
// Locking: golang-migrate grabs a Postgres advisory lock before
// applying, so two concurrent runners serialise safely.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"

	"github.com/Stellar-Index/StellarIndex/internal/version"
)

func main() { //nolint:gocognit,gocyclo // dispatch-heavy; splitting would reduce linearity
	fs := flag.NewFlagSet("stellarindex-migrate", flag.ContinueOnError)
	dsn := fs.String("dsn", "", "Postgres DSN (overrides STELLARINDEX_POSTGRES_DSN env)")
	dir := fs.String("migrations", "migrations", "Path to the migrations directory")
	fs.Usage = func() { printUsage(fs) }

	args := parseArgv(fs, os.Args[1:])

	resolvedDSN := *dsn
	if resolvedDSN == "" {
		resolvedDSN = os.Getenv("STELLARINDEX_POSTGRES_DSN")
	}

	switch args[0] {
	case "up":
		if resolvedDSN == "" {
			die("no DSN: set STELLARINDEX_POSTGRES_DSN or pass -dsn")
		}
		if err := cmdUp(*dir, resolvedDSN); err != nil {
			die("up: %v", err)
		}
	case "down":
		n := 1
		if len(args) > 1 {
			parsed, err := strconv.Atoi(args[1])
			if err != nil || parsed < 1 {
				die("down: N must be a positive integer (got %q)", args[1])
			}
			n = parsed
		}
		if resolvedDSN == "" {
			die("no DSN: set STELLARINDEX_POSTGRES_DSN or pass -dsn")
		}
		if err := cmdDown(*dir, resolvedDSN, n); err != nil {
			die("down: %v", err)
		}
	case "status":
		if resolvedDSN == "" {
			die("no DSN: set STELLARINDEX_POSTGRES_DSN or pass -dsn")
		}
		if err := cmdStatus(*dir, resolvedDSN); err != nil {
			die("status: %v", err)
		}
	case "force":
		if len(args) < 2 {
			die("force: requires a version number. Usage: force <version>")
		}
		v, err := strconv.Atoi(args[1])
		if err != nil || v < 0 {
			die("force: version must be a non-negative integer (got %q)", args[1])
		}
		if resolvedDSN == "" {
			die("no DSN: set STELLARINDEX_POSTGRES_DSN or pass -dsn")
		}
		if err := cmdForce(*dir, resolvedDSN, v); err != nil {
			die("force: %v", err)
		}
	case "version", "--version", "-v":
		fmt.Println(version.String())
	case "help", "--help", "-h":
		printUsage(fs)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", args[0])
		printUsage(fs)
		os.Exit(2)
	}
}

func newMigrator(dir, dsn string) (*migrate.Migrate, error) {
	src := "file://" + dir
	m, err := migrate.New(src, dsn)
	if err != nil {
		return nil, fmt.Errorf("open migrator: %w", err)
	}
	return m, nil
}

func cmdUp(dir, dsn string) error {
	m, err := newMigrator(dir, dsn)
	if err != nil {
		return err
	}
	defer closeSilent(m)

	if err := m.Up(); err != nil {
		if errors.Is(err, migrate.ErrNoChange) {
			fmt.Println("already at latest version — nothing to do")
			return nil
		}
		return err
	}
	v, dirty, vErr := m.Version()
	if vErr != nil {
		return fmt.Errorf("post-up version: %w", vErr)
	}
	fmt.Printf("migrated to version %d (dirty=%v)\n", v, dirty)
	return nil
}

func cmdDown(dir, dsn string, n int) error {
	m, err := newMigrator(dir, dsn)
	if err != nil {
		return err
	}
	defer closeSilent(m)

	if err := m.Steps(-n); err != nil {
		if errors.Is(err, migrate.ErrNoChange) {
			fmt.Println("already at version 0 — nothing to roll back")
			return nil
		}
		return err
	}
	v, dirty, vErr := m.Version()
	if vErr != nil {
		if errors.Is(vErr, migrate.ErrNilVersion) {
			fmt.Println("rolled back to version 0 (nothing applied)")
			return nil
		}
		return fmt.Errorf("post-down version: %w", vErr)
	}
	fmt.Printf("rolled back to version %d (dirty=%v)\n", v, dirty)
	return nil
}

func cmdStatus(dir, dsn string) error {
	m, err := newMigrator(dir, dsn)
	if err != nil {
		return err
	}
	defer closeSilent(m)

	v, dirty, err := m.Version()
	if err != nil {
		if errors.Is(err, migrate.ErrNilVersion) {
			fmt.Println("current version: 0 (no migrations applied)")
			return nil
		}
		return err
	}
	fmt.Printf("current version: %d (dirty=%v)\n", v, dirty)
	return nil
}

// cmdForce sets the schema_migrations.version row to `v` and
// clears the dirty flag. Dangerous — only use when you've
// manually confirmed the DB's actual schema matches version v
// (typically after fixing a partially-applied migration).
func cmdForce(dir, dsn string, v int) error {
	m, err := newMigrator(dir, dsn)
	if err != nil {
		return err
	}
	defer closeSilent(m)

	if err := m.Force(v); err != nil {
		return err
	}
	fmt.Printf("forced to version %d (dirty=false)\n", v)
	return nil
}

func closeSilent(m *migrate.Migrate) {
	srcErr, dbErr := m.Close()
	if srcErr != nil {
		fmt.Fprintf(os.Stderr, "warn: close source: %v\n", srcErr)
	}
	if dbErr != nil {
		fmt.Fprintf(os.Stderr, "warn: close db: %v\n", dbErr)
	}
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "stellarindex-migrate: "+format+"\n", args...)
	os.Exit(1)
}

func printUsage(fs *flag.FlagSet) {
	fmt.Fprintf(os.Stderr, `stellarindex-migrate %s

Apply + manage TimescaleDB schema migrations.

Usage:
  stellarindex-migrate [-dsn DSN] [-migrations DIR] <subcommand> [args]

Subcommands:
  up              Apply every pending migration.
  down [N]        Roll back last N migrations (default 1).
  status          Show current applied version.
  force <V>       Clear dirty flag + set version to V (DANGEROUS —
                  manually verify the DB's actual schema matches V
                  first; only use after partial-apply recovery).
  version         Build version.
  help            This help.

Flags:
`, version.String())
	fs.PrintDefaults()
	fmt.Fprintf(os.Stderr, `
Environment:
  STELLARINDEX_POSTGRES_DSN   Postgres DSN, used when -dsn is not set.
                             Example: postgres://user:pass@host:5432/db?sslmode=disable

Examples:
  export STELLARINDEX_POSTGRES_DSN="postgres://stellarindex@localhost/stellarindex?sslmode=disable"
  stellarindex-migrate up
  stellarindex-migrate status
  stellarindex-migrate down 1
`)
}

// parseArgv resolves the whole command line into `verb` plus its
// positionals, leaving every declared flag set on fs — wherever the
// operator wrote it.
//
// It exists because this was a live production-destruction path. See the
// comment inside for what went wrong and why the fix accepts both
// placements rather than refusing one. Extracted from main() so the
// chokepoint has a name; main() is a dispatch switch and every argv
// decision now happens here.
func parseArgv(fs *flag.FlagSet, argv []string) []string {
	if err := fs.Parse(argv); err != nil {
		os.Exit(2)
	}

	args := fs.Args()
	if len(args) == 0 {
		printUsage(fs)
		os.Exit(2)
	}

	// Flags are accepted BEFORE the verb or AFTER the verb's positionals,
	// and an unknown token is a parse error either way.
	//
	// THE DEFECT THIS REPLACES. Go's flag package stops parsing at the
	// first non-flag argument, so a single `fs.Parse(os.Args[1:])` over
	// the whole argv stops at the verb and leaves everything after it
	// unparsed. In
	//
	//	stellarindex-migrate down 1 -dsn postgres://staging/…
	//
	// the -dsn was never parsed, landed in fs.Args(), was silently
	// dropped, and the DSN fell back to $STELLARINDEX_POSTGRES_DSN — so
	// an operator dropping a migration on what they believed was staging
	// dropped it on production, and the command printed success.
	// Reproduced by building the binary and watching which host it
	// dialled: the flag AFTER the verb resolved the env host, the same
	// flag BEFORE it resolved the flag's host. All four verbs were
	// affected, and so was -migrations. Nothing had fired only because
	// deploy-binary.yml happens to put -migrations first.
	//
	// WHY THIS SHAPE, rather than refusing the trailing form. The trap is
	// a shared CAUSE with one this repo has already solved once:
	// cmd/stellarindex-ops hands every leaf its own FlagSet over args[1:]
	// precisely so a flag after the verb is honoured, and documents this
	// same stop-at-first-positional behaviour at length. Refusing here
	// would have left the two binaries disagreeing about where flags go —
	// which is the inconsistency that produces the mistake in the first
	// place. Accepting both placements removes the trap instead of
	// posting a sign next to it.
	//
	// The verb's leading positionals (down's N, force's V) are taken
	// first, then the remainder is parsed by the SAME flag set. Anything
	// still left after that is a hard error: `down -dsn X 1` is ambiguous
	// about whether 1 is a value or a count, so it is refused rather than
	// guessed.
	rest := args[1:]
	nPos := 0
	for nPos < len(rest) && !strings.HasPrefix(rest[nPos], "-") {
		nPos++
	}
	positionals := rest[:nPos]
	if err := fs.Parse(rest[nPos:]); err != nil {
		os.Exit(2)
	}
	if leftover := fs.Args(); len(leftover) > 0 {
		die("unexpected argument %q after the flags for %q — put every positional "+
			"immediately after the subcommand (stellarindex-migrate %s %s -flag value)",
			leftover[0], args[0], args[0], strings.Join(positionals, " "))
	}
	return append([]string{args[0]}, positionals...)
}
