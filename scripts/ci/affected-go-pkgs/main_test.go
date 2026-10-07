package main

import (
	"reflect"
	"testing"
)

func TestSameTokens(t *testing.T) {
	const base = "package p\n\n// Doc.\nfunc F() int { return 1 } // trailing\n"
	cases := []struct {
		name string
		b    string
		want bool
	}{
		{"comment reworded", "package p\n\n// Other doc.\nfunc F() int { return 1 }\n", true},
		{"comment deleted", "package p\n\nfunc F() int { return 1 }\n", true},
		{"block comment added", "package p\n\n/* a\nb */\nfunc F() int { return 1 }\n", true},
		{"code changed", "package p\n\n// Doc.\nfunc F() int { return 2 } // trailing\n", false},
		{"directive added", "package p\n\n// Doc.\n//go:noinline\nfunc F() int { return 1 } // trailing\n", false},
		{"build constraint added", "//go:build linux\n\npackage p\n\n// Doc.\nfunc F() int { return 1 } // trailing\n", false},
		{"example output changed", "package p\n\n// Output: 2\nfunc F() int { return 1 }\n", false},
		{"unparsable", "package p\n\nfunc F() int { return \"1 }\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sameTokens([]byte(base), []byte(c.b)); got != c.want {
				t.Fatalf("sameTokens = %v, want %v", got, c.want)
			}
		})
	}
}

func TestSameTokensCgoNeedsByteEquality(t *testing.T) {
	a := "package p\n\n// #include <a.h>\nimport \"C\"\n"
	b := "package p\n\n// #include <b.h>\nimport \"C\"\n"
	if sameTokens([]byte(a), []byte(b)) {
		t.Fatal("a cgo preamble change must count as a code change")
	}
}

func TestAffected(t *testing.T) {
	list := `m/a
m/b m/a
m/b [m/b.test] m/a m/x
m/b.test m/b [m/b.test] testing
m/c_test [m/c.test] m/b m/a
m/c
m/d m/x`
	got := affected(list, map[string]bool{"m/a": true})
	want := []string{"m/a", "m/b", "m/c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("affected = %v, want %v", got, want)
	}
}
