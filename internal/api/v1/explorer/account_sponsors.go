package explorer

import (
	"context"
	"net/http"
	"time"
)

// AccountSponsorsView is the wire response for GET /v1/accounts/sponsors
// — the sponsor league table (#351): which accounts have paid the base
// reserves for other accounts' ledger entries.
//
// EVERY FIGURE HERE IS HISTORY. It is derived by replaying sponsorship
// operations, so it says what an account has DONE, never what is
// currently in force. A sponsorship also lapses when the sponsored entry
// is simply deleted or the sponsored account merges away, and neither
// emits a sponsorship operation — so a "currently sponsoring" number
// computed from this source would overstate. Observing the live set
// needs the sponsoringID carried inside each ledger entry, which this
// API does not serve. Nothing in this response is a live-state figure.
//
// This is the counterpart of GET /v1/accounts/creators, and the two must
// not be summed: creation is immutable and one-off, sponsorship is
// revocable and repeatable, and an account can legitimately appear high
// on both boards.
type AccountSponsorsView struct {
	Sponsors []AccountSponsorV `json:"sponsors"`
	Totals   struct {
		// Totals span the whole aggregation, not the returned page.
		Sponsors          int64 `json:"sponsors"`
		SponsorshipsTotal int64 `json:"sponsorships_started"`
		DistinctSponsored int64 `json:"distinct_sponsored"`
		Revocations       int64 `json:"revocations_issued"`
	} `json:"totals"`
	Coverage struct {
		FromLedger uint32 `json:"from_ledger"`
		ThruLedger uint32 `json:"thru_ledger"`
		FromTime   string `json:"from_time"`
		ThruTime   string `json:"thru_time"`
		// AmbiguousTxs counts transactions carrying more than one
		// distinct sponsor. Those are excluded from per-sponsor
		// attribution, and the count is published so the exclusion is
		// visible rather than silent.
		AmbiguousTxs int64 `json:"ambiguous_transactions"`
	} `json:"coverage"`
	ComputedAt string `json:"computed_at"`
}

// AccountSponsorV is one row of the board.
type AccountSponsorV struct {
	Rank    uint32 `json:"rank"`
	Account string `json:"account"`
	// SponsorshipsStarted counts arrangements begun; DistinctSponsored
	// counts the distinct accounts they covered. They differ sharply when
	// a sponsor re-sponsors the same accounts repeatedly, which is common.
	SponsorshipsStarted uint64 `json:"sponsorships_started"`
	DistinctSponsored   uint64 `json:"distinct_sponsored"`
	// RevocationsIssued counts RevokeSponsorship operations this account
	// was the source of. It is a lower bound on arrangements that ended:
	// entries also stop being sponsored when they are deleted.
	RevocationsIssued uint64 `json:"revocations_issued"`
	FirstLedger       uint32 `json:"first_ledger"`
	LastLedger        uint32 `json:"last_ledger"`
	FirstSeenAt       string `json:"first_seen_at"`
	LastSeenAt        string `json:"last_seen_at"`
}

const (
	sponsorsDefaultLimit = 50
	sponsorsMaxLimit     = 500
)

// AccountSponsors serves GET /v1/accounts/sponsors from the
// ch-sponsors-rollup cycle's precomputed tables — a keyed board read
// plus nine metric rows.
func (h *Handler) AccountSponsors(w http.ResponseWriter, r *http.Request) {
	if h.Reader == nil {
		h.unavailable(w, r)
		return
	}
	limit, ok := h.ParseLimit(w, r, sponsorsDefaultLimit, sponsorsMaxLimit)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), explorerReadTimeout)
	defer cancel()

	s, ok, err := h.Reader.AccountSponsors(ctx, limit)
	if err != nil {
		if h.ClientAborted(r, err) {
			return
		}
		if retryableColdMiss(ctx, err) {
			h.writeRetryable(w, r, err, "https://api.stellarindex.io/errors/account-sponsors-timeout",
				"Account sponsors timed out")
			return
		}
		h.Logger.Error("explorer AccountSponsors failed", "err", err)
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}
	if !ok {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/account-sponsors-warming",
			"Account sponsors warming", http.StatusServiceUnavailable,
			"the sponsor rollup hasn't completed its first cycle on this deployment yet; retry shortly")
		return
	}

	out := AccountSponsorsView{
		Sponsors:   make([]AccountSponsorV, 0, len(s.Board)),
		ComputedAt: s.ComputedAt.UTC().Format(time.RFC3339),
	}
	out.Totals.Sponsors = s.SponsorsTotal
	out.Totals.SponsorshipsTotal = s.SponsorshipsTotal
	out.Totals.DistinctSponsored = s.DistinctSponsoredTotal
	out.Totals.Revocations = s.RevocationsTotal
	out.Coverage.FromLedger = s.FromLedger
	out.Coverage.ThruLedger = s.ThruLedger
	out.Coverage.FromTime = s.FromTime.UTC().Format(time.RFC3339)
	out.Coverage.ThruTime = s.ThruTime.UTC().Format(time.RFC3339)
	out.Coverage.AmbiguousTxs = s.AmbiguousTxs
	for _, c := range s.Board {
		out.Sponsors = append(out.Sponsors, AccountSponsorV{
			Rank:                c.Rank,
			Account:             c.Sponsor,
			SponsorshipsStarted: c.SponsorshipsStarted,
			DistinctSponsored:   c.DistinctSponsored,
			RevocationsIssued:   c.RevocationsIssued,
			FirstLedger:         c.FirstLedger,
			LastLedger:          c.LastLedger,
			FirstSeenAt:         c.FirstSeenAt.UTC().Format(time.RFC3339),
			LastSeenAt:          c.LastSeenAt.UTC().Format(time.RFC3339),
		})
	}
	h.WriteJSON(w, out, false)
}
