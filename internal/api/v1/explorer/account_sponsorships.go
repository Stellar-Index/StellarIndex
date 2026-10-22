package explorer

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/Stellar-Index/StellarIndex/internal/entryhistory"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

const (
	sponsorshipDirectionSponsoring = "sponsoring"
	sponsorshipDirectionSponsored  = "sponsored"
	sponsorshipStateInForce        = "in_force"
	sponsorshipStateAll            = "all"
)

// SponsorshipEventView locates one lifecycle transition in the ledger.
type SponsorshipEventView struct {
	Ledger          uint32 `json:"ledger"`
	LedgerCloseTime string `json:"ledger_close_time"`
	IntraLedgerSeq  uint32 `json:"intra_ledger_seq"`
	TxHash          string `json:"tx_hash"`
	OpIndex         int32  `json:"op_index"`
	ChangeIndex     uint32 `json:"change_index"`
}

// SponsorshipEpisodeView is one stretch an arrangement was in force: started is null below the derivation floor,
// ended is null while in force.
type SponsorshipEpisodeView struct {
	Started  *SponsorshipEventView `json:"started"`
	Ended    *SponsorshipEventView `json:"ended"`
	EndEvent string                `json:"end_event,omitempty"`
}

// SponsorshipArrangementView is one sponsor paying the reserves of one entry or signer of one account.
type SponsorshipArrangementView struct {
	Sponsor   string                   `json:"sponsor"`
	Sponsored string                   `json:"sponsored,omitempty"`
	Kind      string                   `json:"kind"`
	KeyID     string                   `json:"key_id,omitempty"`
	Reserves  uint8                    `json:"reserve_count"`
	InForce   bool                     `json:"in_force"`
	Episodes  []SponsorshipEpisodeView `json:"episodes"`
}

// SponsorshipTotalsView counts one side of the account's arrangements through the watermark.
type SponsorshipTotalsView struct {
	InForce         uint64 `json:"in_force"`
	InForceReserves uint64 `json:"in_force_reserve_count"`
	Started         uint64 `json:"started"`
	Revoked         uint64 `json:"revoked"`
	Ended           uint64 `json:"ended"`
}

// SponsorshipSidesView carries both sides' totals whatever direction the page lists.
type SponsorshipSidesView struct {
	Sponsoring SponsorshipTotalsView `json:"sponsoring"`
	Sponsored  SponsorshipTotalsView `json:"sponsored"`
}

// SponsorshipProofView sets the account entry's counters against the derived in-force reserves; match is null
// when the entry changed after through_ledger or does not exist.
type SponsorshipProofView struct {
	AccountLedger     uint32 `json:"account_ledger"`
	NumSponsoring     uint32 `json:"num_sponsoring"`
	NumSponsored      uint32 `json:"num_sponsored"`
	DerivedSponsoring uint64 `json:"derived_sponsoring"`
	DerivedSponsored  uint64 `json:"derived_sponsored"`
	Match             *bool  `json:"match"`
}

// AccountSponsorshipsView is one page of /v1/accounts/{g_strkey}/sponsorships.
type AccountSponsorshipsView struct {
	Account       string                       `json:"account"`
	Direction     string                       `json:"direction"`
	State         string                       `json:"state"`
	Arrangements  []SponsorshipArrangementView `json:"arrangements"`
	NextCursor    string                       `json:"next_cursor,omitempty"`
	Totals        SponsorshipSidesView         `json:"totals"`
	Proof         SponsorshipProofView         `json:"proof"`
	ThroughLedger uint32                       `json:"through_ledger"`
	CoverageNote  string                       `json:"coverage_note"`
}

// AccountSponsorships serves GET /v1/accounts/{g_strkey}/sponsorships: the arrangements the account pays for or
// benefits from, each with its lifecycle, beside a proof of the account entry's own counters against them.
func (h *Handler) AccountSponsorships(w http.ResponseWriter, r *http.Request) {
	if h.Reader == nil {
		h.unavailable(w, r)
		return
	}
	account, ok := h.parseAccountStrkey(w, r)
	if !ok {
		return
	}
	direction, side, ok := h.parseSponsorshipDirection(w, r)
	if !ok {
		return
	}
	state, ok := h.parseSponsorshipState(w, r)
	if !ok {
		return
	}
	limit, ok := h.ParseLimit(w, r, accountMovementsDefaultLimit, accountMovementsMaxLimit)
	if !ok {
		return
	}
	cur, ok := h.parseSponsorshipCursor(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), explorerReadTimeout)
	defer cancel()
	wm, err := h.Reader.EntryHistoryWatermark(ctx)
	if err != nil {
		h.accountSponsorshipsError(w, r, ctx, err, account)
		return
	}
	if wm == 0 {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/sponsorships-not-built", "Sponsorships not yet derived",
			http.StatusServiceUnavailable, "the sponsorship lifecycle has not been derived on this deployment yet; retry later")
		return
	}
	out, err := h.accountSponsorshipsPage(ctx, account, direction, side, state, cur, wm, limit)
	if err != nil {
		h.accountSponsorshipsError(w, r, ctx, err, account)
		return
	}
	h.writeJSONAt(w, out, h.movementsStale(ctx, wm), false, time.Time{})
}

