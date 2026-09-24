package supply

import (
	"bytes"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// TestParseSupplyAuditArgs_AssetAnywhere: the asset is accepted before,
// between or after the flags. Go's flag package stops at the first
// positional, so the asset-first form both triage runbooks prescribe
// parsed no flags and failed with "-config is required" (#1177).
func TestParseSupplyAuditArgs_AssetAnywhere(t *testing.T) {
	t.Parallel()
	want := supplyAuditArgs{cfgPath: "/etc/stellarindex.toml", crossCheck: "CCW6", asset: "USDC-GA5Z", historyHours: 24}
	for name, args := range map[string][]string{
		"asset first":  {"USDC-GA5Z", "-config", "/etc/stellarindex.toml", "-cross-check", "CCW6", "-history-hours", "24"},
		"asset last":   {"-config", "/etc/stellarindex.toml", "-cross-check", "CCW6", "-history-hours", "24", "USDC-GA5Z"},
		"asset middle": {"-config", "/etc/stellarindex.toml", "USDC-GA5Z", "-cross-check", "CCW6", "-history-hours", "24"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := parseSupplyAuditArgs(args)
			if err != nil {
				t.Fatalf("parseSupplyAuditArgs(%q): %v", args, err)
			}
			if got != want {
				t.Fatalf("parseSupplyAuditArgs(%q) = %+v, want %+v", args, got, want)
			}
		})
	}
}

func TestParseSupplyAuditArgs_Rejects(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		args    []string
		wantErr string
	}{
		"no asset":         {[]string{"-config", "c.toml"}, "usage: supply audit"},
		"two assets":       {[]string{"native", "-config", "c.toml", "USDC-GA5Z"}, "got 2 positional"},
		"no config":        {[]string{"native", "-history-hours", "24"}, "-config is required"},
		"negative history": {[]string{"native", "-config", "c.toml", "-history-hours", "-1"}, "-history-hours must be"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := parseSupplyAuditArgs(tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("parseSupplyAuditArgs(%q) err = %v, want %q", tc.args, err, tc.wantErr)
			}
		})
	}
}

var supplyAuditInvocationRE = regexp.MustCompile(`stellarindex-ops supply audit ([^\n` + "`" + `|;]*)`)

// TestDocumentedSupplyAuditInvocationsParse runs every `stellarindex-ops
// supply audit` line under docs/ through the parser, so a documented form
// the handler rejects fails here instead of mid-page.
func TestDocumentedSupplyAuditInvocationsParse(t *testing.T) {
	t.Parallel()
	docs := filepath.Join("..", "..", "..", "docs")
	var checked int
	err := filepath.WalkDir(docs, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".md" || strings.Contains(path, string(filepath.Separator)+"archive"+string(filepath.Separator)) {
			return err
		}
		body, err := os.ReadFile(path) //nolint:gosec // repo-relative, test-only
		if err != nil {
			return err
		}
		joined := regexp.MustCompile(`\\\n[ \t]*`).ReplaceAllString(string(body), " ")
		for _, m := range supplyAuditInvocationRE.FindAllStringSubmatch(joined, -1) {
			checked++
			if _, err := parseSupplyAuditArgs(strings.Fields(m[1])); err != nil {
				t.Errorf("%s: `stellarindex-ops supply audit %s` does not parse: %v", path, m[1], err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk docs: %v", err)
	}
	if checked < 3 {
		t.Fatalf("checked %d documented invocation(s); the runbooks carry at least 3, so the scan is broken", checked)
	}
}

// TestCrossCheckNextActionParses: the next-action line an over-tolerance
// cross-check prints must be a command the handler accepts.
func TestCrossCheckNextActionParses(t *testing.T) {
	t.Parallel()
	classic := supply.Supply{AssetKey: "classic", TotalSupply: big.NewInt(1_000), SACWrappedStroops: big.NewInt(500)}
	sac := supply.Supply{AssetKey: "sac", TotalSupply: big.NewInt(400)}
	result, err := supply.CrossCheckForClass(classic, sac, supply.WrapClassPartial)
	if err != nil {
		t.Fatalf("CrossCheckForClass: %v", err)
	}
	var out bytes.Buffer
	_ = reportCrossCheck(&out, result, "classic")
	m := regexp.MustCompile(`next action: +stellarindex-ops supply audit (.*?) to identify`).FindStringSubmatch(out.String())
	if m == nil {
		t.Fatalf("no supply audit next action in:\n%s", out.String())
	}
	args := strings.Fields(strings.ReplaceAll(m[1], "<asset>", "native"))
	if _, err := parseSupplyAuditArgs(args); err != nil {
		t.Fatalf("next action `supply audit %s` does not parse: %v", m[1], err)
	}
}
