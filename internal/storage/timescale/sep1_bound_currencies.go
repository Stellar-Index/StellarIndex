package timescale

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Sep1AttestationMaxAge bounds how old a cached SEP-1 payload may be and still count as an attestation.
const Sep1AttestationMaxAge = 30 * 24 * time.Hour

// Issuer-bound SEP-1 [[CURRENCIES]] entries: what an issuer DECLARED about its own assets.
//
// Provenance rule, as in [Store.AllSep1Images]: an entry counts only when its declared Issuer
// names the G-address whose stellar.toml carried it. Otherwise any account could publish an entry
// naming another issuer's (code, issuer) and have its claim read as that issuer's.
//
// Every entry NOT kept is COUNTED in a bucket naming why ([Sep1BoundCensus]), so a scan that
// found almost nothing is distinguishable from one that only reports survivors.

// Sep1BoundCurrency is one [[CURRENCIES]] entry that passed the provenance rule, with the issuer-account context a caller needs to attribute it.
type Sep1BoundCurrency struct {
	// Code and Issuer are the (code, issuer) identity the entry binds to. Issuer is the canonical
	// database spelling of the serving account, never the toml's. Code is trimmed but not folded,
	// because Stellar asset codes are case-significant.
	Code   string
	Issuer string
	// HomeDomain is the on-chain home_domain the toml was fetched from.
	HomeDomain string
	// OrgName is DOCUMENTATION.ORG_NAME: advisory display text, never an identity.
	OrgName string
	// Name, AnchorAsset and AnchorAssetType are the entry fields verbatim. AnchorAssetType is free
	// text on the wire; callers normalise it against their own closed vocabulary.
	Name            string
	AnchorAsset     string
	AnchorAssetType string
}

// Sep1BoundCensus accounts for every issuer row and [[CURRENCIES]] entry one scan walked, with a named bucket for each way an entry stopped short, including the silent stages; the arithmetic closes.
type Sep1BoundCensus struct {
	// IssuersWithHomeDomain is every issuer with an on-chain home_domain, the population a SEP-1
	// attestation could exist for. It splits into issuers holding a payload, issuers a fetch
	// reached that hold none (IssuersFetchedWithoutPayload), and the never-attempted remainder.
	IssuersWithHomeDomain int
	// IssuersWithPayload is the rows this scan read: stellar.toml fetched and parsed at least once.
	IssuersWithPayload int
	// IssuersFetchedWithoutPayload counts issuers whose domain a fetch REACHED yet hold no
	// payload: a live failure streak and a NULL sep1_payload, the predicate
	// [Store.IssuerSep1Unreachable] serves per asset. Without it, IssuersWithHomeDomain minus
	// IssuersWithPayload reads as an unfetched backlog when most of it is domains already tried.
	IssuersFetchedWithoutPayload int
	// IssuersPayloadStale counts payloads not fetched within [Sep1AttestationMaxAge], or of unknown
	// fetch time. They are not read: a long-silent domain no longer attests to its last document.
	IssuersPayloadStale int
	// IssuersPayloadUnreadable counts payloads that would not decode.
	IssuersPayloadUnreadable int
	// IssuersDeclaringNothing counts payloads that decoded but carry no
	// [[CURRENCIES]] entry at all.
	IssuersDeclaringNothing int
	// IssuersDeclaring counts payloads carrying at least one entry.
	IssuersDeclaring int

	// Entries is every [[CURRENCIES]] entry across every payload read.
	Entries int
	// EntriesMissingCode and EntriesMissingIssuer count entries that
	// name no asset code, or no issuer, once trimmed. The linked-toml and code_template forms
	// legitimately carry neither, but neither identifies a (code, issuer) pair.
	EntriesMissingCode   int
	EntriesMissingIssuer int
	// EntriesNamingAnotherIssuer counts entries whose declared issuer is not the serving account.
	// This is the provenance rule at work and by far the largest bucket: scam tomls routinely
	// declare hundreds of other issuers' assets.
	EntriesNamingAnotherIssuer int
	// EntriesBound counts entries that passed the provenance rule.
	EntriesBound int
	// EntriesFiltered counts bound entries the caller's keep filter
	// dropped, and EntriesKept those it returned.
	EntriesFiltered int
	EntriesKept     int
}

