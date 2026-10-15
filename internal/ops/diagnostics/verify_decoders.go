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
	"github.com/Stellar-Index/StellarIndex/internal/sources/chainlink_stellar"
	"github.com/Stellar-Index/StellarIndex/internal/sources/comet"
	"github.com/Stellar-Index/StellarIndex/internal/sources/phoenix"
	"github.com/Stellar-Index/StellarIndex/internal/sources/redstone"
	"github.com/Stellar-Index/StellarIndex/internal/sources/reflector"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sdex"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
	"github.com/Stellar-Index/StellarIndex/internal/sources/spectra"
	sushiswap_v3 "github.com/Stellar-Index/StellarIndex/internal/sources/sushiswap_v3"
	"github.com/Stellar-Index/StellarIndex/internal/sources/upshift"
	"github.com/Stellar-Index/StellarIndex/internal/stellarrpc"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// protocolContractsGatedSources are curated-set decoders (buildVerifyDispatcher) that also admit contracts via protocol_contracts. verify-decoders defaults to the in-code set (no Timescale), so such a pool looks falsely silent unless -seed-protocol-contracts unions it in, as production's gate does.
var protocolContractsGatedSources = []string{
	aquarius.SourceName,
	phoenix.SourceName,
	comet.SourceName,
	sushiswap_v3.SourceName,
	upshift.SourceName,
	spectra.SourceName,
}

// loadProtocolContractSeeds unions protocol_contracts into the curated-set gates over a short-lived connection. It fails closed rather than return an empty seed: a caller who asked for this coverage would misread a clean run.
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

