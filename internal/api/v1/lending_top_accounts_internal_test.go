package v1

import "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"

// The production lake reader must serve the pool-wide positions read, or the route answers 503 in every deployment.
var _ lendingPoolPositionsReader = (*clickhouse.ExplorerReader)(nil)
