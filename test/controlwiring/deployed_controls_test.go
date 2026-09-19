//go:build k023evidence

package controlwiring

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
	"github.com/Stellar-Index/StellarIndex/internal/sources/phoenix"
)

// Class K023 — a control exists in the tree but the production path
// never invokes it. One test per control; each asserts the DEPLOYED
// path references the control, not merely that the control exists.
//
// Build-tagged because, as of the commit that added it, every leg is
// RED and each leg's fix lives in files owned by a different unit
// (F048, F050, F085, F133, F144). Print the live status with:
//
//	go test -tags k023evidence ./test/controlwiring/ -run TestK023 -v
//
// When all five are green, drop the build tag: this file is then the
// class's regression guard.

// ─── F144: -fail-on-missed on the units where it can fire ──────────
//
// verify-archive consumes -fail-on-missed ONLY inside the checkpoint
// tier (internal/ops/archive/verify_archive.go: the flag is read by
// checkpointAnchorDecision, reached only `if doCheckpoint`, and
// doCheckpoint is `-tier checkpoint|all`). The tier-a units run
// `-tier chain`, where the flag is inert — adding it there would
// state an invariant the binary never evaluates. The control has to
// be wired on the units that engage the checkpoint tier: tier-b, in
// BOTH trees (the ansible template is r1's authority; deploy/systemd
// is the operator-facing reference copy).
var verifyArchiveUnits = []string{
	"configs/ansible/roles/archival-node/templates/systemd/verify-archive-tier-a.service.j2",
	"configs/ansible/roles/archival-node/templates/systemd/verify-archive-tier-b.service.j2",
	"deploy/systemd/verify-archive-tier-a.service",
	"deploy/systemd/verify-archive-tier-b.service",
}

var tierFlagRE = regexp.MustCompile(`(?m)^\s*-tier\s+(\S+)`)

func TestK023_VerifyArchiveCheckpointUnitsFailOnMissed(t *testing.T) {
	t.Parallel()
	checkpointUnits := 0
	for _, rel := range verifyArchiveUnits {
		body := readRepoFile(t, rel)
		m := tierFlagRE.FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("%s: no -tier flag in ExecStart", rel)
		}
		tier := m[1]
		if tier != "checkpoint" && tier != "all" {
			// chain / peers / archivist never reach the
			// checkpoint-anchor decision; the flag is inert there.
			continue
		}
		checkpointUnits++
		if !execStartHasFlag(body, "-fail-on-missed") {
			t.Errorf("%s runs `-tier %s` without -fail-on-missed: a PARTIAL cross-anchor "+
				"checkpoint miss exits 0 and advances the verified watermark, so ADR-0017 "+
				"X1.7's \"hard invariant\" is a soft tolerance on the deployed path (F144)", rel, tier)
		}
	}
	if checkpointUnits == 0 {
		t.Fatal("no verify-archive unit engages the checkpoint tier — this test is asserting nothing")
	}
}

// execStartHasFlag reports whether flag appears as its own argument on
// a non-comment line (a mention in a unit-file comment does not count).
func execStartHasFlag(body, flag string) bool {
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		for _, field := range strings.Fields(strings.TrimSuffix(trimmed, "\\")) {
			if field == flag {
				return true
			}
		}
	}
	return false
}

