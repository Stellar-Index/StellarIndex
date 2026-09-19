//go:build k023evidence

package controlwiring

import (
	"testing"

	"gopkg.in/yaml.v3"
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
// this package so it guards the default suite — F144, F050, F048 and
// F085 have (verify_archive_fail_on_missed_test.go,
// replay_backfillsafe_test.go, phoenix_factory_admission_test.go,
// explorer_build_guards_test.go), leaving F133 below as the one leg that
// is still red. When it is settled, every leg is guarded from an untagged
// file and this one goes away with its tag.

// ─── F144: -fail-on-missed on the units where it can fire ──────────
//
// GRADUATED. The tier-b units in both trees now pass -fail-on-missed
// after `stellarindex-ops verify-archive`, and the binary no longer
// counts a checkpoint the mirror's fill job has not reached as missing
// archive data, so the flag states an invariant that can hold:
// TestK023_VerifyArchiveCheckpointUnitsFailOnMissed and
// TestVerifyArchiveBinaryArgs_WrapperPrefixIsNotTheBinary live in
// verify_archive_fail_on_missed_test.go and run in the default suite.
// They still show up in the tagged run above, because that file has no
// tag — as do the argv helpers and readRepoFile, which moved there with
// them.

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

// ─── F085: the Cloudflare build must run the export guards ─────
//
// GRADUATED. web/explorer/package.json's postbuild chain now runs the
// `__next.*` segment prune (keeping `__next._tree.txt`),
// scripts/ci/explorer-file-budget.sh and scripts/ci/explorer-seo-lint.sh,
// so every invoker of `pnpm build` runs them — including the Cloudflare
// Pages repository integration, the production publisher, which ran none
// of them. TestK023_ExplorerBuildRunsExportGuards and
// TestK023_ExplorerPruneKeepsTreeSegmentFiles live in
// explorer_build_guards_test.go and run in the default suite. They still
// show up in the tagged run above, because that file has no tag.

// ─── F050: the replay paths must consult BackfillSafe ──────────────
//
// GRADUATED. All three re-derive paths (projector-replay, ch-rebuild,
// projected-rebuild) now ask external.ReplayBackfillSafe, so this leg
// left the build tag: TestK023_ReplayPathsConsultBackfillSafe lives in
// replay_backfillsafe_test.go and runs in the default suite. It still
// shows up in the tagged run above, because that file has no tag.

// ─── F048: phoenix's factory anchor must be able to admit a pool ───
//
// GRADUATED. The phoenix decoder now classifies the factory's
// ("create","liquidity_pool") announcement and Seeds the pool it names,
// gated on reg.IsFactory, so the configured factory anchor and the
// seed-protocol-contracts walk can finally admit a pool. Both legs left
// the build tag: TestK023_PhoenixFactoryCreateEventIsAdmissible and
// TestK023_PhoenixFactoryCreateFromForeignEmitterIsNotAdmitted live in
// phoenix_factory_admission_test.go and run in the default suite, on the
// real lake captures. They still show up in the tagged run above,
// because that file has no tag.

// readRepoFile lives in verify_archive_fail_on_missed_test.go and
// repoRoot in replay_backfillsafe_test.go — both untagged, so both
// builds compile them.
