// Package discovery sights new SEP-41 tokens and oracle-shaped contracts in the ingest stream
// and records them in `discovered_assets` for operator review.
//
// Three pure sniffers, distinguished by [Kind]: [Sniff] matches SEP-41 topic[0] shapes;
// [SniffOracleEvent] a wider oracle symbol set; [SniffOracleCall] InvokeContract function names,
// because an oracle such as Band's StandardReference updates storage without emitting events.
//
// Every sniffer is sighting-only (ADR-0035: discovery is not attribution). It never decodes,
// attributes or feeds price or trade data, and false positives are expected. A sighting becomes
// a source only through the normal add-a-source recipe.
//
// The dispatcher runs the sniffers before decoder dispatch; one [AsyncSink] carries all hits to
// a [Recorder]. Write failures raise the discovery-drops alert.
package discovery
