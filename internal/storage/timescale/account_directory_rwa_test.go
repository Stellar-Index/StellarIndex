package timescale

import (
	"strings"
	"testing"
)

// The contract arm's directory scan.
//
// Two properties are testable without a database: the census must refuse to balance when its
// buckets do not add up, and the count query must use the SAME predicate as the row query,
// else a member could go quietly missing while the funnel reported it as admitted.

// TestDirectoryRWACensus_BalancesOnlyWhenItAddsUp is the accounting
// property. Check must return a reason for each way the buckets can fail to reconcile, else
// the funnel would publish `balanced: true` over figures a reader cannot make meet.
func TestDirectoryRWACensus_BalancesOnlyWhenItAddsUp(t *testing.T) {
	sound := DirectoryRWACensus{
		Entries: 18439, Accounts: 18000, Contracts: 439,
		ContractsScamFlagged: 7, ContractsWithoutIssuingTag: 431, ContractsRecognised: 1,
		AccountsIssuingTagged: 240, AccountsIssuingWithoutAsset: 13,
	}
	if why := sound.Check(); why != "" {
		t.Fatalf("a balanced census reports %q", why)
	}

	for name, break_ := range map[string]func(c *DirectoryRWACensus){
		"address forms do not sum to the entry count": func(c *DirectoryRWACensus) { c.Accounts-- },
		"contract stages do not sum to the contracts": func(c *DirectoryRWACensus) { c.ContractsScamFlagged++ },
		"more issuing-tagged accounts than accounts":  func(c *DirectoryRWACensus) { c.AccountsIssuingTagged = c.Accounts + 1 },
		"more tokenless issuers than issuing ones":    func(c *DirectoryRWACensus) { c.AccountsIssuingWithoutAsset = c.AccountsIssuingTagged + 1 },
	} {
		t.Run(name, func(t *testing.T) {
			c := sound
			break_(&c)
			if why := c.Check(); why == "" {
				t.Errorf("census reports itself balanced with %s", name)
			}
		})
	}
}

// TestDirectoryContractPredicate_IsOneSpelling pins that the census
// count and the row read cannot disagree about who qualified.
//
// They run the same predicate text by construction. Splitting it into two hand-written
// clauses would let count and rows drift apart silently (the funnel says N recognised, the
// set carries M) with nothing to reconcile the difference.
func TestDirectoryContractPredicate_IsOneSpelling(t *testing.T) {
	if !strings.Contains(directoryContractRecognisedSQL, directoryIsContractSQL) {
		t.Error("the recognised-contract predicate does not restrict to contract addresses")
	}
	if !strings.Contains(directoryContractRecognisedSQL, directoryHasIssuingTagSQL) {
		t.Error("the recognised-contract predicate does not require an issuing tag")
	}
	if !strings.Contains(directoryContractRecognisedSQL, "NOT "+directoryScamTaggedSQL) {
		t.Error("the recognised-contract predicate does not exclude scam-class tags")
	}
	// The scam vocabulary is inlined as a literal, not bound, so the predicate can be spliced
	// at any argument offset. It must carry EVERY tag the one Go list carries, or it would
	// admit an address the price gate withholds.
	for _, tag := range DirectoryScamFlagTags {
		if !strings.Contains(directoryScamTaggedSQL, "'"+tag+"'") {
			t.Errorf("scam tag %q is missing from the SQL predicate", tag)
		}
	}
	// One bind parameter, the tag vocabulary, and no others: the row
	// query appends $2 for its limit and the census query appends none,
	// so a second parameter creeping in here would renumber one of them.
	if n := strings.Count(directoryContractRecognisedSQL, "$"); n != strings.Count(directoryContractRecognisedSQL, "$1") {
		t.Errorf("the predicate binds a parameter other than $1: %s", directoryContractRecognisedSQL)
	}
	if bal := strings.Count(directoryContractRecognisedSQL, "(") - strings.Count(directoryContractRecognisedSQL, ")"); bal != 0 {
		t.Errorf("unbalanced parentheses (%+d) in %s", bal, directoryContractRecognisedSQL)
	}
}

// TestLowerTrimAll_NormalisesTheBoundVocabulary pins the normalisation
// the predicate depends on. It lowercases the COLUMN side, so a caller passing `Issuer` would
// match nothing, and the empty set would look like a directory that names no issuers.
func TestLowerTrimAll_NormalisesTheBoundVocabulary(t *testing.T) {
	got := lowerTrimAll([]string{"Issuer", "  ANCHOR ", "", "   ", "custodian"})
	want := []string{"issuer", "anchor", "custodian"}
	if len(got) != len(want) {
		t.Fatalf("lowerTrimAll = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("lowerTrimAll[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
