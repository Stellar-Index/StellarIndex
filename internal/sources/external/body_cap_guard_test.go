// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package external

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// unboundedBodyRead matches a response body handed straight to a reader
// with no io.LimitReader in between.
var unboundedBodyRead = regexp.MustCompile(`(io\.ReadAll|json\.NewDecoder|xml\.NewDecoder)\(\w+\.Body\)`)

// Every third-party response read under internal/sources must be capped:
// a wedged upstream streaming inside the timeout otherwise OOMs the ingester.
func TestSourcesNeverReadUncappedResponseBody(t *testing.T) {
	var hits []string
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(src), "\n") {
			if unboundedBodyRead.MatchString(line) {
				hits = append(hits, path+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) > 0 {
		t.Fatalf("uncapped response-body reads (wrap in io.LimitReader):\n%s", strings.Join(hits, "\n"))
	}
}
