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
// Consumers MUST treat IDs as opaque time-sortable strings (16
// lowercase hex chars in this implementation — but that format is
// internal). Live-event ordering across topics is not guaranteed — a
// multi-topic subscriber's channel interleaves topics in publish
// order, not id order. Buffered REPLAY on [Hub.Subscribe] is merged by
// id across every subscribed topic before it is queued, so a resuming
// multi-topic subscriber's replay never walks the `id:` line backwards
// at a topic boundary (#1033); it may still precede a
// [EventTypeStreamGap] marker when the replay could not cover the
// requested cursor (#1035).
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
// if it still holds a replay buffer. [DefaultMaxTopics] is the
// ceiling: over it the reaper evicts subscriber-less topics
// oldest-first, and since a topic with a live subscriber is never
// reaped, a Subscribe that would mint a topic into a map full of
// subscribed ones is refused with [ErrTopicCapacity] (a 503).
// Connections are admitted against the concurrency caps BEFORE they
// can allocate a topic (see [Stream]), so a refused client never
// leaves one behind. [Hub.TopicCount], [Hub.BufferedTopicCount],
// [Hub.TopicsReaped], and [StreamsRejected] expose the bounds.
package streaming