// add accumulates one payload's census into the running total. The
// issuer-population fields are set by the scan, not per payload.
func (c *Sep1BoundCensus) add(o Sep1BoundCensus) {
	c.IssuersWithPayload += o.IssuersWithPayload
	c.IssuersPayloadStale += o.IssuersPayloadStale
	c.IssuersPayloadUnreadable += o.IssuersPayloadUnreadable
	c.IssuersDeclaringNothing += o.IssuersDeclaringNothing
	c.IssuersDeclaring += o.IssuersDeclaring
	c.Entries += o.Entries
	c.EntriesMissingCode += o.EntriesMissingCode
	c.EntriesMissingIssuer += o.EntriesMissingIssuer
	c.EntriesNamingAnotherIssuer += o.EntriesNamingAnotherIssuer
	c.EntriesBound += o.EntriesBound
	c.EntriesFiltered += o.EntriesFiltered
	c.EntriesKept += o.EntriesKept
}

// Check returns the reason the census does not balance, or "" when it does.
func (c Sep1BoundCensus) Check() string {
	if got := c.IssuersPayloadStale + c.IssuersPayloadUnreadable + c.IssuersDeclaringNothing + c.IssuersDeclaring; got != c.IssuersWithPayload {
		return fmt.Sprintf("issuer stages sum to %d, not IssuersWithPayload %d", got, c.IssuersWithPayload)
	}
	if got := c.EntriesMissingCode + c.EntriesMissingIssuer + c.EntriesNamingAnotherIssuer + c.EntriesBound; got != c.Entries {
		return fmt.Sprintf("entry stages sum to %d, not Entries %d", got, c.Entries)
	}
	if got := c.EntriesFiltered + c.EntriesKept; got != c.EntriesBound {
		return fmt.Sprintf("filter stages sum to %d, not EntriesBound %d", got, c.EntriesBound)
	}
	if c.IssuersWithPayload > c.IssuersWithHomeDomain {
		return fmt.Sprintf("IssuersWithPayload %d exceeds IssuersWithHomeDomain %d", c.IssuersWithPayload, c.IssuersWithHomeDomain)
	}
	// The never-tried third part is a REMAINDER, so an oversized second term would silently eat it
	// and publish a backlog of zero where one exists.
	if got := c.IssuersWithPayload + c.IssuersFetchedWithoutPayload; got > c.IssuersWithHomeDomain {
		return fmt.Sprintf("IssuersWithPayload %d plus IssuersFetchedWithoutPayload %d exceed IssuersWithHomeDomain %d",
			c.IssuersWithPayload, c.IssuersFetchedWithoutPayload, c.IssuersWithHomeDomain)
	}
	return ""
}

// Sep1CurrencyFilter decides which bound entries a scan keeps, per entry during the row walk so narrow callers never materialise tens of thousands of entries; nil keeps every bound entry.
type Sep1CurrencyFilter func(Sep1BoundCurrency) bool

