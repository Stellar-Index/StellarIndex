package v1

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// lendingConfigDecimals is the fixed-point scale of every reserve-config fraction and the backstop take rate (pool storage.rs).
const lendingConfigDecimals = 7

// lendingConfigHistoryReader is the admin-event history behind GET /v1/lending/pools/{pool}/config-history.
type lendingConfigHistoryReader interface {
	LendingConfigHistory(ctx context.Context, pool string, limit int, cur timescale.LendingConfigCursor) ([]timescale.LendingConfigChange, error)
}

var (
	_ lendingConfigHistoryReader = (*timescale.Store)(nil)
	_ lendingConfigHistoryReader = (*CachedLendingReader)(nil)
)

// LendingReserveConfigView is a reserve's ReserveConfig; the fractions are raw contract integers at config_decimals.
type LendingReserveConfigView struct {
	Index             uint32  `json:"index"`
	Decimals          uint32  `json:"decimals"`
	CollateralFactor  string  `json:"collateral_factor"`
	LiabilityFactor   string  `json:"liability_factor"`
	TargetUtilization string  `json:"target_utilization"`
	MaxUtilization    string  `json:"max_utilization"`
	RBase             string  `json:"r_base"`
	ROne              string  `json:"r_one"`
	RTwo              string  `json:"r_two"`
	RThree            string  `json:"r_three"`
	Reactivity        string  `json:"reactivity"`
	SupplyCap         *string `json:"supply_cap"` // base units; null for a V1 pool, which has no cap
	Enabled           bool    `json:"enabled"`
}

// LendingPoolConfigView is a pool's PoolConfig and admin; a config-history item carries only the fields its event set.
type LendingPoolConfigView struct {
	Oracle           *string `json:"oracle,omitempty"`
	Admin            *string `json:"admin,omitempty"`
	BackstopTakeRate *string `json:"backstop_take_rate,omitempty"`
	MaxPositions     *uint32 `json:"max_positions,omitempty"`
	MinCollateral    *string `json:"min_collateral,omitempty"`
	Status           *uint32 `json:"status,omitempty"`
	StatusLabel      *string `json:"status_label,omitempty"`
}

func u32String(n uint32) string { return strconv.FormatUint(uint64(n), 10) }

func bigString(n *big.Int) *string {
	if n == nil {
		return nil
	}
	s := n.String()
	return &s
}

func nonEmptyPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func reserveConfigView(c *blend.ReserveConfig) *LendingReserveConfigView {
	if c == nil {
		return nil
	}
	return &LendingReserveConfigView{
		Index: c.Index, Decimals: c.Decimals,
		CollateralFactor: u32String(c.CFactor), LiabilityFactor: u32String(c.LFactor),
		TargetUtilization: u32String(c.Util), MaxUtilization: u32String(c.MaxUtil),
		RBase: u32String(c.RBase), ROne: u32String(c.ROne), RTwo: u32String(c.RTwo), RThree: u32String(c.RThree),
		Reactivity: u32String(c.Reactivity), SupplyCap: bigString(c.SupplyCap), Enabled: c.Enabled,
	}
}

// poolStatusLabels are the PoolConfig.status codes of the V2 and BLNT pool contracts (pool/src/pool/status.rs).
var poolStatusLabels = [...]string{"admin_active", "active", "admin_on_ice", "on_ice", "admin_frozen", "frozen", "setup"}

// poolStatusLabel names a status code; nil for a V1 pool, whose codes are not verified here, and for an unknown code.
func poolStatusLabel(status uint32, version blend.PoolVersion) *string {
	if version == blend.PoolV1 || status >= uint32(len(poolStatusLabels)) {
		return nil
	}
	return &poolStatusLabels[status]
}

func poolConfigView(inst *clickhouse.PoolInstance, version blend.PoolVersion) *LendingPoolConfigView {
	if inst == nil {
		return nil
	}
	c := inst.Config
	rate, maxPos, status := u32String(c.BstopRate), c.MaxPositions, c.Status
	return &LendingPoolConfigView{
		Oracle: nonEmptyPtr(c.Oracle), Admin: nonEmptyPtr(inst.Admin), BackstopTakeRate: &rate, MaxPositions: &maxPos,
		MinCollateral: bigString(c.MinCollateral), Status: &status, StatusLabel: poolStatusLabel(status, version),
	}
}

