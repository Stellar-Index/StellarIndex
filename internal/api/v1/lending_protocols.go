package v1

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// lendingProtocols are the lending protocols in serve order, each with its token's verified-catalogue ticker.
var lendingProtocols = []struct{ protocol, ticker string }{
	{"blend", "BLND"},
	{blnt.SourceName, "BLNT"},
}

// Exclusion reasons this route adds to those readRatePool and the backstop route already name.
const (
	exclusionPoolListFailed     = "pool_list_read_failed"
	exclusionPoolReadFailed     = "pool_read_failed"
	exclusionNoDecodedState     = "no_decoded_reserve_state"
	exclusionNoUSDValue         = "no_usd_value"
	exclusionPriceWithheld      = "withheld"
	exclusionPriceReadFailed    = "price_read_failed"
	exclusionBackstopNoReader   = "backstop_reader_unavailable"
	exclusionBackstopReadFailed = "backstop_read_failed"
	exclusionBackstopNoBalance  = "backstop_balance_unavailable"
	exclusionTokenNotVerified   = "token_not_in_catalogue"
	exclusionTokenReadFailed    = "token_read_failed"
)

// LendingProtocolStats is one GET /v1/lending/protocols entry: a lending protocol's token and its pools' totals, summed from the
// same cached per-pool view /v1/lending/pools/{pool}/reserves serves.
type LendingProtocolStats struct {
	Protocol         string                `json:"protocol"`
	Token            *LendingProtocolToken `json:"token"`
	PoolCount        int                   `json:"pool_count"`
	ReserveCount     int                   `json:"reserve_count"`
	TotalSuppliedUSD *string               `json:"total_supplied_usd"`
	TotalBorrowedUSD *string               `json:"total_borrowed_usd"`
	// AvailableUSD is supplied minus borrowed: the liquidity the pools still hold, not what they have lent out.
	AvailableUSD *string                   `json:"available_usd"`
	Backstop     []LendingProtocolBackstop `json:"backstop"`
	// LowerBound covers the USD totals: a pool_list, pool or reserve exclusion leaves something out of them.
	LowerBound bool `json:"lower_bound"`
	// BackstopLowerBound covers the backstop sums: a backstop exclusion leaves a pool's deposits out of them.
	BackstopLowerBound bool `json:"backstop_lower_bound"`
	// BackstopTotalUSD sums the valued backstop[] tokens_usd; BackstopUSDLowerBound when an entry or a pool's backstop is left out.
	BackstopTotalUSD      *string                 `json:"backstop_total_usd"`
	BackstopUSDLowerBound bool                    `json:"backstop_usd_lower_bound"`
	Excluded              []LendingStatsExclusion `json:"excluded"`
}

// LendingProtocolToken is the protocol token's price, supply and market cap as /v1/assets/{id} serves them.
type LendingProtocolToken struct {
	AssetID           string  `json:"asset_id"`
	Code              string  `json:"code"`
	Issuer            string  `json:"issuer"`
	Decimals          int     `json:"decimals"`
	PriceUSD          *string `json:"price_usd"`
	CirculatingSupply *string `json:"circulating_supply"`
	TotalSupply       *string `json:"total_supply"`
	MarketCapUSD      *string `json:"market_cap_usd"`
	// ThinMarket marks a price_usd from a market below the substance floor: display-only, never valued.
	ThinMarket bool               `json:"thin_market,omitempty"`
	Substance  *SubstanceEvidence `json:"substance,omitempty"`
}

// LendingProtocolBackstop is the backstop deposits across the protocol's pools in one backstop token, in its base units.
type LendingProtocolBackstop struct {
	BackstopToken string `json:"backstop_token"`
	Tokens        string `json:"tokens"`
	// TokensUSD is tokens at the token's current price; null with USDReason when it cannot be valued exactly.
	TokensUSD  *string `json:"tokens_usd"`
	PriceStale bool    `json:"price_stale"`
	USDReason  *string `json:"usd_reason"`
	asset      string  // the tiers' BackstopAsset; "" for Blend, and when tiers in this token disagree
}

// LendingStatsExclusion names what a protocol's or an account's figures leave out and why.
type LendingStatsExclusion struct {
	Kind   string  `json:"kind"` // pool_list | pool | reserve | backstop | token; an account's legs: supply | borrow | backstop
	Pool   *string `json:"pool"`
	Asset  *string `json:"asset"`
	Reason string  `json:"reason"`
}

