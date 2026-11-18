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
	poolTokens map[string][]string
	metas      map[string]clickhouse.TokenDisplayMeta
}

// resolveBespoke fills the Display fields of b's typed series, breakdown rows and table cells; untyped entries are left as they are.
// ok=false when the pool-token or token-metadata read failed: names and scales were dropped, so the page must not be cached as healthy.
func (s *Server) resolveBespoke(ctx context.Context, source string, b *ProtocolBespoke) (ok bool) {
	if b == nil {
		return true
	}
	r, ok := s.newBespokeResolver(ctx, source, b)
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
	return ok
}

// newBespokeResolver batch-loads what b's typed ids need: pool token lists and the lake's token metadata for vaults and non-SAC tokens.
// ok=false when either read failed.
func (s *Server) newBespokeResolver(ctx context.Context, source string, b *ProtocolBespoke) (*bespokeResolver, bool) {
	r := &bespokeResolver{s: s}
	needPools, ids := bespokeLookups(b)
	poolsOK := true
	if needPools {
		r.poolTokens, poolsOK = s.poolTokenMap(ctx, source)
	}
	metas, metasOK := s.bespokeTokenMetas(ctx, ids)
	r.metas = metas
	return r, poolsOK && metasOK
}

// bespokeLookups lists what b's typed ids need loaded: whether any pool is named, and the contract ids that need token metadata.
func bespokeLookups(b *ProtocolBespoke) (needPools bool, ids map[string]bool) {
	ids = map[string]bool{}
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
	return needPools, ids
}

// bespokeTokenMetas reads token metadata for ids in one lake query; a nil reader yields (nil, true) and a failed or timed-out read
// (nil, false), so those ids stay unresolved either way.
func (s *Server) bespokeTokenMetas(ctx context.Context, ids map[string]bool) (map[string]clickhouse.TokenDisplayMeta, bool) {
	if s.Explorer == nil || len(ids) == 0 {
		return nil, true
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
		return nil, false
	}
	return metas, true
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
		return r.pairLabel(r.poolTokens[id])
	case timescale.BespokeKindVault:
		if m, ok := r.metas[id]; ok && m.HasMeta {
			return explorerpkg.VaultLabel(m.Name, m.Symbol)
		}
	}
	return ""
}

// assetLabel tickers an asset id in any of its forms: a token contract, "native", "crypto:XLM" or "CODE-ISSUER".
// Only XLM and catalogued assets get a ticker: any issuer can mint a "USDC" code, and any contract may call itself USDC.
func (r *bespokeResolver) assetLabel(id string) (string, bool) {
	if canonical.IsContractID(id) {
		sac, ok := r.s.resolveSACAsset(id)
		if !ok {
			return "", false
		}
		return r.s.verifiedTicker(sac)
	}
	if strings.EqualFold(id, "crypto:XLM") {
		return "XLM", true
	}
	a, err := canonical.ParseAsset(id)
	if err != nil || (a.Type != canonical.AssetNative && a.Type != canonical.AssetClassic) {
		return "", false
	}
	return r.s.verifiedTicker(a.String())
}

// pairLabel joins token tickers ("USDC / XLM"); "" when there are no tokens or any one is unknown, never a half pair.
func (r *bespokeResolver) pairLabel(tokens []string) string {
	if len(tokens) == 0 {
		return ""
	}
	syms := make([]string, len(tokens))
	for i, t := range tokens {
		sym, ok := r.assetLabel(t)
		if !ok {
			return ""
		}
		syms[i] = sym
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
		return defaultTokenDecimals, true // decimals-7: native/classic, fixed by the protocol
	case canonical.AssetSoroban:
		if _, sac := r.s.resolveSACAsset(a.ContractID); sac {
			return defaultTokenDecimals, true // decimals-7: known SAC, fixed by the protocol
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
	switch k.Kind {
	case timescale.BespokeKindAmount:
		return r.amountCell(k, row, ci)
	case timescale.BespokeKindPool:
		if name := r.label(k.Kind, row[ci]); name != "" || len(k.poolTokenColumns) == 0 {
			return name
		}
		tokens := make([]string, 0, len(k.poolTokenColumns))
		for _, c := range k.poolTokenColumns {
			if c < 0 || c >= len(row) {
				return ""
			}
			tokens = append(tokens, row[c])
		}
		return r.pairLabel(tokens)
	}
	return r.label(k.Kind, row[ci])
}

// amountCell scales one amount cell by its column's fixed decimals or the same row's asset; "" when the scale is unknown.
func (r *bespokeResolver) amountCell(k BespokeColumnKind, row []string, ci int) string {
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
