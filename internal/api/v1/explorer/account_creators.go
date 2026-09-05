package explorer

import (
	"context"
	"math/big"
	"net/http"
	"time"
)

// AccountCreatorsView is the wire response for GET /v1/accounts/creators
// — the account-creator league table (#351): which accounts brought the
// most other accounts into existence, and what the created set holds
// now.
//
// Stroops-denominated values are decimal STRINGS (ADR-0003); counts are
// JSON numbers (all far below 2^53).
//
// The two relationships #351 names are NOT merged here. This surface is
// the CREATOR one only — funder → created account, from the
// CreateAccount operation, immutable once it happened. The SPONSOR
// relationship (who currently pays an entry's base reserve) is a
// different question over a different source and is not served yet;
// nothing in this response should be read as a sponsorship figure.
type AccountCreatorsView struct {
	Creators []AccountCreatorV `json:"creators"`
	Totals   struct {
		// Creators and AccountsCreated are totals over the WHOLE
		// aggregation, not over the returned page — a caller ranking the
		// top 50 still sees how many creators there are.
		Creators        int64 `json:"creators"`
		AccountsCreated int64 `json:"accounts_created"`
		LiveAccounts    int64 `json:"live_accounts"`
	} `json:"totals"`
	// Coverage is the ledger span the rollup actually aggregated, read
	// off the same cycle that produced the board. It is not a claim
	// about the chain and not a constant: a consumer that wants to know
	// whether these counts cover all of history compares thru_ledger
	// against the tip itself.
	Coverage struct {
		FromLedger uint32 `json:"from_ledger"`
		ThruLedger uint32 `json:"thru_ledger"`
		FromTime   string `json:"from_time"`
		ThruTime   string `json:"thru_time"`
	} `json:"coverage"`
	ComputedAt string `json:"computed_at"`
}

// AccountCreatorV is one row of the board.
type AccountCreatorV struct {
	Rank    uint32 `json:"rank"`
	Account string `json:"account"`
	// AccountsCreated and FundedStroops are immutable history: the count
	// of successful CreateAccount operations this account was the source
	// of, and the sum of the starting balances it paid. FundedStroops is
	// legitimately "0" for a creator that only ever created sponsored
	// accounts (CAP-33), whose reserves someone else covered.
	AccountsCreated uint64 `json:"accounts_created"`
	FundedStroops   string `json:"funded_stroops"`
	// LiveAccounts and LiveStroops are point-in-time as of ComputedAt:
	// how many of the created accounts still exist, and the native XLM
	// they hold now. Both fall when accounts merge away or spend.
	LiveAccounts uint64 `json:"live_accounts"`
	LiveStroops  string `json:"live_stroops"`
	// FirstLedger/LastLedger bound this creator's own activity inside
	// the response's coverage span.
	FirstLedger    uint32 `json:"first_ledger"`
	LastLedger     uint32 `json:"last_ledger"`
	FirstCreatedAt string `json:"first_created_at"`
	LastCreatedAt  string `json:"last_created_at"`
}

const (
	// creatorsDefaultLimit is one screen of the board.
	creatorsDefaultLimit = 50
	// creatorsMaxLimit bounds the page. The rollup holds one row per
	// distinct creator, so a deeper request stays a keyed read; the cap
	// exists to bound the response, not the query.
	creatorsMaxLimit = 500
)

// stroopsString renders an Int128 stroops figure as the decimal string
// the wire contract requires (ADR-0003). A nil big.Int — what a scan of
// a NULL-shaped value would leave — renders "0" rather than panicking.
func stroopsString(v *big.Int) string {
	if v == nil {
		return "0"
	}
	return v.String()
}

// AccountCreators serves GET /v1/accounts/creators from the
// ch-creators-rollup cycle's precomputed tables — a keyed board read
// plus seven metric rows.
func (h *Handler) AccountCreators(w http.ResponseWriter, r *http.Request) {
	if h.Reader == nil {
		h.unavailable(w, r)
		return
	}
	limit, ok := h.ParseLimit(w, r, creatorsDefaultLimit, creatorsMaxLimit)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), explorerReadTimeout)
	defer cancel()

	s, ok, err := h.Reader.AccountCreators(ctx, limit)
	if err != nil {
		if h.ClientAborted(r, err) {
			return
		}
		if retryableColdMiss(ctx, err) {
			h.writeRetryable(w, r, err, "https://api.stellarindex.io/errors/account-creators-timeout",
				"Account creators timed out")
			return
		}
		h.Logger.Error("explorer AccountCreators failed", "err", err)
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}
	if !ok {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/account-creators-warming",
			"Account creators warming", http.StatusServiceUnavailable,
			"the account-creator rollup hasn't completed its first cycle on this deployment yet; retry shortly")
		return
	}

	// Never `null` on the wire: an ok snapshot always has a span, and a
	// span with no rows is a board the caller should read as empty.
	out := AccountCreatorsView{
		Creators:   make([]AccountCreatorV, 0, len(s.Board)),
		ComputedAt: s.ComputedAt.UTC().Format(time.RFC3339),
	}
	out.Totals.Creators = s.CreatorsTotal
	out.Totals.AccountsCreated = s.CreationsTotal
	out.Totals.LiveAccounts = s.LiveAccountsTotal
	out.Coverage.FromLedger = s.FromLedger
	out.Coverage.ThruLedger = s.ThruLedger
	out.Coverage.FromTime = s.FromTime.UTC().Format(time.RFC3339)
	out.Coverage.ThruTime = s.ThruTime.UTC().Format(time.RFC3339)
	for _, c := range s.Board {
		out.Creators = append(out.Creators, AccountCreatorV{
			Rank:            c.Rank,
			Account:         c.Creator,
			AccountsCreated: c.AccountsCreated,
			FundedStroops:   stroopsString(c.FundedStroops),
			LiveAccounts:    c.LiveAccounts,
			LiveStroops:     stroopsString(c.LiveStroops),
			FirstLedger:     c.FirstLedger,
			LastLedger:      c.LastLedger,
			FirstCreatedAt:  c.FirstCreatedAt.UTC().Format(time.RFC3339),
			LastCreatedAt:   c.LastCreatedAt.UTC().Format(time.RFC3339),
		})
	}
	h.WriteJSON(w, out, false)
}
