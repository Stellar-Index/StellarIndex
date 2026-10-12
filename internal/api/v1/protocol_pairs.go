package v1

import (
	"context"
	"strings"
)

// ProtocolPoolTokensReader maps a pool-based protocol's contracts to their token C-strkeys for the roster pair label; nil or empty never fails the roster.
type ProtocolPoolTokensReader interface {
	// PoolTokens returns pool contract_id → ordered token contract C-strkeys
	// for `source`, or (nil, nil) for a source without a per-token table.
	PoolTokens(ctx context.Context, source string) (map[string][]string, error)
}

// tokenSymbolResolver turns a token contract C-strkey into a display symbol from the in-memory verified-currency catalogue (no lake
// read), caching per token.
// It never fails: an unresolvable token degrades to a truncated contract ("CAS3...OWMA").
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
		if r.s.VerifiedCurrencies != nil {
			if vc, ok := r.s.VerifiedCurrencies.LookupByStellarAssetID(id); ok && vc.Ticker != "" {
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

// truncContract is the fallback label for an unresolvable token: first 4 + last 4 ("CAS3...OWMA"); short or empty input passes through.
func truncContract(c string) string {
	if len(c) <= 8 {
		return c
	}
	return c[:4] + "…" + c[len(c)-4:]
}

// enrichContractTokens fills the pair fields (Tokens / TokenSymbols / Pair) on every roster row of a pool-based protocol: Soroswap rows
// carry Token0/Token1, others use PoolTokens.
// Best-effort: a nil reader, read error or unresolvable pool leaves that row's fields absent. Reports false when the pool-token read
// failed.
func (s *Server) enrichContractTokens(ctx context.Context, meta ProtocolMeta, contracts []ProtocolContractView) (ok bool) {
	if len(contracts) == 0 {
		return true
	}
	poolTokens, ok := s.poolTokenMap(ctx, meta.Name)
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
	return ok
}

// poolTokenMap loads the source's pool-to-tokens map, degrading to nil (roster renders without pairs) on a nil reader or read error; ok
// is false only for the read error.
// soroswap returns nil: its rows carry token0/token1 directly.
func (s *Server) poolTokenMap(ctx context.Context, source string) (m map[string][]string, ok bool) {
	if source == "soroswap" || s.protocolPoolTokens == nil {
		return nil, true
	}
	m, err := s.protocolPoolTokens.PoolTokens(ctx, source)
	if err != nil {
		s.logger.Warn("protocol pool-tokens read failed", "source", source, "err", err)
		return nil, false
	}
	return m, true
}

// rawPoolTokens returns a roster row's ordered raw token C-strkeys: soroswap's token0/token1, else the PoolTokens lookup by contract id;
// empty when neither exists.
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
