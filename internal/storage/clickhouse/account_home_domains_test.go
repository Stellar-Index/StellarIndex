package clickhouse

import (
	"context"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/stellar/go-stellar-sdk/xdr"
)

type homeDomainRowsConn struct {
	driver.Conn
	rows *homeDomainRows
}

func (c *homeDomainRowsConn) Query(context.Context, string, ...any) (driver.Rows, error) {
	return c.rows, nil
}

type homeDomainRows struct {
	driver.Rows
	pairs  [][2]string
	pulled int
}

func (r *homeDomainRows) Next() bool {
	if r.pulled >= len(r.pairs) {
		return false
	}
	r.pulled++
	return true
}

func (r *homeDomainRows) Scan(dest ...any) error {
	p := r.pairs[r.pulled-1]
	*dest[0].(*string) = p[0]
	*dest[1].(*string) = p[1]
	return nil
}

func (r *homeDomainRows) Err() error   { return nil }
func (r *homeDomainRows) Close() error { return nil }

// accountEntryB64 encodes an account entry declaring homeDomain. The reader
// keys its result on the scanned account_id column, so the entry's own
// AccountId is an arbitrary valid key.
func accountEntryB64(t *testing.T, homeDomain string) string {
	t.Helper()
	aid := xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: &xdr.Uint256{1}}
	b64, err := xdr.MarshalBase64(xdr.LedgerEntry{Data: xdr.LedgerEntryData{
		Type:    xdr.LedgerEntryTypeAccount,
		Account: &xdr.AccountEntry{AccountId: aid, HomeDomain: xdr.String32(homeDomain)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return b64
}

// TestAccountHomeDomainsReportsADeclaredNone — a live entry that declares no
// home_domain must come back PRESENT with "", so issuer-enrich can clear a
// domain the account has dropped; only an unreadable entry is absent.
func TestAccountHomeDomainsReportsADeclaredNone(t *testing.T) {
	const (
		declared     = "GBFXOHVAS7DXHZPMPZL4HDPPMGSSJBWDGEOXSYHMPTSJKDFHPPFXFZ2K"
		declaresNone = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		unreadable   = "GAEVD52W5E4Q2KTVQXC76ZSZYBEXXR3GGQZAIEP6BW3JMBTUBCHRY6UM"
	)
	rows := &homeDomainRows{pairs: [][2]string{
		{declared, accountEntryB64(t, "ultracapital.xyz")},
		{declaresNone, accountEntryB64(t, "")},
		{unreadable, "not-xdr"},
	}}
	r := newExplorerReader(&homeDomainRowsConn{rows: rows})
	got, err := r.AccountHomeDomains(context.Background(), []string{declared, declaresNone, unreadable})
	if err != nil {
		t.Fatalf("AccountHomeDomains: %v", err)
	}
	if got[declared] != "ultracapital.xyz" {
		t.Errorf("%s = %q, want ultracapital.xyz", declared, got[declared])
	}
	if d, ok := got[declaresNone]; !ok || d != "" {
		t.Errorf("%s = (%q, present=%v), want (\"\", present) — the entry was read and declares none", declaresNone, d, ok)
	}
	if _, ok := got[unreadable]; ok {
		t.Errorf("%s is present, want absent — an undecodable entry was not read", unreadable)
	}
}

// TestAccountEntryReadsUseTheSortKey: ledger_entries_current is ordered by (entry_type, key_xdr); filtering accounts on
// account_id falls back to a bloom scan (40.7M rows for 50 accounts on r1), so these reads must pass LedgerKey XDR.
func TestAccountEntryReadsUseTheSortKey(t *testing.T) {
	const g = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	want, err := accountKeyXDR(g)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{accountsUnspendableQuery, signerCurrentQuery} {
		if !strings.Contains(q, "key_xdr IN (?)") || strings.Contains(q, "account_id IN") {
			t.Errorf("query must filter on key_xdr, not account_id:\n%s", q)
		}
	}
	conn := &stubConn{respond: func(string) (driver.Rows, error) { return &stubRows{}, nil }}
	r := &ExplorerReader{conn: conn}
	if _, err := r.AccountsUnspendable(context.Background(), []string{g, "not-a-strkey"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AccountHomeDomains(context.Background(), []string{g}); err != nil {
		t.Fatal(err)
	}
	if len(conn.queries) != 2 {
		t.Fatalf("queries = %d, want 2", len(conn.queries))
	}
	for i, q := range conn.queries {
		if !strings.Contains(q, "key_xdr IN (?)") {
			t.Errorf("query %d filters on something other than key_xdr:\n%s", i, q)
		}
		if keys, ok := conn.args[i][0].([]string); !ok || len(keys) != 1 || keys[0] != want {
			t.Errorf("query %d args = %v, want [%s]", i, conn.args[i], want)
		}
	}
}
