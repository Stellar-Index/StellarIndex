package timescale

import (
	"context"
	"fmt"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/pgarray"
)

// Directory-driven candidate discovery for the contract arm of the RWA
// definition.
//
// WHY THE DIRECTORY IS THE POPULATION. The classic arm walks issuers,
// which is populated from ONE site — registerIssuerSeen, called when a
// classic asset is registered. An entity whose Stellar presence is
// contract-issued therefore has no row there, gets no SEP-1 fetch, and
// never becomes a candidate. It is not refused by a requirement; it is
// absent before any requirement runs, which is the silent-discard shape
// the funnel exists to make impossible.
//
// account_directory is the only table that ties a real-world entity to a
// Stellar address without going through classic issuance, and its CHECK
// has always accepted both strkey forms (migration 0136 —
// `^[GC][A-Z2-7]{55}$`, because the upstream set tags contract addresses
// too). So the contract arm walks it.
//
// WHAT THIS IS NOT. It is not a second trust surface bolted on. The
// classic arm has read this same table for requirement R3 since the
// surface shipped; the contract arm reads the same rows under the same
// scam vocabulary and a NARROWER recognition vocabulary. What changes is
// which column is the key: the issuer G-address for a classic asset, the
// contract address itself for a contract.

// DirectoryRWACensus accounts for every curated directory row the
// contract-arm scan walked: the population it started from, and a named
// bucket for each way a row stopped short of becoming a candidate.
//
// It exists for the reason [Sep1BoundCensus] does. Returning only the
// survivors makes the two questions that matter unanswerable from
// outside the scan: how large was the population, and which stage
// removed it.
//
// The arithmetic closes, and [DirectoryRWACensus.Check] proves it:
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
	// ContractsWithoutIssuingTag counts contract rows carrying no tag
	// from the caller's issuing vocabulary. This is the largest bucket
	// by construction — the upstream set tags AMM pools, routers and
	// protocol infrastructure, none of which issues a real-world
	// instrument — and it is the number to watch if the vocabulary
	// turns out to be too narrow.
	ContractsWithoutIssuingTag int
	// ContractsRecognised counts contract rows that passed C2 and C3,
	// i.e. the candidates the definition then evaluates.
	ContractsRecognised int

	// AccountsIssuingTagged counts ACCOUNT rows carrying an issuing-class
	// tag and no scam tag: entities a third party recognises as issuing
	// or custodying value.
	AccountsIssuingTagged int
	// AccountsIssuingWithoutAsset counts the subset of those that issue
	// no classic asset this index has ever observed.
	//
	// This is the Franklin Templeton row, and it is the single most
	// useful number on the contract arm. Such an entity is recognised,
	// unflagged and real, and this index holds NO Stellar token for it
	// at all: no classic asset to evaluate, and no directory entry
	// naming a contract of theirs either. Before this count existed the
	// entity was invisible — not refused by any requirement, just never
	// collected — and a reader of the response could not tell it apart
	// from an entity that does not exist.
	//
	// It admits nothing. It is a coverage statement, and the action it
	// implies is an operator one: get the entity's contract address into
	// the curated directory, or into the in-repo curated set.
	AccountsIssuingWithoutAsset int
}

// Check returns the reason the census does not balance, or "" when it
// does. Exported for the same reason [Sep1BoundCensus.Check] is: the
// surface publishing these counts declares its accounting UNBALANCED on
// the wire rather than serving figures a reader would try, and fail, to
// reconcile.
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

// directoryRecognisedScanCap bounds the candidate rows one scan
// materialises. The recognised-contract set is small by construction —
// it requires a third party to have named the exact address with an
// issuing tag — so the cap is a guard against a directory sync that
// suddenly tags thousands of contracts, not an expected condition. When
// it binds, the census says so and the surface reports the set as
// truncated rather than serving a silently short one.
const directoryRecognisedScanCap = 512

