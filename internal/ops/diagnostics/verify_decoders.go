package diagnostics

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	sdkxdr "github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/ledgerstream"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/sources/aquarius"
	"github.com/Stellar-Index/StellarIndex/internal/sources/band"
	"github.com/Stellar-Index/StellarIndex/internal/sources/comet"
	"github.com/Stellar-Index/StellarIndex/internal/sources/phoenix"
	"github.com/Stellar-Index/StellarIndex/internal/sources/redstone"
	"github.com/Stellar-Index/StellarIndex/internal/sources/reflector"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sdex"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
	sushiswap_v3 "github.com/Stellar-Index/StellarIndex/internal/sources/sushiswap_v3"
	"github.com/Stellar-Index/StellarIndex/internal/sources/upshift"
	"github.com/Stellar-Index/StellarIndex/internal/stellarrpc"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// protocolContractsGatedSources are the curated-set decoders
// (buildVerifyDispatcher) that also have an operator-admitted seam
// in Postgres' protocol_contracts (see pipeline.GatedRegistryOptions).
// verify-decoders defaults to the in-code curated set only — the tool
// is documented as a dry harness with no Timescale dependency — so a
// pool admitted only via protocol_contracts (not yet in the curated
// set) looks falsely silent unless -seed-protocol-contracts unions it
// in, mirroring production's gate (CA2-A28).
var protocolContractsGatedSources = []string{
	aquarius.SourceName,
	phoenix.SourceName,
	comet.SourceName,
	sushiswap_v3.SourceName,
	upshift.SourceName,
}

// loadProtocolContractSeeds unions protocol_contracts into the
// curated-set decoders' gate over a short-lived connection, opened
// and closed within this call — verify-decoders otherwise never
// touches Postgres. Returns an error (fail closed) rather than a
// silently empty seed, since a caller who asked for this coverage and
// got none would draw the wrong conclusion from a clean run.
func loadProtocolContractSeeds(ctx context.Context, dsn string) (map[string][]string, error) {
	if dsn == "" {
		return nil, fmt.Errorf("-seed-protocol-contracts requires a postgres DSN (storage.postgres_dsn in -config)")
	}
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("protocol-contracts seed: connect: %w", err)
	}
	defer func() { _ = store.Close() }()

	out := make(map[string][]string, len(protocolContractsGatedSources))
	for _, source := range protocolContractsGatedSources {
		ids, err := store.LoadProtocolContracts(ctx, source)
		if err != nil {
			return nil, fmt.Errorf("protocol-contracts seed: %s: %w", source, err)
		}
		out[source] = ids
	}
	return out, nil
}

