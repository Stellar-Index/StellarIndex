package explorer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/entryhistory"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

type sponsorshipsReader struct {
	*capReader
	wm           uint32
	arrangements []clickhouse.SponsorshipArrangement
	resume       clickhouse.SponsorshipKey
	totals       map[string]clickhouse.SponsorshipTotals
	state        clickhouse.AccountState
	lastSide     string
	lastInForce  bool
	lastAfter    clickhouse.SponsorshipKey
	lastLimit    int
}

func (r *sponsorshipsReader) EntryHistoryWatermark(context.Context) (uint32, error) {
	return r.wm, nil
}

func (r *sponsorshipsReader) AccountSponsorships(_ context.Context, _, side string, inForce bool,
	after clickhouse.SponsorshipKey, _ uint32, limit int,
) ([]clickhouse.SponsorshipArrangement, clickhouse.SponsorshipKey, error) {
	r.lastSide, r.lastInForce, r.lastAfter, r.lastLimit = side, inForce, after, limit
	return r.arrangements, r.resume, nil
}

func (r *sponsorshipsReader) SponsorshipTotals(_ context.Context, _, side string, _ uint32) (clickhouse.SponsorshipTotals, error) {
	return r.totals[side], nil
}

func (r *sponsorshipsReader) AccountStateCached(context.Context, string) (clickhouse.AccountState, bool, error) {
	return r.state, true, nil
}

var (
	spTrustlineKey = clickhouse.SponsorshipKey{Sponsor: aecSponsor, Sponsored: validTestAccount, Kind: "trustline", KeyID: "USD-" + aecIssuer}
	spDataKey      = clickhouse.SponsorshipKey{Sponsor: aecSponsor, Sponsored: validTestAccount, Kind: "data", KeyID: "name.with.dots"}
)

func spEv(ledger uint32, tx, event string) clickhouse.SponsorshipEventRow {
	return clickhouse.SponsorshipEventRow{Ledger: ledger, CloseTime: time.Unix(1_700_000_000, 0).UTC(), TxHash: tx, Event: event}
}

func newSponsorshipsReader(wm uint32) *sponsorshipsReader {
	return &sponsorshipsReader{
		capReader: &capReader{probe: &deadlineProbe{}}, wm: wm,
		arrangements: []clickhouse.SponsorshipArrangement{
			{SponsorshipKey: spTrustlineKey, Reserves: 1, Events: []clickhouse.SponsorshipEventRow{
				spEv(100, "a", entryhistory.SponsorshipStarted), spEv(120, "b", entryhistory.SponsorshipRevoked), spEv(130, "c", entryhistory.SponsorshipStarted),
			}},
			{SponsorshipKey: spDataKey, Reserves: 1, Events: []clickhouse.SponsorshipEventRow{spEv(140, "d", entryhistory.SponsorshipStarted)}},
		},
		totals: map[string]clickhouse.SponsorshipTotals{
			clickhouse.SponsorshipSideSponsored: {InForce: 2, InForceReserves: 2, Started: 3, Revoked: 1},
		},
		state: clickhouse.AccountState{Exists: true, NumSponsored: 2, LastModifiedLedger: 150},
	}
}

