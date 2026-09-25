package controlwiring

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// ─── GH-652: docker/verify/Dockerfile's base images must be digest-pinned ───
// (5ed7ac728 pinned the six stellarindex-*.Dockerfiles by digest but left
// docker/verify/Dockerfile — the image `make prepush` runs in — on a bare
// tag, and Dependabot's docker ecosystem cannot rewrite a digest that was
// never there). A re-tagged or tampered upstream tag would otherwise change
// what `make prepush` compiles with, with no repo diff. This test is the
// guard that catches the next unpinned FROM instead of relying on review.
var verifyDockerfileFromRe = regexp.MustCompile(`(?m)^FROM\s+(\S+)`)

func TestGH652_VerifyDockerfileFromLinesArePinnedByDigest(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)

	path := filepath.Join(root, "docker", "verify", "Dockerfile")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	matches := verifyDockerfileFromRe.FindAllStringSubmatch(string(body), -1)
	if len(matches) == 0 {
		t.Fatalf("%s: no FROM lines found", path)
	}

	for _, m := range matches {
		ref := m[1]
		if !hasDigestPin(ref) {
			t.Errorf("%s: FROM %s is not pinned by digest (want image:tag@sha256:<digest>)", path, ref)
		}
	}
}

func hasDigestPin(ref string) bool {
	return regexp.MustCompile(`@sha256:[0-9a-f]{64}$`).MatchString(ref)
}
