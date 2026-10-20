// Binary stellarindex-migrate applies and rolls back TimescaleDB schema migrations under
// migrations/, wrapping golang-migrate/migrate with env-based DSN resolution and safety rails.
//
// Subcommands:
//
//	stellarindex-migrate up              Apply every pending migration.
//	stellarindex-migrate down [N]        Roll back last N migrations (default 1).
//	                                      Requires -i-know; asks for confirmation on a
//	                                      TTY, and -yes skips that prompt.
//	stellarindex-migrate status          Show current + target version.
//	stellarindex-migrate version         Build version.
//	stellarindex-migrate help            Print usage.
//
// DSN resolution order: --dsn flag, then STELLARINDEX_POSTGRES_DSN env, then fail. There is
// intentionally no default: migrating "whatever DB happens to be local" is how people wipe
// production. A Postgres advisory lock serialises concurrent runners.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"

	"github.com/Stellar-Index/StellarIndex/internal/redact"
	"github.com/Stellar-Index/StellarIndex/internal/version"
)

// cmdUp's stdout is a contract: the Ansible migrate tasks key changed_when
// on it (ansible_changed_when_test.go).
const (
	upAppliedFormat = "migrated to version %d (dirty=%v)\n"
	upNoChangeMsg   = "already at latest version — nothing to do"
)

// newFlagSet declares the tool's whole flag surface; runbook invocations
// are tested against it.
func newFlagSet() (fs *flag.FlagSet, dsn *dsnFlag, dir *string, yes, iKnow *bool) {
	fs = flag.NewFlagSet("stellarindex-migrate", flag.ContinueOnError)
	// The flag package prints its own parse errors — the rejected argument included, verbatim — to the FlagSet's output before
	// it returns them. Nothing this tool is given may be echoed, so it is discarded and [parseArgv] composes the diagnostic
	// instead.
	fs.SetOutput(io.Discard)
	dsn = &dsnFlag{}
	fs.Var(dsn, "dsn", "Postgres `DSN` (overrides STELLARINDEX_POSTGRES_DSN env). Give it at most once and never "+
		"empty: an empty or repeated -dsn is refused, not ignored")
	dir = fs.String("migrations", "migrations", "Path to the migrations directory")
	yes = fs.Bool("yes", false, "skip the interactive confirmation for 'down' (required when stdin is not a TTY)")
	iKnow = fs.Bool("i-know", false, "acknowledge that 'down' runs down.sql against the target database and may be irreversible (required for every 'down')")
	fs.Usage = func() { printUsage(fs) }
	return fs, dsn, dir, yes, iKnow
}

func main() { //nolint:gocognit,gocyclo // dispatch-heavy; splitting would reduce linearity
	fs, dsn, dir, yes, iKnow := newFlagSet()

	args := parseArgv(fs, os.Args[1:])

	resolvedDSN := dsn.value
	if !dsn.set {
		resolvedDSN, dsnSource = os.Getenv("STELLARINDEX_POSTGRES_DSN"), "$STELLARINDEX_POSTGRES_DSN"
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
				die("down: N must be a positive integer (got %s)", describeArg(args[1]))
			}
			n = parsed
		}
		if resolvedDSN == "" {
			die("no DSN: set STELLARINDEX_POSTGRES_DSN or pass -dsn")
		}
		if err := cmdDown(*dir, resolvedDSN, n, *yes, *iKnow); err != nil {
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
			die("force: version must be a non-negative integer (got %s)", describeArg(args[1]))
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
		errf("unknown subcommand %s", describeArg(args[0]))
		printUsage(fs)
		os.Exit(2)
	}
}

func newMigrator(dir, dsn string) (*migrate.Migrate, error) {
	if err := checkDSN(dsn); err != nil {
		return nil, err
	}
	src := "file://" + dir
	m, err := migrate.New(src, dsn)
	if err != nil {
		return nil, fmt.Errorf("open migrator: %w", err)
	}
	return m, nil
}

