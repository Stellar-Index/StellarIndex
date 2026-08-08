package explorer

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// This file serves GET /v1/accounts/{g_strkey}/activity — the segmented
// "what has this address been doing" breakdown: all-time per-op-type
// counts (ClickHouse lake), attributed-trade total, per-(protocol,
// action) DeFi event counts, and bridge-transfer counts (Postgres).
//
// Every segment is a WHOLE-HISTORY aggregate (the op-type arms probe the
// multi-billion-row operations table; the trade count walks the
// address's full trades history), so the payload is served through the
// shared contract-detail stale-while-revalidate cache
// (contract_detail_cache.go) under an "act:"+g key — the compute runs
// DETACHED on the refresh budget, never on the request deadline, and an
// expired entry is served stale (flags.stale + its real as_of) while one
// single-flighted refresh runs. Same mechanism, same
// obs.ObserveExplorerSWRRefresh("contract_detail", …) metric label (a
// bounded enum — do NOT mint a new label per key kind).

// ActivityReader is the narrow Postgres read seam this endpoint needs.
// *timescale.Store satisfies it. Nil disables the endpoint (503).
type ActivityReader interface {
	CountAccountTrades(ctx context.Context, address string) (int64, time.Time, error)
	DefiActionCountsByUser(ctx context.Context, address string) ([]timescale.DefiActionCount, error)
	BridgeActivityByAddress(ctx context.Context, address string) (timescale.BridgeActivity, error)
}

// OpTypeCountView is one op-type's all-time count for the account.
type OpTypeCountView struct {
	OpType string `json:"op_type"`
	Count  int64  `json:"count"`
}

// DefiActionView is one (protocol, action) DeFi event count.
type DefiActionView struct {
	Protocol string `json:"protocol"`
	Action   string `json:"action"`
	Count    int64  `json:"count"`
}

// BridgeTransfersView is the per-bridge, per-direction transfer counts.
type BridgeTransfersView struct {
	RozoOutboundPayments int64  `json:"rozo_outbound_payments"`
	RozoInboundPayments  int64  `json:"rozo_inbound_payments"`
	CCTPOutboundBurns    int64  `json:"cctp_outbound_burns"`
	CCTPInboundMints     int64  `json:"cctp_inbound_mints"`
	Note                 string `json:"note"`
}

// bridgeMatchingNote documents exactly how bridge rows attribute to an
// address — always present on the segment so the counts can't be read
// as more complete than they are.
const bridgeMatchingNote = "rozo matches payment from_addr (outbound) / destination (inbound). " +
	"cctp matches deposit_for_burn depositor (outbound) and the Stellar-side mint_and_withdraw " +
	"mint recipient (inbound) — an inbound mint delivered via a forwarder contract attributes to " +
	"that contract, not the end user, and an outbound burn's destination-chain recipient is not a " +
	"Stellar account so it is never matched."

// AccountActivityView is the wire response for GET
// /v1/accounts/{g_strkey}/activity. Segments that could not be read are
// ABSENT (nil), never zero-filled — coverage_note names them.
type AccountActivityView struct {
	Account     string            `json:"account"`
	OpsByType   []OpTypeCountView `json:"ops_by_type,omitempty"`
	TradesTotal *int64            `json:"trades_total,omitempty"`
	// TradesTotalSince, when set, is the date trades_total counts FROM —
	// the trades compression horizon (older rows aren't per-account
	// searchable yet; site audit 2026-08-08). Absent = all-time.
	TradesTotalSince string               `json:"trades_total_since,omitempty"`
	DefiActions      []DefiActionView     `json:"defi_actions,omitempty"`
	BridgeTransfers  *BridgeTransfersView `json:"bridge_transfers,omitempty"`
	// CoverageNote is the honest-degrade signal (same contract as the
	// movements/positions siblings): present when one or more segments
	// could not be read — those segments are MISSING, not zero.
	CoverageNote string `json:"coverage_note,omitempty"`
}

// activityUnavailable writes the 503 for this endpoint — it needs the
// Postgres activity reader AND the ClickHouse lake reader; a deployment
// with neither has nothing to serve.
func (h *Handler) activityUnavailable(w http.ResponseWriter, r *http.Request) {
	h.WriteProblem(w, r,
		"https://api.stellarindex.io/errors/explorer-unavailable",
		"Explorer unavailable", http.StatusServiceUnavailable,
		"This deployment hasn't wired the account-activity readers.")
}

// errActivityAllSegmentsFailed marks a compute where NOTHING could be
// read — the only case worth failing (and thereby preserving a previous
// cached payload) rather than caching an all-absent shell for the TTL.
var errActivityAllSegmentsFailed = errors.New("account activity: every segment read failed")

