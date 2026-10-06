package wiring

import (
	"context"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/usage"
)

// UsageReaderOrNil returns a v1.UsageReader bound to `c` when
// `c` is non-nil, and a typed-nil v1.UsageReader otherwise. The
// `/v1/account/usage` handler treats `UsageReader == nil` as
// "no usage backend wired" and returns an empty list — the
// correct degradation when Redis is absent.
func UsageReaderOrNil(c *usage.Counter) v1.UsageReader {
	if c == nil {
		return nil
	}
	return usageReaderAdapter{c: c}
}

// usageReaderAdapter bridges *usage.Counter to v1.UsageReader so
// the v1 package stays free of the internal/usage import.
type usageReaderAdapter struct{ c *usage.Counter }

func (a usageReaderAdapter) Read(ctx context.Context, subject string, days int) ([]v1.UsageDay, error) {
	rows, err := a.c.Read(ctx, subject, days)
	if err != nil {
		return nil, err
	}
	out := make([]v1.UsageDay, len(rows))
	for i, d := range rows {
		out[i] = v1.UsageDay{Date: d.Date, Requests: d.Requests}
	}
	return out, nil
}

// UsageRollupReaderOrNil returns a v1.UsageRollupReader over the
// `usage_daily` hypertable when the Timescale store is wired; nil
// otherwise so the handler stays on the legacy per-day Redis path.
func UsageRollupReaderOrNil(s *timescale.Store) v1.UsageRollupReader {
	if s == nil {
		return nil
	}
	return usageRollupReaderAdapter{s: s}
}

// usageRollupReaderAdapter bridges *timescale.Store.ReadUsageDaily
// to v1.UsageRollupReader; see UsageEndpointDay for the derivation.
type usageRollupReaderAdapter struct{ s *timescale.Store }

func (a usageRollupReaderAdapter) ReadRollup(ctx context.Context, subject string, days int) ([]v1.UsageEndpointDay, error) {
	rows, err := a.s.ReadUsageDaily(ctx, subject, days)
	if err != nil {
		return nil, err
	}
	out := make([]v1.UsageEndpointDay, len(rows))
	for i, r := range rows {
		out[i] = UsageEndpointDay(r)
	}
	return out, nil
}

// UsageEndpointDay derives the wire semantics from the granular
// columns: requests = every non-429 outcome (ok + 4xx + 5xx),
// billable = ok + 4xx — the classes middleware.billableClass lets into
// the MonthlyQuota counter, so it equals the legacy per-day total —
// errors = 4xx (excl. 429) + 5xx, throttled = 429s.
func UsageEndpointDay(r timescale.UsageDailyRow) v1.UsageEndpointDay {
	return v1.UsageEndpointDay{
		Date:      r.Day,
		Endpoint:  r.Endpoint,
		Requests:  r.OK + r.ClientErrors + r.ServerErrors,
		Billable:  r.OK + r.ClientErrors,
		Errors:    r.ClientErrors + r.ServerErrors,
		Throttled: r.Throttled,
	}
}
