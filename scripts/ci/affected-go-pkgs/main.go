// Command affected-go-pkgs prints what a diff needs tested: with -files, the
// changed files minus comment-only Go edits; otherwise the import paths of
// every package the diff touches plus every package that depends on them, or
// "./..." when the diff can reach every package.
//
// Usage: go run ./scripts/ci/affected-go-pkgs [-files] BASE_SHA
package main

import (
	"bytes"
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

func main() {
	files := flag.Bool("files", false, "print substantive changed files instead of packages")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: affected-go-pkgs [-files] BASE_SHA")
		os.Exit(2)
	}
	if err := run(flag.Arg(0), *files); err != nil {
		fmt.Fprintln(os.Stderr, "affected-go-pkgs:", err)
		os.Exit(1)
	}
}

func run(base string, filesMode bool) error {
	changed, err := gitLines("diff", "--name-only", "--no-renames", base, "HEAD")
	if err != nil {
		return err
	}
	var substantive []string
	for _, f := range changed {
		if strings.HasSuffix(f, ".go") {
			same, err := commentOnly(base, f)
			if err != nil {
				return err
			}
			if same {
				continue
			}
		}
		substantive = append(substantive, f)
	}
	if filesMode {
		for _, f := range substantive {
			fmt.Println(f)
		}
		return nil
	}

	dirs := map[string]bool{}
	for _, f := range substantive {
		if fullTriggers[f] {
			fmt.Println("./...")
			return nil
		}
		dir := path.Dir(f)
		if strings.HasSuffix(f, ".go") && !hasGoFiles(dir) {
			// A deleted package: unchanged importers may no longer compile.
			fmt.Println("./...")
			return nil
		}
		// Non-Go files reach a package through go:embed or testdata, both of
		// which live under the package directory.
		for d := dir; d != "."; d = path.Dir(d) {
			if hasGoFiles(d) {
				dirs[d] = true
			}
		}
	}
	if len(dirs) == 0 {
		return nil
	}

	mod, err := cmdOutput("go", "list", "-m")
	if err != nil {
		return err
	}
	mod = strings.TrimSpace(mod)
	touched := map[string]bool{}
	for d := range dirs {
		touched[mod+"/"+d] = true
	}
	list, err := cmdOutput("go", "list", "-test", "-f", "{{.ImportPath}} {{join .Deps \" \"}}", "./...")
	if err != nil {
		return err
	}
	for _, p := range affected(list, touched) {
		fmt.Println(p)
	}
	return nil
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
	old, err := exec.Command("git", "show", base+":"+f).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return false, nil // added file
		}
		return false, err
	}
	cur, err := os.ReadFile(f)
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
	cmd := exec.Command(name, args...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return string(out), nil
}
