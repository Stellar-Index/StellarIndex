package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestConfigPathFromArgs(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"-config", "/a.toml", "-x"}, "/a.toml"},
		{[]string{"--config=/b.toml"}, "/b.toml"},
		{[]string{"-config=/c.toml"}, "/c.toml"},
		{[]string{"--config", "/d.toml"}, "/d.toml"},
		{[]string{"-config", "/a.toml", "-config", "/z.toml"}, "/z.toml"},
		{[]string{"-config=/a.toml", "--config", "/z.toml"}, "/z.toml"},
		{[]string{"-source", "x", "-config", "/e.toml"}, "/e.toml"},
		{[]string{"pos", "-config", "/f.toml"}, ""},
		{[]string{"-config", "/a.toml", "pos", "-config", "/f.toml"}, "/a.toml"},
		{[]string{"-config", "/a.toml", "--", "-config", "/f.toml"}, "/a.toml"},
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

func TestInstallNetworkFromArgs_FailsClosed(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.toml")
	cfg := "[supply.sac_wrappers]\nnot-a-strkey = \"x\"\n"
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if code := installNetworkFromArgs([]string{"-config", p}, &stderr); code == 0 {
		t.Fatalf("exit 0 on malformed sac_wrappers; stderr=%q", stderr.String())
	}
}
