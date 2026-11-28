// Package oracleagg reproduces Blend Capital's oracle-aggregator adapters (the SEP-40 oracles Blend and BLNT
// pools point at): it decodes an adapter's configuration from its own ledger entries and recomputes what its
// lastprice(asset) and decimals() return. Evidence and source lines: docs/operations/wasm-audits/oracle-aggregator.md.
package oracleagg

// Program is an audited oracle-aggregator build.
type Program int

// The audited builds. Neither has an upgrade path, so a contract's program is fixed by its WASM hash.
const (
	ProgramUnknown Program = iota
	ProgramV1              // oracle-aggregator v1.0.0: per-asset SEP-40 lastprice passthrough
	ProgramV2              // oracle-aggregator v2.0.0: period walk with max_age and max_dev
)

// WASM hashes of the audited builds, byte-identical to the upstream release assets.
const (
	HashV1 = "a7463634674ca764401e713e4e29aef4e8521877f76ab78b1e91bea9d46ec6bb"
	HashV2 = "41df04894bf2d8291492326f7a6ab9c90a90cc9828e31990ce8aeeab42680bfd"
)

// ProgramForHash maps a lower-hex WASM hash to its audited program; ProgramUnknown for any other.
func ProgramForHash(hash string) Program {
	switch hash {
	case HashV1:
		return ProgramV1
	case HashV2:
		return ProgramV2
	}
	return ProgramUnknown
}

func (p Program) String() string {
	switch p {
	case ProgramV1:
		return "oracle-aggregator v1.0.0"
	case ProgramV2:
		return "oracle-aggregator v2.0.0"
	case ProgramUnknown:
	}
	return "unknown"
}

// FeedProgram is the build of the underlying Reflector feed, which decides v1's lastprice age rule.
type FeedProgram int

// Reflector builds whose price/lastprice the recipe models (docs/operations/wasm-audits/reflector.md).
const (
	FeedUnknown     FeedProgram = iota
	FeedReflectorV6             // v6.0.x: lastprice is None once the last round is two resolutions old
	FeedReflectorV7             // v7.1.0: lastprice has no age limit
)

// ReflectorV71Hash is the attested Reflector v7.1.0 build every Reflector contract runs since L65,079,084.
const ReflectorV71Hash = "25fc306065cbb9b05461db78ec259802cb916053ba5b3ab911b0a72dd1a81439"

// FeedProgramForHash maps a feed's lower-hex WASM hash to the modelled build. The v6.0.x hashes are known only by
// prefix (reflector.md), so a feed running one is unsupported rather than guessed.
func FeedProgramForHash(hash string) FeedProgram {
	if hash == ReflectorV71Hash {
		return FeedReflectorV7
	}
	return FeedUnknown
}
