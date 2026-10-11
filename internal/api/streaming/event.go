package streaming

import (
	"fmt"
	"strconv"
	"sync/atomic"
	"time"
)

// idLen is the width of a generated event ID in hex characters.
const idLen = 16

// Event is one SSE message ready for delivery on a topic (see [WriteFrame] for the wire mapping).
// Data is pre-serialised by the producer; Stream is content-agnostic.
type Event struct {
	// ID is the lexicographically-sortable event identifier.
	// Clients use it as `Last-Event-ID` on reconnect to resume.
	ID string

	// Type is the SSE `event:` field, e.g. "price_update". Empty means the default event, which
	// clients dispatch to the unnamed handler.
	Type string

	// Data is the SSE `data:` payload, written verbatim. It MUST NOT contain a bare \n: Stream
	// emits one `data:` line per record, so pre-encode to single-line JSON.
	Data []byte

	// Timestamp is the producer-side wallclock at publish, used by Hub for buffer eviction.
	Timestamp time.Time

	// Retry, when non-zero, is an SSE `retry:` hint for this event. writeStream sends the
	// connection-wide default once in the prelude instead (see DefaultRetry).
	Retry time.Duration
}

// EventTypeStreamGap is the `event:` value Hub.Subscribe emits when a replay cannot cover the
// requested Last-Event-ID: the ring evicted older events, or the cursor is newer than any ID this
// process could have issued (a foreign ID space). IDs are timestamp-packed, not a per-topic
// sequence, so the client cannot observe a gap otherwise. It is sent before any replay event with
// no ID of its own, so it does not advance the resume cursor.
const EventTypeStreamGap = "stream_gap"

// Generator is a goroutine-safe monotonic event-ID source. [Generator.Next] returns a 16-char
// lowercase hex ID: the high 48 bits are unix millis, the low 16 a per-millisecond counter. The
// only client contract is "lexicographic sort = chronological"; treat IDs as opaque. Non-Hub
// callers (per-connection tickers) can construct one directly.
type Generator struct {
	// state packs (lastMillis << 16) | counter into one uint64 for atomic CAS; the counter resets
	// whenever lastMillis advances.
	state atomic.Uint64
}

// Ahead reports whether id is a well-formed ID newer than anything this generator could have
// issued by now (a cursor from a process whose clock runs ahead). It sorts above every ring event,
// so a resume would replay nothing and hide the loss. Malformed IDs return false.
func (g *Generator) Ahead(id string) bool {
	if len(id) != idLen {
		return false
	}
	v, err := strconv.ParseUint(id, 16, 64)
	if err != nil {
		return false
	}
	ceiling := max(g.state.Load(), uint64(time.Now().UnixMilli())<<16|0xFFFF) //nolint:gosec // wall-clock millis are positive
	return v > ceiling
}

// Next returns the next sortable event ID. Goroutine-safe; never repeats an ID within one generator.
func (g *Generator) Next() string {
	now := uint64(time.Now().UnixMilli()) //nolint:gosec // wall-clock millis bounded; sentinel-safe past year 2106 because we only use lower 48 bits
	for {
		old := g.state.Load()
		oldMillis := old >> 16
		oldCounter := old & 0xFFFF

		var next uint64
		switch {
		case now > oldMillis:
			next = (now << 16) // counter resets to 0 when the millisecond advances
		case oldCounter < 0xFFFF:
			// Same or earlier millisecond: bump the counter, which also keeps IDs strictly
			// increasing if the clock steps backwards.
			next = (oldMillis << 16) | (oldCounter + 1)
		default:
			// Counter saturated within one ms: advance the millis instead of wrapping, since
			// wrapping would re-issue every prior ID. The synthetic-future millis is harmless;
			// the `now > oldMillis` branch catches up once the wall clock passes it.
			next = (oldMillis + 1) << 16
		}
		if g.state.CompareAndSwap(old, next) {
			// Zero-padded %016x with time in the high bits sorts lexicographically.
			return fmt.Sprintf("%016x", next)
		}
	}
}
