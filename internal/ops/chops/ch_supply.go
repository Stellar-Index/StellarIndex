package chops

import (
	"flag"
	"fmt"
	"math/big"
	"os"
	"sort"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// chSupply derives total supply for EVERY token from the ClickHouse lake by
// summing CAP-67 classic + SEP-41 mint/burn/clawback flows per contract:
//
//	supply(contract) = Σ mint − Σ burn − Σ clawback   (baseline 0 at genesis)
//
// (ADR-0034 + docs/architecture/clickhouse-supply-from-ch.md.) The contract_id
// is the asset's SAC (classic) or token (SEP-41) contract — a unique per-token
// key.
//
// Defaults to a report (top-N contracts by supply + coverage count). Window
// [from,to] per partition for the full-history run; a single all-history pass
// holds one in-memory map (thousands of contracts — bounded).
//
// NB: this used to have a -write flag that persisted the per-token rollup to
// a materialized `stellar.token_supply` CH table. That table + writer were
// removed (ROADMAP #66, 2026-07-10): nothing has read `stellar.token_supply`
// since the serving path moved to summing `stellar.supply_flows` live
// (internal/storage/clickhouse/supply_flows.go SupplyReader.TokenSupply —
// "no rollup refresh" by design). -seed-flows below is the still-live
// mechanism that keeps supply_flows itself complete.
func chSupply(args []string) error { //nolint:gocognit,gocyclo,funlen // linear: parse, stream+accumulate, optional seed, report; splitting hurts clarity.
	fs := flag.NewFlagSet("ch-supply", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to stellarindex.toml (required)")
	from := fs.Uint("from", 0, "first ledger sequence (inclusive, required)")
	to := fs.Uint("to", 0, "last ledger sequence (inclusive, required)")
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	topN := fs.Int("top", 25, "print the top-N contracts by absolute supply")
	useFinal := fs.Bool("final", true, "FINAL-dedup reads (correct but ~40x slower over all history; -final=false for a fast all-token estimate)")
	seedFlows := fs.Bool("seed-flows", false, "seed stellar.supply_flows: write one decoded row per mint/burn/clawback event (the decode-at-ingest history backfill; idempotent)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" || *from == 0 || *to == 0 || *to < *from {
		return fmt.Errorf("-config, -from, -to are required; -to must be >= -from")
	}
	if _, err := config.LoadWithEnv(*cfgPath); err != nil {
		return err
	}

	ctx, cancel := opsutil.SignalContext()
	defer cancel()
	lo, hi := uint32(*from), uint32(*to)

	type acc struct {
		mint, burn, clawback *big.Int
		flows                uint64
		lastLedger           uint32
	}
	tokens := make(map[string]*acc)
	skipByType := make(map[string]int)
	var (
		flows        uint64
		decodeErrors uint64
		start        = time.Now()
		lastLog      = time.Now()
	)

	// seed-flows: write one decoded supply_flows row per event, batched during
	// the stream (570M rows can't be held in memory like the rollup map).
	const flowBatchN = 20000
	var (
		flowBatch  []clickhouse.SupplyFlowRow
		flowsWrote int
	)
	if *seedFlows {
		if eerr := clickhouse.EnsureSupplyFlowsTable(ctx, *chAddr); eerr != nil {
			return fmt.Errorf("ch-supply: ensure supply_flows: %w", eerr)
		}
		flowBatch = make([]clickhouse.SupplyFlowRow, 0, flowBatchN)
	}
	flushFlows := func() error {
		if len(flowBatch) == 0 {
			return nil
		}
		if werr := clickhouse.WriteSupplyFlows(ctx, *chAddr, flowBatch); werr != nil {
			return werr
		}
		flowsWrote += len(flowBatch)
		flowBatch = flowBatch[:0]
		return nil
	}

	fmt.Fprintf(os.Stderr, "ch-supply: summing mint/burn/clawback flows for [%d,%d] from %s (final=%v seed-flows=%v)\n", lo, hi, *chAddr, *useFinal, *seedFlows)
	err := clickhouse.StreamMintBurnFlows(ctx, *chAddr, lo, hi, *useFinal, func(f clickhouse.MintBurnFlow) error {
		flows++
		v, skipType, ok := clickhouse.DecodeSupplyAmountXDR(f.DataXDR)
		if !ok {
			// Undecodable / non-i128 body (some SEP-41 variants carry a map);
			// skip rather than misparse. skipType pinpoints the shape to handle.
			decodeErrors++
			skipByType[skipType]++
			return nil
		}
		a := tokens[f.ContractID]
		if a == nil {
			a = &acc{mint: big.NewInt(0), burn: big.NewInt(0), clawback: big.NewInt(0)}
			tokens[f.ContractID] = a
		}
		switch f.Kind {
		case "mint":
			a.mint.Add(a.mint, v)
		case "burn":
			a.burn.Add(a.burn, v)
		case "clawback":
			a.clawback.Add(a.clawback, v)
		}
		a.flows++
		if f.Ledger > a.lastLedger {
			a.lastLedger = f.Ledger
		}
		if *seedFlows {
			flowBatch = append(flowBatch, clickhouse.SupplyFlowRow{
				ContractID: f.ContractID,
				LedgerSeq:  f.Ledger,
				CloseTime:  f.CloseTime,
				TxHash:     f.TxHash,
				OpIndex:    f.OpIndex,
				EventIndex: f.EventIndex,
				Kind:       f.Kind,
				Amount:     v,
			})
			if len(flowBatch) >= flowBatchN {
				if ferr := flushFlows(); ferr != nil {
					return ferr
				}
			}
		}
		if time.Since(lastLog) >= 15*time.Second {
			rate := float64(flows) / time.Since(start).Seconds()
			fmt.Fprintf(os.Stderr, "ch-supply: %d flows, %d tokens, %d flow-rows written (%.0f flows/s)\n", flows, len(tokens), flowsWrote, rate)
			lastLog = time.Now()
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("ch-supply: stream: %w", err)
	}
	if *seedFlows {
		if ferr := flushFlows(); ferr != nil {
			return fmt.Errorf("ch-supply: seed-flows write: %w", ferr)
		}
		fmt.Fprintf(os.Stderr, "ch-supply: seeded %d supply_flows rows\n", flowsWrote)
	}

	net := func(a *acc) *big.Int {
		n := new(big.Int).Set(a.mint)
		n.Sub(n, a.burn)
		n.Sub(n, a.clawback)
		return n
	}

	// ─── report ──────────────────────────────────────────────────────────
	type rrow struct {
		contract string
		supply   *big.Int
		flows    uint64
	}
	rows := make([]rrow, 0, len(tokens))
	for c, a := range tokens {
		rows = append(rows, rrow{c, net(a), a.flows})
	}
	sort.Slice(rows, func(i, j int) bool {
		return new(big.Int).Abs(rows[i].supply).Cmp(new(big.Int).Abs(rows[j].supply)) > 0
	})

	fmt.Printf("\n=== ch-supply [%d,%d] ===\n", lo, hi)
	fmt.Printf("flows: %d  decode-skipped: %d  tokens (distinct contracts): %d\n",
		flows, decodeErrors, len(tokens))
	fmt.Printf("skip-by-type: %v\n\n", skipByType)
	fmt.Printf("%-58s %30s %12s\n", "contract", "supply (raw)", "flows")
	for i, r := range rows {
		if i >= *topN {
			fmt.Printf("… %d more tokens\n", len(rows)-*topN)
			break
		}
		fmt.Printf("%-58s %30s %12d\n", r.contract, r.supply.String(), r.flows)
	}
	return nil
}
