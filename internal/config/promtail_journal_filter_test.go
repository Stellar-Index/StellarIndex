package config_test

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// promtail-config.yaml.j2's journal drop filter is a single-quoted YAML
// scalar: YAML single-quote scalars do no backslash-escape processing at
// all, so whatever bytes sit between the quotes are exactly what
// promtail's RE2 regex compiles. A pattern authored with `\\.` (two
// backslash characters) therefore requires a literal backslash before
// "service" in the unit name, which never appears — the filter matches
// nothing and every one of the "low signal:noise" units it names
// (systemd-tmpfiles-clean.service, systemd-logind.service, cron.service)
// still reaches Loki.
var promtailDropFilterPattern = regexp.MustCompile(`(?m)^\s*regex:\s*'([^']*)'\s*$`)

func readPromtailDropFilter(t *testing.T) *regexp.Regexp {
	t.Helper()
	path := filepath.Join("..", "..", "configs", "ansible", "roles", "loki", "templates", "promtail-config.yaml.j2")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	m := promtailDropFilterPattern.FindStringSubmatch(string(src))
	if m == nil {
		t.Fatalf("%s: no single-quoted `regex:` scalar found for the journal drop filter", path)
	}
	// YAML single-quote scalars perform no escape processing: the
	// pattern RE2 receives is exactly the bytes between the quotes.
	re, err := regexp.Compile(m[1])
	if err != nil {
		t.Fatalf("journal drop filter %q does not compile as RE2: %v", m[1], err)
	}
	return re
}

func TestPromtailJournalDropFilterMatchesNamedNoiseUnits(t *testing.T) {
	re := readPromtailDropFilter(t)

	units := []string{
		"systemd-tmpfiles-clean.service",
		"systemd-logind.service",
		"cron.service",
	}
	for _, unit := range units {
		if !re.MatchString(unit) {
			t.Errorf("journal drop filter %q does not match %q — it will reach Loki instead of being dropped", re.String(), unit)
		}
	}
}
