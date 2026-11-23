package v1

import (
	"context"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A declaration (an issuer's stellar.toml entry, or an RWA member code) reaches an on-chain asset
// by exact (code, issuer); a case-insensitive match is taken only when no on-chain asset under the
// issuer is spelled as declared and exactly one folds to it. A trustline-only, zero-supply copy
// anyone can open therefore never takes from a correctly declared asset.

// classicCaseVariantReader lists an issuer's on-chain spellings of a code, type-asserted off
// [Server.Sep1Cache] like [sep1ImagesReader]; without it nothing folds.
type classicCaseVariantReader interface {
	ClassicCodesFoldingTo(ctx context.Context, keys []timescale.ClassicCodeKey) (map[timescale.ClassicCodeKey][]string, error)
}

// An optional seam that stops matching silently strips every mis-cased issuer's verification.
var _ classicCaseVariantReader = (*timescale.Store)(nil)

// matchAssetCode returns the index of the on-chain code spelled exactly want, else of the one code equal to want ignoring case,
// else -1; complete says codes are every on-chain code under the issuer, without which a fold is never safe.
func matchAssetCode(want string, codes []string, complete bool) int {
	fold := -1
	for i, c := range codes {
		if c == want {
			return i
		}
		if strings.EqualFold(c, want) {
			if fold != -1 {
				complete = false
			}
			fold = i
		}
	}
	if !complete {
		return -1
	}
	return fold
}

// onlySpelling reports whether the chain's spellings folding to code are exactly code itself.
func onlySpelling(spellings []string, code string) bool {
	return len(spellings) == 1 && spellings[0] == code
}

// findMatchingCachedCurrency returns the cached SEP-1 currency naming the asset's exact (code, issuer), nil if none.
func findMatchingCachedCurrency(sep *timescale.IssuerSep1Cached, asset canonical.Asset) *timescale.IssuerSep1Currency {
	if asset.Type != canonical.AssetClassic || asset.Code == "" || asset.Issuer == "" {
		return nil
	}
	for i := range sep.Currencies {
		if c := &sep.Currencies[i]; c.Issuer == asset.Issuer && c.Code == asset.Code {
			return c
		}
	}
	return nil
}

// foldDeclaredCurrency returns the one currency the issuer declares under a different case of the asset's code, nil if none or
// if two spellings are declared.
func foldDeclaredCurrency(sep *timescale.IssuerSep1Cached, asset canonical.Asset) *timescale.IssuerSep1Currency {
	var found *timescale.IssuerSep1Currency
	for i := range sep.Currencies {
		c := &sep.Currencies[i]
		if c.Issuer != asset.Issuer || !strings.EqualFold(c.Code, asset.Code) {
			continue
		}
		if found != nil && found.Code != c.Code {
			return nil
		}
		found = c
	}
	return found
}

// matchSep1Currency is [findMatchingCachedCurrency] plus the case-insensitive match the chain confirms unambiguous (one indexed
// probe, only when the exact match missed and a declaration folds). unread: that probe failed, so nothing folded.
func (s *Server) matchSep1Currency(
	ctx context.Context, sep *timescale.IssuerSep1Cached, asset canonical.Asset,
) (*timescale.IssuerSep1Currency, bool) {
	if c := findMatchingCachedCurrency(sep, asset); c != nil {
		return c, false
	}
	if asset.Type != canonical.AssetClassic || asset.Code == "" || asset.Issuer == "" {
		return nil, false
	}
	c := foldDeclaredCurrency(sep, asset)
	reader, ok := s.Sep1Cache.(classicCaseVariantReader)
	if c == nil || !ok {
		return nil, false
	}
	key := timescale.ClassicCodeKey{Issuer: asset.Issuer, Code: asset.Code}
	got, err := reader.ClassicCodesFoldingTo(ctx, []timescale.ClassicCodeKey{key})
	if err != nil {
		s.logger.Warn("sep1 case-variant read failed (no case-insensitive match)", "asset_id", asset.String(), "err", err)
		return nil, true
	}
	if onlySpelling(got[key], asset.Code) {
		return c, false
	}
	return nil, false
}

// sep1ImageFor looks a logo up by exact (code, issuer): a case variant may be a trustline-only, zero-supply copy anyone can open.
func sep1ImageFor(images map[string]string, code, issuer string) string {
	return images[sep1ImageKey(code, issuer)]
}

// sep1ImageFoldKey keys a logo the issuer declared under exactly one spelling of the code; the "fold:" prefix cannot collide with
// an exact key since codes are alphanumeric. Only [Server.fillImagesFromSep1] reads it, after the chain confirms the fold.
func sep1ImageFoldKey(code, issuer string) string {
	return "fold:" + strings.ToUpper(strings.TrimSpace(code)) + "-" + issuer
}

// buildSep1ImageMap keys each safe logo by exact (code, issuer), plus a [sep1ImageFoldKey] entry where one spelling is declared.
func buildSep1ImageMap(imgs []timescale.Sep1Image) map[string]string {
	built := make(map[string]string, len(imgs))
	spellings := map[string][]string{}
	for _, img := range imgs {
		if !isSafeImageURL(img.Image) {
			continue
		}
		k := sep1ImageKey(img.Code, img.Issuer)
		if _, dup := built[k]; !dup {
			fk := sep1ImageFoldKey(img.Code, img.Issuer)
			spellings[fk] = append(spellings[fk], k)
		}
		built[k] = img.Image
	}
	for fk, keys := range spellings {
		if len(keys) == 1 {
			built[fk] = built[keys[0]]
		}
	}
	return built
}

// fillImagesFromSep1 overlays the SEP-1 [[CURRENCIES]] logo URL onto rows lacking one, from the once-per-TTL logo map (the hot
// listing query deliberately does not JOIN sep1_payload). Best-effort: no verified image or no reader is a no-op. Rows matching
// only a fold key are confirmed in one batched store read (usually none); a failed read folds nothing.
func (s *Server) fillImagesFromSep1(ctx context.Context, rows []AssetDetail) {
	images := s.cachedSep1Images(ctx)
	if len(images) == 0 {
		return
	}
	var folds []int
	for i := range rows {
		if rows[i].Image != nil || rows[i].Issuer == nil || rows[i].Code == "" {
			continue
		}
		if img := sep1ImageFor(images, rows[i].Code, *rows[i].Issuer); img != "" {
			rows[i].Image = &img
		} else if _, ok := images[sep1ImageFoldKey(rows[i].Code, *rows[i].Issuer)]; ok {
			folds = append(folds, i)
		}
	}
	reader, ok := s.Sep1Cache.(classicCaseVariantReader)
	if len(folds) == 0 || !ok {
		return
	}
	keys := make([]timescale.ClassicCodeKey, len(folds))
	for j, i := range folds {
		keys[j] = timescale.ClassicCodeKey{Issuer: *rows[i].Issuer, Code: rows[i].Code}
	}
	got, err := reader.ClassicCodesFoldingTo(ctx, keys)
	if err != nil {
		s.logger.Warn("sep1 logo case-variant read failed (no case-insensitive logos)", "err", err)
		return
	}
	for j, i := range folds {
		if onlySpelling(got[keys[j]], rows[i].Code) {
			img := images[sep1ImageFoldKey(rows[i].Code, *rows[i].Issuer)]
			rows[i].Image = &img
		}
	}
}
