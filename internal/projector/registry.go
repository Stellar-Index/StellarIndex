package projector

import (
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
)

// BuildRegistry builds one [Source] per name whose [pipeline.SourceSpec] has a Projector (others, written by the dispatcher, are skipped); watched specs (sep41) register unprompted unless no SEP-41 contract is watched, since the dispatcher cedes them as sole writer; errors when a source's oracle config is empty.
func BuildRegistry(names []string, oracle config.OracleConfig, watchedSEP41 []string, gated map[string][]contractid.Option, soroswapOpts ...soroswap.DecoderOption) (Registry, error) {
	var sources []Source
	seen := map[string]bool{}
	add := func(name string) error {
		s, ok, err := buildSource(name, oracle, watchedSEP41, gated, soroswapOpts...)
		if ok {
			sources = append(sources, s)
		}
		return err
	}
	for _, name := range names {
		lower := strings.ToLower(strings.TrimSpace(name))
		seen[lower] = true
		if err := add(lower); err != nil {
			return Registry{}, err
		}
	}
	for _, spec := range pipeline.Specs() {
		if !spec.Watched || seen[spec.Name] {
			continue
		}
		if err := add(spec.Name); err != nil {
			return Registry{}, err
		}
	}
	return Registry{Sources: sources}, nil
}

// KnownProjectorSources is the set of source names the projector writes, accepted by BuildRegistry and `stellarindex-ops projector-replay -source`; find-data-gaps reads it to tell whether a gap target is projected (AGENTS.md invariant 7).
var KnownProjectorSources = knownProjectorSources()

func knownProjectorSources() map[string]struct{} {
	out := map[string]struct{}{}
	for _, spec := range pipeline.Specs() {
		if spec.Projector != nil {
			out[spec.Name] = struct{}{}
		}
	}
	return out
}

// buildSource builds the projector [Source] for name from its spec.
// ok=false with a nil error means the projector does not write name, or
// it is a watched source with nothing watched.
func buildSource(name string, oracle config.OracleConfig, watchedSEP41 []string, gated map[string][]contractid.Option, soroswapOpts ...soroswap.DecoderOption) (Source, bool, error) {
	spec, ok := pipeline.SpecByName(name)
	if !ok || spec.Projector == nil {
		return Source{}, false, nil
	}
	p := spec.Projector
	a := pipeline.BuildArgs{Oracle: oracle, WatchedSEP41: watchedSEP41, Gated: gated, SoroswapOpts: soroswapOpts}
	newDecoder := spec.NewDecoder
	if p.NewDecoder != nil {
		newDecoder = p.NewDecoder
	}
	dec, err := newDecoder(a)
	if err != nil || dec == nil {
		return Source{}, false, err
	}
	src := Source{
		Name:                spec.Name,
		Decoder:             dec,
		Topic0Syms:          p.Topic0Syms,
		ExcludeTopic0Syms:   p.ExcludeTopic0Syms,
		NeedsStateWriteKeys: p.NeedsStateWriteKeys,
		Genesis:             p.Genesis,
	}
	if p.ContractIDs != nil {
		src.ContractIDs = p.ContractIDs(a)
	}
	if p.LiveContractIDs != nil {
		src.ContractIDsFunc = p.LiveContractIDs(dec)
	}
	return src, true, nil
}

// IsProjectedSource reports whether the projector owns name's writes (AGENTS.md invariant [7]); a build error from incomplete config still counts as projected.
func IsProjectedSource(name string, oracle config.OracleConfig, watchedSEP41 []string) bool {
	_, ok, err := buildSource(strings.ToLower(strings.TrimSpace(name)), oracle, watchedSEP41, nil)
	return ok || err != nil
}
