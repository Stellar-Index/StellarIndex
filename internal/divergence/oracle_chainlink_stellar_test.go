package divergence

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

type fixedOracleReader struct{ u *canonical.OracleUpdate }

func (r fixedOracleReader) LatestOracleObservation(context.Context, string, []string, []string) (*canonical.OracleUpdate, error) {
	return r.u, nil
}

// TestChainlinkStellarReference_HeartbeatBudget: a 24h-heartbeat feed must read as a quote inside 26h and as
// unavailable past it, never fall through to Reflector's 30-minute ceiling.
func TestChainlinkStellarReference_HeartbeatBudget(t *testing.T) {
	xlm, err := canonical.NewCryptoAsset("XLM")
	if err != nil {
		t.Fatal(err)
	}
	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}
	pair, err := canonical.NewPair(xlm, usd)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	// 0.25 USD at 18 decimals.
	price := new(big.Int).Mul(big.NewInt(25), new(big.Int).Exp(big.NewInt(10), big.NewInt(16), nil))

	for _, tc := range []struct {
		age     time.Duration
		wantErr error
	}{
		{age: 20 * time.Hour},
		{age: 27 * time.Hour, wantErr: ErrPriceUnavailable},
	} {
		ref, err := NewOracleReference(OracleReferenceOptions{
			Source: OracleSourceChainlinkStellar,
			Reader: fixedOracleReader{u: &canonical.OracleUpdate{
				Source: OracleSourceChainlinkStellar, Asset: xlm, Quote: usd,
				Price: canonical.NewAmount(price), Decimals: 18, Timestamp: now.Add(-tc.age),
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		q, err := ref.LookupQuote(context.Background(), pair, now)
		if tc.wantErr != nil {
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("age %s: err = %v, want %v", tc.age, err, tc.wantErr)
			}
			continue
		}
		if err != nil || q.Price != 0.25 {
			t.Errorf("age %s: price = %v, err = %v; want 0.25", tc.age, q.Price, err)
		}
	}
}