// verifyDecoders streams a bounded ledger range from the configured
// Galexie datastore through a Dispatcher wired with EVERY registered
// decoder (regardless of cfg.Ingestion.EnabledSources), then prints
// a per-source table of:
//
//	source | matched events | outputs emitted | first sample line
//
// This is a dry harness — no Timescale, no Redis, no cursor writes.
// Useful for:
//
//   - Proving each decoder fires at least once over a recent window,
//     which is the cheapest way to confirm live pubnet traffic
//     matches the topic bytes + schema we compiled against.
//   - Smoke-testing a decoder change: pick a historical range known
//     to contain the source's events, verify outputs didn't regress.
//
// Oracle-variant decoders need their contract addresses in
// cfg.Oracle; any variant with an empty address is skipped with a
// warning rather than failing the whole run.
func verifyDecoders(args []string) error { //nolint:funlen,gocognit,gocyclo // linear diagnostic, splitting reduces readability
	fs := flag.NewFlagSet("verify-decoders", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	from := fs.Uint("from", 0, "First ledger sequence (inclusive, required)")
	to := fs.Uint("to", 0, "Last ledger sequence (inclusive, required)")
	failOnSilent := fs.Bool("fail-on-silent", true,
		"exit non-zero when any registered decoder emitted zero outputs over the range — the finding this command exists to produce. Pass -fail-on-silent=false for a range not chosen to contain every source's events")
	bucket := fs.String("bucket", "", "galexie bucket override. Default: the range vs ingestion.live_seam_ledger picks archive-or-live; with no seam configured it stays cfg.Storage.S3BucketLive, which does NOT hold historic ranges — pass the archive bucket for those (see opsutil.ResolveStreamBucket)")
	seedProtocolContracts := fs.Bool("seed-protocol-contracts", false,
		"union protocol_contracts into the curated-set decoders' gate (aquarius/phoenix/comet/sushiswap_v3/upshift), mirroring production's GatedRegistryOptions warm. Off by default: verify-decoders is a dry harness with no Timescale dependency; a pool admitted only via protocol_contracts looks falsely silent without this, and a genuinely silent decoder looks the same as one whose pool simply isn't in the curated seed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" || *from == 0 || *to == 0 || *to < *from {
		return fmt.Errorf("-config, -from, and -to are required; -to must be >= -from")
	}

	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return err
	}

	var protocolContractSeeds map[string][]string
	if *seedProtocolContracts {
		protocolContractSeeds, err = loadProtocolContractSeeds(context.Background(), cfg.Storage.PostgresDSN)
		if err != nil {
			return err
		}
	}

	// Build a dispatcher with every decoder we ship, not just the
	// subset in cfg.Ingestion.EnabledSources. The whole point of
	// verify is to confirm each one fires on the range.
	disp, soroswapDec, registered := buildVerifyDispatcher(cfg.Oracle, protocolContractSeeds)
	if len(registered) == 0 {
		return fmt.Errorf("no decoders registered — check oracle contract addresses in config")
	}

	// Optional Soroswap factory seed. Without it, pairs created
	// before the -from ledger are invisible to the decoder (see
	// docs/discovery/dexes-amms/soroswap.md on the swap event's
	// missing token identities).
	if cfg.Oracle.Soroswap.FactoryContract != "" {
		seedEndpoint := cfg.Oracle.Soroswap.SeedRPCEndpoint
		if seedEndpoint == "" && len(cfg.Stellar.RPCEndpoints) > 0 {
			seedEndpoint = cfg.Stellar.RPCEndpoints[0]
		}
		if seedEndpoint == "" {
			return fmt.Errorf("soroswap.factory_contract is set but no RPC endpoint — " +
				"set oracle.soroswap.seed_rpc_endpoint or stellar.rpc_endpoints")
		}
		fmt.Fprintf(os.Stderr, "verify-decoders: seeding soroswap pairs from %s...\n", seedEndpoint)
		seedCtx, seedCancel := context.WithTimeout(context.Background(), 15*time.Minute)
		rpc := stellarrpc.New(seedEndpoint, stellarrpc.WithTimeout(60*time.Second))
		n, err := soroswapDec.SeedFromFactoryRPC(seedCtx, rpc, cfg.Oracle.Soroswap.FactoryContract)
		seedCancel()
		if err != nil {
			return fmt.Errorf("soroswap seed: %w", err)
		}
		fmt.Fprintf(os.Stderr, "verify-decoders: seeded %d soroswap pairs\n", n)
	}

	fmt.Fprintf(os.Stderr, "verify-decoders: registered %d decoders: %s\n",
		len(registered), strings.Join(registered, ", "))
	// Seam-aware bucket choice with a -bucket escape hatch, shared with
	// ch-backfill / census-backfill / sdex-claim-audit rather than
	// re-derived. There was no flag at all here and the bucket was
	// hardcoded to cfg.Storage.S3BucketLive, which is TRIMMED: pointing
	// verify-decoders at a historic range read a prefix of it or none of
	// it, and the table below then reported every decoder as silent —
	// the exact conclusion an operator uses this command to reach
	// (RLT-282).
	streamBucket, err := opsutil.ResolveStreamBucket(cfg, *bucket, uint32(*from), uint32(*to))
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "verify-decoders: streaming ledgers %d..%d from %s bucket %q\n",
		*from, *to, cfg.Storage.S3Endpoint, streamBucket)

	lsCfg := opsutil.NewBoundedLedgerStreamConfig(cfg, streamBucket, 1)

	type perSourceStat struct {
		outputs int
		first   string // one-line summary of the first output
	}
	stats := make(map[string]*perSourceStat)
	var totalLedgers, totalOutputs int

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// Signal channel: total events processed (not emitted outputs —
	// the dispatcher's internal unmatched hit counter tracks "events
	// the decoders saw but ignored"; here we're interested in what
	// each decoder OUTPUTTED, which is the verify claim).
	err = ledgerstream.Stream(ctx, lsCfg, uint32(*from), uint32(*to),
		func(lcm sdkxdr.LedgerCloseMeta) error {
			totalLedgers++
			outputs, perr := disp.ProcessLedger(lcm, cfg.Stellar.Passphrase())
			if perr != nil {
				fmt.Fprintf(os.Stderr, "verify-decoders: ledger %d: %v\n",
					lcm.LedgerSequence(), perr)
				return nil
			}
			for _, ev := range outputs {
				src := ev.Source()
				s, ok := stats[src]
				if !ok {
					s = &perSourceStat{}
					stats[src] = s
				}
				s.outputs++
				if s.first == "" {
					s.first = summariseEvent(ev, lcm.LedgerSequence())
				}
				totalOutputs++
			}
			return nil
		},
	)
	if err != nil {
		return fmt.Errorf("stream: %w", err)
	}

	fmt.Fprintf(os.Stderr, "verify-decoders: processed %d ledgers, %d total outputs\n\n",
		totalLedgers, totalOutputs)

	// Print the per-source table. Include registered-but-silent
	// decoders so operators can see "X was wired but fired zero
	// times" rather than "X was missing from the report."
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "SOURCE\tOUTPUTS\tFIRST SAMPLE")
	names := make([]string, 0, len(registered))
	names = append(names, registered...)
	sort.Strings(names)
	silent := 0
	for _, name := range names {
		s := stats[name]
		if s == nil {
			_, _ = fmt.Fprintf(w, "%s\t0\t(none)\n", name)
			silent++
			continue
		}
		_, _ = fmt.Fprintf(w, "%s\t%d\t%s\n", name, s.outputs, s.first)
	}
	if err := w.Flush(); err != nil {
		return err
	}

	if silent > 0 {
		fmt.Fprintf(os.Stderr, "\nverify-decoders: %d/%d decoders emitted zero outputs — "+
			"either the range genuinely lacks their events, or their topic/schema "+
			"no longer matches.\n", silent, len(registered))
	}

	// Dispatcher-internal stats surface here. They distinguish
	// "matched but Decode errored" (decodeErrors) from "no decoder
	// claimed the event" (unmatchedHits) — essential for localising
	// a silent-source finding to either the match or decode side.
	dispStats := disp.Stats()
	if len(dispStats.DecodeErrors) > 0 || dispStats.UnmatchedHits > 0 {
		fmt.Fprintf(os.Stderr, "\ndispatcher stats — unmatched events: %d\n", dispStats.UnmatchedHits)
		if len(dispStats.DecodeErrors) > 0 {
			fmt.Fprintln(os.Stderr, "decoder errors by source:")
			errNames := make([]string, 0, len(dispStats.DecodeErrors))
			for k := range dispStats.DecodeErrors {
				errNames = append(errNames, k)
			}
			sort.Strings(errNames)
			for _, name := range errNames {
				fmt.Fprintf(os.Stderr, "  %s: %d\n", name, dispStats.DecodeErrors[name])
			}
		}
	}

	// Coverage last, so the operator keeps the full table, but non-zero
	// so a short walk is never read as the verdict (RLT-282). Every
	// number above — and above all the "emitted zero outputs" line, the
	// single claim this command exists to make — describes the ledgers
	// that were actually delivered. A walk that covered a fraction of
	// the range reports the same silent decoders as a decoder that is
	// genuinely broken.
	if err := verifyWalkCoverage(uint32(*from), uint32(*to), totalLedgers, streamBucket); err != nil {
		return err
	}
	return silentVerdict("verify-decoders", "registered decoders", silent, len(registered), *failOnSilent)
}

