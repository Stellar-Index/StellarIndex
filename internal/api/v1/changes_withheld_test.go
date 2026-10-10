package v1_test

import (
	"net/url"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

// getChangeSummaryStatus fetches /v1/changes and returns the status and body.
func getChangeSummaryStatus(t *testing.T, srv *v1.Server, entityType, entityID string) (int, string) {
	t.Helper()
	ts := startHTTPTest(t, srv.Handler())
	resp := mustGet(t, ts.URL+"/v1/changes/"+entityType+"/"+url.PathEscape(entityID))
	body, _ := readAll(resp)
	return resp.StatusCode, body
}
