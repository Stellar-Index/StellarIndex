package explorer

import (
	"context"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// saturationReader is a capReader whose AccountStateCached fails with a
// caller-chosen error, so a test can drive the saturation-vs-genuine-error
// branch of the AccountState handler's status mapping without a live lake.
type saturationReader struct {
	*capReader
	err error
}

func (r *saturationReader) AccountStateCached(context.Context, string) (clickhouse.AccountState, bool, error) {
	return clickhouse.AccountState{}, false, r.err
}
