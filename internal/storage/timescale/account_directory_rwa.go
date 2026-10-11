package timescale

import (
	"context"
	"fmt"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/pgarray"
)

// Directory-driven candidate discovery for the contract arm of the RWA definition.
//
// The directory is the population because the classic arm walks issuers from the classic-asset
// registry, so a contract-issued entity would never become a candidate: absent before any
// requirement runs, the silent-discard shape the funnel exists to prevent. account_directory is
// the only table tying a real-world entity to a Stellar address without classic issuance (its
// CHECK accepts both strkey forms). The contract arm keys on the contract address, under the same
// scam vocabulary and a NARROWER recognition vocabulary.

// DirectoryRWACensus accounts for every curated directory row the contract-arm scan walked: the
// starting population and a named bucket for each way a row stopped short of becoming a candidate,
// as [Sep1BoundCensus] does. The arithmetic closes, and [DirectoryRWACensus.Check] proves it:
//
//	Entries   = Accounts + Contracts
//	Contracts = ContractsScamFlagged + ContractsWithoutIssuingTag +
//	            ContractsRecognised
//	Accounts >= AccountsIssuingTagged >= AccountsIssuingWithoutAsset
type DirectoryRWACensus struct {
	// Entries is every row in the curated directory.
	Entries int
	// Accounts and Contracts split that population by strkey form. A
	// contract address is a token; an account address is an entity that
	// may issue many, or none.
	Accounts  int
	Contracts int

	// ContractsScamFlagged counts contract rows carrying a scam-class
	// tag. Refused under C3 and reported, never merely ranked low.
	ContractsScamFlagged int
	// ContractsWithoutIssuingTag counts contract rows carrying no tag from the caller's issuing
	// vocabulary. It is the largest bucket (AMM pools and routers issue no real-world instrument)
	// and the number to watch if the vocabulary proves too narrow.
	ContractsWithoutIssuingTag int
	// ContractsRecognised counts contract rows that passed C2 and C3,
	// i.e. the candidates the definition then evaluates.
	ContractsRecognised int

	// AccountsIssuingTagged counts ACCOUNT rows carrying an issuing-class
	// tag and no scam tag: entities a third party recognises as issuing
	// or custodying value.
	AccountsIssuingTagged int
	// AccountsIssuingWithoutAsset counts the subset that issue no classic asset this index has ever
	// observed: a real, recognised entity with NO Stellar token held here, which would otherwise be
	// indistinguishable from a nonexistent one. It admits nothing; the operator action is to get
	// the entity's contract address into the curated directory or the in-repo curated set.
	AccountsIssuingWithoutAsset int
}

// Check returns the reason the census does not balance, or "" when it does. Exported like
// [Sep1BoundCensus.Check] so the publishing surface can declare its accounting UNBALANCED on the
// wire rather than serve figures a reader cannot reconcile.
func (c DirectoryRWACensus) Check() string {
	if got := c.Accounts + c.Contracts; got != c.Entries {
		return fmt.Sprintf("address forms sum to %d, not Entries %d", got, c.Entries)
	}
	if got := c.ContractsScamFlagged + c.ContractsWithoutIssuingTag + c.ContractsRecognised; got != c.Contracts {
		return fmt.Sprintf("contract stages sum to %d, not Contracts %d", got, c.Contracts)
	}
	if c.AccountsIssuingTagged > c.Accounts {
		return fmt.Sprintf("AccountsIssuingTagged %d exceeds Accounts %d", c.AccountsIssuingTagged, c.Accounts)
	}
	if c.AccountsIssuingWithoutAsset > c.AccountsIssuingTagged {
		return fmt.Sprintf("AccountsIssuingWithoutAsset %d exceeds AccountsIssuingTagged %d",
			c.AccountsIssuingWithoutAsset, c.AccountsIssuingTagged)
	}
	return ""
}

// directoryRecognisedScanCap bounds the candidate rows one scan materialises. The set is small by
// construction, so the cap only guards against a sync that suddenly tags thousands of contracts;
// when it binds, the census says so and the surface reports truncation, never a silently short set.
const directoryRecognisedScanCap = 512

