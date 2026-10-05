package main

import "testing"

func TestConfigPathFromArgs(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"-config", "/a.toml", "-x"}, "/a.toml"},
		{[]string{"--config=/b.toml"}, "/b.toml"},
		{[]string{"-from", "config"}, ""},
		{[]string{"-config"}, ""},
		{nil, ""},
	}
	for _, c := range cases {
		if got := configPathFromArgs(c.args); got != c.want {
			t.Errorf("configPathFromArgs(%v) = %q, want %q", c.args, got, c.want)
		}
	}
}
