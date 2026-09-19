package ingest

import (
	"path/filepath"
	"strings"
	"testing"

	blend_backstop "github.com/Stellar-Index/StellarIndex/internal/sources/blend_backstop"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	sep41supply "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_supply"
	sep41transfers "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_transfers"
	sushiswap_v3 "github.com/Stellar-Index/StellarIndex/internal/sources/sushiswap_v3"
)

// F050: projector-replay is the documented catch-up procedure for every
// projected source, and it re-decodes history with the CURRENT decoder —
// but only `backfill` consulted external.BackfillSafe. These tests drive
// the real subcommand entry point. The config path does not exist, so a
// run that gets PAST the gate fails at "load config"; which of the two
// errors comes back is therefore exactly "did the gate fire".
func replayArgs(t *testing.T, source string, extra ...string) []string {
	t.Helper()
	missing := filepath.Join(t.TempDir(), "absent.toml")
	return append([]string{"-config", missing, "-source", source, "-from", "52728375"}, extra...)
}

func TestProjectorReplay_RefusesSourceThatIsNotBackfillSafe(t *testing.T) {
	t.Parallel()
	// The registry is the authority; if the audit lands and this flips,
	// the test must be re-pointed at another unaudited source, not deleted.
	if external.BackfillSafe(sushiswap_v3.SourceName) {
		t.Fatalf("%s is now BackfillSafe — pick a source that is still unaudited for this test", sushiswap_v3.SourceName)
	}
	for _, extra := range [][]string{nil, {"-dry-run"}} {
		err := projectorReplay(replayArgs(t, sushiswap_v3.SourceName, extra...))
		if err == nil {
			t.Fatalf("args %v: replay of an unaudited source returned nil", extra)
		}
		if !strings.Contains(err.Error(), "not BackfillSafe") {
			t.Errorf("args %v: replay of %s got past the BackfillSafe gate (F050); error was: %v",
				extra, sushiswap_v3.SourceName, err)
		}
		if !strings.Contains(err.Error(), sushiswap_v3.SourceName) {
			t.Errorf("args %v: refusal does not name the source: %v", extra, err)
		}
	}
}

func TestProjectorReplay_RefusesUnknownSourceName(t *testing.T) {
	t.Parallel()
	// The gap detector's hyphenated per-table name — not a projector source.
	err := projectorReplay(replayArgs(t, "blend-backstop"))
	if err == nil || !strings.Contains(err.Error(), "not BackfillSafe") {
		t.Fatalf("unknown source must be refused fail-closed at the gate, got: %v", err)
	}
	if !strings.Contains(err.Error(), "underscored") {
		t.Errorf("refusal lost the naming hint a typo needs: %v", err)
	}
}

// The destructive branch has a twin: the gate must NOT strand the
// sanctioned replays. Every name here has to get past it (and then fail
// on the absent config, proving the run continued).
func TestProjectorReplay_AuditedAndRegistrylessSourcesPassTheGate(t *testing.T) {
	t.Parallel()
	for _, source := range []string{
		"aquarius", // the finding's own scenario: audited, replay allowed
		blend_backstop.SourceName,
		sep41supply.SourceName,
		sep41transfers.SourceName,
	} {
		err := projectorReplay(replayArgs(t, source))
		if err == nil {
			t.Fatalf("%s: expected the absent config to fail the run", source)
		}
		if strings.Contains(err.Error(), "not BackfillSafe") {
			t.Errorf("%s: a sanctioned replay is refused by the BackfillSafe gate: %v", source, err)
		}
		if !strings.Contains(err.Error(), "load config") {
			t.Errorf("%s: expected to reach the config load, got: %v", source, err)
		}
	}
}

// blend_backstop has no registry row of its own: its replay-safety is
// `blend`'s attestation (docs/operations/wasm-audits/blend.md) and must
// fall with it.
func TestReplayBackfillSafe_BackstopFollowsBlendAttestation(t *testing.T) {
	// Not parallel: mutates the package-level registry.
	orig := external.Registry["blend"]
	t.Cleanup(func() { external.Registry["blend"] = orig })

	if !external.ReplayBackfillSafe(blend_backstop.SourceName) {
		t.Fatalf("%s must be replay-safe while blend is attested", blend_backstop.SourceName)
	}
	withdrawn := orig
	withdrawn.BackfillSafe = false
	external.Registry["blend"] = withdrawn
	if external.ReplayBackfillSafe(blend_backstop.SourceName) {
		t.Errorf("%s stayed replay-safe after blend's attestation was withdrawn", blend_backstop.SourceName)
	}
	if err := checkReplayBackfillSafe(blend_backstop.SourceName, 55_000_000); err == nil {
		t.Errorf("replay of %s must be refused once blend is not BackfillSafe", blend_backstop.SourceName)
	}
}
