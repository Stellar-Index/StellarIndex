package v1

import (
	"context"
	"math/big"
	"net/http"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
)

// OracleReader is the storage-side interface for /v1/oracle/latest lookups.
type OracleReader interface {
	// LatestOracleUpdatesForAsset returns the most-recent observation per source for asset;
	// sourceFilter="" returns every source. Empty slice + nil error means "no observations".
	LatestOracleUpdatesForAsset(ctx context.Context, asset canonical.Asset, sourceFilter string) ([]canonical.OracleUpdate, error)

	// LatestOracleUpdatesForAssets is the multi-key variant: the most-recent observation per source
	// across the union of keys, so user-facing ids (`native`) reach per-oracle forms (`crypto:XLM`).
	LatestOracleUpdatesForAssets(ctx context.Context, assets []canonical.Asset, sourceFilter string) ([]canonical.OracleUpdate, error)

	// LatestOracleStreams returns one row per (source, asset, quote)
	// triple — the most-recent observation in the trailing 7d
	// window. Backs /v1/oracles/streams (the "every active price
	// stream from every oracle" listing for the explorer).
	LatestOracleStreams(ctx context.Context) ([]canonical.OracleUpdate, error)
}

// oracleAtReader is what the TTL cache adds to OracleReader: each read also
// returns when the served rows were computed, which the handlers stamp as as_of.
type oracleAtReader interface {
	LatestOracleUpdatesForAssetsAt(ctx context.Context, assets []canonical.Asset, sourceFilter string) ([]canonical.OracleUpdate, time.Time, error)
	LatestOracleStreamsAt(ctx context.Context) ([]canonical.OracleUpdate, time.Time, error)
}

// oracleAssetCandidates expands an asset into every key form the oracle layer may hold (Reflector keys by global ticker, e.g.
// `crypto:XLM`), through
// [canonical.AssetAliases]; the original comes first. The `crypto:<TICKER>` translation is IDENTITY-GATED: on CODE alone a classic asset
// spelling a ticker
// would get the REAL issuer's rows (attacker-authored pricing), so identity is (code, issuer) and only the catalogue's own issuance may
// claim it.
// A Soroban contract id resolves FIRST through the deterministic SAC derivation (ADR-0013), then faces the same gate.
func (s *Server) oracleAssetCandidates(a canonical.Asset) []canonical.Asset {
	candidates := canonical.AssetAliases(a)

	// SAC wrapper → the native/classic asset it provably wraps.
	if a.Type == canonical.AssetSoroban {
		wrapped, ok := s.verifiedSACAsset(a.ContractID)
		if !ok {
			return candidates
		}
		candidates = appendAssetsUnique(candidates, canonical.AssetAliases(wrapped)...)
		a = wrapped
	}

	// Verified classic credit asset: also try `crypto:<TICKER>` so global stablecoin tickers match
	// Reflector's per-ticker rows (an untracked key just yields zero rows). The granted ticker is
	// expanded through its own family too, since a feed may use a renamed spelling (crypto:EUROC).
	if ticker, ok := s.verifiedTickerFor(a); ok {
		if x, err := canonical.ParseAsset("crypto:" + ticker); err == nil {
			candidates = appendAssetsUnique(candidates, canonical.AssetAliases(x)...)
		}
	}
	return candidates
}

// appendAssetsUnique appends each of add not already in dst, by canonical string.
func appendAssetsUnique(dst []canonical.Asset, add ...canonical.Asset) []canonical.Asset {
	for _, a := range add {
		dup := false
		for _, d := range dst {
			if d.String() == a.String() {
				dup = true
				break
			}
		}
		if !dup {
			dst = append(dst, a)
		}
	}
	return dst
}

