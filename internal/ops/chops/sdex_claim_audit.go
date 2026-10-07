package chops

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/stellar/go-stellar-sdk/ingest"
	sdkxdr "github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ledgerstream"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sdex"
)

// sdexClaimAudit walks a ledger range and runs every classic-DEX claim atom
// through the real SDEX decode path (sdex.AuditOp), tallying which claims the
// decoder DROPS and why. It exists to give an exact diagnosis of SDEX
// trade-count gaps against external anchors (Hubble): Hubble counts one trade
// per claim atom, we emit one per atom we DON'T drop, so the drops here are
// exactly the off-by-N. Reports drops bucketed by reason + per-ledger, with
// examples. Read-only.
func sdexClaimAudit(args []string) error { //nolint:gocognit,gocyclo,funlen // linear walk + tally; splitting reduces clarity.
	fs := flag.NewFlagSet("sdex-claim-audit", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to stellarindex.toml (required)")
	from := fs.Uint("from", 0, "first ledger sequence (inclusive, required)")
	to := fs.Uint("to", 0, "last ledger sequence (inclusive, required)")
	bucket := fs.String("bucket", "", "galexie bucket override. Default: the range vs ingestion.live_seam_ledger picks archive-or-live; with no seam configured it stays cfg.Storage.S3BucketLive, which does NOT hold historic ranges — pass the archive bucket for those (see opsutil.ResolveStreamBucket)")
	examples := fs.Int("examples", 20, "max example drops to print")
	dumpOps := fs.Bool("dump-ops", false, "dump every trade op (type, result codes, claim count) — diagnostic for non-extraction")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" || *from == 0 || *to == 0 || *to < *from {
		return fmt.Errorf("-config, -from, -to are required; -to must be >= -from")
	}

	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return err
	}
	ctx, cancel := opsutil.SignalContext()
	defer cancel()

	// Seam-aware bucket choice, shared with ch-backfill and
	// census-backfill rather than re-derived: a local default
	// (cfg.Storage.S3BucketLive unless -bucket) would send every historic
	// audit at the TRIMMED live bucket, so this tool's
	// headline "total claim atoms (= Hubble trade count)" could be
	// computed over a fraction of the range and still read as a
	// decoder gap.
	streamBucket, err := opsutil.ResolveStreamBucket(cfg, *bucket, uint32(*from), uint32(*to))
	if err != nil {
		return err
	}
	lsCfg := opsutil.NewBoundedLedgerStreamConfig(cfg, streamBucket, 1)
	passphrase := cfg.Stellar.Passphrase()

	fmt.Fprintf(os.Stderr, "sdex-claim-audit: walking ledgers %d..%d from %q\n", *from, *to, streamBucket)

	walked := 0
	var totalClaims, totalDrops int
	var readerFailures, txReadFailures int
	dropsByReason := map[string]int{}
	ledgersWithDrops := map[uint32]int{}
	oneSideZeroByLedger := map[uint32]int{}
	var exampleLines []string

	walkErr := ledgerstream.Stream(ctx, lsCfg, uint32(*from), uint32(*to),
		func(lcm sdkxdr.LedgerCloseMeta) error {
			walked++
			seq := lcm.LedgerSequence()
			tally := auditLedgerClaims(lcm, passphrase, *dumpOps, *examples-len(exampleLines))
			if tally.readerErr != nil {
				readerFailures++
				fmt.Fprintf(os.Stderr, "sdex-claim-audit: reader ledger %d: %v\n", seq, tally.readerErr)
				return nil //nolint:nilerr // walk must continue past one bad ledger; readerFailures fails the command via claimAuditVerdict below
			}
			txReadFailures += tally.txReadFailures
			totalClaims += tally.claims
			totalDrops += tally.drops
			if tally.drops > 0 {
				ledgersWithDrops[seq] += tally.drops
			}
			if tally.oneSideZero > 0 {
				oneSideZeroByLedger[seq] += tally.oneSideZero
			}
			for r, c := range tally.dropsByReason {
				dropsByReason[r] += c
			}
			exampleLines = append(exampleLines, tally.examples...)
			return nil
		},
	)
	if walkErr != nil && !errors.Is(walkErr, context.Canceled) {
		return fmt.Errorf("sdex-claim-audit: stream: %w", walkErr)
	}

	fmt.Printf("\n=== sdex-claim-audit: %d..%d ===\n", *from, *to)
	fmt.Printf("ledgers walked / requested:               %d / %d\n",
		walked, uint64(*to)-uint64(*from)+1)
	fmt.Printf("total claim atoms (= Hubble trade count): %d\n", totalClaims)
	fmt.Printf("total dropped (NOT emitted as trades):    %d\n", totalDrops)
	fmt.Printf("trades we emit (claims - drops):          %d\n", totalClaims-totalDrops)
	fmt.Printf("ledgers with >=1 drop:                    %d\n", len(ledgersWithDrops))
	fmt.Printf("reader failures (ledger skipped):         %d\n", readerFailures)
	fmt.Printf("tx read failures (SDK error, skipped):    %d\n", txReadFailures)

	reasons := make([]string, 0, len(dropsByReason))
	for r := range dropsByReason {
		reasons = append(reasons, r)
	}
	sort.Slice(reasons, func(i, j int) bool { return dropsByReason[reasons[i]] > dropsByReason[reasons[j]] })
	fmt.Printf("\ndrops by reason:\n")
	for _, r := range reasons {
		fmt.Printf("  %-24s %d\n", r, dropsByReason[r])
	}
	if len(oneSideZeroByLedger) > 0 {
		osz := make([]uint32, 0, len(oneSideZeroByLedger))
		for l := range oneSideZeroByLedger {
			osz = append(osz, l)
		}
		sort.Slice(osz, func(i, j int) bool { return osz[i] < osz[j] })
		fmt.Printf("\none-side-zero ledgers (%d):\n", len(osz))
		for _, l := range osz {
			fmt.Printf("  ledger=%d count=%d\n", l, oneSideZeroByLedger[l])
		}
	}
	if len(exampleLines) > 0 {
		fmt.Printf("\nexamples:\n")
		for _, l := range exampleLines {
			fmt.Printf("  %s\n", l)
		}
	}

	// Coverage last, so the operator still gets the full diagnosis, but
	// non-zero so nothing downstream reads a partial audit as the answer.
	// Every number above is a tally over the ledgers the walk
	// delivered, and the tool's entire purpose is to be differenced
	// against an EXTERNAL anchor's count for the same range — so a walk
	// that covered less of the range than Hubble did turns straight into
	// a phantom decoder gap of exactly the ledgers nobody read. A SIGINT
	// lands here too: the walk treats context.Canceled as a clean exit,
	// and an interrupted audit is not an audit.
	coverageErr := walkCoverage("sdex-claim-audit", uint32(*from), uint32(*to), walked, streamBucket)
	return claimAuditVerdict(coverageErr, readerFailures, txReadFailures, totalClaims)
}