func (h *Handler) accountSponsorshipsPage(ctx context.Context, account, direction, side, state string,
	cur clickhouse.SponsorshipKey, wm uint32, limit int,
) (AccountSponsorshipsView, error) {
	// The four reads are independent; run serially they summed to ~0.5s for the busiest sponsor.
	var (
		arrangements          []clickhouse.SponsorshipArrangement
		resume                clickhouse.SponsorshipKey
		sponsoring, sponsored clickhouse.SponsorshipTotals
		st                    clickhouse.AccountState
	)
	g, gctx := errgroup.WithContext(ctx)
	h.goSponsorshipRead(g, "arrangements", func() (err error) {
		arrangements, resume, err = h.Reader.AccountSponsorships(gctx, account, side, state == sponsorshipStateInForce, cur, wm, limit+1)
		return err
	})
	h.goSponsorshipRead(g, "sponsoring totals", func() (err error) {
		sponsoring, err = h.Reader.SponsorshipTotals(gctx, account, clickhouse.SponsorshipSideSponsor, wm)
		return err
	})
	h.goSponsorshipRead(g, "sponsored totals", func() (err error) {
		sponsored, err = h.Reader.SponsorshipTotals(gctx, account, clickhouse.SponsorshipSideSponsored, wm)
		return err
	})
	h.goSponsorshipRead(g, "account state", func() (err error) {
		st, _, err = h.Reader.AccountStateCached(gctx, account)
		return err
	})
	if err := g.Wait(); err != nil {
		return AccountSponsorshipsView{}, err
	}
	out := AccountSponsorshipsView{
		Account: account, Direction: direction, State: state, ThroughLedger: wm,
		Arrangements: make([]SponsorshipArrangementView, 0, len(arrangements)),
		Totals:       SponsorshipSidesView{Sponsoring: SponsorshipTotalsView(sponsoring), Sponsored: SponsorshipTotalsView(sponsored)},
	}
	if len(arrangements) > limit {
		arrangements = arrangements[:limit]
		out.NextCursor = sponsorshipCursor(arrangements[limit-1].SponsorshipKey)
	} else if resume.IsSet() {
		// The reader's scan budget ran out first: the page is short, and the next one resumes past what was read.
		out.NextCursor = sponsorshipCursor(resume)
	}
	for _, a := range arrangements {
		out.Arrangements = append(out.Arrangements, sponsorshipArrangementView(a))
	}
	proof := entryhistory.ProveSponsorships(st, wm, sponsoring, sponsored)
	out.Proof = SponsorshipProofView{
		AccountLedger: proof.LastModifiedLedger, NumSponsoring: proof.NumSponsoring, NumSponsored: proof.NumSponsored,
		DerivedSponsoring: proof.DerivedSponsoring, DerivedSponsored: proof.DerivedSponsored, Match: proof.Match,
	}
	out.CoverageNote = accountSponsorshipsNote(wm, proof.Match)
	return out, nil
}

// goSponsorshipRead runs one read off the handler goroutine, where middleware.Recoverer does not reach, so a panic
// becomes that read's error instead of a crash.
func (h *Handler) goSponsorshipRead(g *errgroup.Group, what string, read func() error) {
	g.Go(func() (err error) {
		defer func() {
			if rec := recover(); rec != nil {
				worker.Report(h.Logger, "explorer-account-sponsorships-read", rec)
				err = fmt.Errorf("sponsorships %s panicked: %v", what, rec)
			}
		}()
		return read()
	})
}

func sponsorshipEventView(e *clickhouse.SponsorshipEventRow) *SponsorshipEventView {
	if e == nil {
		return nil
	}
	return &SponsorshipEventView{
		Ledger: e.Ledger, LedgerCloseTime: e.CloseTime.UTC().Format(time.RFC3339), IntraLedgerSeq: e.IntraLedgerSeq,
		TxHash: e.TxHash, OpIndex: e.OpIndex, ChangeIndex: e.ChangeIndex,
	}
}

