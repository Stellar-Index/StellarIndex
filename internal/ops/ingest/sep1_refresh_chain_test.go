package ingest

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// sep1ChainStub answers the live home_domain re-read from a fixed map.
type sep1ChainStub struct {
	live map[string]clickhouse.AccountAuthFlags
	err  error
}

func (s sep1ChainStub) BulkAccountAuthFlags(_ context.Context, keys []string) (map[string]clickhouse.AccountAuthFlags, error) {
	if s.err != nil {
		return nil, s.err
	}
	out := make(map[string]clickhouse.AccountAuthFlags, len(keys))
	for _, k := range keys {
		if f, ok := s.live[k]; ok {
			out[k] = f
		}
	}
	return out, nil
}

// sep1ChainFor is a chain that still declares every candidate's stored domain.
func sep1ChainFor(cands ...timescale.IssuerSep1Candidate) sep1ChainStub {
	live := make(map[string]clickhouse.AccountAuthFlags, len(cands))
	for _, c := range cands {
		live[c.GStrkey] = clickhouse.AccountAuthFlags{HomeDomain: c.HomeDomain, Source: clickhouse.AuthFlagsSourceLive, AsOfLedger: 1}
	}
	return sep1ChainStub{live: live}
}

// tomlListing is a stellar.toml whose [[CURRENCIES]] lists issuer back, i.e.
// one that passes the bidirectional org check for it.
func tomlListing(org, issuer string) string {
	return fmt.Sprintf("VERSION = \"2.0.0\"\n\n[DOCUMENTATION]\nORG_NAME = %q\n\n[[CURRENCIES]]\ncode = \"USD\"\nissuer = %q\n", org, issuer)
}

// TestSep1RefreshRebindsToTheChainsDomainBeforeFetching: a stale stored domain is re-bound to the chain's declared domain and only that one is fetched.
func TestSep1RefreshRebindsToTheChainsDomainBeforeFetching(t *testing.T) {
	const issuer = "GMOVED"
	log := &sep1CallLog{}
	storedSrv, stored := sep1TestDomain(t, log, "stored", tomlListing("Stored Domain Org", issuer))
	_, current := sep1TestDomain(t, log, "current", tomlListing("Current Org", issuer))
	chain := sep1ChainStub{live: map[string]clickhouse.AccountAuthFlags{
		issuer: {HomeDomain: current, Source: clickhouse.AuthFlagsSourceLive, AsOfLedger: 64228661},
	}}

	ok, failed, _ := sep1RefreshLoop(context.Background(), log, chain, sep1TestResolver(storedSrv),
		[]timescale.IssuerSep1Candidate{{GStrkey: issuer, HomeDomain: stored}}, false)
	if ok != 1 || len(failed) != 0 {
		t.Fatalf("sep1RefreshLoop = (%d, %v); want (1, [])", ok, failed)
	}
	want := "[rebind " + issuer + " " + current + " mark " + issuer + " fetch current write " + issuer + "]"
	if got := fmt.Sprint(log.steps()); got != want {
		t.Errorf("call order = %s;\n            want %s — the domain the account no longer declares must never be fetched", got, want)
	}
}

// TestSep1RefreshClearsADomainTheChainNoLongerDeclares: a cleared home_domain is cleared in the row, not fetched, and not a failed attempt.
func TestSep1RefreshClearsADomainTheChainNoLongerDeclares(t *testing.T) {
	const issuer = "GCLEARED"
	log := &sep1CallLog{}
	srv, stored := sep1TestDomain(t, log, "stored", tomlListing("Stored Domain Org", issuer))
	chain := sep1ChainStub{live: map[string]clickhouse.AccountAuthFlags{
		issuer: {Source: clickhouse.AuthFlagsSourceLive, AsOfLedger: 64228661},
	}}

	ok, failed, _ := sep1RefreshLoop(context.Background(), log, chain, sep1TestResolver(srv),
		[]timescale.IssuerSep1Candidate{{GStrkey: issuer, HomeDomain: stored}}, false)
	if ok != 1 || len(failed) != 0 {
		t.Fatalf("sep1RefreshLoop = (%d, %v); want (1, [])", ok, failed)
	}
	if got := fmt.Sprint(log.steps()); got != "[clear "+issuer+"]" {
		t.Errorf("call order = %s; want [clear %s]", got, issuer)
	}
}

