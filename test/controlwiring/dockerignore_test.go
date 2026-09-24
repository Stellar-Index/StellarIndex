package controlwiring

import (
	"bufio"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/moby/patternmatcher"
	"github.com/moby/patternmatcher/ignorefile"
)

// ─── T524: the Docker build context must carry build inputs and nothing else ──
//
// Every docker/stellarindex-*.Dockerfile builds with the repo root as context
// and runs `COPY . .` in its builder stage, so without a .dockerignore the
// daemon receives — and a builder layer persists — .git (config included),
// every gitignored .env / vault password / service-account key / ansible
// secrets inventory, and web node_modules. Both directions are pinned here
// with Docker's own matcher: secret-shaped paths are excluded, and every file
// the six builds actually read (go list over the Dockerfiles' ./cmd targets,
// plus each COPY source) is still sent.

// mustExclude are paths that must never enter the build context. They are
// deliberately placed both at the root and inside the allowlisted Go trees.
var mustExclude = []string{
	".git/config",
	".git/HEAD",
	".env",
	".env.production",
	"deploy/docker-compose/.env",
	"configs/local.yaml",
	"configs/prod.secret.yaml",
	"configs/ansible/vault-password.txt",
	"configs/ansible/inventory/r1.yml",
	"configs/ansible/inventory/r1.secrets.yaml",
	"web/explorer/node_modules/react/index.js",
	"web/explorer/.next/cache/x.js",
	".discovery-repos/some-repo/main.go",
	"notes/BACKLOG.md",
	"bin/stellarindex-api",
	"cmd/stellarindex-api/.env",
	"internal/api/v1/.env.local",
	"internal/auth/testdata/signing.key",
	"pkg/client/tls.pem",
	"cmd/stellarindex-ops/credentials.json",
	"internal/config/service-account-prod.json",
	"internal/sources/gcp-key-r1.json",
	"migrations/prod.secrets.yaml",
}

func loadDockerignore(t *testing.T, root string) *patternmatcher.PatternMatcher {
	t.Helper()
	var patterns []string
	f, err := os.Open(filepath.Join(root, ".dockerignore"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// No ignore file: Docker sends everything. Assert against that.
	case err != nil:
		t.Fatalf("open .dockerignore: %v", err)
	default:
		defer f.Close()
		if patterns, err = ignorefile.ReadAll(f); err != nil {
			t.Fatalf("parse .dockerignore: %v", err)
		}
	}
	pm, err := patternmatcher.New(patterns)
	if err != nil {
		t.Fatalf("compile .dockerignore: %v", err)
	}
	return pm
}

func excluded(t *testing.T, pm *patternmatcher.PatternMatcher, rel string) bool {
	t.Helper()
	ok, err := pm.MatchesOrParentMatches(filepath.ToSlash(rel))
	if err != nil {
		t.Fatalf("match %s: %v", rel, err)
	}
	return ok
}

func TestT524_DockerContextExcludesSecrets(t *testing.T) {
	t.Parallel()
	pm := loadDockerignore(t, repoRoot(t))
	for _, p := range mustExclude {
		if !excluded(t, pm, p) {
			t.Errorf("%s would be sent in the Docker build context and copied into the builder stage by `COPY . .`", p)
		}
	}
}

var (
	cmdTargetRe = regexp.MustCompile(`\./cmd/[A-Za-z0-9_-]+`)
	copyLineRe  = regexp.MustCompile(`(?i)^\s*COPY\s+(.+)$`)
)

// dockerfileInputs returns the ./cmd build targets and the context-relative
// COPY sources (other than `.`) named by docker/*.Dockerfile.
func dockerfileInputs(t *testing.T, root string) (targets, copySrcs []string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(root, "docker", "*.Dockerfile"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no docker/*.Dockerfile found (err=%v)", err)
	}
	seen := map[string]bool{}
	for _, df := range files {
		body, err := os.ReadFile(df)
		if err != nil {
			t.Fatalf("read %s: %v", df, err)
		}
		for _, m := range cmdTargetRe.FindAllString(string(body), -1) {
			if !seen[m] {
				seen[m] = true
				targets = append(targets, m)
			}
		}
		copySrcs = append(copySrcs, copySources(string(body))...)
	}
	if len(targets) == 0 {
		t.Fatal("no ./cmd/<binary> build target found in docker/*.Dockerfile")
	}
	sort.Strings(targets)
	return targets, copySrcs
}

func copySources(body string) []string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		m := copyLineRe.FindStringSubmatch(sc.Text())
		if m == nil || strings.Contains(m[1], "--from") {
			continue
		}
		fields := strings.Fields(m[1])
		for _, src := range fields[:len(fields)-1] {
			if src != "." && !strings.HasPrefix(src, "--") {
				out = append(out, strings.TrimSuffix(src, "/"))
			}
		}
	}
	return out
}

// goBuildInputs lists every main-module file `go build` reads for targets
// under the Dockerfiles' GOOS/CGO settings.
func goBuildInputs(t *testing.T, root string, targets []string) []string {
	t.Helper()
	const tmpl = `{{if and .Module .Module.Main}}{{$d := .Dir}}` +
		`{{range .GoFiles}}{{$d}}/{{.}}{{"\n"}}{{end}}` +
		`{{range .SFiles}}{{$d}}/{{.}}{{"\n"}}{{end}}` +
		`{{range .EmbedFiles}}{{$d}}/{{.}}{{"\n"}}{{end}}{{end}}`
	args := append([]string{"list", "-deps", "-f", tmpl}, targets...)
	cmd := exec.Command("go", args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOOS=linux", "CGO_ENABLED=0")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list %v: %v", targets, err)
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		rel, err := filepath.Rel(root, line)
		if err != nil {
			t.Fatalf("rel %s: %v", line, err)
		}
		files = append(files, rel)
	}
	if len(files) == 0 {
		t.Fatalf("go list returned no main-module files for %v", targets)
	}
	return files
}

func expandCopySources(t *testing.T, root string, srcs []string) []string {
	t.Helper()
	var files []string
	for _, src := range srcs {
		err := filepath.WalkDir(filepath.Join(root, src), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, rerr := filepath.Rel(root, path)
			files = append(files, rel)
			return rerr
		})
		if err != nil {
			t.Fatalf("walk COPY source %s: %v", src, err)
		}
	}
	return files
}

func TestT524_DockerContextKeepsBuildInputs(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	pm := loadDockerignore(t, root)
	targets, srcs := dockerfileInputs(t, root)
	inputs := append(goBuildInputs(t, root, targets), expandCopySources(t, root, srcs)...)
	for _, rel := range inputs {
		if excluded(t, pm, rel) {
			t.Errorf("%s is a build input of docker/*.Dockerfile but .dockerignore excludes it", rel)
		}
	}
	t.Logf("checked %d build inputs for %d targets and %d COPY sources", len(inputs), len(targets), len(srcs))
}
