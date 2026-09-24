package fixture

import (
	"context"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

func wire(ctx context.Context, addr string) {
	txr, _ := clickhouse.NewTxIndexReader(ctx, addr)
	_ = txr
	sr, _ := dialLakeReaderWithRetry(ctx, addr, clickhouse.NewSupplyReader)
	_ = sr
	_ = clickhouse.NewRefreshGate(4)
}

func dialLakeReaderWithRetry[T any](ctx context.Context, addr string, dial func(context.Context, string) (T, error)) (T, error) {
	return dial(ctx, addr)
}
