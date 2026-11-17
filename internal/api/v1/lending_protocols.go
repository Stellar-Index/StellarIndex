package v1

import (
	"context"
	"math/big"
	"net/http"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// lendingProtocolTokens are the lending protocols in serve order, each with its token's canonical (code, issuer) id.
var lendingProtocolTokens = []struct{ protocol, asset string }{
	{"blend", "BLND-GDJEHTBE6ZHUXSWFI642DCGLUOECLHPF3KSXHPXTSTJ7E3JF6MQ5EZYY"},
	{blnt.SourceName, "BLNT-GAP2ACGH3O6TRFAERGM4USJ6XWU3U2BFIK224JPV6NI3CUNEEEGTBTFB"},
}

// LendingProtocolStats is one GET /v1/lending/protocols entry: a lending protocol's token and its pools' totals, summed from the
// same cached per-pool view /v1/lending/pools/{pool}/reserves serves.
type LendingProtocolStats struct {
	Protocol         string               `json:"protocol"`
	Token            LendingProtocolToken `json:"token"`
	PoolCount        int                  `json:"pool_count"`
	ReserveCount     int                  `json:"reserve_count"`
	TotalSuppliedUSD *string              `json:"total_supplied_usd"`
	TotalBorrowedUSD *string              `json:"total_borrowed_usd"`
	// TVLUSD is supplied minus borrowed: what the pools hold, not what they have lent out.
	TVLUSD   *string                   `json:"tvl_usd"`
	Backstop []LendingProtocolBackstop `json:"backstop"`
	// LowerBound is true when Excluded is non-empty: every total leaves something out and the protocol holds at least it.
	LowerBound bool                    `json:"lower_bound"`
	Excluded   []LendingStatsExclusion `json:"excluded"`
}

// LendingProtocolToken is the protocol token's price, supply and market cap, read as /v1/assets/{id} reads them.
type LendingProtocolToken struct {
	AssetID           string  `json:"asset_id"`
	Code              string  `json:"code"`
	Issuer            string  `json:"issuer"`
	Decimals          int     `json:"decimals"`
	PriceUSD          *string `json:"price_usd"`
	CirculatingSupply *string `json:"circulating_supply"`
	TotalSupply       *string `json:"total_supply"`
	MarketCapUSD      *string `json:"market_cap_usd"`
}

// LendingProtocolBackstop is the backstop deposits across the protocol's pools in one backstop token, in its base units.
type LendingProtocolBackstop struct {
	BackstopToken string `json:"backstop_token"`
	Tokens        string `json:"tokens"`
}

// LendingStatsExclusion names what a protocol's totals leave out and why.
type LendingStatsExclusion struct {
	Kind   string  `json:"kind"` // pool_list | pool | reserve | backstop
	Pool   *string `json:"pool"`
	Asset  *string `json:"asset"`
	Reason string  `json:"reason"`
}

// protocolPoolRead is one pool's cached reserves view and its backstop read.
type protocolPoolRead struct {
	ratePoolRead
	backstop       clickhouse.LendingBackstopState
	backstopReason string
}

// handleLendingProtocols serves GET /v1/lending/protocols: per protocol, its token and its pools' USD totals. A pool, reserve or
// backstop that could not be read or priced is named in excluded and the totals are a lower bound.
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
	var vintage dataVintage
	pools, flags, err := s.lendingRatePools(ctx, "", &vintage)
	if err != nil {
		s.writeListBlendPoolsError(ctx, w, r, err)
		return
	}
	blntListFailed := flags.Degraded
	reads := make([]protocolPoolRead, len(pools))
	forEachBounded(s.logger, len(pools), readFanoutConcurrency, func(i int) {
		reads[i] = s.readProtocolPool(ctx, pools[i]) // distinct indices — no mutex needed
	})
	out := make([]LendingProtocolStats, 0, len(lendingProtocolTokens))
	for _, pt := range lendingProtocolTokens {
		st := LendingProtocolStats{Protocol: pt.protocol, Backstop: []LendingProtocolBackstop{}, Excluded: []LendingStatsExclusion{}}
		tokenFlags := s.fillProtocolToken(ctx, &st.Token, pt.asset)
		flags.Stale, flags.Degraded = flags.Stale || tokenFlags.Stale, flags.Degraded || tokenFlags.Degraded
		if pt.protocol == blnt.SourceName && blntListFailed {
			st.Excluded = append(st.Excluded, LendingStatsExclusion{Kind: "pool_list", Reason: "BLNT pool list read failed"})
		}
		sumProtocolPools(&st, reads, &vintage, &flags)
		st.LowerBound = len(st.Excluded) > 0
		flags.Degraded = flags.Degraded || st.LowerBound
		out = append(out, st)
	}
	writeEnvelope(w, Envelope{Data: out, AsOf: vintage.asOf(), Flags: flags})
}

