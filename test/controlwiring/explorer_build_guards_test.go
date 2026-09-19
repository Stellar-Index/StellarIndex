package controlwiring

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ─── F085 / T325: the build every deploy path shares runs the guards ──
//
// Production publishes through Cloudflare Pages' git integration, whose
// build command is `cd web/explorer && pnpm install --frozen-lockfile &&
// pnpm build` (docs/operations/explorer-deployment.md §Recommended). The
// `__next.*` segment prune, scripts/ci/explorer-file-budget.sh and
// scripts/ci/explorer-seo-lint.sh used to live only in
// .github/workflows/explorer-deploy.yml, which is workflow_dispatch-only
// — so the path that actually deploys ran none of them, which is how the
// Next 15→16 segment-file explosion froze the site on a June-24 build for
// nine days (ADR-0044:36-41).
//
// The one hook every invoker of `pnpm build` shares — CF Pages, the
// dispatch workflow, the web-explorer CI job, a laptop — is package.json's
// prebuild/build/postbuild chain, so that is where the controls are
// referenced. This leg GRADUATED out of the k023evidence build tag (it
// lived in deployed_controls_test.go) once they were.
//
// pnpm runs pre/post scripts by default (verified on pnpm 10.33, the
// packageManager pin), so nothing but `pnpm build` is needed to reach them.

const explorerPkgJSON = "web/explorer/package.json"

// explorerGuards are the three static-export guards that must run on
// every build, with the marker that proves the chain reaches each.
var explorerGuards = map[string]string{
	"the Next 16 `__next.*` segment-file prune":                                        "__next.",
	"scripts/ci/explorer-file-budget.sh (the Cloudflare 20,000-file cap guard)":        "explorer-file-budget",
	"scripts/ci/explorer-seo-lint.sh (title/description/canonical on indexable pages)": "explorer-seo-lint",
}

func TestK023_ExplorerBuildRunsExportGuards(t *testing.T) {
	t.Parallel()
	chain := explorerBuildChain(t)
	for guard, marker := range explorerGuards {
		if !strings.Contains(chain.text, marker) {
			t.Errorf("`pnpm build` in web/explorer never reaches %s — it runs only in the "+
				"workflow_dispatch explorer-deploy.yml, so the Cloudflare git-integration "+
				"build (the production publisher) ships unguarded (F085/T325)", guard)
		}
	}
}

// The prune is a DESTRUCTIVE transform, so the interesting assertion is
// the one where it fires: `__next._tree.txt` is the only segment file the
// client router prefetches, and the first version of this prune deleted it
// too — 404s on every <Link> hover across the site
// (docs/operations/postmortems/2026-08-27-explorer-console-errors.md).
// Run the wired command against a synthetic export and pin both halves.
func TestK023_ExplorerPruneKeepsTreeSegmentFiles(t *testing.T) {
	t.Parallel()
	prune := explorerPruneCommand(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	deleted := []string{"__next.__PAGE__.txt", "__next._full.txt", "__next._head.txt", "assets/__next._index.txt"}
	kept := []string{"__next._tree.txt", "assets/__next._tree.txt", "index.html", "assets/index.txt"}
	for _, rel := range append(append([]string{}, deleted...), kept...) {
		writeExportFile(t, out, rel)
	}

	cmd := exec.Command("sh", "-c", prune) //nolint:gosec // the command under test, read from package.json
	cmd.Dir = dir
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("prune command %q failed: %v\n%s", prune, err, b)
	}
	for _, rel := range kept {
		if _, err := os.Stat(filepath.Join(out, rel)); err != nil {
			t.Errorf("prune deleted %s — the client router PREFETCHES the _tree segment files; "+
				"deleting them 404s every <Link> hover (2026-08-27 postmortem)", rel)
		}
	}
	for _, rel := range deleted {
		if _, err := os.Stat(filepath.Join(out, rel)); err == nil {
			t.Errorf("prune left %s in the export — the per-segment payloads are what push the "+
				"file count past Cloudflare's 20,000-file cap (F085)", rel)
		}
	}
}

func writeExportFile(t *testing.T, out, rel string) {
	t.Helper()
	path := filepath.Join(out, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// explorerPruneCommand returns the single package script that prunes the
// segment files, as the shell sees it.
func explorerPruneCommand(t *testing.T) string {
	t.Helper()
	chain := explorerBuildChain(t)
	var found []string
	for name, body := range chain.scripts {
		if strings.Contains(body, "__next.") {
			found = append(found, name)
		}
	}
	switch len(found) {
	case 1:
		return chain.scripts[found[0]]
	case 0:
		t.Fatal("no script in web/explorer's pnpm build chain prunes `__next.*` (F085); if the " +
			"prune moved out of package.json, point this test at its new home")
	default:
		t.Fatalf("%v all prune `__next.*` — one destructive transform, one home", found)
	}
	return ""
}

// buildChain is the reachable text of `pnpm build` in web/explorer.
type buildChain struct {
	scripts map[string]string // package scripts reached, name → body
	text    string            // those bodies plus every repo script they name
}

// pkgScriptRefRE finds a sibling package script a script delegates to
// (`pnpm run x`, `npm run x`, `pnpm x`).
var pkgScriptRefRE = regexp.MustCompile(`(?:pnpm|npm|yarn)(?:\s+run)?\s+([A-Za-z0-9:_.-]+)`)

// repoScriptRefRE finds a repo script path named inside a package script.
var repoScriptRefRE = regexp.MustCompile(`[\w./-]+\.(?:sh|mjs|js|ts)`)

// explorerBuildChain walks prebuild → build → postbuild transitively: a
// control the chain delegates to counts, wherever the delegation lands.
func explorerBuildChain(t *testing.T) buildChain {
	t.Helper()
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal([]byte(readRepoFile(t, explorerPkgJSON)), &pkg); err != nil {
		t.Fatalf("parse %s: %v", explorerPkgJSON, err)
	}
	chain := buildChain{scripts: map[string]string{}}
	for queue := []string{"prebuild", "build", "postbuild"}; len(queue) > 0; queue = queue[1:] {
		name := queue[0]
		body, ok := pkg.Scripts[name]
		if !ok || chain.scripts[name] != "" {
			continue
		}
		chain.scripts[name] = body
		chain.text += body + "\n" + repoScriptText(t, body)
		for _, m := range pkgScriptRefRE.FindAllStringSubmatch(body, -1) {
			queue = append(queue, m[1])
		}
	}
	if len(chain.scripts) == 0 {
		t.Fatalf("%s has no prebuild/build/postbuild — this test is asserting nothing", explorerPkgJSON)
	}
	return chain
}

// repoScriptText returns the contents of every in-repo script the body
// names, so a guard invoked one hop away still counts as reached.
func repoScriptText(t *testing.T, body string) string {
	t.Helper()
	var text string
	for _, ref := range repoScriptRefRE.FindAllString(body, -1) {
		for _, base := range []string{"web/explorer", "."} {
			b, err := os.ReadFile(filepath.Join(repoRoot(t), base, ref)) //nolint:gosec // repo-relative, test-only
			if err == nil {
				text += string(b) + "\n"
			}
		}
	}
	return text
}
