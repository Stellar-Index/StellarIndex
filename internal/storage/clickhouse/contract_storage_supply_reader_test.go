package clickhouse

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// storageRowsConn serves one scripted ContractStorageSupply result set and
// counts how many rows the reader pulled from it. liveUntil (key_xdr →
// live_until) and tip script the TTL lookup; noTTLTable drops the projection.
type storageRowsConn struct {
	driver.Conn
	rows       *storageRows
	liveUntil  map[string]uint32
	tip        uint32
	noTTLTable bool
}

func (c *storageRowsConn) Query(_ context.Context, query string, args ...any) (driver.Rows, error) {
	if !strings.Contains(query, "ttl_live_until") {
		return c.rows, nil
	}
	byHash := make(map[string]uint32, len(c.liveUntil))
	for k, lu := range c.liveUntil {
		h, err := TTLKeyHash(k)
		if err != nil {
			return nil, err
		}
		byHash[h] = lu
	}
	out := &ttlRows{}
	for _, a := range args {
		if lu, ok := byHash[a.(string)]; ok {
			out.hashes = append(out.hashes, a.(string))
			out.liveUntil = append(out.liveUntil, lu)
		}
	}
	return out, nil
}

func (c *storageRowsConn) QueryRow(_ context.Context, query string, _ ...any) driver.Row {
	switch {
	case strings.HasPrefix(query, "EXISTS TABLE"):
		exists := uint8(1)
		if c.noTTLTable {
			exists = 0
		}
		return scalarRow{v: exists}
	case strings.Contains(query, "max(ledger_seq)"):
		return scalarRow{v: c.tip}
	}
	return scalarRow{err: errors.New("storageRowsConn: unscripted QueryRow " + query)}
}

type scalarRow struct {
	driver.Row
	v   any
	err error
}

func (r scalarRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	switch p := dest[0].(type) {
	case *uint8:
		*p = r.v.(uint8)
	case *uint32:
		*p = r.v.(uint32)
	default:
		return errors.New("scalarRow: unexpected dest type")
	}
	return nil
}

type ttlRows struct {
	driver.Rows
	hashes    []string
	liveUntil []uint32
	i         int
}

func (r *ttlRows) Next() bool { r.i++; return r.i <= len(r.hashes) }
func (r *ttlRows) Scan(dest ...any) error {
	*dest[0].(*string) = r.hashes[r.i-1]
	*dest[1].(*uint32) = r.liveUntil[r.i-1]
	return nil
}
func (r *ttlRows) Err() error   { return nil }
func (r *ttlRows) Close() error { return nil }

type storageRows struct {
	driver.Rows
	lines  []string
	pulled int
}

func (r *storageRows) Next() bool {
	if r.pulled >= len(r.lines) {
		return false
	}
	r.pulled++
	return true
}

func (r *storageRows) Scan(dest ...any) error {
	cols := strings.Split(r.lines[r.pulled-1], "\t")
	*dest[0].(*string) = cols[0]
	*dest[1].(*string) = cols[1]
	*dest[2].(*uint32) = 0
	return nil
}

func (r *storageRows) Err() error   { return nil }
func (r *storageRows) Close() error { return nil }

func fixtureLines(tsv string) []string {
	return strings.Split(strings.TrimSpace(tsv), "\n")
}

// A SAC is refused on its instance entry alone. Its balance rows must neither
// be decoded ahead of that refusal nor be able to pre-empt it with a decode
// error, whichever order the unordered query returns them in.
func TestContractStorageSupplyRefusesSACBeforeDecodingBalances(t *testing.T) {
	lines := fixtureLines(kaleSACStorageTSV)
	instance, balances := lines[0], lines[1:]
	if !isInstanceKeyB64(strings.Split(instance, "\t")[0]) {
		t.Fatal("fixture row 0 is not KALE's instance entry")
	}
	// A balance row whose value the decoder cannot read: had it been decoded
	// before the instance, the reader would fail on it instead of refusing.
	undecodable := strings.Split(balances[0], "\t")[0] + "\tnot-xdr"

	t.Run("instance last", func(t *testing.T) {
		order := append([]string{undecodable}, balances...)
		rows := &storageRows{lines: append(order, instance)}
		r := newExplorerReader(&storageRowsConn{rows: rows})
		_, err := r.ContractStorageSupply(context.Background(), kaleSACContractID)
		if !errors.Is(err, ErrStorageSupplyIsStellarAsset) {
			t.Fatalf("err = %v, want ErrStorageSupplyIsStellarAsset: a balance row was decoded "+
				"before the instance entry that refuses the whole reading", err)
		}
	})

	t.Run("instance first", func(t *testing.T) {
		rows := &storageRows{lines: append([]string{instance, undecodable}, balances...)}
		r := newExplorerReader(&storageRowsConn{rows: rows})
		_, err := r.ContractStorageSupply(context.Background(), kaleSACContractID)
		if !errors.Is(err, ErrStorageSupplyIsStellarAsset) {
			t.Fatalf("err = %v, want ErrStorageSupplyIsStellarAsset", err)
		}
		if rows.pulled != 1 {
			t.Errorf("reader pulled %d rows, want 1: the refusal is known at the instance row "+
				"and every row after it is wasted work", rows.pulled)
		}
	})
}