// DirectoryRecognisedContracts returns the curated directory rows whose
// address is a CONTRACT carrying at least one of `issuingTags` and no
// scam-class tag, together with the census of the whole directory.
//
// The issuing vocabulary is a PARAMETER because it belongs to internal/rwa, which imports this
// package; the scam vocabulary is read locally to avoid the drift [DirectoryScamFlagTags] warns
// about. Rows are ordered by address so a truncated scan is deterministic; tag matching is
// case-insensitive on trimmed tags, as in pricingguard and the listing rank expression.
func (s *Store) DirectoryRecognisedContracts(
	ctx context.Context, issuingTags []string,
) ([]DirectoryEntry, DirectoryRWACensus, error) {
	issuingTags = lowerTrimAll(issuingTags)
	census, err := s.directoryRWACensus(ctx, issuingTags)
	if err != nil {
		return nil, DirectoryRWACensus{}, err
	}
	// The census counted the recognised contracts; this reads them. Both run the SAME predicate
	// text (directoryContractRecognisedSQL) so a mismatch surfaces as an unbalanced funnel rather
	// than a set quietly missing a member.
	q := `
		SELECT address, name, domain, tags, source
		  FROM account_directory
		 WHERE ` + directoryContractRecognisedSQL + `
		 ORDER BY address
		 LIMIT $2`
	rows, err := s.db.QueryContext(ctx, q, issuingTags, directoryRecognisedScanCap)
	if err != nil {
		return nil, DirectoryRWACensus{}, fmt.Errorf("directory: recognised contracts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]DirectoryEntry, 0, 16)
	for rows.Next() {
		var e DirectoryEntry
		if err := rows.Scan(&e.Address, &e.Name, &e.Domain, pgarray.Strings(&e.Tags), &e.Source); err != nil {
			return nil, DirectoryRWACensus{}, fmt.Errorf("directory: scan recognised contract: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, DirectoryRWACensus{}, fmt.Errorf("directory: recognised contract rows: %w", err)
	}
	return out, census, nil
}

// lowerTrimAll normalises a tag vocabulary before it is bound into a query whose predicate
// lowercases the COLUMN side. Without it a caller passing `Issuer` would match nothing, a silent
// empty set produced by a spelling.
func lowerTrimAll(vals []string) []string {
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if t := strings.ToLower(strings.TrimSpace(v)); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// directoryIsContractSQL and directoryIsAccountSQL split the curated set by strkey form; the table
// CHECK makes them exhaustive, which the census arithmetic relies on.
const (
	directoryIsContractSQL = "address LIKE 'C%'"
	directoryIsAccountSQL  = "address LIKE 'G%'"
)

// directoryHasIssuingTagSQL is TRUE when the row carries a tag from the caller's issuing vocabulary
// (bound at $1). Lowercased on both sides to agree with the Go matcher.
const directoryHasIssuingTagSQL = `EXISTS (SELECT 1 FROM unnest(tags) t WHERE lower(t) = ANY($1))`

// directoryScamTaggedSQL is the SQL twin of pricingguard's scam check over the SAME
// [DirectoryScamFlagTags] list, built by the fail-loud literal builder so a tag cannot smuggle a
// quote into composed SQL.
var directoryScamTaggedSQL = `EXISTS (SELECT 1 FROM unnest(tags) t ` +
	`WHERE lower(t) = ANY(` + scamFlagTagsSQLArray + `))`

// directoryContractRecognisedSQL is C2 and C3 as one predicate (contract address, issuing tag, no
// scam tag), shared by BOTH the census count and the row read so they cannot diverge.
var directoryContractRecognisedSQL = directoryIsContractSQL +
	` AND ` + directoryHasIssuingTagSQL +
	` AND NOT ` + directoryScamTaggedSQL

// directoryRWACensus counts the whole curated directory in one pass,
// bucketed the way the contract arm narrows it.
//
// One query with FILTER aggregates rather than six round trips: every bucket must come from the
// same snapshot, since separate statements could straddle a directory sync and fail to reconcile.
func (s *Store) directoryRWACensus(ctx context.Context, issuingTags []string) (DirectoryRWACensus, error) {
	q := `
		SELECT
		  count(*)                                                          AS entries,
		  count(*) FILTER (WHERE ` + directoryIsAccountSQL + `)             AS accounts,
		  count(*) FILTER (WHERE ` + directoryIsContractSQL + `)            AS contracts,
		  count(*) FILTER (WHERE ` + directoryIsContractSQL + `
		                     AND ` + directoryScamTaggedSQL + `)           AS contracts_scam,
		  count(*) FILTER (WHERE ` + directoryIsContractSQL + `
		                     AND NOT ` + directoryScamTaggedSQL + `
		                     AND NOT ` + directoryHasIssuingTagSQL + `)    AS contracts_untagged,
		  count(*) FILTER (WHERE ` + directoryContractRecognisedSQL + `)    AS contracts_recognised,
		  count(*) FILTER (WHERE ` + directoryIsAccountSQL + `
		                     AND ` + directoryHasIssuingTagSQL + `
		                     AND NOT ` + directoryScamTaggedSQL + `)       AS accounts_issuing,
		  -- The coverage row. An account a third party recognises as an
		  -- issuing entity, for which this index holds no classic asset
		  -- at all. NOT EXISTS against the indexed issuer column
		  -- (classic_assets_issuer_idx), so it is an index probe per
		  -- candidate account rather than a scan.
		  count(*) FILTER (WHERE ` + directoryIsAccountSQL + `
		                     AND ` + directoryHasIssuingTagSQL + `
		                     AND NOT ` + directoryScamTaggedSQL + `
		                     AND NOT EXISTS (
		                           SELECT 1 FROM classic_assets ca
		                            WHERE ca.issuer_g_strkey = account_directory.address))
		                                                                    AS accounts_issuing_no_asset
		  FROM account_directory`
	var c DirectoryRWACensus
	err := s.db.QueryRowContext(ctx, q, issuingTags).Scan(
		&c.Entries, &c.Accounts, &c.Contracts,
		&c.ContractsScamFlagged, &c.ContractsWithoutIssuingTag, &c.ContractsRecognised,
		&c.AccountsIssuingTagged, &c.AccountsIssuingWithoutAsset,
	)
	if err != nil {
		return DirectoryRWACensus{}, fmt.Errorf("directory: rwa census: %w", err)
	}
	return c, nil
}

// DirectoryRecognisedIssuersWithoutAsset names the curated directory ACCOUNTS with an issuing tag
// and no scam tag for which this index holds no classic asset (the entities counted by
// [DirectoryRWACensus.AccountsIssuingWithoutAsset]). It admits nothing and feeds no valuation.
func (s *Store) DirectoryRecognisedIssuersWithoutAsset(
	ctx context.Context, issuingTags []string, limit int,
) ([]DirectoryEntry, error) {
	if limit <= 0 || limit > directoryRecognisedScanCap {
		limit = directoryRecognisedScanCap
	}
	issuingTags = lowerTrimAll(issuingTags)
	q := `
		SELECT address, name, domain, tags, source
		  FROM account_directory
		 WHERE ` + directoryIsAccountSQL + `
		   AND ` + directoryHasIssuingTagSQL + `
		   AND NOT ` + directoryScamTaggedSQL + `
		   AND NOT EXISTS (
		         SELECT 1 FROM classic_assets ca
		          WHERE ca.issuer_g_strkey = account_directory.address)
		 ORDER BY address
		 LIMIT $2`
	rows, err := s.db.QueryContext(ctx, q, issuingTags, limit)
	if err != nil {
		return nil, fmt.Errorf("directory: recognised issuers without asset: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]DirectoryEntry, 0, 16)
	for rows.Next() {
		var e DirectoryEntry
		if err := rows.Scan(&e.Address, &e.Name, &e.Domain, pgarray.Strings(&e.Tags), &e.Source); err != nil {
			return nil, fmt.Errorf("directory: scan recognised issuer: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("directory: recognised issuer rows: %w", err)
	}
	return out, nil
}