// checkDSN refuses, without echoing any of it, a DSN the driver would
// quote back in its own error.
func checkDSN(dsn string) error {
	// Parse with the library's own parser so a DSN it would reject never reaches it: net/url's
	// error quotes the piece it choked on, which for a password holding `/`, `?` or `#` is the
	// password itself, and scrubbing cannot tell that fragment from an honest one. A string that
	// parses here parses there, so composing the failure ourselves is the only safe route.
	u, err := url.Parse(dsn)
	if err != nil {
		return fmt.Errorf("the Postgres DSN from %s does not parse: %s. "+
			"None of the value is shown — it carries the password; percent-encode "+
			"every reserved character in it (%% as %%25, @ as %%40)",
			dsnSource, redact.ParseFailure(dsn, err))
	}
	// A DSN that parses can still miss the driver's URL form; lib/pq then reads it as
	// keyword/value and quotes all of it, password included, in `missing "=" after …`. The `//` is
	// checked on the text because net/url takes `postgres:u:pw@h` as opaque. The host stays
	// optional so the socket form `postgres:///db?host=/run/postgresql` passes.
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return fmt.Errorf("the Postgres DSN from %s does not start with postgres:// or postgresql://. "+
			"None of the value is shown — it may carry the password", dsnSource)
	}
	if !strings.HasPrefix(dsn[len(u.Scheme)+1:], "//") {
		return fmt.Errorf("the Postgres DSN from %s is missing the // after its scheme "+
			"(postgres://user:password@host/db). None of the value is shown — it carries the password",
			dsnSource)
	}
	return nil
}

func cmdUp(dir, dsn string) error {
	m, err := newMigrator(dir, dsn)
	if err != nil {
		return err
	}
	defer closeSilent(m)

	if err := m.Up(); err != nil {
		if errors.Is(err, migrate.ErrNoChange) {
			fmt.Println(upNoChangeMsg)
			return nil
		}
		return err
	}
	v, dirty, vErr := m.Version()
	if vErr != nil {
		return fmt.Errorf("post-up version: %w", vErr)
	}
	fmt.Printf(upAppliedFormat, v, dirty)
	return nil
}

