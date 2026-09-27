// Package streaming provides the shared SSE (Server-Sent Events)
// infrastructure for the v1 streaming endpoints (/v1/price/stream,
// /v1/price/tip/stream, /v1/observations/stream, …).
//
// The model is publish/subscribe with per-topic fanout:
//
//   - A single [Hub] holds per-topic [ring] buffers + active subscribers.
//   - Producers (the aggregator, the dispatcher) call [Hub.Publish]
//     to broadcast an event on a topic.
//   - HTTP handlers call [Stream] which subscribes to one or more
//     topics, replays buffered events from the client's
//     `Last-Event-ID` (RFC 8895 §9), and forwards live events as SSE
//     frames until the request context cancels.
//
// Event ordering is per-topic; consumers MUST treat IDs as opaque
// time-sortable strings (16 lowercase hex chars in this implementation
// — but that format is internal). Cross-topic ordering is not
// guaranteed.
//
// Slow subscribers are dropped, not blocked. When the per-subscriber
// channel is full, the offending subscription is closed; the client
// sees the connection drop and reconnects with `Last-Event-ID` to
// resume from the buffer. This keeps a single misbehaving consumer
// from stalling fanout to healthy peers.
//
// Last-Event-ID resume is best-effort and newest-first: replay is the
// buffered events after the requested ID, truncated to the NEWEST ones
// that fit the subscriber queue (see [Hub.Subscribe]), so a client gone
// longer than that sees IDs jump forward rather than a stale backlog.
// An empty Last-Event-ID replays nothing — a fresh connection gets
// only events published after it subscribes. The window the buffer
// covers at each cadence is documented on [DefaultBufferSize].
//
// Which endpoints resume: /v1/price/stream always, and
// /v1/price/tip/stream when a Hub is wired (its shared producers
// publish into it). /v1/observations/stream and /v1/ledger/stream are
// per-connection producers with no buffer and ignore Last-Event-ID.
//
// Topics are reaped, not kept forever. Topic keys come from the
// request on /v1/price/stream, so an unbounded map is an
// unauthenticated memory-exhaustion lever (REL-05). A topic with no
// subscribers is dropped once it has nothing left to offer — right
// away if it was never published to, or after [DefaultTopicIdleTTL]
// if it still holds a replay buffer — and [DefaultMaxTopics] caps the
// map regardless. A topic with a live subscriber is never reaped.
// Connections are admitted against the concurrency caps BEFORE they
// can allocate a topic (see [Stream]), so a refused client never
// leaves one behind. [Hub.TopicCount], [Hub.TopicsReaped], and
// [StreamsRejected] expose the bounds.
package streaming
