package v1_test

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

// TestHistory_CursorBelowDefaultedFromIsNotSkipped: page 2 of a walk with no `from` recomputes from = now-1h; a cursor
// older than that must still bound the read, or the rows between the cursor and the new from are never served.
func TestHistory_CursorBelowDefaultedFromIsNotSkipped(t *testing.T) {
	cursorTs := time.Now().UTC().Add(-3 * time.Hour)
	raw := fmt.Sprintf("%d:%d:%s:%s:%d", cursorTs.UnixNano(), 100, "sdex", strings.Repeat("a", 64), 0)
	cursor := base64.RawURLEncoding.EncodeToString([]byte(raw))
	for _, c := range []struct {
		name, query string
		wantFrom    func(time.Time) bool
	}{
		{"defaulted from", "", func(f time.Time) bool { return !f.After(cursorTs) }},
		{"defaulted window", "&window=1h", func(f time.Time) bool { return !f.After(cursorTs) }},
		{"explicit from is kept", "&from=" + cursorTs.Add(time.Hour).Format(time.RFC3339), func(f time.Time) bool { return f.After(cursorTs) }},
	} {
		reader := &stubHistoryReader{}
		ts := httpTestServer(t, v1.New(v1.Options{History: reader}))
		resp := mustGet(t, ts.URL+"/v1/history?base=native&quote=fiat:USD&cursor="+cursor+c.query)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d", c.name, resp.StatusCode)
		}
		if !c.wantFrom(reader.lastCall.from) {
			t.Errorf("%s: read from = %s with cursor at %s", c.name, reader.lastCall.from, cursorTs)
		}
	}
}
