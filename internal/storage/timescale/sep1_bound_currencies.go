package timescale

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// Issuer-bound SEP-1 [[CURRENCIES]] entries — the raw material for any
// surface that must act on what an issuer DECLARED about one of its own
// assets, rather than on what some account declared about someone else.
//
// The provenance rule is the whole point, and it is the same one
// [Store.AllSep1Images] enforces: an entry counts only when its declared
// Issuer names the G-address whose stellar.toml carried it. Without that
// check any account could publish
//
//	[[CURRENCIES]] code = "USTRY" issuer = "<Etherfuse G-key>"
//	               anchor_asset_type = "bond"
//
// and have its claim read as that of the account it named. The toml a
// domain serves may describe only the issuer that served it.
//
// Every entry the scan does NOT keep is COUNTED, in the bucket that
// names why (see [Sep1BoundCensus]). A scan that reported only what
// survived is indistinguishable from one that found almost nothing,
// and that is exactly how a funnel narrowing tens of thousands of
// attestations down to single digits went unnoticed: the caller could
// see the survivors and had no way to ask what happened to the rest.

// Sep1BoundCurrency is one [[CURRENCIES]] entry that passed the
// provenance rule, carried alongside the issuer-account context a
// caller needs to attribute it.
type Sep1BoundCurrency struct {
	// Code and Issuer are the (code, issuer) identity the entry binds
	// to. Issuer is the account that served the toml — the CANONICAL
	// database spelling of it, never the toml's — so the two are the
	// same value by construction. Code is the declared code with
	// surrounding space removed; nothing else about it is folded,
	// because Stellar asset codes are case-significant.
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

// Sep1BoundCensus accounts for every issuer row and every
// [[CURRENCIES]] entry one scan walked: the population it started from,
// and a named bucket for each way an entry stopped short of the result.
//
// It exists because the alternative — returning only the survivors —
// makes the two most important questions about this scan unanswerable
// from outside it: how large was the population, and which stage
// removed it. Both stages that swallow silently are represented here
// ([Sep1BoundCensus.IssuersPayloadUnreadable] and
// [Sep1BoundCensus.IssuersDeclaringNothing]); a scan that dropped a
// whole population through one of them used to look identical to a
// network where nothing qualified.
//
// The arithmetic closes, and [Sep1BoundCensus.Check] proves it:
//
//	IssuersWithPayload = IssuersPayloadUnreadable +
//	                     IssuersDeclaringNothing + IssuersDeclaring
//	Entries            = EntriesMissingCode + EntriesMissingIssuer +
//	                     EntriesNamingAnotherIssuer + EntriesBound
//	EntriesBound       = EntriesFiltered + EntriesKept
type Sep1BoundCensus struct {
	// IssuersWithHomeDomain is every issuer account carrying an
	// on-chain home_domain — the population a SEP-1 attestation could
	// exist for at all. IssuersWithHomeDomain minus IssuersWithPayload
	// is the number of issuers the refresh cron has never successfully
	// fetched, which no other stage can distinguish from an issuer that
	// declares nothing.
	IssuersWithHomeDomain int
	// IssuersWithPayload is the rows this scan actually read: issuers
	// whose stellar.toml was fetched and parsed at least once.
	IssuersWithPayload int
	// IssuersPayloadUnreadable counts payloads that would not decode.
	// Previously a bare `return nil, 0`.
	IssuersPayloadUnreadable int
	// IssuersDeclaringNothing counts payloads that decoded but carry no
	// [[CURRENCIES]] entry at all.
	IssuersDeclaringNothing int
	// IssuersDeclaring counts payloads carrying at least one entry.
	IssuersDeclaring int

	// Entries is every [[CURRENCIES]] entry across every payload read.
	Entries int
	// EntriesMissingCode and EntriesMissingIssuer count entries that
	// name no asset code, or no issuer, once surrounding space is
	// removed. A SEP-1 entry may legitimately carry neither — the
	// linked-toml and code_template forms do — but neither identifies a
	// (code, issuer) pair, so neither can be bound to one.
	EntriesMissingCode   int
	EntriesMissingIssuer int
	// EntriesNamingAnotherIssuer counts entries whose declared issuer is
	// not the account that served the file. This is the provenance rule
	// doing its job and is expected to be the largest bucket by far: a
	// scam domain's toml routinely declares hundreds of other issuers'
	// assets.
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

// Check returns the reason the census does not balance, or "" when it
// does.
//
// Exported because the API surface that publishes these counts declares
// its accounting UNBALANCED on the wire when this fails, rather than
// serving numbers a reader would try, and fail, to reconcile. It catches
// what the published stage-by-stage arithmetic cannot: a stage clamped
// to zero for presentation would otherwise let an inconsistent census
// read as sound.
func (c Sep1BoundCensus) Check() string {
	if got := c.IssuersPayloadUnreadable + c.IssuersDeclaringNothing + c.IssuersDeclaring; got != c.IssuersWithPayload {
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
	return ""
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
// filter, plus the census of everything it walked.
//
// The census is returned rather than discarded so a caller can report
// the size of the population it narrowed from, per stage. Without it a
// filtered read is indistinguishable from an unfiltered one that found
// little, which is the same defect a silently-ignored query filter has.
//
// Two indexed reads: one aggregate over `issuers` for the population the
// scan could have covered, and one SELECT over the issuers that have a
// payload (~14.6k rows on the production deployment). Intended to run
// off the request path behind a TTL cache, the same way the logo map
// does. A single corrupt payload is skipped, never fatal: one malformed
// toml must not empty the whole result — but it IS counted, because an
// unreadable payload and an issuer that declares nothing are different
// findings.
func (s *Store) BoundSep1Currencies(ctx context.Context, keep Sep1CurrencyFilter) ([]Sep1BoundCurrency, Sep1BoundCensus, error) {
	var census Sep1BoundCensus

	// The population upstream of this scan. An issuer with no
	// home_domain can serve no stellar.toml at all and is out of scope;
	// one WITH a home_domain and no payload is a fetch the refresh cron
	// has not completed, which is an operator fact and not a property of
	// the network.
	const popQ = `SELECT count(*) FILTER (WHERE home_domain IS NOT NULL AND btrim(home_domain) <> '')
	                FROM issuers`
	if err := s.db.QueryRowContext(ctx, popQ).Scan(&census.IssuersWithHomeDomain); err != nil {
		return nil, census, fmt.Errorf("timescale: BoundSep1Currencies population: %w", err)
	}

	const q = `SELECT g_strkey, COALESCE(home_domain, ''), sep1_payload
	             FROM issuers
	            WHERE sep1_payload IS NOT NULL`
	rows, err := s.db.QueryContext(ctx, q)
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
		)
		if err := rows.Scan(&gStrkey, &homeDomain, &payload); err != nil {
			return nil, census, fmt.Errorf("timescale: BoundSep1Currencies scan: %w", err)
		}
		if !payload.Valid || payload.String == "" {
			// `WHERE sep1_payload IS NOT NULL` already excluded these;
			// an empty string reaching here is a payload that decoded to
			// nothing, which is an unreadable payload, not an absent row.
			census.IssuersWithPayload++
			census.IssuersPayloadUnreadable++
			continue
		}
		census.IssuersWithPayload++
		kept, c := boundSep1CurrenciesFromPayload(gStrkey, homeDomain, payload.String, keep)
		out = append(out, kept...)
		census.add(c)
	}
	if err := rows.Err(); err != nil {
		return nil, census, fmt.Errorf("timescale: BoundSep1Currencies rows: %w", err)
	}
	return out, census, nil
}

// boundSep1CurrenciesFromPayload extracts the entries one issuer is
// entitled to declare, and censuses every entry it did not return.
// Split out of [Store.BoundSep1Currencies] so the provenance rule is
// testable without a database.
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
		case !sep1EntryBindsTo(c.Issuer, gStrkey):
			census.EntriesNamingAnotherIssuer++
			continue
		}
		census.EntriesBound++
		e := Sep1BoundCurrency{
			Code: code,
			// The DATABASE spelling of the account, never the toml's:
			// the entry has been proven to name this account, and every
			// downstream join keys on the canonical form.
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

// sep1EntryBindsTo reports whether a [[CURRENCIES]] entry's DECLARED
// issuer names the account that served the file.
//
// The comparison is on the canonical strkey, not on the bytes the toml
// happened to carry: a declaration is either the same account or a
// different one, and neither surrounding whitespace nor the case an
// issuer typed its own key in changes which account it is. A strkey is
// uppercase base32 (RFC 4648 alphabet, no padding), so two spellings
// that differ only in case decode to the same 32 bytes — folding them
// cannot admit a different account, which is the property that makes
// this a canonicalisation and not a relaxation of the rule.
//
// Anything that is not shaped like a G-account strkey binds to nothing.
// That is deliberate: the rule's whole value is that it names ONE
// account, and a value that cannot be an account names none.
func sep1EntryBindsTo(declared, serving string) bool {
	d := canonicalAccountStrkey(declared)
	return d != "" && d == canonicalAccountStrkey(serving)
}

// canonicalAccountStrkey returns the canonical spelling of a Stellar
// G-account strkey, or "" when s is not shaped like one. The shape is
// the same the account_directory CHECK constraint enforces (migration
// 0136): 56 characters, leading G, RFC 4648 base32 alphabet.
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
