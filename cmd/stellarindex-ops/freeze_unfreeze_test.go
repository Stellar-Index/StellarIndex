package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type recordingClearer struct {
	calls []string
	err   error
}

func (c *recordingClearer) Clear(_ context.Context, asset, quote canonical.Asset) error {
	c.calls = append(c.calls, asset.String()+"/"+quote.String())
	return c.err
}

type recordingRecoverer struct {
	calls []string
	err   error
}

func (r *recordingRecoverer) MarkRecovered(_ context.Context, asset, quote canonical.Asset) error {
	r.calls = append(r.calls, asset.String()+"/"+quote.String())
	return r.err
}

func testPair(t *testing.T) (canonical.Asset, canonical.Asset) {
	t.Helper()
	asset, err := canonical.ParseAsset("native")
	if err != nil {
		t.Fatalf("parse native: %v", err)
	}
	quote, err := canonical.ParseAsset("USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("parse quote: %v", err)
	}
	return asset, quote
}

// TestUnfreezePair_ClearsMarkerAndClosesDurableRow pins the whole point of
// the subcommand: ADR-0019 says an escalated freeze "stays active until
// manual unfreeze", and manual unfreeze has to do BOTH halves — the Redis
// marker (the serving path's authority for flags.frozen) and the
// freeze_events row (the explorer's durable timeline). Doing only one leaves
// the two surfaces disagreeing about whether the pair is frozen.
func TestUnfreezePair_ClearsMarkerAndClosesDurableRow(t *testing.T) {
	asset, quote := testPair(t)
	clearer := &recordingClearer{}
	recoverer := &recordingRecoverer{}

	if err := unfreezePair(context.Background(), recoverer, clearer, asset, quote, "oracle recovered", false); err != nil {
		t.Fatalf("unfreezePair: %v", err)
	}
	want := asset.String() + "/" + quote.String()
	if len(clearer.calls) != 1 || clearer.calls[0] != want {
		t.Errorf("redis marker clear calls = %v, want exactly [%s]", clearer.calls, want)
	}
	if len(recoverer.calls) != 1 || recoverer.calls[0] != want {
		t.Errorf("MarkRecovered calls = %v, want exactly [%s]", recoverer.calls, want)
	}
}

// TestUnfreezePair_MarkerFirstAndNothingElseOnFailure — if the Redis clear
// fails, the pair is STILL FROZEN on the serving path, so the durable row
// must not be closed. Closing it would publish a "recovered" timeline for a
// price that is still being withheld: the dishonest direction.
func TestUnfreezePair_MarkerFirstAndNothingElseOnFailure(t *testing.T) {
	asset, quote := testPair(t)
	clearer := &recordingClearer{err: errors.New("redis: connection refused")}
	recoverer := &recordingRecoverer{}

	err := unfreezePair(context.Background(), recoverer, clearer, asset, quote, "why", false)
	if err == nil {
		t.Fatal("a failed Redis clear must fail the run — the pair is still frozen")
	}
	if len(recoverer.calls) != 0 {
		t.Errorf("MarkRecovered must NOT run when the marker clear failed, got %v — that would close the timeline row for a pair the serving path still reports as frozen", recoverer.calls)
	}
	if !strings.Contains(err.Error(), "NOTHING was changed") {
		t.Errorf("the error must tell the operator no state changed, got: %v", err)
	}
}

// TestUnfreezePair_AlreadyClosedRowIsNotAFailure — the recovery worker polls
// every 60s and may have closed the row already, and a repeat unfreeze is a
// legitimate operator action. Neither is an error: the end state is the
// intended one.
func TestUnfreezePair_AlreadyClosedRowIsNotAFailure(t *testing.T) {
	asset, quote := testPair(t)
	clearer := &recordingClearer{}
	recoverer := &recordingRecoverer{err: timescale.ErrNotFound}

	if err := unfreezePair(context.Background(), recoverer, clearer, asset, quote, "repeat", false); err != nil {
		t.Fatalf("an already-closed freeze_events row must not fail the run: %v", err)
	}
	if len(clearer.calls) != 1 {
		t.Errorf("the marker must still be cleared: %v", clearer.calls)
	}
}

