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
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"

	"github.com/Stellar-Index/StellarIndex/internal/redact"
	"github.com/Stellar-Index/StellarIndex/internal/version"
)

func main() { //nolint:gocognit,gocyclo // dispatch-heavy; splitting would reduce linearity
	fs := flag.NewFlagSet("stellarindex-migrate", flag.ContinueOnError)
	fs.SetOutput(stderr) // the flag package echoes the argument it rejects
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
		errf("unknown subcommand %q", args[0])
		printUsage(fs)
		os.Exit(2)
	}
}

func newMigrator(dir, dsn string) (*migrate.Migrate, error) {
	// Parse the DSN here, with the parser the library is about to use, so
	// that a DSN it would reject never reaches it. Its rejection cannot be
	// made safe after the fact: net/url's reason quotes the piece it
	// choked on, and for a password holding `/`, `?` or `#` that piece is
	// the password up to that character, reported as a bad port. Scrubbing
	// at the write catches the URL; it cannot tell that fragment from an
	// honest one. This is the only path on which the library formats the
	// DSN — a string that parses here parses there — so composing the
	// failure ourselves removes the fragment instead of chasing it.
	if _, err := url.Parse(dsn); err != nil {
		return nil, fmt.Errorf("open migrator: database URL does not parse: %s "+
			"(if the password holds a reserved character, percent-encode it)", redact.ParseFailure(dsn, err))
	}
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
		errf("warn: close source: %v", srcErr)
	}
	if dbErr != nil {
		errf("warn: close db: %v", dbErr)
	}
}

// errf is where this binary writes a diagnostic of its own, and it goes
// through [stderr], which strips inline credentials on the way out. The
// two other writers are the flag package, pointed at the same [stderr]
// in main, and printUsage's fixed text, which interpolates nothing an
// operator typed.
//
// This tool is handed the production DSN — password included — on every
// deploy, and its stderr is captured by the deploy job, journald and
// Loki. The leak is not in the format strings above: golang-migrate
// rejects an unparseable database URL by wrapping net/url's *url.Error,
// which renders the WHOLE URL, so a password containing an unescaped
// `%`, `#` or space printed the live credential in full. Redacting the
// two call sites that exist today would leave the next one to remember;
// redacting at the write inherits it (#346 F3).
//
// Scrubbing output is a backstop, not a licence to format a secret on
// purpose — see internal/redact for which helper renders a value we
// compose ourselves.
func errf(format string, args ...any) {
	fmt.Fprintln(stderr, fmt.Sprintf(format, args...))
}

// stderr is where every diagnostic that can repeat operator input goes:
// errf above, and the flag package, which prints its own parse errors —
// argument included — to the FlagSet's output and would otherwise bypass
// errf entirely (`-$DSN` typed for `-dsn $DSN` printed the DSN whole).
//
// It scrubs by VALUE as well as by pattern. This process knows every
// string a credential can have arrived in — argv and the DSN variable —
// so it hands them to redact.Known, which cuts each password they hold,
// whatever characters it contains and in whichever spelling it was
// written (URL userinfo, `?password=`, libpq `password=`), where the
// output repeats it after the text it was held behind. That includes an
// echo that stops short: the flag package prints a rejected "flag name"
// only up to its first `=`, which for `-postgres://u:abc==@host` is the
// password bar its base64 padding, with no `@` left to recognise it by.
// The pattern pass behind it covers text that re-renders the DSN rather
// than repeating it, and is only as good as the boundary that text
// offers. What redact.Known cannot close is listed on it.
var stderr io.Writer = scrubbingWriter{
	w:     os.Stderr,
	known: append([]string{os.Getenv("STELLARINDEX_POSTGRES_DSN")}, os.Args[1:]...),
}

// scrubbingWriter redacts each Write as one message. fmt and flag both
// emit a whole message per Write, so a secret cannot straddle two.
type scrubbingWriter struct {
	w     io.Writer
	known []string
}

func (s scrubbingWriter) Write(p []byte) (int, error) {
	if _, err := io.WriteString(s.w, redact.Known(string(p), s.known...)); err != nil {
		return 0, err
	}
	return len(p), nil
}

func die(format string, args ...any) {
	errf("stellarindex-migrate: "+format, args...)
	os.Exit(1)
}

// printUsage writes its fixed text to the raw os.Stderr on purpose: it
// interpolates only the build version, and sending it through [stderr]
// would redact its own example DSN. fs.PrintDefaults goes wherever the
// FlagSet's output points, which main sets to [stderr].
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
