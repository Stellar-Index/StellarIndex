package main

import (
	"context"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// recordingAudit is the in-memory keys.AuditSink the freeze-unfreeze tests use.
type recordingAudit struct {
	entries []platform.AuditEntry
	err     error
}

func (r *recordingAudit) Append(_ context.Context, e platform.AuditEntry) error {
	if r.err != nil {
		return r.err
	}
	r.entries = append(r.entries, e)
	return nil
}
