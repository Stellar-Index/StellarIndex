// lint-comments fails on a history citation in a product Go comment: a date,
// a review/inventory ticket id, a #nnn or "PR n" reference. History belongs in
// the commit message; a comment keeps only the why.
//
// Scope: every .go file under the root except _test.go files, pkg/ (its godoc
// is the public SDK reference), generated files, and vendor/testdata/
// node_modules/dot dirs. lint-repo-budget.sh still checks added test comments.
//
// The baseline lists today's citations as "<path>\t<first match on the line>",
// one line per comment line. A citation not covered by the baseline fails, and
// so does a baseline line the tree no longer needs, so the file only shrinks.
//
// Usage:
//
//	go run ./scripts/ci/lint-comments                 check the tree
//	go run ./scripts/ci/lint-comments -write          rewrite the baseline from the tree
//	go run ./scripts/ci/lint-comments -list           print every hit with its comment line
//	flags: -root DIR (default .), -baseline FILE (default scripts/ci/lint-comments.baseline)
package main

import (
	"bufio"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// hard matches dates, ticket ids, #nnn and "PR n", including every pattern
// lint-repo-budget.sh rejects in added comments.
var hard = regexp.MustCompile(`(?i)\b20\d\d-\d\d-\d\d\b|\b202[4-9]-[01]\d\b|` +
	`\b(?:RLT|CS|CO|NS|LIVE|INV|LC)-\d+\b|\b(?:F|Q|T|RSWP|CMA|CMB|HIS|DOC|YDC|DRY)-\d{3,4}\b|` +
	`\b[FTQK]\d{3}\b|#\d{3,}\b|\bPR \d+|\bpre-20\d\d\b`)

// goLayout is Go's reference time; a comment showing a time.Format layout is not history.
const goLayout = "2006-01-02"

type hit struct {
	path, match, text string
	line              int
}

func main() {
	root := flag.String("root", ".", "repository root")
	baseline := flag.String("baseline", "scripts/ci/lint-comments.baseline", "baseline file, relative to -root")
	write := flag.Bool("write", false, "rewrite the baseline from the tree")
	list := flag.Bool("list", false, "print every hit")
	flag.Parse()

	hits, err := scan(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "lint-comments:", err)
		os.Exit(2)
	}
	if *list {
		for _, h := range hits {
			fmt.Printf("%s:%d\t%s\t%s\n", h.path, h.line, h.match, h.text)
		}
		return
	}
	blPath := filepath.Join(*root, *baseline)
	if *write {
		if err := writeBaseline(blPath, hits); err != nil {
			fmt.Fprintln(os.Stderr, "lint-comments:", err)
			os.Exit(2)
		}
		fmt.Printf("lint-comments: wrote %s (%d entries)\n", *baseline, len(hits))
		return
	}
	want, err := readBaseline(blPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "lint-comments:", err)
		os.Exit(2)
	}
	os.Exit(check(hits, want, *baseline))
}

func check(hits []hit, want map[string]int, baseline string) int {
	have := map[string]int{}
	var fresh []hit
	for _, h := range hits {
		k := h.path + "\t" + h.match
		have[k]++
		if have[k] > want[k] {
			fresh = append(fresh, h)
		}
	}
	var stale []string
	for k, n := range want {
		if have[k] < n {
			stale = append(stale, fmt.Sprintf("%s (baseline %d, tree %d)", strings.Replace(k, "\t", " ", 1), n, have[k]))
		}
	}
	sort.Strings(stale)
	for _, h := range fresh {
		fmt.Printf("%s:%d: comment cites %q; put history and ids in the commit message and keep the comment to the why: %s\n", h.path, h.line, h.match, h.text)
	}
	for _, s := range stale {
		fmt.Printf("stale baseline entry %s: delete the line from %s (or run -write)\n", s, baseline)
	}
	if len(fresh)+len(stale) > 0 {
		fmt.Printf("lint-comments: FAIL — %d new citation(s), %d stale baseline entr(y/ies)\n", len(fresh), len(stale))
		return 1
	}
	fmt.Printf("lint-comments: OK — %d baselined history citation(s) in product Go comments, 0 new\n", len(hits))
	return 0
}

func scan(root string) ([]hit, error) {
	var hits []hit
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			n := d.Name()
			if rel != "." && (strings.HasPrefix(n, ".") || strings.HasPrefix(n, "_") ||
				n == "vendor" || n == "testdata" || n == "node_modules" || rel == "pkg") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		if ast.IsGenerated(f) {
			return nil
		}
		for _, cg := range f.Comments {
			for _, c := range cg.List {
				line := fset.Position(c.Slash).Line
				for i, text := range strings.Split(c.Text, "\n") {
					if h, ok := match(text); ok {
						hits = append(hits, hit{path: rel, line: line + i, match: h, text: strings.TrimSpace(text)})
					}
				}
			}
		}
		return nil
	})
	return hits, err
}

func match(text string) (string, bool) {
	t := strings.TrimSpace(text)
	if strings.HasPrefix(t, "//go:") || strings.HasPrefix(t, "//nolint") || strings.HasPrefix(t, "//line ") {
		return "", false
	}
	for _, m := range hard.FindAllString(t, -1) {
		if m != goLayout {
			return m, true
		}
	}
	return "", false
}

func readBaseline(path string) (map[string]int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	want := map[string]int{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := sc.Text()
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		want[l]++
	}
	return want, sc.Err()
}

func writeBaseline(path string, hits []hit) error {
	lines := make([]string, 0, len(hits))
	for _, h := range hits {
		lines = append(lines, h.path+"\t"+h.match)
	}
	sort.Strings(lines)
	var b strings.Builder
	b.WriteString("# History citations in product Go comments, one per comment line: <path>\\t<first match>.\n")
	b.WriteString("# Shrink-only: delete a line when its citation goes; scripts/ci/lint-comments fails on new or stale entries.\n")
	for _, l := range lines {
		b.WriteString(l + "\n")
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}