// verifyDecoders streams a bounded range from the Galexie datastore through a Dispatcher with EVERY registered decoder and prints per source: matched events, outputs emitted, first sample line.
// A dry harness (no Timescale, Redis or cursor writes). Oracle variants with an empty cfg.Oracle address are skipped with a warning.
func verifyDecoders(args []string) error { //nolint:funlen,gocognit,gocyclo // linear diagnostic, splitting reduces readability
	fs := flag.NewFlagSet("verify-decoders", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	from := fs.Uint("from", 0, "First ledger sequence (inclusive, required)")
	to := fs.Uint("to", 0, "Last ledger sequence (inclusive, required)")
	failOnSilent := fs.Bool("fail-on-silent", true,
		"exit non-zero when any registered decoder emitted zero outputs over the range — the finding this command exists to produce. Pass -fail-on-silent=false for a range not chosen to contain every source's events")
	bucket := fs.String("bucket", "", "galexie bucket override. Default: the range vs ingestion.live_seam_ledger picks archive-or-live; with no seam configured it stays cfg.Storage.S3BucketLive, which does NOT hold historic ranges — pass the archive bucket for those (see opsutil.ResolveStreamBucket)")
	seedProtocolContracts := fs.Bool("seed-protocol-contracts", false,
		"union protocol_contracts into the curated-set decoders' gate (aquarius/phoenix/comet/sushiswap_v3/upshift/spectra), mirroring production's GatedRegistryOptions warm. Off by default: verify-decoders is a dry harness with no Timescale dependency; a pool admitted only via protocol_contracts looks falsely silent without this, and a genuinely silent decoder looks the same as one whose pool simply isn't in the curated seed")
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
	// before the -from ledger are invisible to the decoder (see Q6
	// in internal/sources/soroswap/README.md on swaps whose pair has
	// no token mapping).
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
	// Seam-aware bucket choice with a -bucket escape hatch, shared with ch-backfill / census-backfill. The live bucket is TRIMMED, so a historic range would read a prefix or nothing and report every decoder as silent.
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

	// Coverage runs last so the operator keeps the table, but exits non-zero: a short walk reports the same silent decoders as a broken decoder, so the table is not a verdict on its own.
	if err := verifyWalkCoverage(uint32(*from), uint32(*to), totalLedgers, streamBucket); err != nil {
		return err
	}
	return silentVerdict("verify-decoders", "registered decoders", silent, len(registered), *failOnSilent)
}

// silentVerdict is the exit status for verify-decoders and verify-external: all members silent always fails; any silent fails under -fail-on-silent. It is the only verdict a script or gate can read.
func silentVerdict(cmd, what string, silent, total int, failOnSilent bool) error {
	if err := opsutil.AssertNonVacuous(total-silent, total, what); err != nil {
		return fmt.Errorf("%s: %w", cmd, err)
	}
	if failOnSilent && silent > 0 {
		return fmt.Errorf("%s: %d of %d %s emitted zero outputs (-fail-on-silent)", cmd, silent, total, what)
	}
	return nil
}

// verifyWalkCoverage turns a walk that did not deliver its whole range into a hard error naming the bucket (same defect class as chops.walkCoverage and ingest.censusCoverage).
// The trimmed live bucket cannot hold a historic range, and TolerateTrailingMissing turns a missing object within 65,536 ledgers of -to into a clean finish. It fails closed because the output is consumed as an assertion, worthless over an unknown subset; re-running is free.
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

// buildVerifyDispatcher wires every shipped decoder and returns the dispatcher, the Soroswap decoder (for factory-RPC seeding) and the registered source names (oracle variants with no address are skipped).
// Non-nil protocolContractSeeds union into each curated decoder's gate (see loadProtocolContractSeeds); a missing entry leaves it on the in-code set.
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
		spectra.NewDecoder(seedOpt(spectra.SourceName)),
	}
	registered := []string{
		soroswap.SourceName,
		aquarius.SourceName,
		phoenix.SourceName,
		comet.SourceName,
		sushiswap_v3.SourceName,
		upshift.SourceName,
		spectra.SourceName,
	}

	// Oracle variants: only register if their contract address is set.
	if oracle.Reflector.DEXContract != "" {
		decoders = append(decoders, reflector.NewDecoder(reflector.VariantDEX, oracle.Reflector.DEXContract,
			reflector.WithDecoderDecimals(oracle.Reflector.DEXDecimals)))
		registered = append(registered, reflector.SourceDEX)
	} else {
		fmt.Fprintln(os.Stderr, "verify-decoders: skip reflector-dex — oracle.reflector.dex_contract empty")
	}
	if oracle.Reflector.CEXContract != "" {
		decoders = append(decoders, reflector.NewDecoder(reflector.VariantCEX, oracle.Reflector.CEXContract,
			reflector.WithDecoderDecimals(oracle.Reflector.CEXDecimals)))
		registered = append(registered, reflector.SourceCEX)
	} else {
		fmt.Fprintln(os.Stderr, "verify-decoders: skip reflector-cex — oracle.reflector.cex_contract empty")
	}
	if oracle.Reflector.FXContract != "" {
		decoders = append(decoders, reflector.NewDecoder(reflector.VariantFX, oracle.Reflector.FXContract,
			reflector.WithDecoderDecimals(oracle.Reflector.FXDecimals)))
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
	if oracle.ChainlinkStellar.CacheContract != "" {
		decoders = append(decoders, chainlink_stellar.NewDecoder(oracle.ChainlinkStellar.CacheContract))
		registered = append(registered, chainlink_stellar.SourceName)
	} else {
		fmt.Fprintln(os.Stderr, "verify-decoders: skip chainlink-stellar — oracle.chainlink_stellar.cache_contract empty")
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

// summariseEvent renders one consumer.Event as a one-line summary for the verify-decoders table: enough to confirm structurally-valid
// decoder output, not the full canonical.Trade / OracleUpdate.
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
	case chainlink_stellar.UpdateEvent:
		return fmt.Sprintf("oracle ledger=%d asset=%s", ledger, e.Update.Asset.String())
	case band.UpdateEvent:
		return fmt.Sprintf("oracle ledger=%d asset=%s", ledger, e.Update.Asset.String())
	default:
		return fmt.Sprintf("event kind=%s ledger=%d", ev.EventKind(), ledger)
	}
}

// ─── verify-external ─────────────────────────────────────────────
