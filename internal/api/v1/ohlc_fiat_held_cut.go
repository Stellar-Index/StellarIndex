// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import "time"

// heldReadCutFrom is the instant a capped held-back read must cut the other books to. Its unread rows lie in [from, readFrom), and
// the gate drops any in a bucket an established spelling answered, so only an unanswered bucket there can hide one: the cut is the
// end of the latest such bucket (clamped to readFrom), zero when every bucket is answered.
func heldReadCutFrom(read bookRead, from time.Time, answered map[time.Time]struct{}) time.Time {
	if !read.capped || len(answered) == 0 || from.IsZero() {
		return read.readFrom
	}
	step := fiatPointGateInterval.duration()
	b := fiatPointGateBucket(read.readFrom)
	if _, ok := answered[b]; !ok && b.Before(read.readFrom) {
		return read.readFrom
	}
	for floor := fiatPointGateBucket(from); !b.Add(-step).Before(floor); {
		b = b.Add(-step)
		if _, ok := answered[b]; !ok {
			return b.Add(step)
		}
	}
	return time.Time{}
}
