// Package discovery sights new SEP-41 tokens and oracle-shaped contracts in
// the ingest stream and records them in `discovered_assets` for operator
// review, without a code change.
//
// Three pure sniffers, distinguished by [Kind]: [Sniff] matches SEP-41
// topic[0] shapes; [SniffOracleEvent] matches a wider oracle symbol set
// ([oracleEventSymbols]); [SniffOracleCall] matches InvokeContract function
// names ([oracleCallFunctions]) because an oracle such as Band's
// StandardReference updates storage without emitting any event.
//
// Every sniffer is sighting-only (ADR-0035: discovery is not attribution).
// It never decodes, attributes or feeds price or trade data, and false
// positives are expected; operator review catches them. A sighting becomes
// a source only through the normal add-a-source recipe.
//
// The dispatcher calls the sniffers on every event and contract call
// before decoder dispatch; one [AsyncSink] carries all hits to a [Recorder]
// (Postgres-backed in internal/storage/timescale). `stellarindex-ops
// discovery` lists sightings; write failures raise the discovery-drops alert.
package discovery
