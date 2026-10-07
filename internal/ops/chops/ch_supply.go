package chops

import (
	"context"
	"errors"
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
// (ADR-0034 + docs/architecture/storage-considerations.md#supply-flows-in-the-lake.) The contract_id
// is the asset's SAC (classic) or token (SEP-41) contract — a unique per-token
// key.
//
// Defaults to a report (top-N contracts by supply + coverage count). Window
// [from,to] per partition for the full-history run; a single all-history pass
// holds one in-memory map (thousands of contracts — bounded).
//
// NB: the per-token supply is not persisted: the serving path sums
// `stellar.supply_flows` live
// (internal/storage/clickhouse/supply_flows.go SupplyReader.TokenSupply —
// "no rollup refresh" by design). -seed-flows below is the
// mechanism that keeps supply_flows itself complete; the shared -write gate
// applies it, and without -write a -seed-flows run counts the rows it would
// write.
func chSupply(args []string) error { //nolint:gocognit,gocyclo,funlen // linear: parse, stream+accumulate, optional seed, report; splitting hurts clarity.
	fs, gate := opsutil.NewMutatingFlagSet("ch-supply")
	cfgPath := fs.String("config", "", "path to stellarindex.toml (required)")
	from := fs.Uint("from", 0, "first ledger sequence (inclusive, required)")
	to := fs.Uint("to", 0, "last ledger sequence (inclusive, required)")
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	topN := fs.Int("top", 25, "print the top-N contracts by absolute supply")
	useFinal := fs.Bool("final", true, "FINAL-dedup reads (correct but ~40x slower over all history; -final=false for a fast all-token estimate)")
	seedFlows := fs.Bool("seed-flows", false, "seed stellar.supply_flows: one decoded row per mint/burn/clawback event (the decode-at-ingest history backfill; idempotent). Written only with -write")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := checkCHSupplyMode(*seedFlows, gate); err != nil {
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

	seeder, err := newSupplyFlowSeeder(ctx, *chAddr, *seedFlows, gate.Enabled())
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "ch-supply: summing mint/burn/clawback flows for [%d,%d] from %s (final=%v seed-flows=%v)\n", lo, hi, *chAddr, *useFinal, *seedFlows)
	err = clickhouse.StreamMintBurnFlows(ctx, *chAddr, lo, hi, *useFinal, func(f clickhouse.MintBurnFlow) error {
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
		if serr := seeder.add(ctx, clickhouse.SupplyFlowRow{
			ContractID: f.ContractID,
			LedgerSeq:  f.Ledger,
			CloseTime:  f.CloseTime,
			TxHash:     f.TxHash,
			OpIndex:    f.OpIndex,
			EventIndex: f.EventIndex,
			Kind:       f.Kind,
			Amount:     v,
		}); serr != nil {
			return serr
		}
		if time.Since(lastLog) >= 15*time.Second {
			rate := float64(flows) / time.Since(start).Seconds()
			fmt.Fprintf(os.Stderr, "ch-supply: %d flows, %d tokens, %d flow-rows written (%.0f flows/s)\n", flows, len(tokens), seeder.seeded, rate)
			lastLog = time.Now()
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("ch-supply: stream: %w", err)
	}
	if err := seeder.finish(ctx); err != nil {
		return err
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

// checkCHSupplyMode ties the shared gate to -seed-flows, the only write. A
// seeding caller must state its mode: run-ch-supply.sh treats exit 0 as
// "window seeded", so a silent preview would stall supply_flows. -write
// without -seed-flows has nothing to apply and is refused, not ignored.
func checkCHSupplyMode(seedFlows bool, gate *opsutil.WriteGate) error {
	if !seedFlows {
		if gate.Enabled() {
			return errors.New("ch-supply: -write applies -seed-flows; without -seed-flows this command is a read-only report")
		}
		return nil
	}
	if err := gate.RequireStatedMode(); err != nil {
		return fmt.Errorf("ch-supply -seed-flows: %w", err)
	}
	return nil
}

// supplyFlowSeeder batches -seed-flows rows into stellar.supply_flows (the
// stream is ~570M rows, so they cannot be held like the report map). Without
// write it only counts, so a -dry-run seed reports what it would write.
type supplyFlowSeeder struct {
	chAddr    string
	on, write bool
	batch     []clickhouse.SupplyFlowRow
	seeded    int
}

const supplyFlowBatchN = 20000

func newSupplyFlowSeeder(ctx context.Context, chAddr string, on, write bool) (*supplyFlowSeeder, error) {
	s := &supplyFlowSeeder{chAddr: chAddr, on: on, write: write}
	if !on {
		return s, nil
	}
	opsutil.PrintWriteBanner(write)
	if write {
		if err := clickhouse.EnsureSupplyFlowsTable(ctx, chAddr); err != nil {
			return nil, fmt.Errorf("ch-supply: ensure supply_flows: %w", err)
		}
	}
	s.batch = make([]clickhouse.SupplyFlowRow, 0, supplyFlowBatchN)
	return s, nil
}

func (s *supplyFlowSeeder) add(ctx context.Context, row clickhouse.SupplyFlowRow) error {
	if !s.on {
		return nil
	}
	s.batch = append(s.batch, row)
	if len(s.batch) < supplyFlowBatchN {
		return nil
	}
	return s.flush(ctx)
}

func (s *supplyFlowSeeder) flush(ctx context.Context) error {
	if len(s.batch) == 0 {
		return nil
	}
	if s.write {
		if err := clickhouse.WriteSupplyFlows(ctx, s.chAddr, s.batch); err != nil {
			return err
		}
	}
	s.seeded += len(s.batch)
	s.batch = s.batch[:0]
	return nil
}

func (s *supplyFlowSeeder) finish(ctx context.Context) error {
	if !s.on {
		return nil
	}
	if err := s.flush(ctx); err != nil {
		return fmt.Errorf("ch-supply: seed-flows write: %w", err)
	}
	if s.write {
		fmt.Fprintf(os.Stderr, "ch-supply: seeded %d supply_flows rows\n", s.seeded)
	} else {
		fmt.Fprintf(os.Stderr, "ch-supply: dry run: would seed %d supply_flows rows; pass -write to apply\n", s.seeded)
	}
	return nil
}