// ─── F133: no gate job may be continue-on-error ────────────────────
//
// A job-level `continue-on-error: true` makes the job's conclusion
// irrelevant to the run. branch-protection-status is the only such job
// in ci.yml; it also only emits a ::warning:: — it cannot fail on an
// unprotected main even without the key. It is honest about that (its
// name says "informational", and the ci.yml header states main has no
// active rules), so the residual defect is the missing ruleset itself:
// a repository Setting and a maintainer policy call, not a file in
// this tree. This leg stays red until that call is made — either a
// ruleset exists and the probe enforces it, or the job is deleted.
func TestK023_NoContinueOnErrorJobsInCI(t *testing.T) {
	t.Parallel()
	var wf struct {
		Jobs map[string]struct {
			ContinueOnError any `yaml:"continue-on-error"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(readRepoFile(t, ".github/workflows/ci.yml")), &wf); err != nil {
		t.Fatalf("parse ci.yml: %v", err)
	}
	if len(wf.Jobs) == 0 {
		t.Fatal("ci.yml parsed to zero jobs — this test is asserting nothing")
	}
	for name, job := range wf.Jobs {
		if b, ok := job.ContinueOnError.(bool); ok && b {
			t.Errorf("ci.yml job %q is continue-on-error: true — its verdict cannot gate "+
				"anything (F133 / K023 enumeration)", name)
		}
	}
}

// scriptRefRE finds repo-script paths named inside a package.json script.
var scriptRefRE = regexp.MustCompile(`[\w./-]+\.(?:sh|mjs|js|ts)`)

// ─── F085: the Cloudflare git build must run the prune + budget ────
//
// Production publishes through Cloudflare Pages' git integration,
// whose build command is `pnpm build` in web/explorer
// (docs/operations/explorer-deployment.md). The `__next.*` segment
// prune and scripts/ci/explorer-file-budget.sh live only in
// explorer-deploy.yml, which is workflow_dispatch-only. The one hook
// every invoker of `pnpm build` shares is package.json's
// prebuild/build/postbuild chain, so that is where the control must
// be referenced.
func TestK023_ExplorerBuildRunsPruneAndFileBudget(t *testing.T) {
	t.Parallel()
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal([]byte(readRepoFile(t, "web/explorer/package.json")), &pkg); err != nil {
		t.Fatalf("parse web/explorer/package.json: %v", err)
	}
	chain := pkg.Scripts["prebuild"] + "\n" + pkg.Scripts["build"] + "\n" + pkg.Scripts["postbuild"]
	if strings.TrimSpace(chain) == "" {
		t.Fatal("web/explorer/package.json has no build scripts — this test is asserting nothing")
	}
	// A script the chain delegates to counts: follow one hop into any
	// repo script the chain names.
	reach := chain
	for _, ref := range scriptRefRE.FindAllString(chain, -1) {
		for _, base := range []string{"web/explorer", "."} {
			b, err := os.ReadFile(filepath.Join(repoRoot(t), base, ref)) //nolint:gosec // repo-relative, test-only
			if err == nil {
				reach += "\n" + string(b)
			}
		}
	}
	if !strings.Contains(reach, "explorer-file-budget") {
		t.Errorf("`pnpm build` (the Cloudflare git-integration build command) never reaches " +
			"scripts/ci/explorer-file-budget.sh — the 20,000-file guard runs only in the " +
			"workflow_dispatch explorer-deploy.yml (F085)")
	}
	if !strings.Contains(reach, "__next.") {
		t.Errorf("`pnpm build` never prunes the Next 16 `__next.*` segment files — the prune " +
			"runs only in the workflow_dispatch explorer-deploy.yml, so the git-integration " +
			"build ships the unpruned export (F085)")
	}
}

// ─── F050: the replay paths must consult BackfillSafe ──────────────
//
// external.BackfillSafe is the per-source "this decoder is safe
// against every historical WASM generation" gate. Its only caller is
// `stellarindex-ops backfill`. The commands operators actually use to
// re-decode history — projector-replay and ch-rebuild — never ask.
// Source-level on purpose: the assertion is "some non-test file on
// each replay path calls the gate"; the owning fix (F050) carries the
// behavioural test.
//
// Status after the F050 fix: projector-replay and ch-rebuild are GREEN
// (they ask through external.ReplayBackfillSafe; behavioural tests in
// internal/ops/ingest/projector_backfillsafe_test.go and
// internal/ops/chops/ch_rebuild_backfillsafe_test.go). projected-rebuild
// was added to this list by that fix and is RED: it is the third
// re-derive path — the bulk sibling the projector-replay runbook sends
// any rewind over ~1M ledgers to — it builds the same current decoder
// over the same history, and it never asks either. Its file was outside
// the fixing unit's scope, so this leg is the evidence, not the fix.
func TestK023_ReplayPathsConsultBackfillSafe(t *testing.T) {
	t.Parallel()
	paths := map[string][]string{
		"projector-replay":  {"internal/ops/ingest/projector*.go", "internal/projector/*.go"},
		"ch-rebuild":        {"internal/ops/chops/ch_rebuild*.go"},
		"projected-rebuild": {"internal/ops/chops/projected_rebuild*.go"},
	}
	for cmd, globs := range paths {
		files, found := scanForCall(t, globs, "BackfillSafe(")
		if files == 0 {
			t.Fatalf("%s: globs %v matched no files — this test is asserting nothing", cmd, globs)
		}
		if !found {
			t.Errorf("%s never calls external.BackfillSafe (searched %d files under %v): the "+
				"per-source historical-WASM gate is consulted only by `backfill` (F050)", cmd, files, globs)
		}
	}
}

// scanForCall reports how many non-test Go files the globs matched and
// whether any of them contains needle.
func scanForCall(t *testing.T, globs []string, needle string) (files int, found bool) {
	t.Helper()
	for _, g := range globs {
		matches, err := filepath.Glob(filepath.Join(repoRoot(t), g))
		if err != nil {
			t.Fatalf("glob %s: %v", g, err)
		}
		for _, f := range matches {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			files++
			b, err := os.ReadFile(f) //nolint:gosec // repo-relative, test-only
			if err != nil {
				t.Fatalf("read %s: %v", f, err)
			}
			if strings.Contains(string(b), needle) {
				found = true
			}
		}
	}
	return files, found
}

// ─── F048: phoenix's factory anchor must be able to admit a pool ───
//
// pipeline.GatedMeta declares phoenix Factories + CreationSym
// "create", and seed-protocol-contracts walks exactly those events —
// but it only calls Decode on events the decoder Matches. Phoenix's
// Matches rejects every factory event (classifyAny has no "create"
// action and reg.Has excludes the factory), so the walk and the
// live-upsert hook are provably inert: a pool the factory creates
// tomorrow is fail-closed until someone edits MainnetPools by hand.
//
// The assertion is deliberately independent of the creation event's
// BODY shape (no in-tree fixture pins it): admission is impossible
// unless Matches accepts the factory's ("create", …) event in at
// least one of the two topic encodings soroban-sdk can emit.
func TestK023_PhoenixFactoryCreateEventIsAdmissible(t *testing.T) {
	t.Parallel()
	hooked := 0
	dec := phoenix.NewDecoder(contractid.WithHook(func(string, string, uint32) { hooked++ }))

	matched := false
	for _, enc := range []func(string) string{scval.MustEncodeString, scval.MustEncodeSymbol} {
		ev := events.Event{
			Type:                     "contract",
			Ledger:                   63_307_000,
			LedgerClosedAt:           "2026-07-02T00:00:00Z",
			ContractID:               phoenix.MainnetFactory,
			TxHash:                   strings.Repeat("ab", 32),
			InSuccessfulContractCall: true,
			Topic:                    []string{enc("create"), enc("liquidity_pool")},
		}
		if dec.Matches(ev) {
			matched = true
		}
	}
	if !matched {
		t.Errorf("phoenix.Decoder.Matches rejects the factory's (\"create\",\"liquidity_pool\") "+
			"event in both topic encodings: seed-protocol-contracts and the live-upsert hook "+
			"(fired %d times) can never admit a factory-created pool (F048)", hooked)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	// test/controlwiring -> repo root
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %s has no go.mod: %v", root, err)
	}
	return root
}

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), rel)) //nolint:gosec // repo-relative, test-only
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}
