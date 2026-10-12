// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// A bare `#N` reads as Stellar-Index/StellarIndex issue N (a private task number once resolved to an unrelated dependency
// bump), so any tracker number here must name its tracker ("Task #52").
var bareTrackerRef = regexp.MustCompile(`(^|[^\w/&])#[0-9]+\b`)

func TestSourcesCiteNoBareTrackerNumbers(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no Go sources found; the scan did not run")
	}
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, loc := range bareTrackerRef.FindAllIndex(src, -1) {
			hash := loc[0] + bytes.IndexByte(src[loc[0]:loc[1]], '#')
			if bytes.HasSuffix(src[:hash], []byte("Task ")) {
				continue
			}
			t.Errorf("%s: bare tracker reference %q; name the tracker or drop it", f, src[hash:loc[1]])
		}
	}
}
