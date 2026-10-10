package clickhouse

import (
	"context"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// stalledConn models a wedged ClickHouse: every batch prepare blocks until
// its ctx ends. Methods the LiveSink never reaches stay nil and would panic.
type stalledConn struct{ driver.Conn }

func (stalledConn) PrepareBatch(ctx context.Context, _ string, _ ...driver.PrepareBatchOption) (driver.Batch, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (stalledConn) Close() error { return nil }
