package v1

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sourcenet"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// SourcesStatsReader is the seam for /v1/sources?include=stats; timescale.Store implements it via GetSourceStats.
type SourcesStatsReader interface {
	GetSourceStats(ctx context.Context) ([]timescale.SourceStats, error)
	GetSourceVolumeHistory24h(ctx context.Context) ([]timescale.SourceVolumeBucket, error)
	GetSourceVolumeHistory7d(ctx context.Context) ([]timescale.SourceVolumeBucket, error)
}

// sourcesStatsAtReader is what the TTL cache adds to SourcesStatsReader:
// each read also returns the fill time of the slot it served, so
// handleSources stamps as_of with the data's vintage instead of now.
type sourcesStatsAtReader interface {
	GetSourceStatsAt(ctx context.Context) ([]timescale.SourceStats, time.Time, error)
	GetSourceVolumeHistory24hAt(ctx context.Context) ([]timescale.SourceVolumeBucket, time.Time, error)
	GetSourceVolumeHistory7dAt(ctx context.Context) ([]timescale.SourceVolumeBucket, time.Time, error)
}

// sourcesStatsAt adapts r to sourcesStatsAtReader; an uncached reader's
// reads report a zero fill time, which stamps as_of now.
func sourcesStatsAt(r SourcesStatsReader) sourcesStatsAtReader {
	if at, ok := r.(sourcesStatsAtReader); ok {
		return at
	}
	return liveSourcesStats{r}
}

type liveSourcesStats struct{ SourcesStatsReader }

func (l liveSourcesStats) GetSourceStatsAt(ctx context.Context) ([]timescale.SourceStats, time.Time, error) {
	rows, err := l.GetSourceStats(ctx)
	return rows, time.Time{}, err
}

func (l liveSourcesStats) GetSourceVolumeHistory24hAt(ctx context.Context) ([]timescale.SourceVolumeBucket, time.Time, error) {
	rows, err := l.GetSourceVolumeHistory24h(ctx)
	return rows, time.Time{}, err
}

func (l liveSourcesStats) GetSourceVolumeHistory7dAt(ctx context.Context) ([]timescale.SourceVolumeBucket, time.Time, error) {
	rows, err := l.GetSourceVolumeHistory7d(ctx)
	return rows, time.Time{}, err
}

// VolumeBucket is one hour bucket on the per-source 24h sparkline; volume_usd is a decimal string to preserve precision.
type VolumeBucket struct {
	Hour       WireTime `json:"hour"`
	VolumeUSD  string   `json:"volume_usd"`
	TradeCount int64    `json:"trade_count"`
}

// parseSourcesInclude reads /v1/sources' `include`; either sparkline implies stats.
func parseSourcesInclude(raw string) (stats, sparkline, sparkline7d bool) {
	for _, f := range strings.Split(raw, ",") {
		switch strings.TrimSpace(f) {
		case "stats":
			stats = true
		case "sparkline":
			sparkline, stats = true, true
		case "sparkline7d":
			sparkline7d, stats = true, true
		}
	}
	return stats, sparkline, sparkline7d
}

// buildSourceVolumeHistory projects the per-(source, hour) raw buckets
// into a per-source trailing-24h series, zero-filling missing hours so
// clients render a continuous chart rather than gappy bars. Carries
// both USD volume + trade count per hour.
func buildSourceVolumeHistory(buckets []timescale.SourceVolumeBucket, hours int) map[string][]VolumeBucket {
	type hourRaw struct {
		vol   string
		count int64
	}
	rawBySource := map[string]map[time.Time]hourRaw{}
	for _, b := range buckets {
		if rawBySource[b.Source] == nil {
			rawBySource[b.Source] = map[time.Time]hourRaw{}
		}
		rawBySource[b.Source][b.Hour.UTC()] = hourRaw{vol: b.VolumeUSD, count: b.TradeCount}
	}
	out := map[string][]VolumeBucket{}
	now := time.Now().UTC().Truncate(time.Hour)
	for src, raw := range rawBySource {
		series := make([]VolumeBucket, 0, hours)
		for i := hours - 1; i >= 0; i-- {
			hour := now.Add(time.Duration(-i) * time.Hour)
			hr := raw[hour]
			vol := hr.vol
			if vol == "" {
				vol = "0"
			}
			series = append(series, VolumeBucket{Hour: WireTime(hour), VolumeUSD: vol, TradeCount: hr.count})
		}
		out[src] = series
	}
	return out
}

func markXLMUnpriced(into map[string]bool, buckets []timescale.SourceVolumeBucket) {
	for _, b := range buckets {
		if b.XLMUnpriced {
			into[b.Source] = true
		}
	}
}

