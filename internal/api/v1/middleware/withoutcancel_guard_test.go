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

// withoutCancelAllowlist is every (file, line-content) pair permitted to
// call context.WithoutCancel in this package. GH-627: TouchUsage and
// UsageTracker used to detach their post-response bookkeeping from the
// request's cancellation with context.WithoutCancel(r.Context()) plus a
// flat timeout, run INLINE on the request goroutine before the response
// was ever flushed — so a wedged Redis added up to 10 s to a request
// entirely outside api.request_timeout. The fix ([AfterResponse]) hands
// that work to a separate goroutine under context.Background() instead,
// so neither file needs WithoutCancel at all any more.
//
// throttleContext (ratelimit.go) is a DIFFERENT, legitimate use: a
// PRE-handler seam that detaches RateLimit's/MonthlyQuota's dwell-clock
// take from a client abort while re-applying the request's deadline —
// see its doc comment. It is the only entry that may remain.
//
// This allowlist may only SHRINK. Add a new context.WithoutCancel call
// under this package and this test fails — that is the point: the next
// "just detach it and bound it separately" post-response shortcut is
// exactly the shape of the defect GH-627 fixed.
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