// irModDecimals is ReserveData.ir_mod's scale: 9 on a V1 pool, 7 on V2 and BLNT.
func irModDecimals(v blend.PoolVersion) int {
	if v == blend.PoolV1 {
		return 9
	}
	return 7
}

// reserveLastUpdate is ResData.last_time, the ledger close time (unix seconds) of the reserve's last accrual; nil when unset.
func reserveLastUpdate(sec uint64) *WireTime {
	if sec == 0 || sec > uint64(1<<62) {
		return nil
	}
	t := WireTime(time.Unix(int64(sec), 0).UTC())
	return &t
}

// LendingConfigHistoryView is the GET /v1/lending/pools/{pool}/config-history body.
type LendingConfigHistoryView struct {
	Pool           string                    `json:"pool"`
	ConfigDecimals int                       `json:"config_decimals"`
	Changes        []LendingConfigChangeView `json:"changes"`
}

// LendingConfigChangeView is one admin event: a reserve-config step (asset set) or a pool-level change.
type LendingConfigChangeView struct {
	Ledger    uint32   `json:"ledger"`
	CloseTime WireTime `json:"close_time"`
	TxHash    string   `json:"tx_hash"`
	Kind      string   `json:"kind"`
	Asset     *string  `json:"asset"`
	// Applied is set on queue_set_reserve: whether the asset's next step was set_reserve rather than a cancel or a re-queue.
	Applied    *bool                     `json:"applied,omitempty"`
	Config     *LendingReserveConfigView `json:"config,omitempty"`
	PoolConfig *LendingPoolConfigView    `json:"pool_config,omitempty"`
}

// poolEventAttrs is the attributes JSON both admin tables write for update_pool and set_status.
type poolEventAttrs struct {
	BackstopTakeRate *uint32 `json:"backstop_take_rate"`
	MaxPositions     *uint32 `json:"max_positions"`
	MinCollateral    *string `json:"min_collateral"`
	Status           *uint32 `json:"status"`
}

func configChangeView(c timescale.LendingConfigChange, version blend.PoolVersion) LendingConfigChangeView {
	v := LendingConfigChangeView{
		Ledger: c.Ledger, CloseTime: WireTime(c.Ts.UTC()), TxHash: c.TxHash, Kind: c.Kind, Asset: nonEmptyPtr(c.Asset),
	}
	if c.Kind == "queue_set_reserve" {
		applied := c.Applied
		v.Applied = &applied
	}
	if c.Metadata != nil {
		if cfg, err := blend.ParseReserveConfigMetadata(c.Metadata); err == nil {
			v.Config = reserveConfigView(&cfg)
		}
	}
	switch c.Kind {
	case "set_admin":
		v.PoolConfig = &LendingPoolConfigView{Admin: nonEmptyPtr(c.NewAdmin)}
	case "update_pool", "set_status":
		var a poolEventAttrs
		if json.Unmarshal(c.Attributes, &a) != nil {
			break
		}
		pc := &LendingPoolConfigView{MaxPositions: a.MaxPositions, MinCollateral: a.MinCollateral, Status: a.Status}
		if a.BackstopTakeRate != nil {
			rate := u32String(*a.BackstopTakeRate)
			pc.BackstopTakeRate = &rate
		}
		if a.Status != nil {
			pc.StatusLabel = poolStatusLabel(*a.Status, version)
		}
		v.PoolConfig = pc
	}
	return v
}

