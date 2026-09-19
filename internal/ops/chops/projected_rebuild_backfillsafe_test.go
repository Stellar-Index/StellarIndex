// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

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

// F050, third leg: projected-rebuild builds the live projector's CURRENT
// decoder and runs it over a historical lake range with a winning
// derive_generation — the bulk sibling the projector-replay runbook sends
// any rewind over ~1M ledgers to — and it never consulted the
// BackfillSafe gate its two siblings now ask.
//
// These tests drive the real subcommand entry point. The config path does
// not exist, so a run that gets PAST the gate fails at the config load;
// which of the two errors comes back is therefore exactly "did the gate
// fire".
func projectedRebuildArgs(t *testing.T, source string, extra ...string) []string {
	t.Helper()
	missing := filepath.Join(t.TempDir(), "absent.toml")
	return append([]string{"-config", missing, "-source", source, "-from", "52728375", "-to", "52800000"}, extra...)
}

func TestProjectedRebuild_RefusesSourceThatIsNotBackfillSafe(t *testing.T) {
	t.Parallel()
	// The registry is the authority; if the audit lands and this flips,
	// the test must be re-pointed at another unaudited source, not deleted.
	if external.BackfillSafe(sushiswap_v3.SourceName) {
		t.Fatalf("%s is now BackfillSafe — pick a source that is still unaudited for this test", sushiswap_v3.SourceName)
	}
	// The default is a dry-run; -write is the destructive mode. Both are
	// refused (see checkProjectedRebuildBackfillSafe for why the preview
	// is gated too).
	for _, extra := range [][]string{nil, {"-write"}, {"-write", "-allow-live-overlap"}} {
		err := projectedRebuild(projectedRebuildArgs(t, sushiswap_v3.SourceName, extra...))
		if err == nil {
			t.Fatalf("args %v: rebuild of an unaudited source returned nil", extra)
		}
		if !strings.Contains(err.Error(), "not BackfillSafe") {
			t.Errorf("args %v: projected-rebuild of %s got past the BackfillSafe gate (F050); error was: %v",
				extra, sushiswap_v3.SourceName, err)
		}
		if !strings.Contains(err.Error(), `"`+sushiswap_v3.SourceName+`"`) {
			t.Errorf("args %v: refusal does not name the source: %v", extra, err)
		}
	}
}

func TestProjectedRebuild_RefusesUnknownSourceName(t *testing.T) {
	t.Parallel()
	// The gap detector's hyphenated per-table name — not a projector source.
	err := projectedRebuild(projectedRebuildArgs(t, "blend-backstop", "-write"))
	if err == nil || !strings.Contains(err.Error(), "not BackfillSafe") {
		t.Fatalf("unknown source must be refused fail-closed at the gate, got: %v", err)
	}
	if !strings.Contains(err.Error(), "underscored") {
		t.Errorf("refusal lost the naming hint a typo needs: %v", err)
	}
}

// The destructive branch has a twin: the gate must NOT strand the
// sanctioned rebuilds. Every name here has to get past it and then fail
// on the absent config, proving the run continued. blend_backstop and
// the sep41 pair deliberately have no external.Registry row.
func TestProjectedRebuild_AuditedAndRegistrylessSourcesPassTheGate(t *testing.T) {
	t.Parallel()
	for _, source := range []string{
		"aquarius", // the finding's own scenario: audited, rebuild allowed
		"blend",
		blend_backstop.SourceName,
		sep41supply.SourceName,
		sep41transfers.SourceName,
	} {
		err := projectedRebuild(projectedRebuildArgs(t, source, "-write"))
		if err == nil {
			t.Fatalf("%s: expected the absent config to fail the run", source)
		}
		if strings.Contains(err.Error(), "not BackfillSafe") {
			t.Errorf("%s: a sanctioned rebuild is refused by the BackfillSafe gate: %v", source, err)
		}
		if !strings.Contains(err.Error(), "absent.toml") {
			t.Errorf("%s: expected to reach the config load, got: %v", source, err)
		}
	}
}

// blend_backstop's rebuild-safety is `blend`'s attestation
// (docs/operations/wasm-audits/blend.md) and must fall with it on this
// path exactly as it does on projector-replay.
func TestProjectedRebuild_BackstopFollowsBlendAttestation(t *testing.T) {
	// Not parallel: mutates the package-level registry.
	orig := external.Registry["blend"]
	t.Cleanup(func() { external.Registry["blend"] = orig })

	if err := checkProjectedRebuildBackfillSafe(blend_backstop.SourceName, 51_500_000); err != nil {
		t.Fatalf("%s must be rebuildable while blend is attested: %v", blend_backstop.SourceName, err)
	}
	withdrawn := orig
	withdrawn.BackfillSafe = false
	external.Registry["blend"] = withdrawn
	if err := checkProjectedRebuildBackfillSafe(blend_backstop.SourceName, 51_500_000); err == nil {
		t.Errorf("rebuild of %s must be refused once blend is not BackfillSafe", blend_backstop.SourceName)
	}
}