// verifiedTickerFor returns the global ticker a CLASSIC asset may be answered under: non-empty only when this exact (code, issuer) is the
// catalogue's
// own Stellar issuance. A nil catalogue or unverified issuer fails CLOSED; `reference_only` tickers (USDT, BTC) are never granted: any
// classic `USDT-G...` impersonates.
func (s *Server) verifiedTickerFor(a canonical.Asset) (string, bool) {
	if a.Type != canonical.AssetClassic || a.Code == "" || a.Issuer == "" {
		return "", false
	}
	vc, ok := s.VerifiedCurrencies.LookupByStellarAssetID(a.String())
	if !ok || vc.Ticker == "" {
		return "", false
	}
	return vc.Ticker, true
}

// verifiedSACAsset maps a contract id back to the native or verified classic asset whose SAC it is. The trust anchor is DERIVATION, not
// metadata:
// the same internal/xdrjson call as [Server.buildKnownSACs] (v1 must not import the SDK xdr package, ADR-0013). Cached; empty without a
// passphrase (fail-closed).
func (s *Server) verifiedSACAsset(contractID string) (canonical.Asset, bool) {
	s.verifiedSACsOnce.Do(func() { s.verifiedSACs = s.buildVerifiedSACs() })
	a, ok := s.verifiedSACs[contractID]
	return a, ok
}

func (s *Server) buildVerifiedSACs() map[string]canonical.Asset {
	set := make(map[string]canonical.Asset)
	if s.NetworkPassphrase == "" {
		return set
	}
	add := func(assetID string) {
		a, err := canonical.ParseAsset(assetID)
		if err != nil {
			return
		}
		if cid, ok := xdrjson.SACContractID(assetID, s.NetworkPassphrase); ok {
			set[cid] = a
		}
	}
	add("native")
	if s.VerifiedCurrencies != nil {
		for _, vc := range s.VerifiedCurrencies.Browseable() {
			if se := vc.StellarEntry(); se != nil && se.AssetID != "" {
				add(se.AssetID)
			}
		}
	}
	return set
}

// OracleReading is a /v1/oracle/latest entry; Price is a decimal string scaled by Decimals, with the raw integer and scale also reported.
type OracleReading struct {
	Source     string   `json:"source"`
	ContractID string   `json:"contract_id,omitempty"`
	Asset      string   `json:"asset"`
	Quote      string   `json:"quote"`
	Timestamp  WireTime `json:"ts"`

	// Price is the human-facing decimal string at Decimals scale.
	Price string `json:"price"`

	// PriceRaw is the integer form of Price at Decimals scale, so a
	// client can verify the rendering. For a feed published inverted it
	// is the reciprocal we derived, not the publisher's value; see
	// PriceOnchain.
	PriceRaw string `json:"price_raw"`

	// PriceOnchain is the publisher's own integer at Decimals scale, in
	// the publisher's orientation, when Price was derived from it by
	// inversion (ADR-0003 — the 8-dp reciprocal is lossy). Absent when no
	// separate published value was recorded.
	PriceOnchain *string `json:"price_onchain,omitempty"`

	// Decimals is the source-declared scale. 14 for Reflector.
	Decimals uint8 `json:"decimals"`

	// Confidence is the oracle's own confidence score (0–1) when
	// published. Zero means "not reported", not "zero confidence."
	Confidence float64 `json:"confidence,omitempty"`

	// Observer is the on-chain address (account or contract) that
	// published the update. Empty when unknown.
	Observer string `json:"observer,omitempty"`

	// Mapped is false for a `raw:<symbol>` row: an oracle symbol recorded verbatim because it maps
	// to no canonical asset. Such rows are reference-only (orientation-unknown, never compared or
	// aggregated). /v1/oracle/streams omits them unless include_unmapped=true; /v1/oracle/latest
	// returns one only for its exact `raw:` key.
	Mapped bool `json:"mapped"`
	// Credit names the vendor behind Source when it is a licensed off-chain feed; absent for on-chain oracles.
	Credit string `json:"credit,omitempty"`
}