// readFailed reports an exclusion caused by a failed read rather than by what the chain holds.
func (e LendingStatsExclusion) readFailed() bool {
	switch e.Kind {
	case "pool_list", "pool":
		return true
	case "backstop":
		return e.Reason == exclusionBackstopReadFailed || e.Reason == exclusionBackstopNoReader
	case "token":
		return e.Reason == exclusionTokenReadFailed
	}
	return false
}

// protocolPoolRead is one pool's cached reserves view and its backstop read.
type protocolPoolRead struct {
	ratePoolRead
	backstop       clickhouse.LendingBackstopState
	backstopReason string
}

// lendingProtocolsBuild is one built GET /v1/lending/protocols body.
type lendingProtocolsBuild struct {
	stats []LendingProtocolStats
	flags Flags
	asOf  WireTime
}

// lendingProtocolsPartial carries a build that hit a failed read: served to the requests waiting on it, never cached as good.
type lendingProtocolsPartial struct{ build lendingProtocolsBuild }

func (e *lendingProtocolsPartial) Error() string { return "lending protocols: a read failed" }

// handleLendingProtocols serves GET /v1/lending/protocols: per protocol, its token and its pools' USD totals. A pool, reserve,
// backstop or token that could not be read or priced is named in excluded.
func (s *Server) handleLendingProtocols(w http.ResponseWriter, r *http.Request) {
	if s.Explorer == nil {
		s.explorerUnavailable(w, r)
		return
	}
	if s.Lending == nil {
		writeJSON(w, []LendingProtocolStats{}, Flags{})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), maxHandlerBudget)
	defer cancel()
	b, _, cacheStale, err := s.lendingProtocolsCache.get(ctx, "lending_protocols", "all", s.buildLendingProtocols)
	var partial *lendingProtocolsPartial
	if errors.As(err, &partial) {
		b, err = partial.build, nil
	}
	if err != nil {
		s.writeListBlendPoolsError(ctx, w, r, err)
		return
	}
	flags := b.flags
	flags.Stale = flags.Stale || cacheStale
	writeEnvelope(w, Envelope{Data: b.stats, AsOf: b.asOf, Flags: flags})
}

// buildLendingProtocols reads every listed pool and both tokens; a failed read returns the build as a lendingProtocolsPartial.
func (s *Server) buildLendingProtocols(ctx context.Context) (lendingProtocolsBuild, error) {
	var vintage dataVintage
	pools, flags, err := s.lendingRatePools(ctx, "", &vintage)
	if err != nil {
		return lendingProtocolsBuild{}, err
	}
	blntListFailed := flags.Degraded
	reads := make([]protocolPoolRead, len(pools))
	for i := range pools {
		// A panicking read leaves its slot as written here, so the pool is still named rather than silently dropped.
		reads[i] = protocolPoolRead{ratePoolRead: ratePoolRead{ratePool: pools[i], omitted: exclusionPoolReadFailed, reason: exclusionPoolReadFailed}}
	}
	forEachBounded(s.logger, len(pools), readFanoutConcurrency, func(i int) {
		reads[i] = s.readProtocolPool(ctx, pools[i]) // distinct indices — no mutex needed
	})
	out := make([]LendingProtocolStats, 0, len(lendingProtocols))
	lowerBound := false
	for _, pt := range lendingProtocols {
		st := LendingProtocolStats{Protocol: pt.protocol, Backstop: []LendingProtocolBackstop{}, Excluded: []LendingStatsExclusion{}}
		var reason string
		st.Token, reason = s.readProtocolToken(ctx, pt.ticker, &flags)
		if reason != "" {
			st.Excluded = append(st.Excluded, LendingStatsExclusion{Kind: "token", Asset: strRef(pt.ticker), Reason: reason})
		}
		if pt.protocol == blnt.SourceName && blntListFailed {
			st.Excluded = append(st.Excluded, LendingStatsExclusion{Kind: "pool_list", Reason: exclusionPoolListFailed})
		}
		sumProtocolPools(&st, reads, &vintage, &flags)
		for _, e := range st.Excluded {
			st.LowerBound = st.LowerBound || e.Kind == "pool_list" || e.Kind == "pool" || e.Kind == "reserve"
			st.BackstopLowerBound = st.BackstopLowerBound || e.Kind == "backstop"
			flags.Degraded = flags.Degraded || e.readFailed()
		}
		flags.Stale = s.valueProtocolBackstop(ctx, &st) || flags.Stale
		lowerBound = lowerBound || st.LowerBound
		out = append(out, st)
	}
	failed := flags.Degraded
	flags.Degraded = failed || lowerBound
	b := lendingProtocolsBuild{stats: out, flags: flags, asOf: vintage.asOf()}
	if failed {
		return b, &lendingProtocolsPartial{build: b}
	}
	return b, nil
}

