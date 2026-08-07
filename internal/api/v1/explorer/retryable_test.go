package explorer

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

type fakeNetTimeout struct{}

func (fakeNetTimeout) Error() string   { return "read tcp 127.0.0.1:34612->127.0.0.1:9300: i/o timeout" }
func (fakeNetTimeout) Timeout() bool   { return true }
func (fakeNetTimeout) Temporary() bool { return true }

// Site audit 2026-08-07: /accounts/{g}/activity served "detached refresh
// capacity saturated" as a 500 Internal error (its handler tested only
// readTimedOut), and /operations did the same with driver-level conn
// i/o timeouts. Every capacity-class error must classify retryable.
func TestRetryableColdMiss_CapacityClasses(t *testing.T) {
	ctx := context.Background()
	for name, err := range map[string]error{
		"pkg saturation sentinel":    errRefreshSaturated,
		"clickhouse saturation":      clickhouse.ErrRefreshSaturated,
		"wrapped saturation":         fmt.Errorf("explorer: %w", errRefreshSaturated),
		"deadline":                   context.DeadlineExceeded,
		"driver net timeout":         fakeNetTimeout{},
		"wrapped driver net timeout": fmt.Errorf("query processing: %w", fakeNetTimeout{}),
	} {
		if !retryableColdMiss(ctx, err) {
			t.Errorf("%s: classified as non-retryable — this renders as 500 Internal error", name)
		}
	}
	if retryableColdMiss(ctx, errors.New("column not found")) {
		t.Error("a real bug classified retryable — 503 would mask it")
	}
}
