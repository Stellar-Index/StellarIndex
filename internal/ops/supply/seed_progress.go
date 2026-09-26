package supply

import (
	"fmt"

	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
)

// startSeedHeartbeat starts a seed pass's node_exporter heartbeat (inert off
// r1 unless -heartbeat names a path). The caller Stops it with the outcome.
func startSeedHeartbeat(job, path string) *opsutil.JobHeartbeat {
	hb := opsutil.NewJobHeartbeat(job, path, nil)
	if hb.Enabled() {
		fmt.Printf("%s: heartbeat -> %s\n", job, hb.Path())
	}
	hb.Start()
	return hb
}

// seedProgress feeds a seed pass's heartbeat one monotone total: ledgers
// reduced by the lake walk, then rows emitted. The walk emits nothing until its
// last window, so without the ledger count stellarindex_ops_job_no_progress
// could not tell hours of healthy reduction from a hang. The cursor is the
// highest ledger reduced.
type seedProgress struct {
	hb     *opsutil.JobHeartbeat
	total  uint64
	cursor uint64
}

func (p *seedProgress) window(from, to uint32) {
	if p == nil {
		return
	}
	p.total += uint64(to-from) + 1
	p.cursor = uint64(to)
	p.hb.Progress(p.total, p.cursor)
}

func (p *seedProgress) row() {
	if p == nil {
		return
	}
	p.total++
	p.hb.Progress(p.total, p.cursor)
}
