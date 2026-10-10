package v1_test

// The read path of /v1/issuers/{g_strkey} consults the issuer's live
// AccountEntry for every row, filled or not: a drained, live-sourced row is
// exactly the shape that can hold a home_domain the account no longer
// declares, and the SEP-1 identity stored beside it was fetched from that
// domain.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// stubIssuerAuthFlags is the narrow point-lookup seam; calls records every
// key it was asked for.
type stubIssuerAuthFlags struct {
	live  map[string]clickhouse.AccountAuthFlags
	calls []string
}

func (s *stubIssuerAuthFlags) BulkAccountAuthFlags(_ context.Context, keys []string) (map[string]clickhouse.AccountAuthFlags, error) {
	s.calls = append(s.calls, keys...)
	out := make(map[string]clickhouse.AccountAuthFlags, len(keys))
	for _, k := range keys {
		if f, ok := s.live[k]; ok {
			out[k] = f
		}
	}
	return out, nil
}

func getIssuer(t *testing.T, opts v1.Options, g string) v1.Issuer {
	t.Helper()
	ts := startHTTPTest(t, v1.New(opts).Handler())
	var env struct {
		Data v1.Issuer `json:"data"`
	}
	resp := mustGet(t, ts.URL+"/v1/issuers/"+g)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	mustDecode(t, resp, &env)
	return env.Data
}

// drainedVerifiedRow is a filled, live-sourced row whose SEP-1 identity was
// fetched from, and verified against, lapsed-former.example.
func drainedVerifiedRow() timescale.IssuerRow {
	resolved := "2026-09-01T00:00:00Z"
	return timescale.IssuerRow{
		GStrkey:             mergedIssuerG,
		HomeDomain:          "lapsed-former.example",
		OrgName:             "Former Domain Org",
		OrgVerified:         true,
		AuthRequired:        boolPtr(true),
		AuthFlagsSource:     string(clickhouse.AuthFlagsSourceLive),
		AuthFlagsAsOfLedger: u32(64100000),
		SEP1ResolvedAt:      &resolved,
		SEP1Payload:         json.RawMessage(`{"OrgName":"Former Domain Org","OrgVerified":true}`),
	}
}