func callAccountSponsorships(t *testing.T, reader ExplorerReader, q url.Values, limit int) (int, AccountSponsorshipsView) {
	t.Helper()
	var view AccountSponsorshipsView
	h := newProbeHandler(reader, nil)
	h.ParseLimit = func(http.ResponseWriter, *http.Request, int, int) (int, bool) { return limit, true }
	h.WriteJSONAt = func(w http.ResponseWriter, v any, _, _ bool, _ time.Time) {
		view = v.(AccountSponsorshipsView)
		w.WriteHeader(http.StatusOK)
	}
	h.WriteJSON = func(w http.ResponseWriter, v any, _ bool) {
		view = v.(AccountSponsorshipsView)
		w.WriteHeader(http.StatusOK)
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/accounts/x/sponsorships?"+q.Encode(), nil)
	r.SetPathValue("g_strkey", validTestAccount)
	rec := httptest.NewRecorder()
	h.AccountSponsorships(rec, r)
	return rec.Code, view
}

func TestAccountSponsorships_ShortPageCarriesTheReadersResumeKey(t *testing.T) {
	reader := newSponsorshipsReader(200)
	reader.arrangements = reader.arrangements[:1]
	reader.resume = spDataKey
	code, view := callAccountSponsorships(t, reader, url.Values{"direction": {"sponsored"}}, 5)
	if code != http.StatusOK || len(view.Arrangements) != 1 {
		t.Fatalf("status %d, %d arrangements; want 200 with the one found", code, len(view.Arrangements))
	}
	if view.NextCursor != sponsorshipCursor(spDataKey) {
		t.Fatalf("next_cursor = %q, want the resume key so the client keeps paging", view.NextCursor)
	}
}

func TestAccountSponsorships_PageEpisodesAndProof(t *testing.T) {
	reader := newSponsorshipsReader(200)
	code, view := callAccountSponsorships(t, reader, url.Values{"direction": {"sponsored"}}, 1)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if reader.lastSide != clickhouse.SponsorshipSideSponsored || !reader.lastInForce || reader.lastLimit != 2 || reader.lastAfter.IsSet() {
		t.Fatalf("reader call = side %q in force %v limit %d after %+v", reader.lastSide, reader.lastInForce, reader.lastLimit, reader.lastAfter)
	}
	if view.Direction != "sponsored" || view.State != "in_force" || view.ThroughLedger != 200 || len(view.Arrangements) != 1 {
		t.Fatalf("view = %+v", view)
	}
	a := view.Arrangements[0]
	if !a.InForce || a.KeyID != spTrustlineKey.KeyID || len(a.Episodes) != 2 {
		t.Fatalf("arrangement = %+v, want the in-force trustline with two episodes", a)
	}
	if e := a.Episodes[0]; e.Started == nil || e.Started.Ledger != 100 || e.Ended == nil || e.Ended.Ledger != 120 || e.EndEvent != "revoked" {
		t.Fatalf("episode 0 = %+v", e)
	}
	if e := a.Episodes[1]; e.Started == nil || e.Started.Ledger != 130 || e.Ended != nil || e.EndEvent != "" {
		t.Fatalf("episode 1 = %+v", e)
	}
	if view.NextCursor != sponsorshipCursor(spTrustlineKey) {
		t.Fatalf("next_cursor = %q", view.NextCursor)
	}
	if view.Totals.Sponsored.InForce != 2 || view.Totals.Sponsoring.InForce != 0 {
		t.Fatalf("totals = %+v", view.Totals)
	}
	if p := view.Proof; p.Match == nil || !*p.Match || p.NumSponsored != 2 || p.DerivedSponsored != 2 || p.AccountLedger != 150 {
		t.Fatalf("proof = %+v, want a match", p)
	}
	if !strings.Contains(view.CoverageNote, "agree with") {
		t.Fatalf("coverage_note = %q", view.CoverageNote)
	}
}

func TestAccountSponsorships_CursorKeepsDotsInKeyID(t *testing.T) {
	reader := newSponsorshipsReader(200)
	code, _ := callAccountSponsorships(t, reader, url.Values{"cursor": {sponsorshipCursor(spDataKey)}, "state": {"all"}}, 25)
	if code != http.StatusOK || reader.lastAfter != spDataKey || reader.lastInForce {
		t.Fatalf("status %d after %+v in force %v, want the data key with its dotted name and every state", code, reader.lastAfter, reader.lastInForce)
	}
	if code, _ := callAccountSponsorships(t, reader, url.Values{"cursor": {"nodots"}}, 25); code != http.StatusBadRequest {
		t.Fatalf("malformed cursor status = %d, want 400", code)
	}
}

func TestAccountSponsorships_ProofNotComparableAndMismatch(t *testing.T) {
	reader := newSponsorshipsReader(200)
	reader.state.LastModifiedLedger = 250
	_, view := callAccountSponsorships(t, reader, nil, 25)
	if view.Proof.Match != nil || !strings.Contains(view.CoverageNote, "not comparable") {
		t.Fatalf("entry modified after the watermark: proof = %+v note %q", view.Proof, view.CoverageNote)
	}
	reader.state.LastModifiedLedger = 150
	reader.state.NumSponsored = 3
	_, view = callAccountSponsorships(t, reader, nil, 25)
	if view.Proof.Match == nil || *view.Proof.Match || !strings.Contains(view.CoverageNote, "DISAGREE") {
		t.Fatalf("counter drift: proof = %+v note %q", view.Proof, view.CoverageNote)
	}
	if reader.lastSide != clickhouse.SponsorshipSideSponsor || view.Direction != "sponsoring" {
		t.Fatalf("default direction = %q over side %q", view.Direction, reader.lastSide)
	}
}

func TestAccountSponsorships_NotBuiltAndBadParams(t *testing.T) {
	if code, _ := callAccountSponsorships(t, newSponsorshipsReader(0), nil, 25); code != http.StatusServiceUnavailable {
		t.Fatalf("unbuilt status = %d, want 503", code)
	}
	reader := newSponsorshipsReader(200)
	for _, q := range []url.Values{{"direction": {"both"}}, {"state": {"ended"}}} {
		if code, _ := callAccountSponsorships(t, reader, q, 25); code != http.StatusBadRequest {
			t.Fatalf("%v status = %d, want 400", q, code)
		}
	}
}

// barrierSponsorshipsReader holds every read until all four have started, so a serial handler times out.
type barrierSponsorshipsReader struct {
	*sponsorshipsReader
	started    sync.WaitGroup
	panicState bool
}

func (r *barrierSponsorshipsReader) barrier() error {
	r.started.Done()
	all := make(chan struct{})
	go func() { r.started.Wait(); close(all) }()
	select {
	case <-all:
		return nil
	case <-time.After(2 * time.Second):
		return errors.New("reads ran one at a time")
	}
}

func (r *barrierSponsorshipsReader) AccountSponsorships(ctx context.Context, account, side string, inForce bool,
	after clickhouse.SponsorshipKey, wm uint32, limit int,
) ([]clickhouse.SponsorshipArrangement, clickhouse.SponsorshipKey, error) {
	if err := r.barrier(); err != nil {
		return nil, clickhouse.SponsorshipKey{}, err
	}
	return r.sponsorshipsReader.AccountSponsorships(ctx, account, side, inForce, after, wm, limit)
}

func (r *barrierSponsorshipsReader) SponsorshipTotals(ctx context.Context, account, side string, wm uint32) (clickhouse.SponsorshipTotals, error) {
	if err := r.barrier(); err != nil {
		return clickhouse.SponsorshipTotals{}, err
	}
	return r.sponsorshipsReader.SponsorshipTotals(ctx, account, side, wm)
}

func (r *barrierSponsorshipsReader) AccountStateCached(ctx context.Context, account string) (clickhouse.AccountState, bool, error) {
	if err := r.barrier(); err != nil {
		return clickhouse.AccountState{}, false, err
	}
	if r.panicState {
		panic("account state exploded")
	}
	return r.sponsorshipsReader.AccountStateCached(ctx, account)
}

func newBarrierSponsorshipsReader(panicState bool) *barrierSponsorshipsReader {
	r := &barrierSponsorshipsReader{sponsorshipsReader: newSponsorshipsReader(200), panicState: panicState}
	r.started.Add(4)
	return r
}

func TestAccountSponsorships_ReadsRunConcurrentlyAndAPanicIsA500(t *testing.T) {
	code, view := callAccountSponsorships(t, newBarrierSponsorshipsReader(false), url.Values{"direction": {"sponsored"}}, 25)
	if code != http.StatusOK || len(view.Arrangements) != 2 || view.Totals.Sponsored.InForce != 2 || view.Proof.Match == nil || !*view.Proof.Match {
		t.Fatalf("status/arrangements/in-force/match = %d/%d/%d/%v, want 200 with both arrangements and a matching proof",
			code, len(view.Arrangements), view.Totals.Sponsored.InForce, view.Proof.Match != nil)
	}
	if code, _ := callAccountSponsorships(t, newBarrierSponsorshipsReader(true), nil, 25); code != http.StatusInternalServerError {
		t.Fatalf("panicking read: status = %d, want 500", code)
	}
}
