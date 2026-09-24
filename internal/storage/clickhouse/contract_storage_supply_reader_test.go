package clickhouse

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// storageRowsConn serves one scripted ContractStorageSupply result set and
// counts how many rows the reader pulled from it.
type storageRowsConn struct {
	driver.Conn
	rows *storageRows
}

func (c *storageRowsConn) Query(context.Context, string, ...any) (driver.Rows, error) {
	return c.rows, nil
}

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
