package v1

import (
	"errors"
	"maps"
	"math/big"
	"slices"
	"sort"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend/oracleagg"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// Account classes on /health.
const (
	healthClassHealthy       = "healthy"
	healthClassLiquidatable  = "liquidatable"
	healthClassBadDebt       = "bad_debt"
	healthClassInAuction     = "in_auction"
	healthClassIndeterminate = "indeterminate"
	healthClassExempt        = "liquidation_exempt" // the pool contract or its backstop: the contract refuses to auction it
	healthClassUnpriced      = "unpriced"
)

// Reasons an account with debt has no health factor, or no determinate class; oracle verdicts are used verbatim.
const (
	healthReasonStateNewer          = "state_newer"
	healthReasonRateUnread          = "rate_unread"
	healthReasonConfigUnread        = "config_unread"
	healthReasonPriceMissing        = "oracle_price_missing"
	healthReasonPriceTooOld         = "oracle_price_older_than_24h"
	healthReasonBaseMismatch        = "oracle_base_mismatch"
	healthReasonInputInvalid        = "health_input_invalid"
	healthReasonOracleUnknown       = "oracle_unknown"
	healthReasonFormula             = "health_formula_unverified"
	healthReasonAccrualPending      = "accrual_pending"
	healthReasonAccrualUnbounded    = "accrual_unbounded"
	healthReasonAuctionsUnread      = "auctions_unread"
	healthReasonUnreconciled        = "positions_unreconciled"
	healthReasonInstanceUnread      = "pool_instance_unread"
	healthReasonBackstopUnread      = "backstop_unread"
	healthReasonCollateralValueZero = "collateral_value_zero"
)

const lendingHealthNote = "Health is computed on the pool's own oracle (basis.kind pool_oracle), reproduced at basis.ledger, with every " +
	"position, reserve and auction entry read as of that ledger; an entry changed after it is state_newer. hf and the bases use " +
	"stored rates: the contract first accrues interest to the ledger, which is not reproduced (accrual), so an account whose " +
	"class could change within that accrual at the stored borrow rate is indeterminate. An account is liquidatable only when " +
	"the gate admits it at both ends of that bound, no user-liquidation auction is live for it, it is neither the pool nor its " +
	"backstop, and every price it needs is fresh. Accounts without a health factor are named in summary.unpriced and every total " +
	"is then a lower bound."

// healthBands are the HF band upper bounds as num/den; the last band is open.
var healthBands = [][2]int64{{1, 1}, {21, 20}, {11, 10}, {5, 4}, {3, 2}, {2, 1}}

// LendingPoolHealthView is the GET /v1/lending/pools/{pool}/health body.
type LendingPoolHealthView struct {
	Pool     string                 `json:"pool"`
	Protocol string                 `json:"protocol"`
	Version  *string                `json:"version"` // v1 | v2; null for blnt
	Basis    LendingHealthBasis     `json:"basis"`
	Accrual  LendingHealthAccrual   `json:"accrual"`
	Summary  LendingHealthSummary   `json:"summary"`
	Bands    []LendingHealthBand    `json:"bands"`
	Accounts []LendingHealthAccount `json:"accounts"`
	// ViewChanged: the cursor came from a view at another ledger; this page is from the current one.
	ViewChanged bool   `json:"view_changed"`
	Note        string `json:"note"`
}

// LendingHealthBasis is the price basis: the pool's oracle as reproduced at one ledger.
type LendingHealthBasis struct {
	Kind           string                      `json:"kind"` // pool_oracle
	Oracle         *string                     `json:"oracle"`
	AdapterWasm    *string                     `json:"adapter_wasm_hash"`
	AdapterVersion *string                     `json:"adapter_version"`
	Ledger         uint32                      `json:"ledger"`
	CloseTime      *WireTime                   `json:"close_time"`
	Reserves       []LendingHealthReservePrice `json:"reserves"`
}

// LendingHealthReservePrice is one reserve's oracle price (raw integer at decimals) or the verdict that replaces it.
type LendingHealthReservePrice struct {
	Asset     string    `json:"asset"`
	Price     *string   `json:"price"`
	Decimals  *uint32   `json:"decimals"`
	Timestamp *WireTime `json:"timestamp"`
	Quote     *string   `json:"quote"`
	Verdict   *string   `json:"verdict"`
}

// LendingHealthAccrual is the caveat that hf and the bases are on stored rates.
type LendingHealthAccrual struct {
	Reproduced        bool   `json:"reproduced"`
	PendingSecondsMax uint64 `json:"pending_seconds_max"`
}

// LendingHealthSummary counts accounts by class; bases are oracle-base integers summed over priced accounts with debt.
type LendingHealthSummary struct {
	AccountsTotal     int                      `json:"accounts_total"`
	AccountsWithDebt  int                      `json:"accounts_with_debt"`
	NoDebt            int                      `json:"no_debt"`
	Healthy           int                      `json:"healthy"`
	Liquidatable      int                      `json:"liquidatable"`
	BadDebt           int                      `json:"bad_debt"`
	InAuction         int                      `json:"in_auction"`
	Indeterminate     int                      `json:"indeterminate"`
	LiquidationExempt int                      `json:"liquidation_exempt"`
	Unpriced          map[string]int           `json:"unpriced"`
	CollateralBase    string                   `json:"collateral_base"`
	LiabilitiesBase   string                   `json:"liabilities_base"`
	LowerBound        bool                     `json:"lower_bound"`
	Excluded          []LendingHealthExclusion `json:"excluded"`
}

// LendingHealthExclusion names what makes the summary a lower bound; Accounts is 0 for a pool-wide reason.
type LendingHealthExclusion struct {
	Reason   string `json:"reason"`
	Accounts int    `json:"accounts"`
}

// LendingHealthBand is one HF band [from, to): its priced accounts with debt and their effective liabilities.
type LendingHealthBand struct {
	From            string  `json:"from"`
	To              *string `json:"to"`
	Accounts        int     `json:"accounts"`
	LiabilitiesBase string  `json:"liabilities_base"`
}

// LendingHealthAccount is one account with debt.
type LendingHealthAccount struct {
	Account            string  `json:"account"`
	HF                 *string `json:"hf"` // 4 dp rounded down, on stored rates; null when unpriced
	Class              string  `json:"class"`
	Reason             *string `json:"reason"`
	CollateralBase     *string `json:"collateral_base"`
	LiabilitiesBase    *string `json:"liabilities_base"`
	InAuction          bool    `json:"in_auction"`
	PoolOrBackstop     bool    `json:"pool_or_backstop"`
	LastModifiedLedger uint32  `json:"last_modified_ledger"`
}

// healthRow is one listed account and its exact sort key; c and l are nil when unpriced.
type healthRow struct {
	view LendingHealthAccount
	c, l *big.Int
}

// healthInputs is the per-pool state every account is valued against.
type healthInputs struct {
	pool, backstop string
	version        blend.PoolVersion
	ledger         uint32
	now            uint64
	poolReason     string
	reserves, hi   map[string]blend.HealthReserve
	newer          map[string]bool // reserves whose ResData changed after the ledger
	prices         map[string]blend.OraclePrice
	verdicts       map[string]string
	auctions       map[string]uint32 // user-liquidation auctions by user: the entry's ledger
	touched        map[string]uint32 // any auction naming the user: the newest entry's ledger
	auctionsUnread bool
	backstopReason string // why the backstop is unknown; "" when known
}

// healthBackstop is the pool's backstop, or why it is unknown (an unread instance, or one without a readable Backstop).
func healthBackstop(b poolTopAccounts) (backstop, reason string) {
	switch {
	case b.instance == nil:
		return "", healthReasonInstanceUnread
	case b.instance.Backstop == "":
		return "", healthReasonBackstopUnread
	}
	return b.instance.Backstop, ""
}

func newHealthInputs(pool string, b poolTopAccounts) healthInputs {
	h := b.health
	in := healthInputs{
		pool: pool, version: b.version, ledger: h.basis.Ledger, now: healthNow(h.basis.CloseTime),
		reserves: map[string]blend.HealthReserve{}, hi: map[string]blend.HealthReserve{}, newer: map[string]bool{},
		prices: map[string]blend.OraclePrice{}, verdicts: map[string]string{}, auctions: map[string]uint32{}, touched: map[string]uint32{},
		auctionsUnread: h.auctionsErr || h.auctions.Truncated || h.auctions.Unrecognised > 0,
	}
	in.backstop, in.backstopReason = healthBackstop(b)
	switch {
	case b.version != blend.PoolV1 && b.version != blend.PoolV2:
		in.poolReason = healthReasonFormula // BLNT is closed source; its formula is not pinned
	case h.basis.Oracle == "":
		in.poolReason = healthReasonOracleUnknown
	case b.instance != nil && in.ledger > 0 && b.instance.Ledger > in.ledger:
		in.poolReason = healthReasonStateNewer
	}
	for a, st := range b.states {
		in.addReserve(a, st, b.positions.Reserves)
	}
	for a, p := range h.prices {
		if p.Verdict != "" {
			in.verdicts[a] = string(p.Verdict)
			continue
		}
		in.prices[a] = p.Price
	}
	for _, au := range h.auctions.Auctions {
		if au.AuctionType == 0 {
			in.auctions[au.User] = au.Ledger
		}
		in.touched[au.User] = max(in.touched[au.User], au.Ledger)
	}
	for _, au := range h.auctions.Gone {
		in.touched[au.User] = max(in.touched[au.User], au.Ledger) // a fill or delete after the ledger changed the account
	}
	return in
}

// addReserve values a reserve on its applied ResConfig ledger entry only, at its ledger: the event-sourced config carries
// no ledger. A config naming another ResList slot than the one positions index the reserve by is unread.
func (in *healthInputs) addReserve(a string, st clickhouse.BlendReserveState, resList []string) {
	rd, cfg := st.Data, st.LakeConfig
	if i := slices.Index(resList, a); cfg != nil && i >= 0 && uint32(i) != cfg.Index { //nolint:gosec // a ResList index
		cfg = nil
	}
	in.reserves[a] = blend.HealthReserve{Data: &rd, Config: cfg}
	in.newer[a] = st.Ledger > in.ledger || (cfg != nil && st.ConfigLedger > in.ledger)
	if cfg != nil {
		if up, err := blend.AccrualCeiling(st.Data, *cfg, in.now); err == nil {
			in.hi[a] = blend.HealthReserve{Data: &up, Config: cfg}
		}
	}
}

// healthTally accumulates the summary and bands.
type healthTally struct {
	sum         LendingHealthSummary
	coll, liab  *big.Int
	bandN       []int
	bandL       []*big.Int
	excludedBy  map[string]int
	poolReasons []string
}

// deriveHealth values every account of the snapshot; it never mutates b. rows are sorted worst first.
func deriveHealth(pool string, b poolTopAccounts) (LendingPoolHealthView, []healthRow) {
	in := newHealthInputs(pool, b)
	view := LendingPoolHealthView{
		Pool: pool, Protocol: b.protocol, Version: healthVersion(b.version), Basis: healthBasisView(b), Bands: []LendingHealthBand{},
		Accounts: []LendingHealthAccount{}, Note: lendingHealthNote,
	}
	t := healthTally{
		sum:  LendingHealthSummary{Unpriced: map[string]int{}, Excluded: []LendingHealthExclusion{}},
		coll: new(big.Int), liab: new(big.Int), bandN: make([]int, len(healthBands)+1), excludedBy: map[string]int{},
	}
	for range len(healthBands) + 1 {
		t.bandL = append(t.bandL, new(big.Int))
	}
	var rows []healthRow
	for _, acct := range b.positions.Accounts {
		if row, listed := t.account(in, acct); listed {
			rows = append(rows, row)
		}
	}
	view.Accrual = healthAccrual(b, in.now)
	t.poolExclusions(b, in, view.Accrual)
	view.Summary, view.Bands = t.finish()
	if b.positions.Truncated || !b.positions.Readable {
		rows = nil // the read stopped at its cap in key order: the worst account may be past it
	}
	sortHealthRows(rows)
	return view, rows
}

// account values one account, counts it, and returns its row when it has debt (or its state at the ledger is unknown).
func (t *healthTally) account(in healthInputs, acct clickhouse.LendingPoolPosition) (healthRow, bool) {
	t.sum.AccountsTotal++
	_, inAuction := in.auctions[acct.Account]
	row := healthRow{view: LendingHealthAccount{
		Account: acct.Account, InAuction: inAuction,
		PoolOrBackstop:     acct.Account == in.pool || (in.backstop != "" && acct.Account == in.backstop),
		LastModifiedLedger: acct.Ledger,
	}}
	// Ledger 0 is no evaluation ledger: every reserve then carries a verdict, which names the account below.
	if in.ledger > 0 && (acct.Ledger > in.ledger || in.touched[acct.Account] > in.ledger) {
		return t.unpriced(row, healthReasonStateNewer), true
	}
	if !hasPositive(acct.Borrow) {
		t.sum.NoDebt++
		return healthRow{}, false
	}
	pos := blend.Positions{Collateral: acct.Collateral, Liabilities: acct.Borrow, Supply: acct.NonCollateral}
	if in.newerHeld(pos) { // counted once, as state_newer, never also as an account with debt at the ledger
		return t.unpriced(row, healthReasonStateNewer), true
	}
	t.sum.AccountsWithDebt++
	if reason := in.heldReason(pos); reason != "" {
		return t.unpriced(row, reason), true
	}
	h, err := blend.AccountHealth(pos, in.reserves, in.prices, in.version, in.now)
	if err != nil {
		return t.unpriced(row, healthErrReason(err)), true
	}
	row.c, row.l = h.CollateralBase, h.LiabilityBase
	row.view.CollateralBase, row.view.LiabilitiesBase = strRef(h.CollateralBase.String()), strRef(h.LiabilityBase.String())
	if hf, ok := blend.HFFloor4(h.CollateralBase, h.LiabilityBase); ok {
		row.view.HF = &hf
	}
	class, reason := in.classify(pos, h, row.view)
	row.view.Class = class
	if reason != "" {
		row.view.Reason = strRef(reason)
	}
	t.count(class, reason, row)
	return row, true
}

func (t *healthTally) unpriced(row healthRow, reason string) healthRow {
	row.view.Class, row.view.Reason = healthClassUnpriced, strRef(reason)
	t.sum.Unpriced[reason]++
	t.excludedBy[reason]++
	return row
}

func (t *healthTally) count(class, reason string, row healthRow) {
	switch class {
	case healthClassHealthy:
		t.sum.Healthy++
	case healthClassLiquidatable:
		t.sum.Liquidatable++
	case healthClassBadDebt:
		t.sum.BadDebt++
	case healthClassInAuction:
		t.sum.InAuction++
	case healthClassExempt:
		t.sum.LiquidationExempt++
	case healthClassIndeterminate:
		t.sum.Indeterminate++
		if reason == healthReasonAuctionsUnread || reason == healthReasonInstanceUnread || reason == healthReasonBackstopUnread {
			t.excludedBy[reason]++
		}
	}
	t.coll.Add(t.coll, row.c)
	t.liab.Add(t.liab, row.l)
	if row.view.PoolOrBackstop {
		return // the backstop's bad debt is not a borrower's health
	}
	i := healthBandOf(row.c, row.l)
	t.bandN[i]++
	t.bandL[i].Add(t.bandL[i], row.l)
}

// newerHeld reports that the pool instance, or a reserve the account holds, changed after the ledger.
func (in healthInputs) newerHeld(pos blend.Positions) bool {
	if in.poolReason != "" || in.ledger == 0 {
		return in.poolReason == healthReasonStateNewer
	}
	return slices.ContainsFunc(heldReserves(pos), func(a string) bool { return in.newer[a] })
}

// heldReason is why a reserve the account holds collateral or debt in cannot be valued at the ledger; "" when all can.
func (in healthInputs) heldReason(pos blend.Positions) string {
	if in.poolReason != "" {
		return in.poolReason
	}
	for _, a := range heldReserves(pos) {
		if v, ok := in.verdicts[a]; ok {
			return v
		}
		res, ok := in.reserves[a]
		switch {
		case !ok:
			return healthReasonRateUnread
		case res.Config == nil:
			return healthReasonConfigUnread
		}
	}
	return ""
}

// classify applies Classify at both ends of the accrual bracket and then health.go's caller obligations: a live
// user-liquidation auction, and the pool or its backstop, are never liquidatable.
func (in healthInputs) classify(pos blend.Positions, h blend.Health, row LendingHealthAccount) (class, reason string) {
	status, reason := in.bracketed(pos, h)
	switch {
	case row.InAuction:
		return healthClassInAuction, ""
	case reason != "":
		return healthClassIndeterminate, reason
	case status == blend.StatusBadDebt && hasPositive(pos.Collateral):
		// The contract's bad debt is an empty collateral map (bad_debt.rs check_and_handle_user_bad_debt); collateral
		// that is held but values to zero is not it, and whether it can be auctioned is not pinned.
		return healthClassIndeterminate, healthReasonCollateralValueZero
	case status == blend.StatusBadDebt:
		return healthClassBadDebt, ""
	case status != blend.StatusLiquidatable:
		return healthClassHealthy, ""
	case row.PoolOrBackstop:
		return healthClassExempt, ""
	case in.backstopReason != "":
		return healthClassIndeterminate, in.backstopReason
	case in.auctionsUnread:
		return healthClassIndeterminate, healthReasonAuctionsUnread
	}
	return healthClassLiquidatable, ""
}

// bracketed classifies on stored rates and on the accrual ceiling: most favourable (collateral up, debt as stored) and
// least favourable (collateral as stored, debt up). Agreement is the class; disagreement names the accrual.
func (in healthInputs) bracketed(pos blend.Positions, lo blend.Health) (blend.Status, string) {
	if lo.AccrualPendingSeconds == 0 {
		s, _ := blend.Classify(in.version, lo)
		return s, ""
	}
	for _, a := range heldReserves(pos) {
		if _, ok := in.hi[a]; !ok {
			return 0, healthReasonAccrualUnbounded
		}
	}
	hi, err := blend.AccountHealth(pos, in.hi, in.prices, in.version, in.now)
	if err != nil {
		return 0, healthReasonAccrualUnbounded
	}
	best, _ := blend.Classify(in.version, blend.Health{CollateralBase: hi.CollateralBase, LiabilityBase: lo.LiabilityBase, CollateralRaw: hi.CollateralRaw})
	worst, _ := blend.Classify(in.version, blend.Health{CollateralBase: lo.CollateralBase, LiabilityBase: hi.LiabilityBase, CollateralRaw: lo.CollateralRaw})
	if best != worst {
		return 0, healthReasonAccrualPending
	}
	return best, ""
}

// heldReserves lists, sorted, the reserves with a non-zero collateral or liability balance.
func heldReserves(pos blend.Positions) []string {
	var out []string
	for _, m := range []map[string]*big.Int{pos.Collateral, pos.Liabilities} {
		for a, v := range m {
			if v != nil && v.Sign() != 0 && !slices.Contains(out, a) {
				out = append(out, a)
			}
		}
	}
	sort.Strings(out)
	return out
}

func hasPositive(m map[string]*big.Int) bool {
	for _, v := range m {
		if v != nil && v.Sign() > 0 {
			return true
		}
	}
	return false
}

func healthErrReason(err error) string {
	switch {
	case errors.Is(err, blend.ErrStalePrice):
		return healthReasonPriceTooOld
	case errors.Is(err, blend.ErrPriceBaseMismatch):
		return healthReasonBaseMismatch
	case errors.Is(err, blend.ErrMissingPrice):
		return healthReasonPriceMissing
	case errors.Is(err, blend.ErrMissingConfig):
		return healthReasonConfigUnread
	case errors.Is(err, blend.ErrMissingRate), errors.Is(err, blend.ErrMissingReserve):
		return healthReasonRateUnread
	}
	return healthReasonInputInvalid
}

func healthBandOf(c, l *big.Int) int {
	for i, b := range healthBands {
		if blend.InBand(c, l, big.NewInt(b[0]), big.NewInt(b[1])) {
			return i
		}
	}
	return len(healthBands)
}

// poolExclusions names the pool-wide reasons the summary is a lower bound.
func (t *healthTally) poolExclusions(b poolTopAccounts, in healthInputs, accrual LendingHealthAccrual) {
	pos := b.positions
	switch {
	case !pos.Readable:
		t.poolReasons = append(t.poolReasons, topAccountsReasonList)
	case pos.Truncated:
		t.poolReasons = append(t.poolReasons, topAccountsReasonTruncated)
	case !b.reconciles():
		t.poolReasons = append(t.poolReasons, healthReasonUnreconciled)
	}
	if pos.Unreadable > 0 {
		t.poolReasons = append(t.poolReasons, topAccountsReasonUnreadable)
	}
	if in.auctionsUnread && t.excludedBy[healthReasonAuctionsUnread] == 0 {
		t.poolReasons = append(t.poolReasons, healthReasonAuctionsUnread)
	}
	if in.backstopReason != "" && t.excludedBy[in.backstopReason] == 0 {
		t.poolReasons = append(t.poolReasons, in.backstopReason)
	}
	if !accrual.Reproduced && t.liab.Sign() > 0 {
		t.poolReasons = append(t.poolReasons, healthReasonAccrualPending) // the totals are on stored rates, so debt is understated
	}
}

func (t *healthTally) finish() (LendingHealthSummary, []LendingHealthBand) {
	s := t.sum
	s.CollateralBase, s.LiabilitiesBase = t.coll.String(), t.liab.String()
	for _, r := range t.poolReasons {
		s.Excluded = append(s.Excluded, LendingHealthExclusion{Reason: r})
	}
	for _, r := range slices.Sorted(maps.Keys(t.excludedBy)) {
		s.Excluded = append(s.Excluded, LendingHealthExclusion{Reason: r, Accounts: t.excludedBy[r]})
	}
	s.LowerBound = len(s.Excluded) > 0
	bands := make([]LendingHealthBand, 0, len(t.bandN))
	from := "0"
	for i := range t.bandN {
		band := LendingHealthBand{From: from, Accounts: t.bandN[i], LiabilitiesBase: t.bandL[i].String()}
		if i < len(healthBands) {
			to := ratString(big.NewRat(healthBands[i][0], healthBands[i][1]), 2)
			band.To, from = &to, to
		}
		bands = append(bands, band)
	}
	return s, bands
}

// sortHealthRows orders worst health first by exact cross-multiplication, then unpriced accounts; ties by address.
func sortHealthRows(rows []healthRow) {
	sort.Slice(rows, func(i, j int) bool {
		return compareHealthKey(rows[i].c, rows[i].l, rows[i].view.Account, rows[j].c, rows[j].l, rows[j].view.Account) < 0
	})
}

func compareHealthKey(c1, l1 *big.Int, a1 string, c2, l2 *big.Int, a2 string) int {
	p1, p2 := l1 != nil, l2 != nil
	switch {
	case p1 && !p2:
		return -1
	case !p1 && p2:
		return 1
	case p1:
		if c := new(big.Int).Mul(c1, l2).Cmp(new(big.Int).Mul(c2, l1)); c != 0 {
			return c
		}
	}
	switch {
	case a1 < a2:
		return -1
	case a1 > a2:
		return 1
	}
	return 0
}

func healthVersion(v blend.PoolVersion) *string {
	switch v {
	case blend.PoolV1:
		return strRef("v1")
	case blend.PoolV2:
		return strRef("v2")
	case blend.PoolVersionUnknown:
	}
	return nil
}

func healthBasisView(b poolTopAccounts) LendingHealthBasis {
	h := b.health
	out := LendingHealthBasis{
		Kind: "pool_oracle", Oracle: nonEmptyPtr(h.basis.Oracle), AdapterWasm: nonEmptyPtr(h.basis.WasmHash),
		Ledger: h.basis.Ledger, Reserves: []LendingHealthReservePrice{},
	}
	if h.basis.Program != oracleagg.ProgramUnknown {
		out.AdapterVersion = strRef(h.basis.Program.String())
	}
	if !h.basis.CloseTime.IsZero() {
		t := WireTime(h.basis.CloseTime)
		out.CloseTime = &t
	}
	for _, a := range slices.Sorted(maps.Keys(h.prices)) {
		p := h.prices[a]
		rp := LendingHealthReservePrice{Asset: a}
		if p.Verdict != "" {
			rp.Verdict = strRef(string(p.Verdict))
		} else {
			d := p.Price.Decimals
			ts := WireTime(time.Unix(int64(p.Price.Timestamp), 0).UTC()) //nolint:gosec // a ledger-era unix second fits int64
			rp.Price, rp.Decimals, rp.Timestamp, rp.Quote = strRef(p.Price.Price.String()), &d, &ts, strRef(p.Quote.String())
		}
		out.Reserves = append(out.Reserves, rp)
	}
	return out
}

// healthNow is the evaluation ledger's close time in unix seconds; 0 when it was not read (every reserve then has a verdict).
func healthNow(at time.Time) uint64 {
	if at.IsZero() || at.Unix() < 0 {
		return 0
	}
	return uint64(at.Unix())
}

// healthAccrual reports the largest interval stored rates have not accrued over; reproduced only when there is none.
func healthAccrual(b poolTopAccounts, now uint64) LendingHealthAccrual {
	out := LendingHealthAccrual{}
	for _, st := range b.states {
		if st.Data.LastTime < now && now-st.Data.LastTime > out.PendingSecondsMax {
			out.PendingSecondsMax = now - st.Data.LastTime
		}
	}
	out.Reproduced = now > 0 && out.PendingSecondsMax == 0
	return out
}
