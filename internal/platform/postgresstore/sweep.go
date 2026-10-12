package postgresstore

import (
	"context"
	"fmt"
	"time"
)

// deleteInBatches runs `DELETE ... WHERE pk IN (SELECT pk ... WHERE <cutoff $1> LIMIT $2)` until a batch comes back short or
// sweepMaxBatchesPerCall is reached, under the token sweeps' per-statement bound.
func (s *Store) deleteInBatches(ctx context.Context, op, q string, olderThan time.Time, batchRows int) (int64, error) {
	var total int64
	for i := 0; i < sweepMaxBatchesPerCall; i++ {
		res, err := s.db.ExecContext(ctx, q, olderThan, batchRows)
		if err != nil {
			return total, fmt.Errorf("%s: %w", op, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("%s: rows affected: %w", op, err)
		}
		total += n
		if n < int64(batchRows) {
			break
		}
	}
	return total, nil
}