// The deferred decode must reproduce the verified total through the real
// reader loop, with the instance arriving after the balances it vouches for.
func TestContractStorageSupplyReaderDecodesDeferredBalances(t *testing.T) {
	lines := fixtureLines(caocxwnxStorageTSV)
	var instance string
	var rest []string
	for _, l := range lines {
		if isInstanceKeyB64(strings.Split(l, "\t")[0]) {
			instance = l
			continue
		}
		rest = append(rest, l)
	}
	if instance == "" {
		t.Fatal("fixture carries no instance entry")
	}
	rows := &storageRows{lines: append(rest, instance)}
	got, err := newExplorerReader(&storageRowsConn{rows: rows}).
		ContractStorageSupply(context.Background(), caocxwnxContractID)
	if err != nil {
		t.Fatalf("ContractStorageSupply: %v", err)
	}
	if got.Total.String() != caocxwnxRawTotal || got.BalanceEntries != caocxwnxHolders {
		t.Errorf("total = %s over %d entries, want %s over %d",
			got.Total, got.BalanceEntries, caocxwnxRawTotal, caocxwnxHolders)
	}
	if !got.SelfConsistent() {
		t.Error("SelfConsistent() = false on the verified fixture")
	}
}

// The lake never records an eviction, so a balance whose TTL lapsed still reads
// as current state. A lapsed TEMPORARY balance was deleted by the network and
// must leave the sum; a lapsed PERSISTENT one was archived — still owned and
// restorable — so it stays in the sum and is disclosed as archived.
func TestContractStorageSupplyJudgesBalancesAgainstTheirTTL(t *testing.T) {
	const tip = 64_000_000
	folded := foldFixture(t, caocxwnxContractID, caocxwnxStorageTSV)
	if len(folded.balances) < 2 {
		t.Fatalf("fixture yields %d balances, want at least 2", len(folded.balances))
	}
	archived, live := folded.balances[0], folded.balances[1]
	tempKey, tempEntry := balanceRow(t, xdr.ContractDataDurabilityTemporary, 5_000)

	conn := &storageRowsConn{
		rows: &storageRows{lines: append(fixtureLines(caocxwnxStorageTSV), tempKey+"\t"+tempEntry)},
		tip:  tip,
		liveUntil: map[string]uint32{
			archived.keyB64: tip - 1,
			live.keyB64:     tip,
			tempKey:         tip - 1,
		},
	}
	got, err := newExplorerReader(conn).ContractStorageSupply(context.Background(), caocxwnxContractID)
	if err != nil {
		t.Fatalf("ContractStorageSupply: %v", err)
	}
	if got.Total.String() != caocxwnxRawTotal || got.BalanceEntries != caocxwnxHolders {
		t.Errorf("total = %s over %d entries, want %s over %d: an expired temporary balance "+
			"was summed as if it still existed", got.Total, got.BalanceEntries, caocxwnxRawTotal, caocxwnxHolders)
	}
	if got.ArchivedEntries != 1 || got.ArchivedTotal == nil || got.ArchivedTotal.Cmp(archived.amount) != 0 {
		t.Errorf("archived = %d entries / %v, want 1 / %s", got.ArchivedEntries, got.ArchivedTotal, archived.amount)
	}
	if !got.SelfConsistent() {
		t.Error("SelfConsistent() = false: the archived persistent balance is still in the contract's own " +
			"TotalSupply and HolderCount, so dropping it would manufacture a disagreement")
	}
}

// Without the TTL projection the sum cannot be judged, and an unjudged sum may
// include balances the network has already deleted.
func TestContractStorageSupplyRefusesWithoutTTLProjection(t *testing.T) {
	conn := &storageRowsConn{rows: &storageRows{lines: fixtureLines(caocxwnxStorageTSV)}, noTTLTable: true}
	_, err := newExplorerReader(conn).ContractStorageSupply(context.Background(), caocxwnxContractID)
	if !errors.Is(err, errTTLLiveUntilTableMissing) {
		t.Fatalf("err = %v, want errTTLLiveUntilTableMissing", err)
	}
}
