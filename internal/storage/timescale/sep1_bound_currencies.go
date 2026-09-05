package timescale

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// Issuer-bound SEP-1 [[CURRENCIES]] entries — the raw material for any
// surface that must act on what an issuer DECLARED about one of its own
// assets, rather than on what some account declared about someone else.
//
// The provenance rule is the whole point, and it is the same one
// [Store.AllSep1Images] enforces: an entry counts only when its declared
// Issuer equals the G-address whose stellar.toml carried it. Without that
// check any account could publish
//
//	[[CURRENCIES]] code = "USTRY" issuer = "<Etherfuse G-key>"
//	               anchor_asset_type = "bond"
//
// and have its claim read as that of the account it named. The toml a
// domain serves may describe only the issuer that served it.

// Sep1BoundCurrency is one [[CURRENCIES]] entry that passed the
// provenance rule, carried alongside the issuer-account context a
// caller needs to attribute it.
type Sep1BoundCurrency struct {
	// Code and Issuer are the (code, issuer) identity the entry binds
	// to. Issuer is the account that served the toml, so the two are
	// the same value by construction.
	Code   string
	Issuer string
	// HomeDomain is the domain the issuer account set ON CHAIN, i.e.
	// the domain the toml was fetched from. Empty when the account has
	// no home_domain (in which case there is no payload either).
	HomeDomain string
	// OrgName is DOCUMENTATION.ORG_NAME from the same toml. Advisory
	// display text authored by the issuer, never an identity.
	OrgName string
	// Name, AnchorAsset and AnchorAssetType are the entry fields
	// verbatim. AnchorAssetType is free text on the wire; callers
	// normalise it against their own closed vocabulary rather than
	// trusting the spelling.
	Name            string
	AnchorAsset     string
	AnchorAssetType string
}

// Sep1CurrencyFilter decides which bound entries a scan keeps. It runs
// per entry during the row walk, so a caller narrowing to a handful of
// declarations never materialises the rest — the production payload set
// holds tens of thousands of bound entries, most of them from accounts
// that declare thousands each.
//
// A nil filter keeps every bound entry.
type Sep1CurrencyFilter func(Sep1BoundCurrency) bool

// BoundSep1Currencies walks every issuer carrying a cached SEP-1
// payload and returns the entries that pass the provenance rule and the
// filter, plus how many bound entries the FILTER dropped.
//
// The dropped count is returned rather than discarded so a caller can
// report the size of the population it narrowed from. Without it a
// filtered read is indistinguishable from an unfiltered one that found
// little, which is the same defect a silently-ignored query filter has.
//
// One indexed SELECT over the issuers that have a payload (~13.5k rows
// on the production deployment). Intended to run off the request path
// behind a TTL cache, the same way the logo map does. A single corrupt
// payload is skipped, never fatal: one malformed toml must not empty
// the whole result.
func (s *Store) BoundSep1Currencies(ctx context.Context, keep Sep1CurrencyFilter) ([]Sep1BoundCurrency, int, error) {
	const q = `SELECT g_strkey, COALESCE(home_domain, ''), sep1_payload
	             FROM issuers
	            WHERE sep1_payload IS NOT NULL`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, 0, fmt.Errorf("timescale: BoundSep1Currencies: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]Sep1BoundCurrency, 0, 64)
	dropped := 0
	for rows.Next() {
		var (
			gStrkey    string
			homeDomain string
			payload    sql.NullString
		)
		if err := rows.Scan(&gStrkey, &homeDomain, &payload); err != nil {
			return nil, 0, fmt.Errorf("timescale: BoundSep1Currencies scan: %w", err)
		}
		if !payload.Valid || payload.String == "" {
			continue
		}
		kept, n := boundSep1CurrenciesFromPayload(gStrkey, homeDomain, payload.String, keep)
		out = append(out, kept...)
		dropped += n
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("timescale: BoundSep1Currencies rows: %w", err)
	}
	return out, dropped, nil
}

// boundSep1CurrenciesFromPayload extracts the entries one issuer is
// entitled to declare, and counts the bound entries the filter dropped.
// Split out of [Store.BoundSep1Currencies] so the provenance rule is
// testable without a database.
func boundSep1CurrenciesFromPayload(
	gStrkey, homeDomain, payload string,
	keep Sep1CurrencyFilter,
) ([]Sep1BoundCurrency, int) {
	var parsed IssuerSep1Cached
	if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
		return nil, 0
	}
	out := make([]Sep1BoundCurrency, 0, len(parsed.Currencies))
	dropped := 0
	for _, c := range parsed.Currencies {
		if c.Code == "" || c.Issuer == "" {
			continue
		}
		if c.Issuer != gStrkey {
			continue
		}
		e := Sep1BoundCurrency{
			Code:            c.Code,
			Issuer:          gStrkey,
			HomeDomain:      homeDomain,
			OrgName:         parsed.OrgName,
			Name:            c.Name,
			AnchorAsset:     c.AnchorAsset,
			AnchorAssetType: c.AnchorAssetType,
		}
		if keep != nil && !keep(e) {
			dropped++
			continue
		}
		out = append(out, e)
	}
	return out, dropped
}
