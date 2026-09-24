package timescale

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// safetyClaimRe matches a down-migration header asserting the drop is safe
// to run as-is. Such a claim is only true when nothing live still names the
// dropped relation: a query against a missing table or column is a 42P01 /
// 42703 error, never an empty result or a fallback.
var safetyClaimRe = regexp.MustCompile(`(?i)correctness[- ]safe`)

var (
	droppedRelationRe = regexp.MustCompile(
		`(?i)\bDROP\s+(?:TABLE|MATERIALIZED\s+VIEW|VIEW)\s+(?:IF\s+EXISTS\s+)?` +
			`([a-z_][a-z0-9_.]*(?:\s*,\s*[a-z_][a-z0-9_.]*)*)`)
	droppedColumnRe = regexp.MustCompile(`(?i)\bDROP\s+COLUMN\s+(?:IF\s+EXISTS\s+)?([a-z_][a-z0-9_]*)`)
)

// TestDownMigrationSafetyClaims_NoLiveReader fails when a .down.sql header
// claims "correctness-safe" while dropping a table or column that
// non-test Go under internal/ or cmd/ still names outside a comment.
func TestDownMigrationSafetyClaims_NoLiveReader(t *testing.T) {
	root := findRepoRoot(t)
	downs, err := filepath.Glob(filepath.Join(root, "migrations", "*.down.sql"))
	if err != nil || len(downs) == 0 {
		t.Fatalf("glob down migrations: %v (found %d)", err, len(downs))
	}
	corpus := liveGoCorpus(t, root)
	claims := 0
	for _, path := range downs {
		comments, sqlBody := splitDownMigration(t, path)
		if !safetyClaimRe.MatchString(comments) {
			continue
		}
		claims++
		for _, name := range droppedNames(sqlBody) {
			if file := firstLiveReference(corpus, name); file != "" {
				t.Errorf("%s claims the drop is correctness-safe, but live code still names %q (%s): "+
					"with it gone that code fails with an undefined-relation error; "+
					"say the code must be reverted first instead",
					filepath.Base(path), name, file)
			}
		}
	}
	t.Logf("checked %d down migrations, %d carrying a safety claim", len(downs), claims)
}

func splitDownMigration(t *testing.T, path string) (comments, sqlBody string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var c, s strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			c.WriteString(line + "\n")
			continue
		}
		s.WriteString(line + "\n")
	}
	return c.String(), s.String()
}

func droppedNames(sqlBody string) []string {
	var out []string
	for _, m := range droppedColumnRe.FindAllStringSubmatch(sqlBody, -1) {
		out = append(out, strings.ToLower(m[1]))
	}
	for _, m := range droppedRelationRe.FindAllStringSubmatch(sqlBody, -1) {
		for _, n := range strings.Split(m[1], ",") {
			n = strings.ToLower(strings.TrimSpace(n))
			if i := strings.LastIndex(n, "."); i >= 0 {
				n = n[i+1:]
			}
			out = append(out, n)
		}
	}
	return out
}

// liveGoCorpus maps each non-test .go file under internal/ and cmd/ to its
// text with // comment lines removed, so prose that merely mentions a
// table does not count as a reader.
func liveGoCorpus(t *testing.T, root string) map[string]string {
	t.Helper()
	corpus := map[string]string{}
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return err
			}
			raw, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			var b strings.Builder
			for _, line := range strings.Split(string(raw), "\n") {
				if !strings.HasPrefix(strings.TrimSpace(line), "//") {
					b.WriteString(line + "\n")
				}
			}
			rel, _ := filepath.Rel(root, p)
			corpus[rel] = b.String()
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	if len(corpus) == 0 {
		t.Fatal("no live Go files found; the reference check would pass vacuously")
	}
	return corpus
}

func firstLiveReference(corpus map[string]string, name string) string {
	files := make([]string, 0, len(corpus))
	for f := range corpus {
		files = append(files, f)
	}
	sort.Strings(files)
	for _, file := range files {
		if containsIdent(corpus[file], name) {
			return file
		}
	}
	return ""
}

// containsIdent reports whether ident occurs in text as a whole SQL/Go
// identifier (a regexp \b scan over the corpus is ~100x slower).
func containsIdent(text, ident string) bool {
	for i := 0; i < len(text); {
		j := strings.Index(text[i:], ident)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(ident)
		if (start == 0 || !isIdentByte(text[start-1])) && (end == len(text) || !isIdentByte(text[end])) {
			return true
		}
		i = start + 1
	}
	return false
}

func isIdentByte(b byte) bool {
	return b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}