// silentVerdict is verify-decoders' and verify-external's exit status
// once the table is printed. Every member silent is always a failure —
// nothing was verified; any member silent fails too under
// -fail-on-silent. The stderr line above is for a human; this is the
// only verdict a script or deploy gate can read.
func silentVerdict(cmd, what string, silent, total int, failOnSilent bool) error {
	if err := opsutil.AssertNonVacuous(total-silent, total, what); err != nil {
		return fmt.Errorf("%s: %w", cmd, err)
	}
	if failOnSilent && silent > 0 {
		return fmt.Errorf("%s: %d of %d %s emitted zero outputs (-fail-on-silent)", cmd, silent, total, what)
	}
	return nil
}

// verifyWalkCoverage turns a verify-decoders walk that did not deliver
// its whole range into a hard error, naming the bucket it read.
//
// Same defect class as chops.walkCoverage / chops.backfillCoverage /
// ingest.censusCoverage, reached here by the same two roads: the trimmed
// live bucket cannot hold a historic range, and
// opsutil.NewBoundedLedgerStreamConfig always sets
// TolerateTrailingMissing, which converts the SDK's missing-object error
// into a clean walk-complete for any hole within 65,536 ledgers of -to.
// Either way ledgerstream.Stream returns nil and the per-source table is
// simply computed over fewer ledgers.
//
// It fails closed rather than warning because this command's output is
// consumed as an assertion — "decoder X fired / did not fire over
// [from,to]" — and that assertion is worthless over an unknown subset.
// Re-running is free: the command writes nothing.
func verifyWalkCoverage(from, to uint32, delivered int, bucket string) error {
	requested := uint64(to) - uint64(from) + 1
	if uint64(delivered) == requested {
		return nil
	}
	if delivered == 0 {
		return fmt.Errorf(
			"verify-decoders processed 0 ledgers in [%d, %d] from bucket %q — nothing was examined, "+
				"so every decoder is reported silent; historical ranges need -bucket galexie-archive. "+
				"Refusing to report on a range that was never opened",
			from, to, bucket)
	}
	return fmt.Errorf(
		"verify-decoders processed %d of %d requested ledgers in [%d, %d] from bucket %q — the per-source "+
			"table above covers only that subset, so a decoder reported silent may simply be absent from "+
			"the part that was read; historical ranges need -bucket galexie-archive",
		delivered, requested, from, to, bucket)
}