// TestUnfreezePair_DryRunTouchesNothing.
func TestUnfreezePair_DryRunTouchesNothing(t *testing.T) {
	asset, quote := testPair(t)
	clearer := &recordingClearer{}
	recoverer := &recordingRecoverer{}

	if err := unfreezePair(context.Background(), recoverer, clearer, asset, quote, "look only", true); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if len(clearer.calls) != 0 || len(recoverer.calls) != 0 {
		t.Errorf("-dry-run must touch nothing, got clear=%v recover=%v", clearer.calls, recoverer.calls)
	}
}

// TestFreezeUnfreeze_RequiresReasonForAMutation — an unfreeze overrides an
// automated safety control on a money surface. The admin API hard-400s a
// privileged write with no X-Reason; the CLI equivalent must refuse too,
// BEFORE it opens Postgres or Redis.
func TestFreezeUnfreeze_RequiresReasonForAMutation(t *testing.T) {
	err := freezeUnfreeze([]string{
		"-config", "/nonexistent/stellarindex.toml",
		"-asset", "native",
		"-quote", "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
	})
	if err == nil {
		t.Fatal("expected a refusal with no -reason")
	}
	if !strings.Contains(err.Error(), "-reason is required") {
		t.Errorf("expected the missing-reason refusal (and BEFORE any config load), got: %v", err)
	}

	// Whitespace is not a reason.
	err = freezeUnfreeze([]string{
		"-config", "/nonexistent/stellarindex.toml",
		"-asset", "native",
		"-quote", "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
		"-reason", "   ",
	})
	if err == nil || !strings.Contains(err.Error(), "-reason is required") {
		t.Errorf("a whitespace-only -reason must be refused, got: %v", err)
	}
}

// TestFreezeUnfreeze_RequiresPairOrList — the command must not fall through
// to some default target when neither a pair nor -list was given.
func TestFreezeUnfreeze_RequiresPairOrList(t *testing.T) {
	err := freezeUnfreeze([]string{"-config", "/nonexistent/stellarindex.toml"})
	if err == nil || !strings.Contains(err.Error(), "-asset and -quote are required") {
		t.Errorf("expected the missing-pair refusal, got: %v", err)
	}
}

// TestSubcommandDispatch_LeafHandlersSeeTheirFlags is the coverage that was
// missing and let every leaf subcommand ship UNINVOKABLE.
//
// The dispatch table hands a handler the FULL argv (args[0] == the verb).
// Go's flag package stops parsing at the first non-flag argument, so a leaf
// handler doing fs.Parse(args) on that argv parses NOTHING and every flag
// keeps its zero value: `stellarindex-ops usage-rollup-backfill -config
// /etc/stellarindex.toml -from 2026-07-19` answered "-config is required"
// and there was no way to run it at all. The existing unit tests all call
// the handlers DIRECTLY with flags-only argv, so they exercised the
// convention the handlers wanted and never the one the dispatcher used —
// which is precisely why nothing caught it.
//
// This test goes through `subcommands`, the way main() does. The assertion
// is deliberately "the error is NOT the no-flags-parsed symptom": each
// handler's next gate differs, but every one of them reports
// "-config is required" if and only if the flags were dropped.
func TestSubcommandDispatch_LeafHandlersSeeTheirFlags(t *testing.T) {
	cases := []struct {
		verb string
		argv []string
	}{
		{"mint-key", []string{"mint-key", "-config", "/nonexistent.toml", "-identifier", "customer-acme", "-label", "Acme"}},
		{"upgrade-key", []string{"upgrade-key", "-config", "/nonexistent.toml"}},
		{"emit-incident", []string{"emit-incident", "-config", "/nonexistent.toml", "-slug", "s", "-event", "sev1"}},
		{"usage-rollup-backfill", []string{"usage-rollup-backfill", "-config", "/nonexistent.toml", "-from", "2026-07-19"}},
		{"freeze-unfreeze", []string{"freeze-unfreeze", "-config", "/nonexistent.toml", "-list"}},
	}
	for _, tc := range cases {
		t.Run(tc.verb, func(t *testing.T) {
			run, ok := subcommands[tc.verb]
			if !ok {
				t.Fatalf("%s is not in the dispatch table", tc.verb)
			}
			err := run(tc.argv)
			if err == nil {
				t.Fatalf("%s: expected an error (the config file does not exist)", tc.verb)
			}
			if strings.Contains(err.Error(), "-config is required") {
				t.Fatalf("%s: dispatch dropped the flags — -config was passed but the handler never saw it (got %q). The subcommand is uninvokable from the CLI.", tc.verb, err)
			}
		})
	}
}