// Source is the wire shape for /v1/sources entries, copied field by field from external.Metadata so an internal-only field cannot change the JSON contract.
type Source struct {
	Name string `json:"name"`
	// Class is the top-level taxonomy (exchange / aggregator / oracle / authority_sanity / lending /
	// router / bridge). It drives the aggregator's class filter: only `exchange` contributes to VWAP
	// by default. /v1/methodology's `source_classes` defines each one.
	Class string `json:"class"`
	// Subclass refines `class=exchange` into `dex` / `cex` / `fx`; empty for every other class.
	Subclass          string `json:"subclass,omitempty"`
	IncludeInVWAP     bool   `json:"include_in_vwap"`
	Paid              bool   `json:"paid"`
	BackfillAvailable bool   `json:"backfill_available"`
	// BackfillSafe gates the `stellarindex-ops backfill` subcommand because Soroban contracts upgrade
	// in place. On-chain Soroban sources stay false until a per-WASM-hash audit
	// (`docs/operations/wasm-audits/`); off-chain CEX/FX sources are always true.
	BackfillSafe  bool `json:"backfill_safe"`
	DefaultWeight int  `json:"default_weight"`
	// OnChain is true when the source observes the Stellar network directly rather than an
	// off-chain vendor API (see external.IsOnChain). Explorer surfaces filter on it so
	// reference-pricing feeds do not appear as Stellar on-chain activity.
	OnChain bool `json:"on_chain"`
	// Selectable is true when `source=` accepts this name on the
	// single-source routes: on-chain sources and CEX venues. False for
	// data vendors, which those routes refuse with 400.
	Selectable bool `json:"selectable"`
	// Stats columns — populated only when `?include=stats` is set.
	// 0 / "" when the source had no trades in 24h.
	TradeCount24h int64  `json:"trade_count_24h,omitempty"`
	VolumeUSD24h  string `json:"volume_24h_usd,omitempty"`
	// VolumeLowerBound is true when the USD volume figures (24h total and
	// the volume histories) leave out XLM-leg trades that carry no
	// trade-time usd_volume. Those are never valued at today's XLM price.
	VolumeLowerBound bool  `json:"volume_lower_bound,omitempty"`
	MarketsCount24h  int64 `json:"markets_count_24h,omitempty"`
	// VolumeHistory24h holds per-hour USD volume buckets for the trailing 24h, only with
	// `?include=stats,sparkline`. Holes are zero-filled so there are always 24 entries, oldest first.
	VolumeHistory24h []VolumeBucket `json:"volume_history_24h,omitempty"`
	// VolumeHistory7d — same per-hour shape over the trailing 7 days
	// (168 buckets), populated only when the request includes
	// `sparkline7d`. Powers the source page's 24h/7d activity toggle.
	VolumeHistory7d []VolumeBucket `json:"volume_history_7d,omitempty"`
}

// validSourceClasses is the allow-list of values accepted for the
// `class` query parameter: every external.Class, because every class is
// served on some row. TestSourceClassSurfacesAgree holds it to the
// constants and to both spec enums.
var validSourceClasses = map[string]bool{
	string(external.ClassExchange):        true,
	string(external.ClassAggregator):      true,
	string(external.ClassOracle):          true,
	string(external.ClassAuthoritySanity): true,
	string(external.ClassLending):         true,
	string(external.ClassRouter):          true,
	string(external.ClassBridge):          true,
}