// buildVerifyDispatcher wires every decoder we ship, returning the
// dispatcher, the Soroswap decoder (so callers can seed it from the
// factory RPC), and the list of source names that were actually
// registered (oracle variants with an unset contract address are
// skipped). protocolContractSeeds, when non-nil, unions each curated-
// set decoder's protocol_contracts warm into its gate (see
// loadProtocolContractSeeds); a nil/missing entry leaves the decoder
// on its in-code curated set only, unchanged from before.
func buildVerifyDispatcher(oracle config.OracleConfig, protocolContractSeeds map[string][]string) (*dispatcher.Dispatcher, *soroswap.Decoder, []string) {
	seedOpt := func(source string) contractid.Option {
		return contractid.WithSeed(protocolContractSeeds[source])
	}
	soroswapDec := soroswap.NewDecoder()
	decoders := []dispatcher.Decoder{
		soroswapDec,
		aquarius.NewDecoder(seedOpt(aquarius.SourceName)),
		phoenix.NewDecoder(seedOpt(phoenix.SourceName)),
		comet.NewDecoder(seedOpt(comet.SourceName)),
		sushiswap_v3.NewDecoder(seedOpt(sushiswap_v3.SourceName)),
		upshift.NewDecoder(seedOpt(upshift.SourceName)),
	}
	registered := []string{
		soroswap.SourceName,
		aquarius.SourceName,
		phoenix.SourceName,
		comet.SourceName,
		sushiswap_v3.SourceName,
		upshift.SourceName,
	}

	// Oracle variants: only register if their contract address is set.
	if oracle.Reflector.DEXContract != "" {
		decoders = append(decoders, reflector.NewDecoder(reflector.VariantDEX, oracle.Reflector.DEXContract))
		registered = append(registered, reflector.SourceDEX)
	} else {
		fmt.Fprintln(os.Stderr, "verify-decoders: skip reflector-dex — oracle.reflector.dex_contract empty")
	}
	if oracle.Reflector.CEXContract != "" {
		decoders = append(decoders, reflector.NewDecoder(reflector.VariantCEX, oracle.Reflector.CEXContract))
		registered = append(registered, reflector.SourceCEX)
	} else {
		fmt.Fprintln(os.Stderr, "verify-decoders: skip reflector-cex — oracle.reflector.cex_contract empty")
	}
	if oracle.Reflector.FXContract != "" {
		decoders = append(decoders, reflector.NewDecoder(reflector.VariantFX, oracle.Reflector.FXContract))
		registered = append(registered, reflector.SourceFX)
	} else {
		fmt.Fprintln(os.Stderr, "verify-decoders: skip reflector-fx — oracle.reflector.fx_contract empty")
	}

	var callDecoders []dispatcher.ContractCallDecoder
	if oracle.Redstone.AdapterContract != "" {
		decoders = append(decoders, redstone.NewDecoder(oracle.Redstone.AdapterContract))
		registered = append(registered, redstone.SourceName)
	} else {
		fmt.Fprintln(os.Stderr, "verify-decoders: skip redstone — oracle.redstone.adapter_contract empty")
	}
	if oracle.Band.StandardReferenceContract != "" {
		callDecoders = append(callDecoders, band.NewDecoder(oracle.Band.StandardReferenceContract))
		registered = append(registered, band.SourceName)
	} else {
		fmt.Fprintln(os.Stderr, "verify-decoders: skip band — oracle.band.standard_reference_contract empty")
	}

	disp := dispatcher.New(decoders...)
	disp.AddOpDecoder(sdex.NewDecoder())
	registered = append(registered, sdex.SourceName)
	for _, ccd := range callDecoders {
		disp.AddContractCallDecoder(ccd)
	}
	return disp, soroswapDec, registered
}

