package v1

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// lendingEmissionsReader is the lake read of a pool's emission streams.
type lendingEmissionsReader interface {
	LendingEmissions(ctx context.Context, pool string, tiered bool) (clickhouse.LendingEmissionsState, error)
}

// lendingEmissionClaimReader is the claim totals behind GET /v1/lending/pools/{pool}/emissions.
type lendingEmissionClaimReader interface {
	LendingEmissionClaims(ctx context.Context, pool string, isBLNT bool, asOf time.Time) (timescale.LendingEmissionClaims, error)
}

var (
	_ lendingEmissionsReader     = (*clickhouse.ExplorerReader)(nil)
	_ lendingEmissionClaimReader = (*timescale.Store)(nil)
	_ lendingEmissionClaimReader = (*CachedLendingReader)(nil)
)

// LendingEmissionsView is the GET /v1/lending/pools/{pool}/emissions body: the pool's emission streams from contract storage
// and its claim totals from events. Amounts are base-unit decimal strings of the emission token.
type LendingEmissionsView struct {
	Pool     string `json:"pool"`
	Protocol string `json:"protocol"` // blend | blnt
	// EmissionToken is the pool instance's BLNDTkn (Blend) or BLNTTkn (BLNT); null when unread.
	EmissionToken         *string `json:"emission_token"`
	EmissionTokenDecimals *int    `json:"emission_token_decimals"`
	// EmissionTokenPriceUSD is the current USD price per whole emission token every usd_per_day uses; null when unpriced.
	EmissionTokenPriceUSD *string `json:"emission_token_price_usd"`
	// EPSDecimals scales every eps: eps / 10^eps_decimals is emission-token base units per second.
	EPSDecimals int `json:"eps_decimals"`
	// StorageAvailable is false when the emission streams cannot be read; StorageUnavailableReason says why and the
	// reserves and backstop lists are empty.
	StorageAvailable         bool                          `json:"storage_available"`
	StorageUnavailableReason *string                       `json:"storage_unavailable_reason,omitempty"`
	Reserves                 []LendingReserveEmissionView  `json:"reserves"`
	Backstop                 []LendingBackstopEmissionView `json:"backstop"`
	Claims                   LendingEmissionClaimsView     `json:"claims"`
	AsOfLedger               uint32                        `json:"as_of_ledger,omitempty"`
	// AsOfTime is when live and the 30-day claim window are judged.
	AsOfTime WireTime `json:"as_of_time"`
}

// LendingEmissionStreamView is one emission stream's storage entry.
type LendingEmissionStreamView struct {
	EPS        string   `json:"eps"`
	Expiration WireTime `json:"expiration"`
	LastUpdate WireTime `json:"last_update"`
	Live       bool     `json:"live"`     // expiration is after as_of_time
	Archived   bool     `json:"archived"` // the entry is TTL-archived: its last written value, restorable but not live
	LendingEmissionRateView
}

// LendingReserveEmissionView is one reserve token's stream: token_id = reserve_index*2 (borrow, dToken) or +1 (supply, bToken).
type LendingReserveEmissionView struct {
	TokenID uint32  `json:"token_id"`
	Asset   *string `json:"asset"` // null when the id is past the pool's reserve list
	Side    string  `json:"side"`
	LendingEmissionStreamView
	// EmissionsAPR is usd_per_day x 365 / the side's current USD size (supplied or borrowed) at 4 dp, "0.0000" for an ended
	// stream; null with EmissionsAPRReason when the USD rate or the side's size is unavailable or the side is empty.
	EmissionsAPR       *string `json:"emissions_apr"`
	EmissionsAPRReason *string `json:"emissions_apr_reason"`
}

// LendingBackstopEmissionView is the pool's stream in its backstop; Tier is null for Blend's single stream.
type LendingBackstopEmissionView struct {
	Tier *string `json:"tier"`
	LendingEmissionStreamView
}

// LendingEmissionClaimsView is the pool's claim totals; Backstop is null when backstop claims cannot be attributed to the pool.
type LendingEmissionClaimsView struct {
	Available                 bool                           `json:"available"`
	UnavailableReason         *string                        `json:"unavailable_reason,omitempty"`
	Pool                      *LendingClaimTotalsView        `json:"pool"`
	Backstop                  []LendingBackstopClaimTierView `json:"backstop"`
	BackstopUnavailableReason *string                        `json:"backstop_unavailable_reason,omitempty"`
	// LowerBound: the totals may miss claims, because the protocol's projection is not verified back to its first ledger
	// (history_unverified, history_verdict_stale or _unread) or a claim's amount was not decoded (amount_unread).
	LowerBound       bool    `json:"lower_bound"`
	LowerBoundReason *string `json:"lower_bound_reason,omitempty"`
}