// claimAuditVerdict combines the coverage gate with the reader/tx
// read-failure gate. A ledger the reader couldn't open, or a transaction
// the SDK couldn't read, is silently excluded from "total claim atoms
// (= Hubble trade count)" — walked already counted the ledger, so
// coverageErr alone cannot see the gap. Either failure count above zero
// means the headline total is an undercount, not the Hubble-comparable
// figure it claims to be, regardless of whether coverage itself was
// complete.
func claimAuditVerdict(coverageErr error, readerFailures, txReadFailures, totalClaims int) error {
	if readerFailures == 0 && txReadFailures == 0 {
		return coverageErr
	}
	return errors.Join(coverageErr, fmt.Errorf(
		"sdex-claim-audit: %d ledger(s) unreadable and %d transaction(s) hit an SDK read "+
			"error, both silently excluded from the %d claim atoms above — the total is not "+
			"the Hubble-comparable count it claims to be; refusing to certify an undercounted total",
		readerFailures, txReadFailures, totalClaims))
}

// claimAuditTally is one ledger's claim-audit walk: the claim/drop counts
// sdex.AuditOp reports, plus the two failure classes sdexClaimAudit refuses
// to fold into "no claims here" — a whole-ledger reader failure and a
// per-tx SDK read error (terr != nil), each distinct from an ordinary
// failed transaction (!tx.Result.Successful(), which legitimately carries
// no claims and is correctly skipped).
type claimAuditTally struct {
	readerErr      error
	txReadFailures int
	claims, drops  int
	dropsByReason  map[string]int
	oneSideZero    int
	examples       []string
}

// auditLedgerClaims runs every successful transaction in one ledger
// through the real SDEX decode path (sdex.AuditOp), tallying claim atoms
// and drops. maxExamples caps how many example drop lines it collects
// (the running total across the whole walk, not just this ledger).
func auditLedgerClaims(lcm sdkxdr.LedgerCloseMeta, passphrase string, dumpOps bool, maxExamples int) claimAuditTally {
	seq := lcm.LedgerSequence()
	reader, rerr := ingest.NewLedgerTransactionReaderFromLedgerCloseMeta(passphrase, lcm)
	if rerr != nil {
		return claimAuditTally{readerErr: rerr}
	}
	defer func() { _ = reader.Close() }()

	tally := claimAuditTally{dropsByReason: map[string]int{}}
	for {
		tx, terr := reader.Read()
		if errors.Is(terr, io.EOF) {
			break
		}
		if terr != nil {
			tally.txReadFailures++
			continue
		}
		if !tx.Result.Successful() {
			continue
		}
		auditTxClaims(seq, tx, dumpOps, maxExamples, &tally)
	}
	return tally
}

