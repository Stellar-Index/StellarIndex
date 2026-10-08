// Command affected-go-pkgs prints what a diff needs tested: with -files, the
// changed files minus comment-only Go edits; otherwise the import paths of
// every package the diff touches plus every package that depends on them, or
// "./..." when the diff can reach every package.
//
// Usage: go run ./scripts/ci/affected-go-pkgs [-files] [-worktree] BASE_SHA
//
// -worktree diffs BASE_SHA against the working tree (staged, unstaged and
// untracked files) instead of HEAD, for local verification of edits not yet
// committed.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"go/scanner"
	"go/token"
	"os"
	"os/exec"
	"path"
	"sort"
	"strings"
)

// fullTriggers change the build or test inputs of every package.
var fullTriggers = map[string]bool{
	"go.mod":                              true,
	"go.sum":                              true,
	"scripts/ci/affected-go-pkgs/main.go": true,
}

// fullPrefixes hold files that tests across the tree read by relative path.
var fullPrefixes = []string{"migrations/", "deploy/", "openapi/", "configs/", "test/"}

// wiringPkg reads workflows, scripts, docs and the Makefile to check that
// they agree, so any change outside a Go package runs it.
const wiringPkg = "test/controlwiring"

func main() {
	files := flag.Bool("files", false, "print substantive changed files instead of packages")
	worktree := flag.Bool("worktree", false, "diff BASE_SHA against the working tree, untracked files included, instead of HEAD")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: affected-go-pkgs [-files] [-worktree] BASE_SHA")
		os.Exit(2)
	}
	if err := run(flag.Arg(0), *files, *worktree); err != nil {
		fmt.Fprintln(os.Stderr, "affected-go-pkgs:", err)
		os.Exit(1)
	}
}

func run(base string, filesMode, worktree bool) error {
	substantive, err := substantiveFiles(base, worktree)
	if err != nil {
		return err
	}
	if filesMode {
		for _, f := range substantive {
			fmt.Println(f)
		}
		return nil
	}
	dirs, full, err := touchedDirs(substantive)
	if err != nil {
		return err
	}
	if full {
		fmt.Println("./...")
		return nil
	}
	if len(dirs) == 0 {
		return nil
	}
	pkgs, err := dependents(dirs)
	if err != nil {
		return err
	}
	for _, p := range pkgs {
		fmt.Println(p)
	}
	return nil
}

// substantiveFiles lists the files changed since base, minus comment-only
// Go edits.
func substantiveFiles(base string, worktree bool) ([]string, error) {
	changed, err := changedFiles(base, worktree)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, f := range changed {
		if strings.HasSuffix(f, ".go") {
			same, err := commentOnly(base, f)
			if err != nil {
				return nil, err
			}
			if same {
				continue
			}
		}
		out = append(out, f)
	}
	return out, nil
}