// TestSep1RefreshDoesNotFetchAnUnconfirmedDomain: an unconfirmable domain is not fetched; the attempt is marked failed so a lake outage stays in the systemic unwind.
func TestSep1RefreshDoesNotFetchAnUnconfirmedDomain(t *testing.T) {
	for name, tc := range map[string]struct {
		chain   sep1ChainStub
		ourSide bool
	}{
		"no live entry": {},
		"lake error":    {chain: sep1ChainStub{err: errors.New("clickhouse down")}, ourSide: true},
	} {
		t.Run(name, func(t *testing.T) {
			const issuer = "GUNREAD"
			log := &sep1CallLog{}
			srv, stored := sep1TestDomain(t, log, "stored", tomlListing("Stored Domain Org", issuer))

			ok, failed, ourFault := sep1RefreshLoop(context.Background(), log, tc.chain, sep1TestResolver(srv),
				[]timescale.IssuerSep1Candidate{{GStrkey: issuer, HomeDomain: stored}}, false)
			if ok != 0 || len(failed) != 1 || failed[0] != issuer {
				t.Fatalf("sep1RefreshLoop = (%d, %v); want (0, [%s])", ok, failed, issuer)
			}
			if got := len(ourFault) == 1; got != tc.ourSide {
				t.Errorf("our-fault keys = %v; want our-side=%v — only a lake failure is ours to unwind", ourFault, tc.ourSide)
			}
			if got := fmt.Sprint(log.steps()); got != "[mark "+issuer+"]" {
				t.Errorf("call order = %s; want [mark %s] — nothing may be fetched or written", got, issuer)
			}
		})
	}
}

// TestSep1RefreshDryRunRebindsNothing: a dry run fetches the chain's domain but writes no row.
func TestSep1RefreshDryRunRebindsNothing(t *testing.T) {
	const issuer = "GMOVED"
	log := &sep1CallLog{}
	srv, stored := sep1TestDomain(t, log, "stored", tomlListing("Stored Domain Org", issuer))
	_, current := sep1TestDomain(t, log, "current", tomlListing("Current Org", issuer))
	chain := sep1ChainStub{live: map[string]clickhouse.AccountAuthFlags{
		issuer: {HomeDomain: current, Source: clickhouse.AuthFlagsSourceLive, AsOfLedger: 64228661},
	}}

	ok, failed, _ := sep1RefreshLoop(context.Background(), log, chain, sep1TestResolver(srv),
		[]timescale.IssuerSep1Candidate{{GStrkey: issuer, HomeDomain: stored}}, true)
	if ok != 1 || len(failed) != 0 {
		t.Fatalf("sep1RefreshLoop = (%d, %v); want (1, [])", ok, failed)
	}
	if got := fmt.Sprint(log.steps()); got != "[fetch current]" {
		t.Errorf("dry run call log = %s; want [fetch current]", got)
	}
}

// TestSep1RefreshClassifiesAFailedWrite: a content-refused payload is the document's fault and keeps its ladder step; other write failures are ours and unwound.
func TestSep1RefreshClassifiesAFailedWrite(t *testing.T) {
	for name, tc := range map[string]struct {
		err     error
		ourSide bool
	}{
		"payload rejected": {err: fmt.Errorf("timescale: %w: 22P05", timescale.ErrSep1PayloadRejected)},
		"store down":       {err: errors.New("connection refused"), ourSide: true},
	} {
		t.Run(name, func(t *testing.T) {
			const issuer = "GWRITE"
			log := &sep1CallLog{writeErr: map[string]error{issuer: tc.err}}
			srv, stored := sep1TestDomain(t, log, "stored", tomlListing("Stored Domain Org", issuer))
			cands := []timescale.IssuerSep1Candidate{{GStrkey: issuer, HomeDomain: stored}}

			ok, failed, ourFault := sep1RefreshLoop(context.Background(), log, sep1ChainFor(cands...), sep1TestResolver(srv), cands, false)
			if ok != 0 || len(failed) != 1 || failed[0] != issuer {
				t.Fatalf("sep1RefreshLoop = (%d, %v); want (0, [%s])", ok, failed, issuer)
			}
			if got := len(ourFault) == 1; got != tc.ourSide {
				t.Errorf("our-fault keys = %v; want our-side=%v", ourFault, tc.ourSide)
			}
		})
	}
}
