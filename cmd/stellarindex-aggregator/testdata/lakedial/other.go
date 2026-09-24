package fixture

import (
	"context"

	ch "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

func dialInline(ctx context.Context, addr, user, pass string) {
	r, _ := ch.NewExplorerReaderAuth(ctx, addr, user, pass)
	_ = r
}
