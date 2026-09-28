package v1_test

import (
	"context"
	"net/http"
	"sync"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

// countingPoolTokensReader counts upstream PoolTokens calls per source.
type countingPoolTokensReader struct {
	mu    sync.Mutex
	calls map[string]int
}

func (c *countingPoolTokensReader) PoolTokens(_ context.Context, source string) (map[string][]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls[source]++
	return map[string][]string{"CPOOL1": {"CUSDC1", "CXLM1"}}, nil
}

// The positions route is keyed by address, so each new address is a cache
// miss there; the pool-token scans behind it must not repeat per address.
func TestExplorer_AccountPositions_PoolTokensReadOncePerSource(t *testing.T) {
	t.Parallel()
	upstream := &countingPoolTokensReader{calls: map[string]int{}}
	srv := v1.New(v1.Options{Positions: &stubPositionsReader{}, ProtocolPoolTokens: upstream})
	base := httpTestServer(t, srv).URL

	for _, g := range []string{testG, otherG} {
		resp := mustGet(t, base+"/v1/accounts/"+g+"/positions")
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d", g, resp.StatusCode)
		}
	}

	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	for _, source := range []string{"blend", "aquarius"} {
		if n := upstream.calls[source]; n != 1 {
			t.Errorf("PoolTokens(%q) upstream calls = %d across two addresses, want 1", source, n)
		}
	}
}
