// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package ratelimit_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
)

func TestThrottleIPKey(t *testing.T) {
	cases := []struct {
		name string
		ip   string
		want string
	}{
		{"ipv4 unchanged", "203.0.113.7", "203.0.113.7"},
		{"ipv4-in-ipv6 unmapped to the ipv4 spelling", "::ffff:203.0.113.7", "203.0.113.7"},
		{"ipv6 masked to /64", "2001:db8:1234:5678::1", "2001:db8:1234:5678::"},
		{"ipv6 same /64, other host bits", "2001:db8:1234:5678:ffff:ffff:ffff:ffff", "2001:db8:1234:5678::"},
		{"ipv6 different /64", "2001:db8:1234:9999::1", "2001:db8:1234:9999::"},
		{"zoned ipv6 loses its zone", "fe80::1%eth0", "fe80::"},
		{"invalid input passed through", "not-an-ip", "not-an-ip"},
		{"empty passed through", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ratelimit.ThrottleIPKey(tc.ip); got != tc.want {
				t.Errorf("ThrottleIPKey(%q) = %q, want %q", tc.ip, got, tc.want)
			}
		})
	}
	if a, b := ratelimit.ThrottleIPKey("198.51.100.9"), ratelimit.ThrottleIPKey("::ffff:198.51.100.9"); a != b {
		t.Fatalf("two spellings of one IPv4 address produced two throttle keys: %q vs %q", a, b)
	}
}

// ipv6ThrottleMask matches a /64 prefix mask, the shape every hand-rolled
// copy of the throttle identity has taken.
var ipv6ThrottleMask = regexp.MustCompile(`netip\.PrefixFrom\(.*,\s*64\)`)

// TestThrottleIPMaskHasOneDefinition fails when production code outside
// this file derives a /64 throttle identity itself. Four copies once
// disagreed on whether "::ffff:1.2.3.4" and "1.2.3.4" were one caller.
func TestThrottleIPMaskHasOneDefinition(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	var offenders []string
	scanned := 0
	for _, top := range []string{"cmd", "internal", "pkg"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if rel == "internal/ratelimit/throttlekey.go" {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			scanned++
			if ipv6ThrottleMask.Match(src) {
				offenders = append(offenders, rel)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", top, err)
		}
	}
	if scanned < 100 {
		t.Fatalf("scanned only %d Go files — the walk is not reaching the tree", scanned)
	}
	if len(offenders) > 0 {
		t.Errorf("per-IP throttle identity re-derived outside ratelimit.ThrottleIPKey in %v — call it instead", offenders)
	}
}
