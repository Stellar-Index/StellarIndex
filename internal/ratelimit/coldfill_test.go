package ratelimit_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
)

func TestChargeColdFill_ContextContract(t *testing.T) {
	if err := ratelimit.ChargeColdFill(context.Background()); err != nil {
		t.Fatalf("no charge in context: %v, want free", err)
	}
	var got int
	refusing := ratelimit.WithColdFillCharge(context.Background(), func(n int) error {
		got = n
		return ratelimit.ErrColdFillRefused
	})
	if err := ratelimit.ChargeColdFill(refusing); !errors.Is(err, ratelimit.ErrColdFillRefused) || got != ratelimit.ColdFillCost {
		t.Fatalf("charge = (%v, %d tokens), want the refusal for ColdFillCost (%d)", err, got, ratelimit.ColdFillCost)
	}
	if err := ratelimit.ChargeColdFill(ratelimit.WithColdFillCharge(refusing, nil)); err != nil {
		t.Fatalf("nil charge over a refusing one: %v, want exempt", err)
	}
}
