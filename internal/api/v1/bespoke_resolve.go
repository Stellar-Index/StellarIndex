package v1

import (
	"context"
	"math/big"
	"strings"

	explorerpkg "github.com/Stellar-Index/StellarIndex/internal/api/v1/explorer"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// bespokeResolver names the ids in one bespoke block and finds amount scales; an id it cannot resolve stays an id, never a guess.
type bespokeResolver struct {
	s          *Server
	syms       *tokenSymbolResolver
	poolTokens map[string][]string
	metas      map[string]clickhouse.TokenDisplayMeta
}

// resolveBespoke fills the Display fields of b's typed series, breakdown rows and table cells; untyped entries are left as they are.
func (s *Server) resolveBespoke(ctx context.Context, source string, b *ProtocolBespoke) {
	if b == nil {
		return
	}
	r := s.newBespokeResolver(ctx, source, b)
	for i := range b.Series {
		b.Series[i].Display = r.label(b.Series[i].Kind, b.Series[i].ID)
	}
	for i := range b.Breakdowns {
		rows := b.Breakdowns[i].Rows
		for j := range rows {
			rows[j].Display = r.label(rows[j].Kind, rows[j].ID)
		}
	}
	for i := range b.Tables {
		r.resolveTable(&b.Tables[i])
	}
}

// newBespokeResolver batch-loads what b's typed ids need: pool token lists and the lake's token metadata for vaults and non-SAC tokens.
func (s *Server) newBespokeResolver(ctx context.Context, source string, b *ProtocolBespoke) *bespokeResolver {
	r := &bespokeResolver{s: s, syms: s.newTokenSymbolResolver()}
	ids := map[string]bool{}
	needPools := false
	note := func(kind, id string) {
		switch kind {
		case timescale.BespokeKindPool:
			needPools = true
		case timescale.BespokeKindVault, timescale.BespokeKindAsset:
			if canonical.IsContractID(id) {
				ids[id] = true
			}
		}
	}
	for _, e := range b.Series {
		note(e.Kind, e.ID)
	}
	for _, bd := range b.Breakdowns {
		for _, row := range bd.Rows {
			note(row.Kind, row.ID)
		}
	}
	for _, t := range b.Tables {
		for _, row := range t.Rows {
			for ci, k := range t.ColumnKinds {
				if ci < len(row) {
					note(k.Kind, row[ci])
				}
			}
		}
	}
	if needPools {
		r.poolTokens, _ = s.poolTokenMap(ctx, source)
	}
	r.metas = s.bespokeTokenMetas(ctx, ids)
	return r
}

// bespokeTokenMetas reads token metadata for ids in one lake query; a nil reader or failed read yields nil, so those ids stay unresolved.
func (s *Server) bespokeTokenMetas(ctx context.Context, ids map[string]bool) map[string]clickhouse.TokenDisplayMeta {
	if s.Explorer == nil || len(ids) == 0 {
		return nil
	}
	list := make([]string, 0, len(ids))
	for id := range ids {
		list = append(list, id)
	}
	mctx, cancel := context.WithTimeout(ctx, tokenMetadataReadTimeout)
	defer cancel()
	metas, err := s.Explorer.TokenDisplays(mctx, list)
	if err != nil {
		s.logger.Warn("bespoke token metadata read failed; ids served unresolved", "err", err)
		return nil
	}
	return metas
}

// label is the display name for a typed id, "" when kind carries no name or the id did not resolve.
func (r *bespokeResolver) label(kind, id string) string {
	if id == "" {
		return ""
	}
	switch kind {
	case timescale.BespokeKindAsset:
		sym, _ := r.assetLabel(id)
		return sym
	case timescale.BespokeKindPool:
		return r.poolLabel(id)
	case timescale.BespokeKindVault:
		if m, ok := r.metas[id]; ok && m.HasMeta {
			return explorerpkg.VaultLabel(m.Name, m.Symbol)
		}
	}
	return ""
}

// assetLabel tickers an asset id in any of its forms: a token contract, "native", "crypto:XLM" or "CODE-ISSUER".
// Contract-authored token symbols are not used: any contract may call itself USDC.
func (r *bespokeResolver) assetLabel(id string) (string, bool) {
	if canonical.IsContractID(id) {
		return r.syms.lookup(id)
	}
	if strings.EqualFold(id, "crypto:XLM") {
		return "XLM", true
	}
	a, err := canonical.ParseAsset(id)
	if err != nil || (a.Type != canonical.AssetNative && a.Type != canonical.AssetClassic) {
		return "", false
	}
	return r.s.assetIDTicker(a.String())
}

// poolLabel joins a pool's token tickers ("USDC / XLM"); "" when the pool's tokens are unknown.
func (r *bespokeResolver) poolLabel(id string) string {
	tokens := r.poolTokens[id]
	if len(tokens) == 0 {
		return ""
	}
	syms := make([]string, len(tokens))
	for i, t := range tokens {
		syms[i] = r.syms.symbol(t)
	}
	return strings.Join(syms, " / ")
}

// assetDecimals is an asset's on-chain scale: 7 for XLM, classic assets and SACs (fixed by the protocol), else the token's own
// decimals() from the lake; ok=false when unread, so the amount stays base units rather than scaled by a guessed 7.
func (r *bespokeResolver) assetDecimals(id string) (int, bool) {
	a, err := canonical.ParseAsset(id)
	if err != nil {
		return 0, false
	}
	switch a.Type {
	case canonical.AssetNative, canonical.AssetClassic:
		return defaultTokenDecimals, true
	case canonical.AssetSoroban:
		if _, sac := r.s.resolveSACAsset(a.ContractID); sac {
			return defaultTokenDecimals, true
		}
		if m, ok := r.metas[a.ContractID]; ok && m.HasMeta {
			return int(m.Decimals), true
		}
		return 0, false
	case canonical.AssetCrypto, canonical.AssetFiat, canonical.AssetOracleRaw, canonical.AssetRWA:
		return 0, false // off-chain ids have no on-chain scale; their amounts stay as served
	}
	return 0, false
}

// resolveTable fills t.Display from t.ColumnKinds; Display stays nil when no cell resolved.
func (r *bespokeResolver) resolveTable(t *BespokeTable) {
	if len(t.ColumnKinds) == 0 {
		return
	}
	display := make([][]string, len(t.Rows))
	resolved := false
	for ri, row := range t.Rows {
		cells := make([]string, len(row))
		for ci := range row {
			if ci >= len(t.ColumnKinds) {
				break
			}
			cells[ci] = r.cell(t.ColumnKinds[ci], row, ci)
			resolved = resolved || cells[ci] != ""
		}
		display[ri] = cells
	}
	if resolved {
		t.Display = display
	}
}

// cell is one typed cell's display text: an entity's name or an amount in human units, "" when neither resolved.
func (r *bespokeResolver) cell(k BespokeColumnKind, row []string, ci int) string {
	if k.Kind != timescale.BespokeKindAmount {
		return r.label(k.Kind, row[ci])
	}
	decimals, ok := 0, false
	switch {
	case k.Decimals != nil:
		decimals, ok = *k.Decimals, true
	case k.AssetColumn != nil && *k.AssetColumn >= 0 && *k.AssetColumn < len(row):
		decimals, ok = r.assetDecimals(row[*k.AssetColumn])
	}
	if !ok {
		return ""
	}
	scaled, ok := scaleBaseUnits(row[ci], decimals)
	if !ok {
		return ""
	}
	return scaled
}

// scaleBaseUnits renders an integer base-unit amount in human units as an exact decimal string, trailing zeros trimmed;
// ok=false for a non-integer cell or a negative scale.
func scaleBaseUnits(raw string, decimals int) (string, bool) {
	n, ok := new(big.Int).SetString(raw, 10)
	if !ok || decimals < 0 {
		return "", false
	}
	digits := new(big.Int).Abs(n).String()
	if len(digits) <= decimals {
		digits = strings.Repeat("0", decimals-len(digits)+1) + digits
	}
	out := digits[:len(digits)-decimals]
	if frac := strings.TrimRight(digits[len(digits)-decimals:], "0"); frac != "" {
		out += "." + frac
	}
	if n.Sign() < 0 {
		out = "-" + out
	}
	return out, true
}
