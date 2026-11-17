package timescale

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"testing"
)

// TestQueryVenueFold_ReportsTheCap: a fold holding more than the venue cap returns the first PositionsVenueLimit rows and
// ErrPositionsTruncated; one holding exactly the cap is complete.
func TestQueryVenueFold_ReportsTheCap(t *testing.T) {
	scan := func(r *sql.Rows) (string, error) {
		var v string
		return v, r.Scan(&v)
	}
	for _, c := range []struct {
		served    int
		wantRows  int
		truncated bool
	}{
		{PositionsVenueLimit + 1, PositionsVenueLimit, true},
		{PositionsVenueLimit, PositionsVenueLimit, false},
	} {
		rows := make([][]driver.Value, c.served)
		for i := range rows {
			rows[i] = []driver.Value{"C"}
		}
		store, conn := newScriptedStore(t, scriptedResult{cols: []string{"venue"}, rows: rows})
		got, err := queryVenueFold(context.Background(), store.db, "Probe", "SELECT venue LIMIT $2", "GUSER", scan)
		if len(got) != c.wantRows || errors.Is(err, ErrPositionsTruncated) != c.truncated {
			t.Errorf("served %d: rows=%d err=%v, want rows=%d truncated=%v", c.served, len(got), err, c.wantRows, c.truncated)
		}
		if args := conn.only(t).args; len(args) != 2 || fmt.Sprint(args[1]) != fmt.Sprint(PositionsVenueLimit+1) {
			t.Errorf("served %d: args = %v, want the cap plus one", c.served, args)
		}
	}
}
