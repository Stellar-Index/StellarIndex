package controlwiring

import (
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
)

// lint-replay-plan.sh derives the projected set by parsing source_spec.go
// with awk, so a spec written in a shape the parser does not know would
// silently drop out of the gate. Compare it with the registry itself.
func TestReplayPlanGateProjectedSetMatchesRegistry(t *testing.T) {
	var want []string
	for _, s := range pipeline.Specs() {
		if s.Projector != nil {
			want = append(want, s.Name)
		}
	}
	slices.Sort(want)

	root := repoRoot(t)
	cmd := exec.Command("bash", filepath.Join(root, "scripts/ci/lint-replay-plan.sh"), "--list-projected")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("lint-replay-plan.sh --list-projected: %v\n%s", err, out)
	}
	got := strings.Fields(string(out))
	slices.Sort(got)

	if !slices.Equal(got, want) {
		t.Errorf("lint-replay-plan.sh projected set differs from pipeline.Specs():\n gate:     %v\n registry: %v", got, want)
	}
}