// BoundSep1Currencies walks every issuer with a cached SEP-1 payload and returns entries passing the provenance rule and filter plus the census; a payload older than [Sep1AttestationMaxAge] is counted, not read.
func (s *Store) BoundSep1Currencies(ctx context.Context, keep Sep1CurrencyFilter) ([]Sep1BoundCurrency, Sep1BoundCensus, error) {
	var census Sep1BoundCensus

	// The second count separates our backlog from a domain that WAS reached and served nothing
	// storable. It reads the failure streak, not sep1_resolved_at, which an outage unwind leaves
	// stamped on rows that never failed. One aggregate keeps both counts at one moment.
	const popQ = `SELECT count(*) FILTER (WHERE home_domain IS NOT NULL AND btrim(home_domain) <> ''),
	                     count(*) FILTER (WHERE home_domain IS NOT NULL AND btrim(home_domain) <> ''
	                                        AND COALESCE(sep1_consecutive_failures, 0) > 0
	                                        AND sep1_payload IS NULL)
	                FROM issuers`
	if err := s.db.QueryRowContext(ctx, popQ).Scan(
		&census.IssuersWithHomeDomain, &census.IssuersFetchedWithoutPayload,
	); err != nil {
		return nil, census, fmt.Errorf("timescale: BoundSep1Currencies population: %w", err)
	}

	// Freshness is judged on sep1_payload_fetched_at, never sep1_resolved_at: every attempt stamps
	// the latter, so a dead domain looks fresh there while its payload ages. NULL reads as stale.
	const q = `SELECT g_strkey, COALESCE(home_domain, ''), sep1_payload,
	                  COALESCE(sep1_payload_fetched_at >= NOW() - $1::interval, false)
	             FROM issuers
	            WHERE sep1_payload IS NOT NULL`
	rows, err := s.db.QueryContext(ctx, q, intervalArg(Sep1AttestationMaxAge))
	if err != nil {
		return nil, census, fmt.Errorf("timescale: BoundSep1Currencies: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]Sep1BoundCurrency, 0, 64)
	for rows.Next() {
		var (
			gStrkey    string
			homeDomain string
			payload    sql.NullString
			fresh      bool
		)
		if err := rows.Scan(&gStrkey, &homeDomain, &payload, &fresh); err != nil {
			return nil, census, fmt.Errorf("timescale: BoundSep1Currencies scan: %w", err)
		}
		kept, c := boundSep1CurrenciesFromRow(gStrkey, homeDomain, payload, fresh, keep)
		out = append(out, kept...)
		census.add(c)
	}
	if err := rows.Err(); err != nil {
		return nil, census, fmt.Errorf("timescale: BoundSep1Currencies rows: %w", err)
	}
	return out, census, nil
}

// boundSep1CurrenciesFromRow judges one payload-bearing issuer row. A stale payload is counted and
// never decoded, so what a dark domain last said binds nothing.
func boundSep1CurrenciesFromRow(
	gStrkey, homeDomain string, payload sql.NullString, fresh bool,
	keep Sep1CurrencyFilter,
) ([]Sep1BoundCurrency, Sep1BoundCensus) {
	census := Sep1BoundCensus{IssuersWithPayload: 1}
	switch {
	case !fresh:
		census.IssuersPayloadStale++
		return nil, census
	case !payload.Valid || payload.String == "":
		// NULL is already excluded by the query; an empty string is an unreadable payload.
		census.IssuersPayloadUnreadable++
		return nil, census
	}
	kept, c := boundSep1CurrenciesFromPayload(gStrkey, homeDomain, payload.String, keep)
	census.add(c)
	return kept, census
}

// boundSep1CurrenciesFromPayload extracts the entries one issuer is entitled to declare, and
// censuses every entry it did not return.
func boundSep1CurrenciesFromPayload(
	gStrkey, homeDomain, payload string,
	keep Sep1CurrencyFilter,
) ([]Sep1BoundCurrency, Sep1BoundCensus) {
	var census Sep1BoundCensus
	var parsed IssuerSep1Cached
	if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
		census.IssuersPayloadUnreadable++
		return nil, census
	}
	if len(parsed.Currencies) == 0 {
		census.IssuersDeclaringNothing++
		return nil, census
	}
	census.IssuersDeclaring++

	out := make([]Sep1BoundCurrency, 0, len(parsed.Currencies))
	for _, c := range parsed.Currencies {
		census.Entries++
		code := strings.TrimSpace(c.Code)
		switch {
		case code == "":
			census.EntriesMissingCode++
			continue
		case strings.TrimSpace(c.Issuer) == "":
			census.EntriesMissingIssuer++
			continue
		case !Sep1EntryBindsTo(c.Issuer, gStrkey):
			census.EntriesNamingAnotherIssuer++
			continue
		}
		census.EntriesBound++
		e := Sep1BoundCurrency{
			Code: code,
			// The DATABASE spelling, never the toml's: downstream joins key on the canonical form.
			Issuer:          gStrkey,
			HomeDomain:      homeDomain,
			OrgName:         parsed.OrgName,
			Name:            c.Name,
			AnchorAsset:     c.AnchorAsset,
			AnchorAssetType: c.AnchorAssetType,
		}
		if keep != nil && !keep(e) {
			census.EntriesFiltered++
			continue
		}
		census.EntriesKept++
		out = append(out, e)
	}
	return out, census
}

// Sep1EntryBindsTo reports whether a [[CURRENCIES]] entry's DECLARED issuer names the account that served the file, comparing canonical strkeys so case and whitespace variants fold to the same 32 bytes.
func Sep1EntryBindsTo(declared, serving string) bool {
	d := canonicalAccountStrkey(declared)
	return d != "" && d == canonicalAccountStrkey(serving)
}

// canonicalAccountStrkey returns the canonical spelling of a G-account strkey, or "" when s is not
// shaped like one: 56 characters, leading G, base32 alphabet (the account_directory CHECK).
func canonicalAccountStrkey(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	if len(s) != 56 || s[0] != 'G' {
		return ""
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= 'A' && c <= 'Z':
		case c >= '2' && c <= '7':
		default:
			return ""
		}
	}
	return s
}