// changedFiles lists base..HEAD, or with worktree the files that differ
// between base and the working tree plus untracked files, sorted.
func changedFiles(base string, worktree bool) ([]string, error) {
	if !worktree {
		return gitLines("diff", "--name-only", "--no-renames", base, "HEAD")
	}
	changed, err := gitLines("diff", "--name-only", "--no-renames", base)
	if err != nil {
		return nil, err
	}
	untracked, err := gitLines("ls-files", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	changed = append(changed, untracked...)
	sort.Strings(changed)
	return changed, nil
}

// touchedDirs maps changed files to the package directories whose tests
// they can reach; full reports a change that can reach every package.
func touchedDirs(files []string) (map[string]bool, bool, error) {
	dirs := map[string]bool{}
	for _, f := range files {
		if fullTriggers[f] {
			return nil, true, nil
		}
		dir := path.Dir(f)
		if strings.HasSuffix(f, ".go") && !hasGoFiles(dir) {
			// A deleted package: unchanged importers may no longer compile.
			return nil, true, nil
		}
		// Non-Go files reach a package through go:embed or testdata, both of
		// which live under the package directory.
		if markPackageDirs(dir, dirs) || strings.HasSuffix(f, ".go") {
			continue
		}
		// Outside every package: a test reads it by path, if at all.
		for _, p := range fullPrefixes {
			if strings.HasPrefix(f, p) {
				return nil, true, nil
			}
		}
		dirs[wiringPkg] = true
		readers, err := gitLines("grep", "-lF", path.Base(f), "--", "*.go")
		if err != nil && !isNoMatch(err) {
			return nil, false, err
		}
		for _, r := range readers {
			dirs[path.Dir(r)] = true
		}
	}
	return dirs, false, nil
}

// markPackageDirs marks dir and every ancestor holding Go files, and
// reports whether any did.
func markPackageDirs(dir string, dirs map[string]bool) bool {
	inPkg := false
	for d := dir; d != "."; d = path.Dir(d) {
		if hasGoFiles(d) {
			dirs[d] = true
			inPkg = true
		}
	}
	return inPkg
}

// dependents returns the import paths of the packages in dirs and of
// every package that depends on them.
func dependents(dirs map[string]bool) ([]string, error) {
	mod, err := cmdOutput("go", "list", "-m")
	if err != nil {
		return nil, err
	}
	mod = strings.TrimSpace(mod)
	touched := map[string]bool{}
	for d := range dirs {
		touched[mod+"/"+d] = true
	}
	list, err := cmdOutput("go", "list", "-test", "-f", "{{.ImportPath}} {{join .Deps \" \"}}", "./...")
	if err != nil {
		return nil, err
	}
	return affected(list, touched), nil
}

// affected reads `go list -test` lines ("importpath dep dep ...") and returns
// the packages that are, or depend on, a touched package.
func affected(list string, touched map[string]bool) []string {
	out := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(list), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		// Test variants print as "p [p.test]", "p.test" and "p_test [p.test]".
		self := strings.TrimSuffix(strings.TrimSuffix(fields[0], ".test"), "_test")
		hit := touched[self]
		for _, dep := range fields[1:] {
			if strings.HasPrefix(dep, "[") {
				continue
			}
			if touched[dep] {
				hit = true
				break
			}
		}
		if hit {
			out[self] = true
		}
	}
	pkgs := make([]string, 0, len(out))
	for p := range out {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)
	return pkgs
}

// commentOnly reports whether f differs from its base version only in
// comments that the compiler and test runner ignore.
func commentOnly(base, f string) (bool, error) {
	old, err := exec.CommandContext(context.Background(), "git", "show", base+":"+f).Output() //nolint:gosec // base and f come from git diff in CI
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return false, nil // added file
		}
		return false, err
	}
	cur, err := os.ReadFile(f) //nolint:gosec // f is a repo-relative path from git diff
	if errors.Is(err, os.ErrNotExist) {
		return false, nil // deleted file
	}
	if err != nil {
		return false, err
	}
	return sameTokens(old, cur), nil
}

// sameTokens compares two Go sources token by token, skipping comments
// except those that carry meaning: compiler directives, build constraints,
// cgo preambles and Example output blocks.
func sameTokens(a, b []byte) bool {
	if bytes.Contains(a, []byte(`import "C"`)) || bytes.Contains(b, []byte(`import "C"`)) {
		return bytes.Equal(a, b)
	}
	ta, tb := tokens(a), tokens(b)
	if ta == nil || tb == nil || len(ta) != len(tb) {
		return false
	}
	for i := range ta {
		if ta[i] != tb[i] {
			return false
		}
	}
	return true
}

func tokens(src []byte) []string {
	fset := token.NewFileSet()
	file := fset.AddFile("", fset.Base(), len(src))
	var s scanner.Scanner
	failed := false
	s.Init(file, src, func(token.Position, string) { failed = true }, scanner.ScanComments)
	var out []string
	for {
		_, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		switch tok {
		case token.COMMENT:
			if !meaningfulComment(lit) {
				continue
			}
		case token.SEMICOLON:
			lit = "" // "\n" and ";" are the same token
		default:
		}
		out = append(out, tok.String()+" "+lit)
	}
	if failed {
		return nil
	}
	return out
}

func meaningfulComment(c string) bool {
	return strings.HasPrefix(c, "//go:") || strings.HasPrefix(c, "//line ") ||
		strings.HasPrefix(c, "//export ") || strings.HasPrefix(c, "// +build") ||
		strings.Contains(strings.ToLower(c), "output:")
}

func hasGoFiles(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
			return true
		}
	}
	return false
}

// isNoMatch reports git grep's "nothing found" exit status.
func isNoMatch(err error) bool {
	var ee *exec.ExitError
	return errors.As(err, &ee) && ee.ExitCode() == 1
}

func gitLines(args ...string) ([]string, error) {
	out, err := cmdOutput("git", args...)
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines, nil
}

func cmdOutput(name string, args ...string) (string, error) {
	cmd := exec.CommandContext(context.Background(), name, args...) //nolint:gosec // callers pass git or go with fixed verbs
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return string(out), nil
}