// AccountActivity serves GET /v1/accounts/{g_strkey}/activity.
//
// SWR contract: a fresh cached payload serves as-is; an expired one
// serves immediately with flags.stale=true and its real as_of while a
// detached single-flight recompute runs; a never-computed account waits
// for the detached compute up to the request deadline only (a heavy
// account 503s the first request and the retry lands warm) — the same
// contract ContractDetail documents.
func (h *Handler) AccountActivity(w http.ResponseWriter, r *http.Request) {
	if h.Activity == nil && h.Reader == nil {
		h.activityUnavailable(w, r)
		return
	}
	g, ok := h.parseAccountStrkey(w, r)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), explorerReadTimeout)
	defer cancel()

	v, asOf, degraded, err := h.contractDetailCached(ctx, "act:"+g, func(rctx context.Context) (any, error) {
		return h.computeAccountActivity(rctx, g)
	})
	if err != nil {
		if h.ClientAborted(r, err) {
			return
		}
		if retryableColdMiss(ctx, err) {
			h.Logger.Warn("explorer AccountActivity deadline exceeded", "account", g)
			h.writeReadTimeout(w, r, "https://api.stellarindex.io/errors/account-activity-timeout",
				"Account activity timed out")
			return
		}
		h.Logger.Error("explorer AccountActivity failed", "err", err, "account", g)
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}
	out, ok := v.(AccountActivityView)
	if !ok {
		h.Logger.Error("explorer AccountActivity: unexpected cached payload type", "account", g)
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}
	h.writeJSONAt(w, out, degraded, asOf)
}

// computeAccountActivity runs the four segment reads. Each segment
// degrades independently (absent + named in coverage_note — the
// positions endpoint's C3-045 posture: a partial answer must never be
// mistakable for "this address did nothing"); only the all-segments-
// failed case returns an error, so a transient single-store outage
// can't poison the cache with an empty shell NOR throw away the other
// stores' real answers.
func (h *Handler) computeAccountActivity(ctx context.Context, g string) (AccountActivityView, error) {
	out := AccountActivityView{Account: g}
	var failed []string

	if h.Reader == nil {
		failed = append(failed, "ops_by_type (no lake reader wired)")
	} else if opCounts, err := h.Reader.AccountOperationTypeCounts(ctx, g); err != nil {
		h.Logger.Error("activity: op-type counts read failed", "err", err, "account", g)
		failed = append(failed, "ops_by_type")
	} else {
		out.OpsByType = make([]OpTypeCountView, len(opCounts))
		for i, c := range opCounts {
			out.OpsByType[i] = OpTypeCountView{OpType: c.OpType, Count: c.Count}
		}
	}

	if h.Activity == nil {
		failed = append(failed, "trades_total, defi_actions, bridge_transfers (no Postgres activity reader wired)")
	} else {
		if n, horizon, err := h.Activity.CountAccountTrades(ctx, g); err != nil {
			h.Logger.Error("activity: trades count read failed", "err", err, "account", g)
			failed = append(failed, "trades_total")
		} else {
			out.TradesTotal = &n
			if !horizon.IsZero() && horizon.Year() > 1971 {
				out.TradesTotalSince = horizon.UTC().Format("2006-01-02")
			}
		}
		if actions, err := h.Activity.DefiActionCountsByUser(ctx, g); err != nil {
			h.Logger.Error("activity: defi action counts read failed", "err", err, "account", g)
			failed = append(failed, "defi_actions")
		} else {
			out.DefiActions = make([]DefiActionView, len(actions))
			for i, a := range actions {
				out.DefiActions[i] = DefiActionView{Protocol: a.Protocol, Action: a.Action, Count: a.Count}
			}
		}
		if bridge, err := h.Activity.BridgeActivityByAddress(ctx, g); err != nil {
			h.Logger.Error("activity: bridge activity read failed", "err", err, "account", g)
			failed = append(failed, "bridge_transfers")
		} else {
			out.BridgeTransfers = &BridgeTransfersView{
				RozoOutboundPayments: bridge.RozoOutbound,
				RozoInboundPayments:  bridge.RozoInbound,
				CCTPOutboundBurns:    bridge.CCTPOutboundBurns,
				CCTPInboundMints:     bridge.CCTPInboundMints,
				Note:                 bridgeMatchingNote,
			}
		}
	}

	if out.OpsByType == nil && out.TradesTotal == nil && out.DefiActions == nil && out.BridgeTransfers == nil {
		return AccountActivityView{}, errActivityAllSegmentsFailed
	}
	if len(failed) > 0 {
		out.CoverageNote = "INCOMPLETE: the following segments could not be read and are MISSING " +
			"from this response, not zero: " + strings.Join(failed, "; ") +
			". Retry before treating this breakdown as complete."
	}
	return out, nil
}
