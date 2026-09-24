package fixture

import (
	"context"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

func dialInTest(ctx context.Context, addr string) {
	_, _ = clickhouse.NewWatermarkReader(ctx, addr)
}