// readProtocolPool reads one pool through the reserves cache and, when that succeeds, its backstop balances.
func (s *Server) readProtocolPool(ctx context.Context, p ratePool) protocolPoolRead {
	out := protocolPoolRead{ratePoolRead: s.readRatePool(ctx, p)}
	if out.omitted != "" {
		return out
	}
	br, ok := s.Explorer.(lendingBackstopReader)
	isBLNT := p.protocol == blnt.SourceName
	switch {
	case !ok:
		out.backstopReason = exclusionBackstopNoReader
	case !isBLNT && out.res.version == blend.PoolV1:
		out.backstopReason = backstopReasonV1
	case !isBLNT && out.res.version != blend.PoolV2:
		out.backstopReason = backstopReasonUnknown
	default:
		var err error
		if out.backstop, err = br.LendingBackstop(ctx, p.pool, isBLNT); err != nil {
			s.logger.Warn("lending protocols: backstop read failed", "err", err, "pool", p.pool)
			out.backstopReason = exclusionBackstopReadFailed
		}
	}
	return out
}

// sumProtocolPools totals st.Protocol's pools in exact decimals. A reserve counts only with both USD values, so available stays
// supplied minus borrowed over the same reserves.
func sumProtocolPools(st *LendingProtocolStats, reads []protocolPoolRead, vintage *dataVintage, flags *Flags) {
	supplied, borrowed := new(big.Rat), new(big.Rat)
	priced := 0
	backstop, assets := map[string]*big.Int{}, map[string]string{}
	var tokens []string
	for i := range reads {
		pr := &reads[i]
		if pr.protocol != st.Protocol {
			continue
		}
		st.PoolCount++
		pool := pr.pool
		if pr.omitted != "" {
			st.Excluded = append(st.Excluded, LendingStatsExclusion{Kind: "pool", Pool: &pool, Reason: pr.reason})
			continue
		}
		vintage.note(pr.at)
		flags.Stale = flags.Stale || pr.res.flags.Stale || pr.cacheStale
		flags.Degraded = flags.Degraded || pr.res.flags.Degraded
		v := pr.res.view
		st.ReserveCount += len(v.Reserves) + len(v.OmittedReserves)
		for _, a := range v.OmittedReserves {
			st.Excluded = append(st.Excluded, LendingStatsExclusion{Kind: "reserve", Pool: &pool, Asset: strRef(a), Reason: exclusionNoDecodedState})
		}
		for _, rv := range v.Reserves {
			sup, supOK := parseUSD(rv.SuppliedUSD)
			bor, borOK := parseUSD(rv.BorrowedUSD)
			if !supOK || !borOK {
				reason := exclusionNoUSDValue
				if rv.usdExclusion != "" {
					reason = rv.usdExclusion
				}
				st.Excluded = append(st.Excluded, LendingStatsExclusion{Kind: "reserve", Pool: &pool, Asset: strRef(rv.Asset), Reason: reason})
				continue
			}
			supplied.Add(supplied, sup)
			borrowed.Add(borrowed, bor)
			priced++
		}
		tokens = addProtocolBackstop(st, pr, backstop, assets, tokens)
	}
	if priced > 0 {
		sup, bor, avail := supplied.FloatString(2), borrowed.FloatString(2), new(big.Rat).Sub(supplied, borrowed).FloatString(2)
		st.TotalSuppliedUSD, st.TotalBorrowedUSD, st.AvailableUSD = &sup, &bor, &avail
	}
	for _, t := range tokens {
		st.Backstop = append(st.Backstop, LendingProtocolBackstop{BackstopToken: t, Tokens: backstop[t].String(), asset: assets[t]})
	}
}

