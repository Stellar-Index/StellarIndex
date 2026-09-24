package blend

import (
	"os"
	"regexp"
	"strconv"
	"testing"
)

var replayFromRE = regexp.MustCompile(`-source blend -from (\d+)`)

// TestDocs_ReplayFromIsFactoryGenesis pins every documented blend replay
// command to FactoryGenesisLedger: a page quoting a later ledger starts
// the replay after events the source must re-derive.
func TestDocs_ReplayFromIsFactoryGenesis(t *testing.T) {
	for _, path := range []string{"../../../docs/protocols/blend.md", "README.md"} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		matches := replayFromRE.FindAllStringSubmatch(string(body), -1)
		if len(matches) == 0 {
			t.Errorf("%s: no `-source blend -from N` replay command found", path)
		}
		for _, m := range matches {
			got, err := strconv.ParseUint(m[1], 10, 32)
			if err != nil {
				t.Fatalf("%s: parse %q: %v", path, m[1], err)
			}
			if uint32(got) != FactoryGenesisLedger {
				t.Errorf("%s: replay starts at %d, FactoryGenesisLedger is %d", path, got, FactoryGenesisLedger)
			}
		}
	}
}
