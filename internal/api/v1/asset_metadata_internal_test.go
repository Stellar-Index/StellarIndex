package v1

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// sacOnlyExplorer answers the SAC lookup and has no account state.
type sacOnlyExplorer struct{ sacStubReader }

func (*sacOnlyExplorer) AccountStateCached(context.Context, string) (clickhouse.AccountState, bool, error) {
	return clickhouse.AccountState{}, false, nil
}

// The metadata route resolves an id the way the detail does and carries the detail's ticker-collision verdict, so an
// impersonator's issuer-written branding is never served there without the warning the detail gives it.
func TestAssetMetadata_ResolvesAndWarnsLikeTheDetail(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	const usdc = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	plain := New(Options{VerifiedCurrencies: cat})
	// The stub answers only the SAC lookup, so it backs only the SAC case.
	withLake := New(Options{
		VerifiedCurrencies: cat,
		Explorer:           &sacOnlyExplorer{sacStubReader{name: "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN", found: true}},
	})
	for name, tc := range map[string]struct {
		id        string
		wantID    string
		collision bool
	}{
		"verified classic":   {id: usdc, wantID: usdc},
		"its SAC":            {id: "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75", wantID: usdc},
		"impersonating USDC": {id: "USDC-" + cetesIssuer, wantID: "USDC-" + cetesIssuer, collision: true},
	} {
		t.Run(name, func(t *testing.T) {
			srv := plain
			if tc.id[0] == 'C' {
				srv = withLake
			}
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/assets/"+tc.id+"/metadata", nil))
			var body struct {
				Data  AssetMetadata `json:"data"`
				Flags Flags         `json:"flags"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			if body.Data.AssetID != tc.wantID {
				t.Errorf("asset_id = %q, want %q (the detail's resolution)", body.Data.AssetID, tc.wantID)
			}
			if body.Flags.UnverifiedTickerCollision != tc.collision {
				t.Errorf("unverified_ticker_collision = %v, want %v", body.Flags.UnverifiedTickerCollision, tc.collision)
			}
		})
	}
}
