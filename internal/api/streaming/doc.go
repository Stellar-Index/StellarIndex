// Package streaming is the SSE fan-out behind the v1 stream endpoints: a [Hub] of per-topic ring
// buffers that producers [Hub.Publish] into and [Stream] serves. Live events interleave topics in
// publish order; replay is merged by id across topics, so a resume never walks `id:` backwards.
//
// A full subscriber queue drops that subscriber rather than stall the rest. Last-Event-ID resume is
// best-effort, newest-first: the buffered events after the cursor, cut to what fits the queue, with
// an [EventTypeStreamGap] marker when the buffer could not cover it. Only /v1/price/stream, and
// /v1/price/tip/stream when a Hub is wired, resume.
//
// Topic keys come from unauthenticated requests, so topics are reaped: a subscriber-less topic goes
// at once if never published to, otherwise after [DefaultTopicIdleTTL]. Over [DefaultMaxTopics] the
// reaper evicts subscriber-less topics oldest-first and a new topic is refused with
// [ErrTopicCapacity] (503). [Hub.TopicCount], [Hub.BufferedTopicCount],
// [Hub.TopicsReaped] and [StreamsRejected] expose the bounds.
package streaming
