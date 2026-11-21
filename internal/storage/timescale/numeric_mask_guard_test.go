// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A fixed-width numeric to_char mask such as 'FM999999990.00'. Postgres renders a value wider than the
// mask as '#' characters, so a served number would silently become "#########.##".
var numericToCharMask = regexp.MustCompile(`'(?i:FM)?(?:[0,]*9[90,]+(?:\.[90]+)?|[90,]*9[90,]*\.[90]+)'`)

func TestNoFixedWidthNumericMask(t *testing.T) {
	for _, q := range []string{"to_char(x, 'FM999999990.00')", "to_char(x, '990.0')", "to_char(x, '999,999')"} {
		if !numericToCharMask.MatchString(q) {
			t.Errorf("mask not detected in %q", q)
		}
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range numericToCharMask.FindAllString(string(src), -1) {
			t.Errorf("%s: fixed-width numeric mask %s overflows to '#'; render with changePctText or ROUND(...)::text", f, m)
		}
	}
}