// handleOracleLatest serves GET /v1/oracle/latest?asset=&source=&quote=: one OracleReading per (source, quote), so MORE than one per
// source when it
// publishes several quotes; the source filter is on-chain only (sourceFilterOK). No observations is a 200 with an empty array.
func (s *Server) handleOracleLatest(w http.ResponseWriter, r *http.Request) {
	reader := s.Oracle
	if reader == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/oracle-unavailable",
			"Oracle readings not configured", http.StatusServiceUnavailable,
			"this deployment has no OracleReader wired — check binary configuration")
		return
	}

	rawAsset := r.URL.Query().Get("asset")
	if rawAsset == "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/missing-asset",
			"Missing asset parameter", http.StatusBadRequest,
			"asset query parameter is required. For the full per-source latest snapshot across every observed asset, call /v1/oracle/streams instead — it returns one row per (source, asset, quote) triple in the trailing 7d.")
		return
	}
	asset, err := canonical.ParseAsset(rawAsset)
	if err != nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-asset-id",
			"Invalid asset identifier", http.StatusBadRequest,
			err.Error())
		return
	}

	var quoteFilter canonical.Asset
	rawQuote := r.URL.Query().Get("quote") // optional
	if rawQuote != "" {
		// Some sources publish the SAME (source, asset) pair against
		// two distinct live quotes — e.g. Redstone's EUROC/EUR and
		// EUROC/USD (feeds.go) — so an unfiltered read can return more
		// than one row per source. ?quote= disambiguates.
		quoteFilter, err = canonical.ParseAsset(rawQuote)
		if err != nil {
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/invalid-asset-id",
				"Invalid quote identifier", http.StatusBadRequest,
				err.Error())
			return
		}
	}

	source := r.URL.Query().Get("source")
	if !sourceFilterOK(w, r, source) {
		return
	}

	olCtx, olCancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer olCancel()
	var vintage dataVintage
	var updates []canonical.OracleUpdate
	if at, ok := reader.(oracleAtReader); ok {
		var filled time.Time
		updates, filled, err = at.LatestOracleUpdatesForAssetsAt(olCtx, s.oracleAssetCandidates(asset), source)
		vintage.note(filled)
	} else {
		updates, err = reader.LatestOracleUpdatesForAssets(olCtx, s.oracleAssetCandidates(asset), source)
		vintage.note(time.Time{})
	}
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		if handlerTimedOut(olCtx, err) {
			s.logger.Warn("LatestOracleUpdatesForAsset deadline exceeded",
				"asset", asset.String(), "source", source)
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/oracle-latest-timeout",
				"Oracle latest query timed out", http.StatusServiceUnavailable,
				"the oracle_updates hypertable scan didn't return in 8s; retry shortly.")
			return
		}
		if IsCacheUnavailable(err) {
			s.logger.Warn("LatestOracleUpdatesForAsset cache unavailable",
				"err", err, "asset", asset.String(), "source", source)
			writeCacheUnavailableProblem(w, r)
			return
		}
		s.logger.Error("LatestOracleUpdatesForAsset failed",
			"err", err, "asset", asset.String(), "source", source)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}

	writeEnvelope(w, Envelope{Data: latestOracleReadings(updates, rawQuote != "", quoteFilter), AsOf: vintage.asOf()})
}

// latestOracleReadings keeps the rows for quote (when filtered) from sources still live: a retired feed's last row would read
// as a live price frozen in time.
func latestOracleReadings(updates []canonical.OracleUpdate, filterQuote bool, quote canonical.Asset) []OracleReading {
	rows := make([]OracleReading, 0, len(updates))
	for _, u := range updates {
		if filterQuote && !u.Quote.Equal(quote) {
			continue
		}
		if external.Registry[u.Source].Retired {
			continue
		}
		rows = append(rows, oracleReadingFrom(u))
	}
	return rows
}