// DirectoryRecognisedContracts returns the curated directory rows whose
// address is a CONTRACT carrying at least one of `issuingTags` and no
// scam-class tag, together with the census of the whole directory.
//
// The tag vocabulary is a PARAMETER rather than a constant here. It
// belongs to internal/rwa, which already imports this package for
// [DirectoryScamFlagTags]; owning it here too would either duplicate the
// vocabulary or invert the dependency. The scam vocabulary is the
// exception and is read locally, because it is this package's own list
// and every other consumer already reads it from here — a second
// spelling of it is precisely the drift its doc comment warns about.
//
// Rows come back ordered by address so a truncated scan is at least
// deterministic. Matching is case-insensitive on trimmed tags, the same
// rule pricingguard and the listing rank expression apply to the same
// column.
func (s *Store) DirectoryRecognisedContracts(
	ctx context.Context, issuingTags []string,
) ([]DirectoryEntry, DirectoryRWACensus, error) {
	issuingTags = lowerTrimAll(issuingTags)
	census, err := s.directoryRWACensus(ctx, issuingTags)
	if err != nil {
		return nil, DirectoryRWACensus{}, err
	}
	// The census counted the recognised contracts; this reads them. Both
	// run the SAME predicate text (directoryContractRecognisedSQL) so the
	// count and the rows can never disagree about who qualified — a
	// mismatch between them would show up as an unbalanced funnel rather
	// than as a set quietly missing a member.
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

// lowerTrimAll normalises a tag vocabulary before it is bound into a
// query whose predicate lowercases the COLUMN side. Without it a caller
// passing `Issuer` would match nothing and the refusal would look like a
// directory that named no issuers — a silent empty set produced by a
// spelling, which is the failure mode this whole surface is built to
// avoid.
func lowerTrimAll(vals []string) []string {
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if t := strings.ToLower(strings.TrimSpace(v)); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// directoryIsContractSQL and directoryIsAccountSQL split the curated set
// by strkey form. The table CHECK already guarantees every address is
// one or the other, so the two are exhaustive and the census arithmetic
// can rely on it.
const (
	directoryIsContractSQL = "address LIKE 'C%'"
	directoryIsAccountSQL  = "address LIKE 'G%'"
)

// directoryHasIssuingTagSQL is TRUE when the row carries a tag from the
// caller's issuing vocabulary, bound at $1. Lowercased on both sides so
// the predicate agrees with the Go matcher tag for tag.
const directoryHasIssuingTagSQL = `EXISTS (SELECT 1 FROM unnest(tags) t WHERE lower(t) = ANY($1))`

// directoryScamTaggedSQL is the SQL twin of pricingguard's scam check
// over the SAME [DirectoryScamFlagTags] list, rendered through the same
// fail-loud literal builder the listing rank expression uses so a future
// tag can never smuggle a quote into composed SQL.
var directoryScamTaggedSQL = `EXISTS (SELECT 1 FROM unnest(tags) t ` +
	`WHERE lower(t) = ANY(` + scamFlagTagsSQLArray + `))`

// directoryContractRecognisedSQL is C2 and C3 as one predicate: a
// contract address, carrying an issuing tag, carrying no scam tag.
// Spelled once and used by BOTH the census count and the row read.
var directoryContractRecognisedSQL = directoryIsContractSQL +
	` AND ` + directoryHasIssuingTagSQL +
	` AND NOT ` + directoryScamTaggedSQL

// directoryRWACensus counts the whole curated directory in one pass,
// bucketed the way the contract arm narrows it.
//
// One query with FILTER aggregates rather than six round trips: the
// table is ~18.5k rows with a GIN index on tags, and the funnel needs
// every bucket from the same snapshot — counts taken across separate
// statements could straddle a directory sync and fail to reconcile
// through no fault of the arithmetic.
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

// DirectoryRecognisedIssuersWithoutAsset returns the curated directory
// ACCOUNTS carrying an issuing-class tag and no scam tag for which this
// index holds no classic asset — the entities behind
// [DirectoryRWACensus.AccountsIssuingWithoutAsset], named.
//
// It exists so an operator reading the RWA response can see that a
// specific real-world issuer is recognised and still absent, rather than
// reading a bare count and having to open a database session to learn
// which entity it stands for. That was the whole defect the funnel work
// closed on the classic side, and a count with no names reintroduces it
// one level down.
//
// It admits nothing and feeds no valuation. The names are the curated
// third party's own display labels, already served on every asset row as
// issuer_directory_name.
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