func sponsorshipArrangementView(a clickhouse.SponsorshipArrangement) SponsorshipArrangementView {
	episodes, inForce := entryhistory.SponsorshipEpisodes(a.Events)
	v := SponsorshipArrangementView{
		Sponsor: a.Sponsor, Sponsored: a.Sponsored, Kind: a.Kind, KeyID: a.KeyID, Reserves: a.Reserves, InForce: inForce,
		Episodes: make([]SponsorshipEpisodeView, 0, len(episodes)),
	}
	for _, ep := range episodes {
		ev := SponsorshipEpisodeView{Started: sponsorshipEventView(ep.Start), Ended: sponsorshipEventView(ep.End)}
		if ep.End != nil {
			ev.EndEvent = ep.End.Event
		}
		v.Episodes = append(v.Episodes, ev)
	}
	return v
}

func (h *Handler) parseSponsorshipDirection(w http.ResponseWriter, r *http.Request) (direction, side string, ok bool) {
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get("direction"))) {
	case "", sponsorshipDirectionSponsoring:
		return sponsorshipDirectionSponsoring, clickhouse.SponsorshipSideSponsor, true
	case sponsorshipDirectionSponsored:
		return sponsorshipDirectionSponsored, clickhouse.SponsorshipSideSponsored, true
	}
	h.WriteProblem(w, r, "https://api.stellarindex.io/errors/invalid-direction", "Invalid direction", http.StatusBadRequest,
		"direction must be sponsoring or sponsored")
	return "", "", false
}

func (h *Handler) parseSponsorshipState(w http.ResponseWriter, r *http.Request) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get("state"))) {
	case "", sponsorshipStateInForce:
		return sponsorshipStateInForce, true
	case sponsorshipStateAll:
		return sponsorshipStateAll, true
	}
	h.WriteProblem(w, r, "https://api.stellarindex.io/errors/invalid-state", "Invalid state", http.StatusBadRequest,
		"state must be in_force or all")
	return "", false
}

// sponsorshipCursor joins the key on dots; the first three parts never contain one, so key_id keeps the rest.
func sponsorshipCursor(k clickhouse.SponsorshipKey) string {
	return strings.Join([]string{k.Sponsor, k.Sponsored, k.Kind, k.KeyID}, ".")
}

func (h *Handler) parseSponsorshipCursor(w http.ResponseWriter, r *http.Request) (clickhouse.SponsorshipKey, bool) {
	raw := r.URL.Query().Get("cursor")
	if raw == "" {
		return clickhouse.SponsorshipKey{}, true
	}
	parts := strings.SplitN(raw, ".", 4)
	if len(parts) != 4 || parts[0] == "" || parts[2] == "" {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/invalid-cursor", "Invalid cursor", http.StatusBadRequest,
			"cursor must be an opaque value returned in a prior next_cursor")
		return clickhouse.SponsorshipKey{}, false
	}
	return clickhouse.SponsorshipKey{Sponsor: parts[0], Sponsored: parts[1], Kind: parts[2], KeyID: parts[3]}, true
}

func accountSponsorshipsNote(wm uint32, match *bool) string {
	note := fmt.Sprintf("sponsorship arrangements derived from the lake's ledger-entry changes through ledger %d; "+
		"an arrangement that began below the derivation floor is listed without a start", wm)
	switch {
	case match == nil:
		return note + "; the proof is not comparable: the account entry changed after that ledger or does not exist"
	case !*match:
		return note + "; the account entry's counters DISAGREE with the derived in-force reserves"
	}
	return note + "; the account entry's counters agree with the derived in-force reserves"
}

func (h *Handler) accountSponsorshipsError(w http.ResponseWriter, r *http.Request, ctx context.Context, err error, account string) {
	if h.ClientAborted(r, err) {
		return
	}
	if retryableColdMiss(ctx, err) {
		h.Logger.Warn("explorer AccountSponsorships deadline exceeded", "account", account)
		h.writeRetryable(w, r, err, "https://api.stellarindex.io/errors/account-sponsorships-timeout",
			"Account sponsorships timed out")
		return
	}
	h.Logger.Error("explorer AccountSponsorships failed", "err", err, "account", account)
	h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal", "Internal error", http.StatusInternalServerError, "")
}