// handleLendingConfigHistory serves GET /v1/lending/pools/{pool}/config-history: the pool's reserve-config and pool-level admin
// events, newest first, keyset-paginated.
func (s *Server) handleLendingConfigHistory(w http.ResponseWriter, r *http.Request) {
	pool := r.PathValue("pool")
	if !canonical.IsContractID(pool) {
		writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-pool",
			"Invalid pool", http.StatusBadRequest, "pool must be a C-strkey contract id")
		return
	}
	limit, ok := parseExplorerLimit(w, r, 50, 200)
	if !ok {
		return
	}
	cur, ok := parseLendingCursor(w, r, lendingConfigHistoryKinds)
	if !ok {
		return
	}
	view := LendingConfigHistoryView{Pool: pool, ConfigDecimals: lendingConfigDecimals, Changes: []LendingConfigChangeView{}}
	hr, ok := s.Lending.(lendingConfigHistoryReader)
	if !ok {
		writeJSON(w, view, Flags{})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), maxHandlerBudget)
	defer cancel()
	version, err := s.Lending.BlendPoolVersion(ctx, pool)
	if err != nil {
		s.writeLendingLookupError(ctx, w, r, "BlendPoolVersion", pool, err)
		return
	}
	rows, err := hr.LendingConfigHistory(ctx, pool, limit+1, cur)
	if err != nil {
		s.writeLendingLookupError(ctx, w, r, "LendingConfigHistory", pool, err)
		return
	}
	env := Envelope{Data: &view, AsOf: WireTime(time.Now().UTC())}
	if len(rows) > limit {
		rows = rows[:limit]
		env.Pagination = &Pagination{Next: encodeLendingConfigCursor(rows[limit-1])}
	}
	for _, c := range rows {
		view.Changes = append(view.Changes, configChangeView(c, version))
	}
	writeEnvelope(w, env)
}

// lendingConfigHistoryKinds are the event kinds the config history serves; a cursor naming any other is malformed.
var lendingConfigHistoryKinds = map[string]bool{
	"queue_set_reserve": true, "cancel_set_reserve": true, "set_reserve": true, "update_pool": true, "set_status": true, "set_admin": true,
}

// encodeLendingConfigCursor renders a row's keyset position as "<ts unixnano>.<ledger>.<tx_hash>.<op_index>.<event_index>.<kind>.<tx_order>".
func encodeLendingConfigCursor(c timescale.LendingConfigChange) string {
	return strconv.FormatInt(c.Ts.UTC().UnixNano(), 10) + "." + u32String(c.Ledger) + "." + c.TxHash + "." +
		u32String(c.OpIndex) + "." + u32String(c.EventIndex) + "." + c.Kind + "." + strconv.FormatInt(c.TxOrder, 10)
}

// parseLendingCursor decodes ?cursor= naming one of kinds; ok=false after a problem+json on a malformed value.
func parseLendingCursor(w http.ResponseWriter, r *http.Request, kinds map[string]bool) (timescale.LendingConfigCursor, bool) {
	raw := r.URL.Query().Get("cursor")
	if raw == "" {
		return timescale.LendingConfigCursor{}, true
	}
	if c, ok := decodeLendingConfigCursor(raw, kinds); ok {
		return c, true
	}
	writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-cursor",
		"Invalid cursor", http.StatusBadRequest, "cursor must be an opaque value returned in a prior pagination.next")
	return timescale.LendingConfigCursor{}, false
}

func decodeLendingConfigCursor(raw string, kinds map[string]bool) (timescale.LendingConfigCursor, bool) {
	parts := strings.Split(raw, ".")
	if len(parts) != 7 || len(parts[2]) != 64 || !kinds[parts[5]] {
		return timescale.LendingConfigCursor{}, false
	}
	txOrder, err := strconv.ParseInt(parts[6], 10, 32)
	if err != nil || txOrder < -1 {
		return timescale.LendingConfigCursor{}, false
	}
	nanos, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || nanos <= 0 {
		return timescale.LendingConfigCursor{}, false
	}
	var n [3]uint32
	for i, p := range []string{parts[1], parts[3], parts[4]} {
		v, err := strconv.ParseUint(p, 10, 32)
		if err != nil {
			return timescale.LendingConfigCursor{}, false
		}
		n[i] = uint32(v)
	}
	return timescale.LendingConfigCursor{
		Ts: time.Unix(0, nanos).UTC(), Ledger: n[0], TxHash: parts[2], OpIndex: n[1], EventIndex: n[2], Kind: parts[5], TxOrder: txOrder,
	}, true
}
