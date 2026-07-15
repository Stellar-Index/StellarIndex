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
	"github.com/Stellar-Index/StellarIndex/internal/stellarrpc"
)

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

	// Build a dispatcher with every decoder we ship, not just the
	// subset in cfg.Ingestion.EnabledSources. The whole point of
	// verify is to confirm each one fires on the range.
	disp, soroswapDec, registered := buildVerifyDispatcher(cfg.Oracle)
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
	fmt.Fprintf(os.Stderr, "verify-decoders: streaming ledgers %d..%d from %s\n",
		*from, *to, cfg.Storage.S3Endpoint)

	lsCfg := opsutil.NewBoundedLedgerStreamConfig(cfg, cfg.Storage.S3BucketLive, 1)

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
	return nil
}

// buildVerifyDispatcher wires every decoder we ship, returning the
// dispatcher, the Soroswap decoder (so callers can seed it from the
// factory RPC), and the list of source names that were actually
// registered (oracle variants with an unset contract address are
// skipped).
func buildVerifyDispatcher(oracle config.OracleConfig) (*dispatcher.Dispatcher, *soroswap.Decoder, []string) {
	soroswapDec := soroswap.NewDecoder()
	decoders := []dispatcher.Decoder{
		soroswapDec,
		aquarius.NewDecoder(),
		phoenix.NewDecoder(),
		comet.NewDecoder(),
	}
	registered := []string{
		soroswap.SourceName,
		aquarius.SourceName,
		phoenix.SourceName,
		comet.SourceName,
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
