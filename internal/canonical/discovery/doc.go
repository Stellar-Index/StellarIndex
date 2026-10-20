// Package discovery sights new SEP-41 tokens and oracle-shaped contracts in the ingest stream and
// records them in `discovered_assets` for operator review.
//
// Three pure sniffers, distinguished by [Kind]: [Sniff] (SEP-41 topic[0] shapes),
// [SniffOracleEvent] and [SniffOracleCall] (InvokeContract names, since Band's StandardReference
// emits no events). Sighting only (ADR-0035: discovery is not attribution): never decodes,
// attributes or feeds price or trade data, and false positives are expected. One [AsyncSink]
// carries hits to a [Recorder]; write failures raise the discovery-drops alert.
package discovery
