package config_test

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// The promtail journal drop filter is a single-quoted YAML scalar with no backslash escaping, so its bytes are the RE2
// regex. A `\\.` pattern needs a literal backslash in the unit name, matching nothing, so noisy units (tmpfiles-clean,
// logind, cron) reach Loki.
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