// handleOracleStreams serves GET /v1/oracle/streams[?include_unmapped=true]: the latest observation per (source, asset, quote) over the
// trailing 7d.
// include_unmapped (default false) adds `raw:<symbol>` rows (mapped:false); storage is unfiltered, so a raw row is never silently
// dropped.
func (s *Server) handleOracleStreams(w http.ResponseWriter, r *http.Request) {
	reader := s.Oracle
	if reader == nil {
		writeJSON(w, []OracleReading{}, Flags{})
		return
	}
	// 8s ceiling on the oracle_updates hypertable scan, matching the other hypertable-scanning
	// endpoints. Steady state is well under 1s, but a cold-cache 7d scan can take 5-10s.
	osCtx, osCancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer osCancel()
	var vintage dataVintage
	var updates []canonical.OracleUpdate
	var err error
	if at, ok := reader.(oracleAtReader); ok {
		var filled time.Time
		updates, filled, err = at.LatestOracleStreamsAt(osCtx)
		vintage.note(filled)
	} else {
		updates, err = reader.LatestOracleStreams(osCtx)
		vintage.note(time.Time{})
	}
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		if handlerTimedOut(osCtx, err) {
			s.logger.Warn("LatestOracleStreams deadline exceeded")
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/oracle-streams-timeout",
				"Oracle streams query timed out", http.StatusServiceUnavailable,
				"the oracle_updates hypertable scan didn't return in 8s; retry shortly.")
			return
		}
		if IsCacheUnavailable(err) {
			s.logger.Warn("LatestOracleStreams cache unavailable", "err", err)
			writeCacheUnavailableProblem(w, r)
			return
		}
		s.logger.Error("LatestOracleStreams failed", "err", err)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}
	// Keep only ClassOracle sources: CoinGecko (aggregator) and ECB (sanity feed) share the
	// oracle_updates hypertable for divergence comparison but are not oracles. The class is a
	// registry fact, not a per-row flag, so filter at the wire boundary.
	includeUnmapped := r.URL.Query().Get("include_unmapped") == "true"
	rows := make([]OracleReading, 0, len(updates))
	for _, u := range updates {
		if external.Lookup(u.Source).Class != external.ClassOracle {
			continue
		}
		if !includeUnmapped && !u.Asset.IsMapped() {
			continue
		}
		rows = append(rows, oracleReadingFrom(u))
	}
	writeEnvelope(w, Envelope{Data: rows, AsOf: vintage.asOf()})
}

// oracleReadingFrom converts canonical.OracleUpdate → wire shape,
// rendering Price at its declared Decimals scale.
func oracleReadingFrom(u canonical.OracleUpdate) OracleReading {
	var onchain *string
	if u.PublishedPrice != nil {
		v := u.PublishedPrice.String()
		onchain = &v
	}
	return OracleReading{
		Source:       u.Source,
		ContractID:   u.ContractID,
		Asset:        u.Asset.String(),
		Quote:        u.Quote.String(),
		Timestamp:    WireTime(u.Timestamp),
		Price:        scaledDecimalString(u.Price.BigInt(), u.Decimals),
		PriceRaw:     u.Price.String(),
		PriceOnchain: onchain,
		Decimals:     u.Decimals,
		Confidence:   u.Confidence,
		Observer:     u.Observer,
		Mapped:       u.Asset.IsMapped(),
		Credit:       external.Credit(u.Source),
	}
}

// scaledDecimalString renders integer/10^decimals as a decimal string, truncating (floor) to `decimals` places, sign-correct (like
// ratToDecimal).
func scaledDecimalString(value *big.Int, decimals uint8) string {
	if value == nil {
		return "0"
	}
	if decimals == 0 {
		return value.String()
	}

	sign := ""
	abs := new(big.Int).Abs(value)
	if value.Sign() < 0 {
		sign = "-"
	}

	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	integerPart, fracPart := new(big.Int).DivMod(abs, scale, new(big.Int))

	// Pad fractional part to `decimals` digits.
	frac := fracPart.String()
	if len(frac) < int(decimals) {
		pad := int(decimals) - len(frac)
		frac = leftPad(frac, pad, '0')
	}
	return sign + integerPart.String() + "." + frac
}