// summariseEvent renders one consumer.Event as a one-line human
// summary for the verify-decoders table. We don't need the full
// canonical.Trade / OracleUpdate — just enough to confirm the
// decoder produced structurally-valid output.
func summariseEvent(ev consumer.Event, ledger uint32) string {
	switch e := any(ev).(type) {
	case soroswap.TradeEvent:
		return fmt.Sprintf("trade ledger=%d pair=%s", ledger, e.Trade.Pair.String())
	case aquarius.TradeEvent:
		return fmt.Sprintf("trade ledger=%d pair=%s", ledger, e.Trade.Pair.String())
	case phoenix.TradeEvent:
		return fmt.Sprintf("trade ledger=%d pair=%s", ledger, e.Trade.Pair.String())
	case comet.TradeEvent:
		return fmt.Sprintf("trade ledger=%d pair=%s", ledger, e.Trade.Pair.String())
	case sdex.TradeEvent:
		return fmt.Sprintf("trade ledger=%d pair=%s", ledger, e.Trade.Pair.String())
	case reflector.UpdateEvent:
		return fmt.Sprintf("oracle ledger=%d asset=%s", ledger, e.Update.Asset.String())
	case redstone.UpdateEvent:
		return fmt.Sprintf("oracle ledger=%d asset=%s", ledger, e.Update.Asset.String())
	case band.UpdateEvent:
		return fmt.Sprintf("oracle ledger=%d asset=%s", ledger, e.Update.Asset.String())
	default:
		return fmt.Sprintf("event kind=%s ledger=%d", ev.EventKind(), ledger)
	}
}

// ─── verify-external ─────────────────────────────────────────────
