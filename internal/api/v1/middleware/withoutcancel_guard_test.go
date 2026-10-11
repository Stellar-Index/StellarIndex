// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package middleware_test

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withoutCancelAllowlist is every (file, line-content) pair permitted to call
// context.WithoutCancel in this package. TouchUsage and UsageTracker must not detach
// post-response bookkeeping with context.WithoutCancel(r.Context()) plus a flat timeout run
// INLINE on the request goroutine: a wedged Redis would add up to 10 s to a request outside
// api.request_timeout. [AfterResponse] instead hands that work to a separate goroutine under
// context.Background(), so neither file needs WithoutCancel.
//
// throttleContext (ratelimit.go) is a DIFFERENT, legitimate use: a PRE-handler seam that detaches
// RateLimit's/MonthlyQuota's dwell-clock take from a client abort while re-applying the request's
// deadline. It is the only entry that may remain.
//
// This allowlist may only SHRINK: a new context.WithoutCancel call here fails the test, so the
// next "detach it and bound it separately" shortcut is caught.
var withoutCancelAllowlist = map[string]bool{
	"ratelimit.go:\treturn context.WithDeadline(context.WithoutCancel(r.Context()), deadline)": true,
}

func TestNoNewContextWithoutCancel(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		scanFileForWithoutCancel(t, name)
	}
}

func scanFileForWithoutCancel(t *testing.T, name string) {
	t.Helper()
	f, err := os.Open(filepath.Join(".", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue // doc comments are allowed to name the pattern they forbid
		}
		if !strings.Contains(line, "context.WithoutCancel(") {
			continue
		}
		key := name + ":" + line
		if !withoutCancelAllowlist[key] {
			t.Errorf("%s: new context.WithoutCancel call not on the allowlist: %q\n"+
				"post-response bookkeeping must use middleware.AfterResponse + "+
				"context.Background(), never detach-and-bound-separately (GH-627); "+
				"a genuinely new pre-handler use belongs on withoutCancelAllowlist "+
				"with the same justification throttleContext carries", name, line)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
}
