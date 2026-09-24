package controlwiring

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// ─── T521: the Dockerfiles' golang base image must match go.mod's `go` ───
// directive, exactly one minor apart from a prior drift (F-1240, codex
// audit-2026-05-12): the Dockerfiles floated ahead of go.mod because
// Dependabot's docker ecosystem bumps `docker/*.Dockerfile` independently
// of whatever bumps go.mod. docker/README.md documents the invariant; this
// test is the guard that catches the next drift instead of relying on a
// human to re-read the README.

var (
	goModVersionRe   = regexp.MustCompile(`(?m)^go (\d+)\.(\d+)`)
	dockerfileFromRe = regexp.MustCompile(`(?m)^FROM golang:(\d+)\.(\d+)`)
)

func TestT521_DockerfileGoVersionMatchesGoMod(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)

	modBody, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	m := goModVersionRe.FindStringSubmatch(string(modBody))
	if m == nil {
		t.Fatal("go.mod has no `go X.Y` directive")
	}
	wantMajorMinor := m[1] + "." + m[2]

	files, err := filepath.Glob(filepath.Join(root, "docker", "stellarindex-*.Dockerfile"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no docker/stellarindex-*.Dockerfile found (err=%v)", err)
	}

	for _, df := range files {
		body, err := os.ReadFile(df)
		if err != nil {
			t.Fatalf("read %s: %v", df, err)
		}
		fm := dockerfileFromRe.FindStringSubmatch(string(body))
		if fm == nil {
			t.Errorf("%s: no `FROM golang:X.Y-alpine` builder stage found", df)
			continue
		}
		gotMajorMinor := fm[1] + "." + fm[2]
		if gotMajorMinor != wantMajorMinor {
			t.Errorf("%s: builder is golang:%s but go.mod's go directive is %s (F-1240 drift, see docker/README.md)",
				df, gotMajorMinor, wantMajorMinor)
		}
	}
}
