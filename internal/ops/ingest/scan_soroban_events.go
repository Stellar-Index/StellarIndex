package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	sdkxdr "github.com/stellar/go-stellar-sdk/xdr"

	"github.com/StellarIndex/stellar-index/internal/config"
	"github.com/StellarIndex/stellar-index/internal/consumer"
	"github.com/StellarIndex/stellar-index/internal/dispatcher"
	"github.com/StellarIndex/stellar-index/internal/events"
	"github.com/StellarIndex/stellar-index/internal/ledgerstream"
	"github.com/StellarIndex/stellar-index/internal/ops/opsutil"
	"github.com/StellarIndex/stellar-index/internal/scval"
)

// scanSorobanEvents streams a bounded galexie ledger range and dumps
// every Soroban contract event (optionally filtered to a topic[0]
// string and/or a single contract) as one JSON object per line:
//
//	{ledger, contract_id, tx_hash, op_index, topics:[...], body:{...}}
//
// It is the in-infra analogue of hubble-soroban-events (which needs
// BigQuery): the authoritative "what does contract X actually emit
// on-chain" answer, used to discover real contract addresses + event
// schemas before writing or auditing a decoder. No DB writes — a
// catch-all dispatcher.Decoder reuses the dispatcher's LCM→event
// extraction so we never reimplement SCVal/LCM parsing.
func scanSorobanEvents(args []string) error { //nolint:funlen,gocognit,gocyclo // linear diagnostic; splitting reduces readability
	fs := flag.NewFlagSet("scan-soroban-events", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	from := fs.Uint("from", 0, "First ledger sequence (inclusive, required)")
	to := fs.Uint("to", 0, "Last ledger sequence (inclusive, required)")
	topic0 := fs.String("topic0", "", "Only events whose decoded topic[0] String/Symbol == this (default: any)")
	contract := fs.String("contract", "", "Only events from this contract C-strkey (default: any)")
	limit := fs.Int("limit", 50, "Stop after this many matching events")
	bucketOverride := fs.String("bucket", "", "Override bucket (default: s3_bucket_archive, then s3_bucket_live)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" || *from == 0 || *to == 0 || *to < *from {
		return fmt.Errorf("-config, -from, and -to are required; -to must be >= -from")
	}
	if *limit <= 0 {
		return fmt.Errorf("-limit must be > 0")
	}

	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return err
	}
	bucket := *bucketOverride
	if bucket == "" {
		bucket = cfg.Storage.S3BucketArchive
	}
	if bucket == "" {
		bucket = cfg.Storage.S3BucketLive
	}
	if bucket == "" {
		return fmt.Errorf("no bucket: set -bucket or storage.s3_bucket_archive / s3_bucket_live")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	col := &scanCollector{
		topic0:   *topic0,
		contract: *contract,
		limit:    *limit,
		cancel:   cancel,
		out:      json.NewEncoder(os.Stdout),
	}
	disp := dispatcher.New(col)

	fmt.Fprintf(os.Stderr,
		"scan-soroban-events: bucket=%s ledgers=%d..%d topic0=%q contract=%q limit=%d\n",
		bucket, *from, *to, *topic0, *contract, *limit)

	lsCfg := opsutil.NewBoundedLedgerStreamConfig(cfg, bucket, 1)

	var totalLedgers int
	streamErr := ledgerstream.Stream(ctx, lsCfg, uint32(*from), uint32(*to),
		func(lcm sdkxdr.LedgerCloseMeta) error {
			totalLedgers++
			if _, perr := disp.ProcessLedger(lcm, cfg.Stellar.Passphrase()); perr != nil {
				fmt.Fprintf(os.Stderr, "scan-soroban-events: ledger %d: %v\n",
					lcm.LedgerSequence(), perr)
			}
			return nil
		},
	)
	// Hitting -limit cancels ctx on purpose — that's success, not error.
	if streamErr != nil && !errors.Is(streamErr, context.Canceled) {
		return fmt.Errorf("stream: %w", streamErr)
	}
	if col.firstErr != nil {
		return col.firstErr
	}
	fmt.Fprintf(os.Stderr,
		"scan-soroban-events: scanned %d ledgers, %d matching events emitted\n",
		totalLedgers, col.matched)
	return nil
}

// scanCollector is the catch-all dispatcher.Decoder backing
// scan-soroban-events. It emits no consumer.Event; Decode renders
// the event and Matches applies the optional topic0/contract filter.
type scanCollector struct {
	topic0   string // "" = any
	contract string // "" = any
	limit    int
	matched  int
	cancel   context.CancelFunc
	out      *json.Encoder
	firstErr error
}

func (c *scanCollector) Name() string { return "scan" }

func (c *scanCollector) Matches(ev events.Event) bool {
	if c.matched >= c.limit {
		return false
	}
	if c.contract != "" && ev.ContractID != c.contract {
		return false
	}
	if c.topic0 != "" {
		if len(ev.Topic) == 0 {
			return false
		}
		t0, err := scval.Parse(ev.Topic[0])
		if err != nil || scalarString(t0) != c.topic0 {
			return false
		}
	}
	return true
}

func (c *scanCollector) Decode(ev events.Event) ([]consumer.Event, error) {
	topics := make([]any, 0, len(ev.Topic))
	for _, t := range ev.Topic {
		topics = append(topics, renderB64SCVal(t, 3))
	}
	rec := map[string]any{
		"ledger":      ev.Ledger,
		"contract_id": ev.ContractID,
		"tx_hash":     ev.TxHash,
		"op_index":    ev.OperationIndex,
		"topics":      topics,
		"body":        renderB64SCVal(ev.Value, 3),
	}
	if err := c.out.Encode(rec); err != nil && c.firstErr == nil {
		c.firstErr = err
		if c.cancel != nil {
			c.cancel()
		}
	}
	c.matched++
	if c.matched >= c.limit && c.cancel != nil {
		c.cancel()
	}
	return nil, nil
}

// scalarString returns the text of a String/Symbol ScVal, else "".
func scalarString(sv sdkxdr.ScVal) string {
	if s, err := scval.AsString(sv); err == nil {
		return s
	}
	if s, err := scval.AsSymbol(sv); err == nil {
		return s
	}
	return ""
}

// renderB64SCVal parses a base64 SCVal and renders it to a
// JSON-friendly value, recursing up to depth. Maps render with their
// keys as the JSON object keys (the field names we're hunting), each
// value as a short "type=sample" — enough to reconstruct an event
// schema without dumping unbounded nested data.
func renderB64SCVal(b64 string, depth int) any {
	sv, err := scval.Parse(b64)
	if err != nil {
		return "<unparseable:" + err.Error() + ">"
	}
	return renderSCVal(sv, depth)
}

func renderSCVal(sv sdkxdr.ScVal, depth int) any { //nolint:gocyclo,gocognit,funlen // flat type switch; one arm per SCVal kind
	switch sv.Type {
	case sdkxdr.ScValTypeScvBool:
		return sv.MustB()
	case sdkxdr.ScValTypeScvVoid:
		return nil
	case sdkxdr.ScValTypeScvString:
		return "str:" + string(sv.MustStr())
	case sdkxdr.ScValTypeScvSymbol:
		return "sym:" + string(sv.MustSym())
	case sdkxdr.ScValTypeScvU32:
		return uint32(sv.MustU32())
	case sdkxdr.ScValTypeScvI32:
		return int32(sv.MustI32())
	case sdkxdr.ScValTypeScvU64:
		return uint64(sv.MustU64())
	case sdkxdr.ScValTypeScvI64:
		return int64(sv.MustI64())
	case sdkxdr.ScValTypeScvU128:
		if a, err := scval.AsAmountFromU128(sv); err == nil {
			return "u128:" + a.String()
		}
		return "u128:?"
	case sdkxdr.ScValTypeScvI128:
		if a, err := scval.AsAmountFromI128(sv); err == nil {
			return "i128:" + a.String()
		}
		return "i128:?"
	case sdkxdr.ScValTypeScvAddress:
		if s, err := scval.AsAddressStrkey(sv); err == nil {
			return "addr:" + s
		}
		return "addr:?"
	case sdkxdr.ScValTypeScvBytes:
		b := []byte(sv.MustBytes())
		n := len(b)
		if n > 16 {
			b = b[:16]
		}
		return fmt.Sprintf("bytes[%d]:%x", n, b)
	case sdkxdr.ScValTypeScvVec:
		if depth <= 0 {
			return "vec[..]"
		}
		vec, err := scval.AsVec(sv)
		if err != nil {
			return "vec:?"
		}
		out := make([]any, 0, len(vec))
		for i, e := range vec {
			if i >= 16 {
				out = append(out, "...")
				break
			}
			out = append(out, renderSCVal(e, depth-1))
		}
		return out
	case sdkxdr.ScValTypeScvMap:
		if depth <= 0 {
			return "map{..}"
		}
		entries, err := scval.AsMap(sv)
		if err != nil {
			return "map:?"
		}
		m := make(map[string]any, len(entries))
		for _, e := range entries {
			k := scalarString(e.Key)
			if k == "" {
				k = fmt.Sprintf("<%s>", e.Key.Type.String())
			}
			m[k] = renderSCVal(e.Val, depth-1)
		}
		return m
	default:
		return sv.Type.String()
	}
}
