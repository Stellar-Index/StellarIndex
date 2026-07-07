package v1

import (
	"context"
	"strings"
)

// ProtocolPoolTokensReader maps a pool-based protocol's contracts to the
// token contract C-strkeys each holds, so the /v1/protocols/{name} roster can
// render a human asset pair ("XLM/USDC") instead of two raw C-strkeys.
// Production wiring is timescale.Store.PoolTokens. Nil / a nil result → the
// roster still serves, just without the pair label (soroswap keeps its own
// token0/token1 path). Never fails the roster.
type ProtocolPoolTokensReader interface {
	// PoolTokens returns pool contract_id → ordered token contract C-strkeys
	// for `source`, or (nil, nil) for a source without a per-token table.
	PoolTokens(ctx context.Context, source string) (map[string][]string, error)
}

// tokenSymbolResolver turns a token contract C-strkey into a human display
// symbol ("XLM", "USDC", "AQUA", …), caching within its lifetime because a
// protocol's pools share the same handful of tokens. It resolves entirely
// in-memory off the verified-currency catalogue (SAC → classic/native asset
// → ticker) — no lake read — and NEVER fails: an unresolvable token degrades
// to a short truncated contract ("CAS3…OWMA"), so the roster always renders.
type tokenSymbolResolver struct {
	s     *Server
	cache map[string]string
}

func (s *Server) newTokenSymbolResolver() *tokenSymbolResolver {
	return &tokenSymbolResolver{s: s, cache: map[string]string{}}
}

// symbol returns the display symbol for a token contract, memoised.
func (r *tokenSymbolResolver) symbol(contractC string) string {
	if contractC == "" {
		return ""
	}
	if sym, ok := r.cache[contractC]; ok {
		return sym
	}
	sym := r.resolve(contractC)
	r.cache[contractC] = sym
	return sym
}

// resolve is the uncached resolution: SAC → canonical asset → ticker, else a
// truncated contract fallback.
func (r *tokenSymbolResolver) resolve(contractC string) string {
	if id, ok := r.s.resolveSACAsset(contractC); ok {
		if id == "native" {
			return "XLM"
		}
		if r.s.verifiedCurrencies != nil {
			if vc, ok := r.s.verifiedCurrencies.LookupByStellarAssetID(id); ok && vc.Ticker != "" {
				return vc.Ticker
			}
		}
		// Known SAC but not a catalogued currency — fall back to the classic
		// asset CODE (CODE-ISSUER → CODE) rather than the raw contract.
		if code, _, cut := strings.Cut(id, "-"); cut && code != "" {
			return code
		}
	}
	return truncContract(contractC)
}

// truncContract renders a graceful fallback label for an unresolvable token:
// "CAS3J7GY…XOWMA" collapses to "CAS3…OWMA" (first 4 + last 4). Short/empty
// inputs pass through unchanged.
func truncContract(c string) string {
	if len(c) <= 8 {
		return c
	}
	return c[:4] + "…" + c[len(c)-4:]
}

// enrichContractTokens fills the human-pair fields (Tokens / TokenSymbols /
// Pair) on every roster row of a pool-based protocol. Contract order:
//   - soroswap rows already carry Token0/Token1 → those become Tokens;
//   - other pool protocols look their tokens up in the PoolTokens map;
//   - lending (blend) rows resolve their reserve-asset set the same way.
//
// Best-effort and empty-safe: a nil reader, a read error, or a pool with no
// resolvable tokens leaves that row's pair fields absent — the roster always
// renders. The symbol resolver is shared across the roster so shared tokens
// resolve once.
func (s *Server) enrichContractTokens(ctx context.Context, meta ProtocolMeta, contracts []ProtocolContractView) {
	if len(contracts) == 0 {
		return
	}
	poolTokens := s.poolTokenMap(ctx, meta.Name)
	resolver := s.newTokenSymbolResolver()
	for i := range contracts {
		c := &contracts[i]
		raw := rawPoolTokens(c, poolTokens)
		if len(raw) == 0 {
			continue
		}
		c.Tokens = raw
		syms := make([]string, len(raw))
		for j, t := range raw {
			syms[j] = resolver.symbol(t)
		}
		c.TokenSymbols = syms
		c.Pair = strings.Join(syms, "/")
	}
}

// poolTokenMap loads the source's pool→tokens map, degrading to nil (roster
// still renders, just without pairs) on a nil reader or read error. soroswap
// returns nil here — its rows carry token0/token1 directly.
func (s *Server) poolTokenMap(ctx context.Context, source string) map[string][]string {
	if source == "soroswap" || s.protocolPoolTokens == nil {
		return nil
	}
	m, err := s.protocolPoolTokens.PoolTokens(ctx, source)
	if err != nil {
		s.logger.Warn("protocol pool-tokens read failed", "source", source, "err", err)
		return nil
	}
	return m
}

// rawPoolTokens returns the ordered raw token contract C-strkeys for one
// roster row: soroswap's existing token0/token1 pair, else the PoolTokens
// lookup keyed by the contract id. Empty when neither is available.
func rawPoolTokens(c *ProtocolContractView, poolTokens map[string][]string) []string {
	if c.Token0 != "" || c.Token1 != "" {
		out := make([]string, 0, 2)
		if c.Token0 != "" {
			out = append(out, c.Token0)
		}
		if c.Token1 != "" {
			out = append(out, c.Token1)
		}
		return out
	}
	return poolTokens[c.ContractID]
}