// LendingClaimTotalsView is one claim stream's totals; amount is the emission token claimed, in base units.
type LendingClaimTotalsView struct {
	Claims      int64  `json:"claims"`
	Claims30d   int64  `json:"claims_30d"`
	Claimers    int64  `json:"claimers"`
	Claimers30d int64  `json:"claimers_30d"`
	Amount      string `json:"amount"`
	Amount30d   string `json:"amount_30d"`
}

// LendingBackstopClaimTierView is one BLNT backstop tier's claims: the BLNT claimed, the backstop LP tokens it bought, and the
// tier shares minted to the claimer (shares, not a token payout).
type LendingBackstopClaimTierView struct {
	Tier string `json:"tier"`
	LendingClaimTotalsView
	LPTokens        string `json:"lp_tokens"`
	LPTokens30d     string `json:"lp_tokens_30d"`
	SharesMinted    string `json:"shares_minted"`
	SharesMinted30d string `json:"shares_minted_30d"`
}

const (
	emissionsReasonV1         = "blend_v1_emission_storage_unverified"
	emissionsReasonNoReader   = "emission_storage_reader_unavailable"
	emissionsReasonNoClaims   = "claim_reader_unavailable"
	emissionsReasonBlendBstop = "blend_backstop_claim_names_no_pool"
	// emissionsReasonAmountUnread: a claim row with a NULL amount is counted but not summed.
	emissionsReasonAmountUnread = "amount_unread"
)

// errNoEmissionClaimReader is a lending reader wired without claim totals; zeros would read as "nothing claimed".
var errNoEmissionClaimReader = errors.New("lending: emission claim reader not wired")

// handleLendingPoolEmissions serves GET /v1/lending/pools/{pool}/emissions.
func (s *Server) handleLendingPoolEmissions(w http.ResponseWriter, r *http.Request) {
	pool := r.PathValue("pool")
	if !canonical.IsContractID(pool) {
		writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-pool",
			"Invalid pool", http.StatusBadRequest, "pool must be a C-strkey contract id")
		return
	}
	if s.Lending == nil {
		writeLendingPoolNotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), maxHandlerBudget)
	defer cancel()
	protocol, reason, err := s.backstopLineage(ctx, pool)
	if err != nil {
		s.writeLendingLookupError(ctx, w, r, "lending emissions lineage", pool, err)
		return
	}
	if protocol == "" {
		writeLendingPoolNotFound(w, r)
		return
	}
	asOf := time.Now().UTC()
	view := LendingEmissionsView{
		Pool: pool, Protocol: protocol, EPSDecimals: clickhouse.EmissionEPSDecimals, AsOfTime: WireTime(asOf),
		Reserves: []LendingReserveEmissionView{}, Backstop: []LendingBackstopEmissionView{},
	}
	isBLNT := protocol == blnt.SourceName
	var flags Flags
	if reason == backstopReasonV1 {
		view.StorageUnavailableReason = strRef(emissionsReasonV1)
	} else if err := s.fillEmissionStreams(ctx, &view, isBLNT); err != nil {
		s.writeLendingLookupError(ctx, w, r, "LendingEmissions", pool, err)
		return
	} else if view.StorageAvailable {
		s.valueEmissions(ctx, &view, &flags)
	}
	if err := s.fillEmissionClaims(ctx, &view, isBLNT, asOf, &flags); err != nil {
		s.writeLendingLookupError(ctx, w, r, "LendingEmissionClaims", pool, err)
		return
	}
	wm, stale, _ := s.lakeWatermark(ctx)
	view.AsOfLedger = wm
	flags.Stale = flags.Stale || stale
	writeEnvelope(w, Envelope{Data: view, AsOf: WireTime(asOf), Flags: flags})
}

func writeLendingPoolNotFound(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, "https://api.stellarindex.io/errors/pool-not-found",
		"Lending pool not found", http.StatusNotFound, "no Blend or BLNT lending pool is known at this contract id")
}