func cmdDown(dir, dsn string, n int, yes, iKnow bool) error {
	if !iKnow {
		return errDownNeedsIKnow
	}
	// Before the prompt: it prints the DSN through a pattern that cannot
	// see the password in one missing its `//`.
	if err := checkDSN(dsn); err != nil {
		return err
	}
	if err := confirmDown(n, dsn, yes, os.Stdin, stderr); err != nil {
		return err
	}

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

// isInteractive reports whether the real stdin is a terminal that can answer a prompt; a non-interactive stdin (CI, `go test`) must
// fail closed rather than block. ModeCharDevice alone rules out pipes and files, but /dev/null is a character device too, so it is
// excluded by device + inode.
func isInteractive() bool {
	stat, err := os.Stdin.Stat()
	if err != nil || stat.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	null, err := os.Open(os.DevNull)
	if err != nil {
		return true // can't rule it out; assume the char device is real
	}
	defer null.Close()
	nullStat, err := null.Stat()
	return err != nil || !os.SameFile(stat, nullStat)
}

// errDownNeedsIKnow is the rail -yes cannot pass: non-interactive callers set -yes and the DSN host cannot identify production
// (r1 reaches Postgres at 127.0.0.1), so every rollback must be acknowledged explicitly.
var errDownNeedsIKnow = errors.New("refusing to roll back without -i-know: down runs each " +
	"migration's down.sql against the target database, which may drop columns, " +
	"constraints or data that no later 'up' restores. Prefer rolling forward; " +
	"pass -i-know to acknowledge")

// confirmDown gates `down` behind an explicit operator "yes". No deploy pipeline runs `down`
// (only `up`), and it rolls back applied schema, so it asks first and refuses rather than
// guesses when nothing can answer.
func confirmDown(n int, dsn string, yes bool, in io.Reader, out io.Writer) error {
	if yes {
		return nil
	}
	if !isInteractive() {
		return errors.New("stdin is not a TTY and -yes was not given; " +
			"refusing to guess on a destructive rollback. Pass -yes to confirm non-interactively")
	}
	fmt.Fprintf(out, "About to roll back %d migration(s) against %s\n", n, redact.Credentials(dsn))
	fmt.Fprint(out, "Proceed? [y/N] ")
	scanner := bufio.NewScanner(in)
	if !scanner.Scan() {
		return errors.New("no confirmation read; aborting")
	}
	if ans := strings.TrimSpace(scanner.Text()); ans != "y" && ans != "Y" {
		return errors.New("aborted: down not confirmed")
	}
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

// cmdForce sets the schema_migrations.version row to `v` and clears the dirty flag. Dangerous: only after confirming the DB's actual
// schema matches v (typically after fixing a partially-applied migration).
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

// errf writes diagnostics through [stderr], which strips inline credentials; the flag package's own output goes to io.Discard because
// it echoes the rejected argument. golang-migrate wraps net/url's *url.Error (the WHOLE URL), so redacting at the write covers every
// call site. A backstop, not a licence to format a secret.
func errf(format string, args ...any) {
	fmt.Fprintln(stderr, fmt.Sprintf(format, args...))
}

// stderr is the SECOND line of defence (the first: no message interpolates text this tool was given). It catches what a dependency
// formats, such as the driver embedding the URL. A malformed DSN is unparseable, so it scrubs by VALUE too: redact.Known cuts each
// password from argv and the DSN variable in any spelling; the pattern pass covers text that re-renders the DSN.
var stderr io.Writer = scrubbingWriter{
	w:     os.Stderr,
	known: append([]string{os.Getenv("STELLARINDEX_POSTGRES_DSN")}, os.Args[1:]...),
}

// dsnSource names where the resolved DSN came from. It replaces echoing the value: the operator can go and look at the source
// without anything about it being written to a log.
var dsnSource = "-dsn"

// describeArg renders an operator-typed argument for a message; text that could be a connection string is never echoed, even scrubbed.
// Allowlist, because the denylist failed twice: a plain-word token is quoted back, anything else (maybe a pasted DSN) is described
// by position.
var plainArg = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$`)

func describeArg(v string) string {
	if plainArg.MatchString(v) {
		return strconv.Quote(v)
	}
	return "<withheld — it may carry a credential>"
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

// printUsage writes its fixed text to the raw os.Stderr on purpose: it interpolates only the build version, and [stderr] would
// redact its own example DSN. The FlagSet output is io.Discard (parse errors echo the rejected argument), so it is pointed at stderr
// for this one write and restored.
func printUsage(fs *flag.FlagSet) {
	defer fs.SetOutput(io.Discard)
	fmt.Fprintf(os.Stderr, `stellarindex-migrate %s

Apply + manage TimescaleDB schema migrations.

Usage:
  stellarindex-migrate [-dsn DSN] [-migrations DIR] <subcommand> [args]

Subcommands:
  up              Apply every pending migration.
  down [N]        Roll back last N migrations (default 1). Always
                  requires -i-know. Prompts for confirmation on a TTY;
                  pass -yes to skip it (required when stdin is not a
                  TTY).
  status          Show current applied version.
  force <V>       Clear dirty flag + set version to V (DANGEROUS —
                  manually verify the DB's actual schema matches V
                  first; only use after partial-apply recovery).
  version         Build version.
  help            This help.

Flags:
`, version.String())
	fs.SetOutput(os.Stderr)
	fs.PrintDefaults()
	fmt.Fprintf(os.Stderr, `
Environment:
  STELLARINDEX_POSTGRES_DSN   Postgres DSN, used when -dsn is not set.
                             Example: postgres://user:pass@host:5432/db?sslmode=disable

Examples:
  export STELLARINDEX_POSTGRES_DSN="postgres://stellarindex@localhost/stellarindex?sslmode=disable"
  stellarindex-migrate up
  stellarindex-migrate status
  stellarindex-migrate -i-know down 1
`)
}

// parseArgv resolves the whole command line into `verb` plus its positionals, leaving every
// declared flag set on fs wherever the operator wrote it. Every argv decision happens here;
// see the comment inside for why both flag placements are accepted.
func parseArgv(fs *flag.FlagSet, argv []string) []string {
	if err := fs.Parse(argv); err != nil {
		reportFlagFailure(fs, argv, 1)
	}

	args := fs.Args()
	if len(args) == 0 {
		printUsage(fs)
		os.Exit(2)
	}

	// Flags are accepted BEFORE the verb or AFTER the verb's positionals; an unknown token is a
	// parse error either way.
	//
	// Go's flag package stops at the first non-flag argument, so in
	//
	// 	stellarindex-migrate down 1 -dsn postgres://staging/…
	//
	// the -dsn would be silently dropped and the DSN would fall back to $STELLARINDEX_POSTGRES_DSN,
	// running the rollback on production while printing success.
	//
	// The trailing form is accepted rather than refused because cmd/stellarindex-ops also parses
	// flags over args[1:], and two binaries disagreeing on placement causes the mistake. The verb's
	// leading positionals (down's N, force's V) are taken first, then the SAME flag set parses the
	// rest. Anything left is a hard error: `down -dsn X 1` is ambiguous.
	rest := args[1:]
	nPos := 0
	for nPos < len(rest) && !strings.HasPrefix(rest[nPos], "-") {
		nPos++
	}
	positionals := rest[:nPos]
	if err := fs.Parse(rest[nPos:]); err != nil {
		reportFlagFailure(fs, rest[nPos:], len(argv)-len(rest)+nPos+1)
	}
	if leftover := fs.Args(); len(leftover) > 0 {
		die("unexpected argument at position %d after the flags for %s — put every positional "+
			"immediately after the subcommand "+
			"(stellarindex-migrate <subcommand> <positionals> -flag value)",
			len(argv)-len(leftover)+1, describeArg(args[0]))
	}
	return append([]string{args[0]}, positionals...)
}

// reportFlagFailure says why the command line was refused and exits, never repeating the offending argument (the flag package would,
// e.g. `-postgres://user:SECRET@host/db`; hence io.Discard). offset is the 1-based position of argv[0] on the whole command line.
func reportFlagFailure(fs *flag.FlagSet, argv []string, offset int) {
	if d, ok := fs.Lookup("dsn").Value.(*dsnFlag); ok && d.rejection != "" {
		errf("stellarindex-migrate: %s", d.rejection)
		os.Exit(2)
	}
	i, name, defined := unknownFlag(fs, argv)
	switch {
	case i < 0:
		errf("stellarindex-migrate: the command line could not be parsed — " +
			"run 'stellarindex-migrate help' for the flags this tool defines")
	case defined:
		errf("stellarindex-migrate: the %s flag (argument %d) needs a value", describeFlagName(name), offset+i)
	default:
		errf("stellarindex-migrate: argument %d is not a flag this tool defines (%s) — "+
			"only -dsn, -migrations and -yes are", offset+i, describeFlagName(name))
	}
	os.Exit(2)
}

// unknownFlag walks argv like the flag package and returns the index of the first token it cannot accept, its flag name, and
// whether the name IS one this tool defines (so its value is what is missing).
func unknownFlag(fs *flag.FlagSet, argv []string) (idx int, name string, defined bool) {
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if len(a) < 2 || a[0] != '-' || a == "--" {
			return -1, "", false // the flag package stops at the first non-flag
		}
		n, hasValue := cutFlagName(a)
		if fs.Lookup(n) == nil {
			return i, n, false
		}
		if !hasValue {
			if i+1 >= len(argv) {
				return i, n, true
			}
			i++ // the next token is this flag's value, not a flag
		}
	}
	return -1, "", false
}

// dsnFlag is -dsn. It records whether the flag was given so only its absence falls back to
// $STELLARINDEX_POSTGRES_DSN; `-dsn ""` from a quoted unset variable would otherwise resolve the
// production database. Set refuses repeats because parseArgv parses the same FlagSet twice.
type dsnFlag struct {
	value     string
	set       bool
	rejection string // why Set refused; never quotes the value
}

func (d *dsnFlag) String() string {
	if d == nil {
		return ""
	}
	return d.value
}

func (d *dsnFlag) Set(v string) error {
	switch {
	case d.set:
		d.rejection = "-dsn was given more than once — refusing rather than guessing which " +
			"database was meant; pass it exactly once"
	case strings.TrimSpace(v) == "":
		d.rejection = "-dsn was given an empty value — refusing rather than falling back to " +
			"$STELLARINDEX_POSTGRES_DSN, which may name a different database; check the variable " +
			"expanded into -dsn, or omit -dsn to use the environment on purpose"
	default:
		d.value, d.set = v, true
		return nil
	}
	return errors.New(d.rejection)
}

// cutFlagName strips the leading dashes and any `=value` from a flag
// argument, the way the flag package does.
func cutFlagName(a string) (name string, hasValue bool) {
	name = strings.TrimLeft(a, "-")
	name, _, hasValue = strings.Cut(name, "=")
	return name, hasValue
}

// describeFlagName renders a rejected flag NAME on the same terms as
// [describeArg]: a plain word is the operator's typo and worth showing,
// anything else may be the DSN they meant to pass to -dsn.
func describeFlagName(name string) string {
	switch {
	case name == "":
		return "an empty flag name"
	case plainArg.MatchString(name):
		return "-" + name
	default:
		return "<withheld — it may carry a credential>"
	}
}