// sourceClassList renders validSourceClasses for the invalid-class 400,
// so the message cannot drift from what the filter accepts.
func sourceClassList() string {
	names := make([]string, 0, len(validSourceClasses))
	for c := range validSourceClasses {
		names = append(names, c)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// servesPubnetReference reports whether the compiled-in reference registries
// (verified-currency catalogue, routers, off-chain feeds) describe this
// network. They name pubnet identities only; an unknown network reads as
// pubnet, as in sourcenet.Applicable.
func (s *Server) servesPubnetReference() bool {
	return s.Network != sourcenet.Testnet && s.Network != sourcenet.Futurenet
}

// handleSources serves GET /v1/sources.
//
// Returns the static external.Registry scoped to the configured network (sourcenet.Applicable),
// sorted by name so responses are deterministic and CDN-cacheable. The catalogue is small enough
// that it is not paginated. The optional `class` filter must be in validSourceClasses, else 400.
//
// It renders the metadata the aggregator's class filter consults, so `include_in_vwap=false` is
// intentional policy (aggregator/oracle/authority-sanity classes), not a missing connector.
func (s *Server) handleSources(w http.ResponseWriter, r *http.Request) { //nolint:gocognit,gocyclo // include parsing + per-source stat-fill + sparkline backfill are linear; splitting would scatter the request lifecycle
	classFilter := r.URL.Query().Get("class")
	if classFilter != "" && !validSourceClasses[classFilter] {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-class",
			"Invalid class", http.StatusBadRequest,
			"class must be one of: "+sourceClassList())
		return
	}

	// `include=stats` opts in to per-source 24h trade counts so callers that do not want them keep
	// the all-static fast path; `include=stats,sparkline` also joins the per-hour 24h volume series.
	includeStats, includeSparkline, includeSparkline7d := parseSourcesInclude(r.URL.Query().Get("include"))
	type stats struct {
		trades  int64
		volume  string
		markets int64
	}
	statsBySource := map[string]stats{}
	// Any soft-failed read below ships a partial listing the next request fills.
	partial := false
	historyBySource := map[string][]VolumeBucket{}
	lowerBound := map[string]bool{}
	history7dBySource := map[string][]VolumeBucket{}
	var vintage dataVintage
	var reads sourcesStatsAtReader
	if s.SourcesStats != nil {
		reads = sourcesStatsAt(s.SourcesStats)
	}
	if includeStats && reads != nil {
		// 8s ceiling on the stats fan-out, as in /v1/markets and /v1/pools. Stats soft-fail to the
		// bare registry, so a deadline degrades gracefully instead of hanging the listing cold.
		statsCtx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		got, at, err := reads.GetSourceStatsAt(statsCtx)
		cancel()
		if err != nil {
			s.logger.Warn("source stats", "err", err)
			partial = true
			// Soft-fail: serve the registry without stats.
		} else {
			vintage.note(at)
			for _, ss := range got {
				vol := ""
				if ss.VolumeUSD24h.Valid {
					vol = ss.VolumeUSD24h.String
				}
				if ss.UnpricedTrades24h > 0 {
					lowerBound[ss.Source] = true
				}
				statsBySource[ss.Source] = stats{
					trades:  ss.TradeCount24h,
					volume:  vol,
					markets: ss.MarketsCount24h,
				}
			}
		}
	}
	if includeSparkline && reads != nil {
		var failed bool
		historyBySource, failed = s.sourceVolumeHistory(r.Context(), reads.GetSourceVolumeHistory24hAt, 24,
			"source volume history", &vintage, lowerBound)
		partial = partial || failed
	}
	if includeSparkline7d && reads != nil {
		var failed bool
		history7dBySource, failed = s.sourceVolumeHistory(r.Context(), reads.GetSourceVolumeHistory7dAt, 24*7,
			"source volume history 7d", &vintage, lowerBound)
		partial = partial || failed
	}

	out := make([]Source, 0, len(external.Registry))
	for name, md := range external.Registry {
		if classFilter != "" && string(md.Class) != classFilter {
			continue
		}
		// The registry is compiled in for every network; a test net must
		// not list pubnet-only venues it never ingests.
		if ok, _ := sourcenet.Applicable(name, s.Network); !ok {
			continue
		}
		st := statsBySource[name]
		out = append(out, Source{
			Name:              name,
			Class:             string(md.Class),
			Subclass:          string(md.Subclass),
			IncludeInVWAP:     md.IncludeInVWAP,
			Paid:              md.Paid,
			BackfillAvailable: md.BackfillAvailable,
			BackfillSafe:      md.BackfillSafe(),
			DefaultWeight:     md.DefaultWeight,
			OnChain:           external.IsOnChain(name),
			Selectable:        sourceSelectable(name),
			TradeCount24h:     st.trades,
			VolumeUSD24h:      st.volume,
			VolumeLowerBound:  lowerBound[name],
			MarketsCount24h:   st.markets,
			VolumeHistory24h:  historyBySource[name],
			VolumeHistory7d:   history7dBySource[name],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeEnvelope(w, Envelope{Data: out, AsOf: vintage.asOf(), Flags: Flags{Degraded: partial}})
}

// sourceVolumeHistory loads one sparkline series under the same 8s ceiling
// as the stats fan-out. A failed read soft-fails: empty series, failed=true.
func (s *Server) sourceVolumeHistory(
	ctx context.Context,
	read func(context.Context) ([]timescale.SourceVolumeBucket, time.Time, error),
	hours int,
	what string,
	vintage *dataVintage,
	lowerBound map[string]bool,
) (map[string][]VolumeBucket, bool) {
	sparkCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	buckets, at, err := read(sparkCtx)
	cancel()
	if err != nil {
		s.logger.Warn(what, "err", err)
		return map[string][]VolumeBucket{}, true
	}
	vintage.note(at)
	markXLMUnpriced(lowerBound, buckets)
	return buildSourceVolumeHistory(buckets, hours), false
}
