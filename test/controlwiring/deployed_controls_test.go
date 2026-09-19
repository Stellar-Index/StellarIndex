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
	"github.com/Stellar-Index/StellarIndex/internal/sources/phoenix"
)

// Class K023 — a control exists in the tree but the production path
// never invokes it. One test per control; each asserts the DEPLOYED
// path references the control, not merely that the control exists.
//
// Build-tagged because, as of the commit that added it, every leg was
// RED and each leg's fix lives in files owned by a different unit
// (F048, F050, F085, F133, F144). Print the live status with:
//
//	go test -tags k023evidence ./test/controlwiring/ -run TestK023 -v
//
// A leg whose fix has landed GRADUATES: it moves to an untagged file in
// this package so it guards the default suite (F050 has — see
// replay_backfillsafe_test.go). When the remaining four are green, drop
// the build tag: this file is then the class's regression guard.

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
// GRADUATED. All three re-derive paths (projector-replay, ch-rebuild,
// projected-rebuild) now ask external.ReplayBackfillSafe, so this leg
// left the build tag: TestK023_ReplayPathsConsultBackfillSafe lives in
// replay_backfillsafe_test.go and runs in the default suite. It still
// shows up in the tagged run above, because that file has no tag.

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
// Driven by the REAL lake captures under test/fixtures/phoenix/
// factory-create (loader + shape pins: phoenix_factory_create_fixture_
// test.go, untagged). The assertion is ADMISSION, not recognition: every
// announced pool is already in the curated seed, so reg.Has proves
// nothing, but contractid.Registry.Seed fires its hook on every call —
// so the hook receiving (announced pool, factory, creation ledger) is
// the one observation that only a decoder which really seeds from the
// event can produce. A patch that makes Matches accept the event and
// then drops it on the floor leaves this RED, as it should: the control
// would still be inert, and ch-recognition (one exemplar per
// (contract, topic_0_sym) shape, run through Matches) could then report
// the factory as recognised while nothing is admitted.
//
// STILL RED, deliberately (2026-09-19). Auto-admission is blocked on a
// security property nobody has established: that create_liquidity_pool
// is allow-listed AND that the published address is the one the factory
// deployed, not a caller-supplied argument — for the factory WASM
// installed NOW (the in-repo WASM walk ends at ledger 57,856,963; the
// factory is admin-upgradeable). defindex's self-registration was
// removed on 2026-08-25 for that exact poisoning vector. See
// docs/operations/wasm-audits/phoenix.md, "Factory create event".
func TestK023_PhoenixFactoryCreateEventIsAdmissible(t *testing.T) {
	t.Parallel()
	type seeded struct {
		child, factory string
		ledger         uint32
	}
	var got []seeded
	dec := phoenix.NewDecoder(contractid.WithHook(func(child, factory string, ledger uint32) {
		got = append(got, seeded{child, factory, ledger})
	}))

	rows := phoenixCreateRows(t)
	var want []seeded
	for _, r := range rows {
		ev := r.event()
		announcedPool := phoenixAnnouncedPool(t, r)
		want = append(want, seeded{announcedPool, phoenix.MainnetFactory, r.LedgerSeq})
		if !dec.Matches(ev) {
			t.Errorf("ledger %d: phoenix.Decoder.Matches rejects the factory's real "+
				"(\"create\",\"liquidity_pool\") event announcing %s", r.LedgerSeq, announcedPool)
			continue
		}
		if _, err := dec.Decode(ev); err != nil {
			t.Errorf("ledger %d: decode factory create: %v", r.LedgerSeq, err)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("live-upsert hook fired %d times over %d real factory create events: "+
			"seed-protocol-contracts and the indexer can never admit a factory-created pool (F048)",
			len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("hook call %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestK023_PhoenixFactoryCreateFromForeignEmitterIsNotAdmitted is the
// property any F048 fix must keep: topic shape is forgeable, so the same
// real event republished by a contract that is NOT the factory must
// neither match nor seed. Green today (nothing matches); it is here so
// the fix that turns the test above green cannot do it by trusting the
// topic alone.
func TestK023_PhoenixFactoryCreateFromForeignEmitterIsNotAdmitted(t *testing.T) {
	t.Parallel()
	hooked := 0
	dec := phoenix.NewDecoder(contractid.WithHook(func(string, string, uint32) { hooked++ }))
	for _, r := range phoenixCreateRows(t) {
		ev := r.event()
		// A curated POOL is the strongest forger: it already passes reg.Has.
		ev.ContractID = phoenix.MainnetPools[0]
		if dec.Matches(ev) {
			t.Errorf("ledger %d: a non-factory emitter of (\"create\",\"liquidity_pool\") matches", r.LedgerSeq)
			_, _ = dec.Decode(ev)
		}
	}
	if hooked != 0 {
		t.Errorf("a non-factory emitter seeded the registry %d time(s)", hooked)
	}
}

// repoRoot lives in replay_backfillsafe_test.go (untagged), which both
// builds compile.

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), rel)) //nolint:gosec // repo-relative, test-only
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}