func (s *Server) fillEmissionStreams(ctx context.Context, view *LendingEmissionsView, isBLNT bool) error {
	er, ok := s.Explorer.(lendingEmissionsReader)
	if s.Explorer == nil || !ok {
		view.StorageUnavailableReason = strRef(emissionsReasonNoReader)
		return nil
	}
	st, err := er.LendingEmissions(ctx, view.Pool, isBLNT)
	if err != nil {
		return err
	}
	if st.Instance == nil {
		view.StorageUnavailableReason = strRef(backstopReasonNoState)
		return nil
	}
	view.StorageAvailable = true
	asOf := time.Time(view.AsOfTime)
	if st.EmissionToken != "" {
		view.EmissionToken = strRef(st.EmissionToken)
		if d, known, err := s.lakeTokenDecimals(ctx, st.EmissionToken); known && err == nil {
			view.EmissionTokenDecimals = &d
		}
	}
	for _, re := range st.Reserves {
		view.Reserves = append(view.Reserves, LendingReserveEmissionView{
			TokenID: re.TokenID, Asset: nonEmptyPtr(re.Asset), Side: re.Side, LendingEmissionStreamView: emissionStreamView(re.Data, re.Archived, asOf),
		})
	}
	for _, b := range st.Backstop {
		view.Backstop = append(view.Backstop, LendingBackstopEmissionView{
			Tier: nonEmptyPtr(b.Tier), LendingEmissionStreamView: emissionStreamView(b.Data, b.Archived, asOf),
		})
	}
	return nil
}

func emissionStreamView(d clickhouse.EmissionData, archived bool, asOf time.Time) LendingEmissionStreamView {
	exp := unixSeconds(d.Expiration)
	return LendingEmissionStreamView{
		EPS: strconv.FormatUint(d.EPS, 10), Expiration: WireTime(exp), LastUpdate: WireTime(unixSeconds(d.LastTime)),
		Live: exp.After(asOf), Archived: archived,
	}
}

// unixSeconds is a contract u64 timestamp; one past int64 is clamped, which no real ledger time reaches.
func unixSeconds(u uint64) time.Time {
	if u > math.MaxInt64 {
		u = math.MaxInt64
	}
	return time.Unix(int64(u), 0).UTC() //nolint:gosec // clamped above
}

func (s *Server) fillEmissionClaims(ctx context.Context, view *LendingEmissionsView, isBLNT bool, asOf time.Time, flags *Flags) error {
	cr, ok := s.Lending.(lendingEmissionClaimReader)
	if !ok {
		view.Claims.UnavailableReason = strRef(emissionsReasonNoClaims)
		return nil
	}
	c, err := cr.LendingEmissionClaims(ctx, view.Pool, isBLNT, asOf)
	if errors.Is(err, errNoEmissionClaimReader) {
		view.Claims.UnavailableReason = strRef(emissionsReasonNoClaims)
		return nil
	}
	if err != nil {
		return err
	}
	pool := claimTotalsView(c.Pool)
	view.Claims.Available, view.Claims.Pool = true, &pool
	// Pool and BLNT backstop claims are both written by the protocol's own projection.
	source := "blend"
	if isBLNT {
		source = blnt.SourceName
	}
	switch s.projectionHistoryVerdicts(ctx, source)[source] {
	case historyVerified:
	case historyVerdictStale:
		view.Claims.LowerBound, view.Claims.LowerBoundReason, flags.Stale = true, strRef(earningsReasonHistoryVerdictStale), true
	case historyVerdictUnread:
		view.Claims.LowerBound, view.Claims.LowerBoundReason, flags.Degraded = true, strRef(earningsReasonHistoryVerdictUnread), true
	case historyUnverified:
		view.Claims.LowerBound, view.Claims.LowerBoundReason = true, strRef(earningsReasonHistoryUnverified)
	}
	if !view.Claims.LowerBound && claimAmountsUnread(c) {
		view.Claims.LowerBound, view.Claims.LowerBoundReason = true, strRef(emissionsReasonAmountUnread)
	}
	if !isBLNT {
		view.Claims.BackstopUnavailableReason = strRef(emissionsReasonBlendBstop)
		return nil
	}
	view.Claims.Backstop = []LendingBackstopClaimTierView{}
	for _, b := range c.Backstop {
		view.Claims.Backstop = append(view.Claims.Backstop, LendingBackstopClaimTierView{
			Tier: b.Tier, LendingClaimTotalsView: claimTotalsView(b.LendingClaimTotals),
			LPTokens: b.LP.String(), LPTokens30d: b.LP30d.String(), SharesMinted: b.Shares.String(), SharesMinted30d: b.Shares30d.String(),
		})
	}
	return nil
}

func claimAmountsUnread(c timescale.LendingEmissionClaims) bool {
	if c.Pool.AmountUnread > 0 {
		return true
	}
	for _, b := range c.Backstop {
		if b.AmountUnread > 0 {
			return true
		}
	}
	return false
}

func claimTotalsView(t timescale.LendingClaimTotals) LendingClaimTotalsView {
	return LendingClaimTotalsView{
		Claims: t.Claims, Claims30d: t.Claims30d, Claimers: t.Claimers, Claimers30d: t.Claimers30d,
		Amount: t.Amount.String(), Amount30d: t.Amount30d.String(),
	}
}