// addProtocolBackstop adds one pool's backstop tiers to the per-token sums; a Blend tier is denominated in the backstop's own
// token, a BLNT tier in the token it names. assets records each token's BackstopAsset so the sum is valued as its tiers would be.
func addProtocolBackstop(st *LendingProtocolStats, pr *protocolPoolRead, sums map[string]*big.Int, assets map[string]string, tokens []string) []string {
	pool := pr.pool
	exclude := func(reason string) {
		st.Excluded = append(st.Excluded, LendingStatsExclusion{Kind: "backstop", Pool: &pool, Reason: reason})
	}
	if pr.backstopReason != "" {
		exclude(pr.backstopReason)
		return tokens
	}
	if pr.backstop.Instance == nil || pr.backstop.Instance.Backstop == "" {
		exclude(backstopReasonNoState)
		return tokens
	}
	for _, t := range pr.backstop.Tiers {
		token := t.Token
		if token == "" {
			token = pr.backstop.Token
		}
		if t.Balance == nil || token == "" {
			exclude(exclusionBackstopNoBalance)
			continue
		}
		if sums[token] == nil {
			sums[token] = new(big.Int)
			tokens = append(tokens, token)
			assets[token] = t.Asset
		} else if assets[token] != t.Asset {
			assets[token] = "" // one token named as two asset kinds: withhold rather than pick one
		}
		sums[token].Add(sums[token], t.Balance.Tokens)
	}
	return tokens
}

// readProtocolToken resolves ticker through the verified catalogue and reads it through the /v1/assets/{id} handler and its
// response cache, so the figures are that route's by construction. A non-empty reason means the block is absent or incomplete.
func (s *Server) readProtocolToken(ctx context.Context, ticker string, flags *Flags) (*LendingProtocolToken, string) {
	var a canonical.Asset
	found := false
	if vc, ok := s.VerifiedCurrencies.LookupByTicker(ticker); ok {
		for _, n := range vc.Issuance {
			if parsed, err := canonical.ParseAsset(n.AssetID); err == nil && n.Network == "stellar" && parsed.Code == ticker && parsed.Issuer != "" {
				a, found = parsed, true
				break
			}
		}
	}
	if !found {
		return nil, exclusionTokenNotVerified
	}
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/v1/assets/"+a.String()+"?include_thin=true", nil)
	req.SetPathValue("asset_id", a.String())
	rec := httptest.NewRecorder()
	s.handleAssetGet(rec, req)
	var body struct {
		Data struct {
			Decimals          int                `json:"decimals"`
			PriceUSD          *string            `json:"price_usd"`
			CirculatingSupply *string            `json:"circulating_supply"`
			TotalSupply       *string            `json:"total_supply"`
			MarketCapUSD      *string            `json:"market_cap_usd"`
			ThinMarket        bool               `json:"thin_market"`
			Substance         *SubstanceEvidence `json:"substance"`
		} `json:"data"`
		Flags Flags `json:"flags"`
	}
	degraded := rec.Header().Get("Cache-Control") == "no-store" // Flags.Degraded never reaches the wire
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
		s.logger.Warn("lending protocols: token read failed", "asset", a.String(), "status", rec.Code)
		return nil, exclusionTokenReadFailed
	}
	d := body.Data
	tok := &LendingProtocolToken{
		AssetID: a.String(), Code: a.Code, Issuer: a.Issuer, Decimals: d.Decimals,
		PriceUSD: d.PriceUSD, CirculatingSupply: d.CirculatingSupply, TotalSupply: d.TotalSupply, MarketCapUSD: d.MarketCapUSD,
		ThinMarket: d.ThinMarket, Substance: d.Substance,
	}
	if tok.ThinMarket {
		tok.MarketCapUSD = nil // the asset route derives none from a thin price; held here too so no later change can
	}
	flags.Stale = flags.Stale || body.Flags.Stale
	// The asset body is degraded by reads this route does not serve too; only a missing served figure counts as a failure.
	if degraded && (d.PriceUSD == nil || d.CirculatingSupply == nil) {
		return tok, exclusionTokenReadFailed
	}
	return tok, ""
}