// auditTxClaims runs every operation of one successful transaction through
// the real SDEX decode path (sdex.AuditOp), folding claim/drop counts into
// tally. maxExamples caps how many example drop lines tally collects (the
// running total across the whole ledger walk, not just this transaction).
func auditTxClaims(seq uint32, tx ingest.LedgerTransaction, dumpOps bool, maxExamples int, tally *claimAuditTally) {
	ops := tx.Envelope.Operations()
	opResults, ok := tx.Result.Result.OperationResults()
	if !ok {
		return
	}
	for i := range ops {
		if i >= len(opResults) {
			break
		}
		claims, drops := sdex.AuditOp(ops[i], opResults[i])
		if dumpOps && isTradeOpType(ops[i].Body.Type) {
			inner, hasInner := innerTradeCode(ops[i], opResults[i])
			fmt.Printf("ledger=%d tx=%d op=%d type=%s outerCode=%d innerCode=%d(%v) claims=%d emitted=%d\n",
				seq, tx.Index, i, ops[i].Body.Type.String(), opResults[i].Code, inner, hasInner, claims, claims-len(drops))
		}
		tally.claims += claims
		recordClaimDrops(seq, i, drops, maxExamples, tally)
	}
}

// recordClaimDrops folds one operation's dropped claim atoms into tally,
// bucketing by reason and capping the collected example lines at maxExamples.
func recordClaimDrops(seq uint32, opIdx int, drops []sdex.ClaimDrop, maxExamples int, tally *claimAuditTally) {
	for _, d := range drops {
		tally.drops++
		reason := classifyDrop(d.Reason)
		if strings.HasPrefix(reason, "non-positive: one-side-zero") {
			tally.oneSideZero++
		}
		tally.dropsByReason[reason]++
		if len(tally.examples) < maxExamples {
			tally.examples = append(tally.examples, fmt.Sprintf("ledger=%d op=%d atomType=%d: %s", seq, opIdx, d.AtomType, d.Reason))
		}
	}
}

// isTradeOpType reports whether an op type can emit ClaimAtoms.
func isTradeOpType(t sdkxdr.OperationType) bool {
	switch t {
	case sdkxdr.OperationTypeManageSellOffer, sdkxdr.OperationTypeManageBuyOffer,
		sdkxdr.OperationTypeCreatePassiveSellOffer, sdkxdr.OperationTypePathPaymentStrictReceive,
		sdkxdr.OperationTypePathPaymentStrictSend:
		return true
	}
	return false
}

// innerTradeCode returns the op's inner result code (the per-type result arm's
// Code) for diagnosing why claim extraction yields nothing.
func innerTradeCode(op sdkxdr.Operation, r sdkxdr.OperationResult) (int32, bool) {
	if r.Code != sdkxdr.OperationResultCodeOpInner {
		return 0, false
	}
	tr, ok := r.GetTr()
	if !ok {
		return 0, false
	}
	switch op.Body.Type {
	case sdkxdr.OperationTypeManageSellOffer:
		if x, ok := tr.GetManageSellOfferResult(); ok {
			return int32(x.Code), true
		}
	case sdkxdr.OperationTypeManageBuyOffer:
		if x, ok := tr.GetManageBuyOfferResult(); ok {
			return int32(x.Code), true
		}
	case sdkxdr.OperationTypeCreatePassiveSellOffer:
		if x, ok := tr.GetCreatePassiveSellOfferResult(); ok {
			return int32(x.Code), true
		}
	case sdkxdr.OperationTypePathPaymentStrictReceive:
		if x, ok := tr.GetPathPaymentStrictReceiveResult(); ok {
			return int32(x.Code), true
		}
	case sdkxdr.OperationTypePathPaymentStrictSend:
		if x, ok := tr.GetPathPaymentStrictSendResult(); ok {
			return int32(x.Code), true
		}
	}
	return 0, false
}

// classifyDrop buckets a decoder error string into a stable reason category.
func classifyDrop(reason string) string {
	switch {
	// The decoder reports this drop class as "both-zero no-op claim";
	// decoders that also rejected one-side-zero fills spelled it
	// "non-positive amounts". Matching only one spelling sends the
	// DOMINANT drop class to "other" and makes the
	// one-side-zero split below unreachable — in the one tool whose
	// stated purpose is "an exact diagnosis of SDEX trade-count gaps
	// against external anchors".
	//
	// Both spellings are matched so the tool still classifies correctly
	// when run against older decoder output.
	case strings.Contains(reason, "both-zero no-op claim"),
		strings.Contains(reason, "non-positive amounts"):
		// Split: both-zero claims are not real trades (Hubble drops them too,
		// no mismatch); one-side-zero claims ARE trades Hubble records but our
		// OR-guard rejects — the exact off-by-one vs Hubble.
		//
		// NOTE: the current DECODER does not reject one-side-zero
		// fills, so they never appear here — but they are still dropped, one
		// layer down, by filterStorableTrades (the trades CHECK forbids a
		// zero leg). The off-by-one vs Hubble therefore persists, outside
		// this tool's view.
		if strings.Contains(reason, "sold=0 bought=0") {
			return "non-positive: both-zero (Hubble also drops)"
		}
		return "non-positive: one-side-zero (Hubble counts -> off-by-one)"
	case strings.Contains(reason, "pair:"):
		return "invalid-pair (base==quote?)"
	case strings.Contains(reason, "sold asset"), strings.Contains(reason, "bought asset"):
		return "asset-conversion"
	case strings.Contains(reason, "type="):
		return "unknown-claim-atom-type"
	default:
		return "other"
	}
}