// readProtocolPool reads one pool through the reserves cache and, when that succeeds, its backstop balances.
func (s *Server) readProtocolPool(ctx context.Context, p ratePool) protocolPoolRead {
	out := protocolPoolRead{ratePoolRead: s.readRatePool(ctx, p)}
	if out.omitted != "" {
		return out
	}
	br, ok := s.Explorer.(lendingBackstopReader)
	switch {
	case !ok:
		out.backstopReason = "backstop reader unavailable"
	case p.protocol != blnt.SourceName && out.res.version != blend.PoolV2:
		out.backstopReason = backstopReasonV1
	default:
		var err error
		if out.backstop, err = br.LendingBackstop(ctx, p.pool, p.protocol == blnt.SourceName); err != nil {
			s.logger.Warn("lending protocols: backstop read failed", "err", err, "pool", p.pool)
			out.backstopReason = "backstop read failed"
		}
	}
	return out
}

// sumProtocolPools totals st.Protocol's pools in exact decimals. A reserve counts only with both USD values, so TVL stays
// supplied minus borrowed over the same reserves.
func sumProtocolPools(st *LendingProtocolStats, reads []protocolPoolRead, vintage *dataVintage, flags *Flags) {
	supplied, borrowed := new(big.Rat), new(big.Rat)
	priced := 0
	backstop := map[string]*big.Int{}
	var tokens []string
	for i := range reads {
		pr := &reads[i]
		if pr.protocol != st.Protocol {
			continue
		}
		st.PoolCount++
		pool := pr.pool
		if pr.omitted != "" {
			st.Excluded = append(st.Excluded, LendingStatsExclusion{Kind: "pool", Pool: &pool, Reason: pr.omitted})
			continue
		}
		vintage.note(pr.at)
		flags.Stale = flags.Stale || pr.res.flags.Stale || pr.cacheStale
		flags.Degraded = flags.Degraded || pr.res.flags.Degraded
		v := pr.res.view
		st.ReserveCount += len(v.Reserves) + len(v.OmittedReserves)
		for _, a := range v.OmittedReserves {
			st.Excluded = append(st.Excluded, LendingStatsExclusion{Kind: "reserve", Pool: &pool, Asset: strRef(a), Reason: "no decoded reserve state"})
		}
		for _, rv := range v.Reserves {
			sup, supOK := parseUSD(rv.SuppliedUSD)
			bor, borOK := parseUSD(rv.BorrowedUSD)
			if !supOK || !borOK {
				st.Excluded = append(st.Excluded, LendingStatsExclusion{Kind: "reserve", Pool: &pool, Asset: strRef(rv.Asset), Reason: "no USD value"})
				continue
			}
			supplied.Add(supplied, sup)
			borrowed.Add(borrowed, bor)
			priced++
		}
		tokens = addProtocolBackstop(st, pr, backstop, tokens)
	}
	if priced > 0 {
		sup, bor, tvl := supplied.FloatString(2), borrowed.FloatString(2), new(big.Rat).Sub(supplied, borrowed).FloatString(2)
		st.TotalSuppliedUSD, st.TotalBorrowedUSD, st.TVLUSD = &sup, &bor, &tvl
	}
	for _, t := range tokens {
		st.Backstop = append(st.Backstop, LendingProtocolBackstop{BackstopToken: t, Tokens: backstop[t].String()})
	}
}

// addProtocolBackstop adds one pool's backstop tiers to the per-token sums; a Blend tier is denominated in the backstop's own
// token, a BLNT tier in the token it names.
func addProtocolBackstop(st *LendingProtocolStats, pr *protocolPoolRead, sums map[string]*big.Int, tokens []string) []string {
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
			exclude("backstop tier balance unread")
			continue
		}
		if sums[token] == nil {
			sums[token] = new(big.Int)
			tokens = append(tokens, token)
		}
		sums[token].Add(sums[token], t.Balance.Tokens)
	}
	return tokens
}

// fillProtocolToken reads the token's price, supply and market cap through the /v1/assets/{id} F2 overlay, so the two agree.
func (s *Server) fillProtocolToken(ctx context.Context, tok *LendingProtocolToken, id string) Flags {
	a, err := canonical.ParseAsset(id)
	if err != nil {
		s.logger.Error("lending protocols: protocol token id does not parse", "asset", id, "err", err)
		return Flags{Degraded: true}
	}
	d := detailFromAsset(a)
	s.applyF2Fields(ctx, &d, a)
	tok.AssetID, tok.Code, tok.Issuer, tok.Decimals = a.String(), a.Code, a.Issuer, d.Decimals
	tok.PriceUSD, tok.CirculatingSupply, tok.TotalSupply, tok.MarketCapUSD = d.PriceUSD, d.CirculatingSupply, d.TotalSupply, d.MarketCapUSD
	return Flags{Stale: d.priceStale || d.SupplyStale, Degraded: d.f2ReadFailed}
}
