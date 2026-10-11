package sorocredit

import (
	"fmt"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// collateralNamePrefix is the "Collateral-" literal that prefixes the
// per-position child-contract name in a NewCollateralContract body. The
// UUID after it is the stable position identity that StatementPublished
// / Liquidation reference by `position_uuid`.
const collateralNamePrefix = "Collateral-"

// classify maps topic[0] to the decoder-side [EventType], or "" for an untracked event, via a
// byte-equality lookup in classifyBySym (events.go) with no SCVal re-decode. Contract identity is
// gated in the dispatcher adapter's Matches, NOT here: two look-alike contracts emit the same
// symbols, so classify alone never decides a match.
func classify(e *events.Event) EventType {
	if len(e.Topic) == 0 {
		return ""
	}
	return classifyBySym[e.Topic[0]]
}

// decoded is the intermediate the per-event helpers fill — the promoted
// columns plus the per-kind Attributes remainder. consumer.go stamps the
// universal identity fields (contract, ledger, tx, op, event index, ts)
// on top.
type decoded struct {
	CollateralContract string
	PositionUUID       string
	StatementUUID      string
	PositionName       string
	Owner              string
	Account            string
	Asset              string
	Amount             string
	StatementTime      *time.Time
	Attributes         map[string]any
}

// ── topic helpers (no xdr named — sv is := inferred, per ADR-0013) ──

// addrTopic decodes topic[1] as an Address strkey — every event whose
// topic carries an address puts it at index 1 (the trust-root emitter is
// topic[0]'s symbol). Callers guard len(Topic) >= 2 first.
func addrTopic(e *events.Event, field string) (string, error) {
	sv, err := scval.Parse(e.Topic[1])
	if err != nil {
		return "", fmt.Errorf("%w: %s topic[1] parse: %w", ErrMalformedPayload, field, err)
	}
	addr, err := scval.AsAddressStrkey(sv)
	if err != nil {
		return "", fmt.Errorf("%w: %s address: %w", ErrMalformedPayload, field, err)
	}
	return addr, nil
}

// strTopic decodes topic[i] as a String in its scval.ToText form: the UUIDs are contract-chosen
// bytes bound to `text NOT NULL` columns, and Postgres rejects NUL / invalid UTF-8 there
// (SQLSTATE 22021) on every retry, which would lose the row permanently.
func strTopic(e *events.Event, i int, field string) (string, error) {
	sv, err := scval.Parse(e.Topic[i])
	if err != nil {
		return "", fmt.Errorf("%w: %s topic[%d] parse: %w", ErrMalformedPayload, field, i, err)
	}
	s, err := scval.AsText(sv)
	if err != nil {
		return "", fmt.Errorf("%w: %s string: %w", ErrMalformedPayload, field, err)
	}
	return s, nil
}

// ── per-event decoders ──────────────────────────────────────────────

// decodeNewCollateralContract:
//
//	topics = [Symbol("NewCollateralContract"), Address(child collateral C-addr)]
//	data   = Vec[ String("Collateral-<uuid>"), Address(owner G-addr) ]
//
// topic[1] is the new child contract's C-address, the value the childgate seeds. The body's String
// is its symbolic name; the UUID after "Collateral-" is the position identity that statements and
// settlements reference.
func decodeNewCollateralContract(e *events.Event) (decoded, error) {
	if len(e.Topic) < 2 {
		return decoded{}, fmt.Errorf("%w: NewCollateralContract needs 2 topics, got %d", ErrMalformedPayload, len(e.Topic))
	}
	child, err := addrTopic(e, "collateral_contract")
	if err != nil {
		return decoded{}, err
	}
	body, err := scval.Parse(e.Value)
	if err != nil {
		return decoded{}, fmt.Errorf("%w: NewCollateralContract body parse: %w", ErrMalformedPayload, err)
	}
	vec, err := scval.AsVec(body)
	if err != nil {
		return decoded{}, fmt.Errorf("%w: NewCollateralContract body: %w", ErrMalformedPayload, err)
	}
	if len(vec) < 2 {
		return decoded{}, fmt.Errorf("%w: NewCollateralContract body not a 2-Vec (len=%d)", ErrMalformedPayload, len(vec))
	}
	// AsText, not AsString: position_name is bound to a `text NOT NULL`
	// column (see strTopic).
	name, err := scval.AsText(vec[0])
	if err != nil {
		return decoded{}, fmt.Errorf("%w: NewCollateralContract name: %w", ErrMalformedPayload, err)
	}
	// TrimPrefix would return the whole string when the prefix is absent, giving PositionUUID ==
	// PositionName, which never matches the UUID in statement / settlement topics; the row would
	// look like "no statement yet" instead of the decode failure it is.
	if !strings.HasPrefix(name, collateralNamePrefix) {
		return decoded{}, fmt.Errorf("%w: NewCollateralContract name %q missing %q prefix", ErrMalformedPayload, name, collateralNamePrefix)
	}
	owner, err := scval.AsAddressStrkey(vec[1])
	if err != nil {
		return decoded{}, fmt.Errorf("%w: NewCollateralContract owner: %w", ErrMalformedPayload, err)
	}
	return decoded{
		CollateralContract: child,
		PositionName:       name,
		PositionUUID:       strings.TrimPrefix(name, collateralNamePrefix),
		Owner:              owner,
		Attributes:         map[string]any{},
	}, nil
}

// decodeStatement (StatementPublished):
//
//	topics = [Symbol, String(statement_uuid), String(position_uuid)]
//	data   = Vec[ i128(amount), Address(collateral_contract), u64(timestamp) ]
//
// closedAt is the ledger close time (decodeOne fails the event earlier if it can't parse); it is
// canonical.SafeUnixSeconds's bound and out-of-range fallback.
func decodeStatement(e *events.Event, closedAt time.Time) (decoded, error) {
	if len(e.Topic) < 3 {
		return decoded{}, fmt.Errorf("%w: StatementPublished needs 3 topics, got %d", ErrMalformedPayload, len(e.Topic))
	}
	stmtUUID, err := strTopic(e, 1, "statement_uuid")
	if err != nil {
		return decoded{}, err
	}
	posUUID, err := strTopic(e, 2, "position_uuid")
	if err != nil {
		return decoded{}, err
	}
	body, err := scval.Parse(e.Value)
	if err != nil {
		return decoded{}, fmt.Errorf("%w: StatementPublished body parse: %w", ErrMalformedPayload, err)
	}
	vec, err := scval.AsVec(body)
	if err != nil {
		return decoded{}, fmt.Errorf("%w: StatementPublished body: %w", ErrMalformedPayload, err)
	}
	if len(vec) < 3 {
		return decoded{}, fmt.Errorf("%w: StatementPublished body not a 3-Vec (len=%d)", ErrMalformedPayload, len(vec))
	}
	amount, err := nonNegAmount(vec[0])
	if err != nil {
		return decoded{}, fmt.Errorf("%w: StatementPublished amount: %w", ErrMalformedPayload, err)
	}
	collateral, err := scval.AsAddressStrkey(vec[1])
	if err != nil {
		return decoded{}, fmt.Errorf("%w: StatementPublished collateral_contract: %w", ErrMalformedPayload, err)
	}
	tsUnix, err := scval.AsU64(vec[2])
	if err != nil {
		return decoded{}, fmt.Errorf("%w: StatementPublished timestamp: %w", ErrMalformedPayload, err)
	}
	// SafeUnixSeconds bound-checks tsUnix on its raw u64 form before any
	// int64 cast: an unchecked cast of a contract-supplied value > MaxInt64
	// wraps negative and stamps a far-past StatementTime (the router
	// deadline_ts overflow class — see internal/canonical/safetime.go).
	ts := canonical.SafeUnixSeconds(tsUnix, closedAt)
	return decoded{
		StatementUUID:      stmtUUID,
		PositionUUID:       posUUID,
		CollateralContract: collateral,
		Amount:             amount.String(),
		StatementTime:      &ts,
		Attributes:         map[string]any{},
	}, nil
}

// decodeSettlement decodes the on-wire "Liquidation" event as a
// SCHEDULED SETTLEMENT (see the package doc; it is NOT a distressed liquidation).
//
//	topics = [Symbol("Liquidation"), Address(collateral_contract),
//	          String(position_uuid), String(statement_uuid)]
//	data   = Vec[ Address(settler/keeper), Vec[Address](debt_assets),
//	              Vec[i128](amounts), 4 protocol-internal trailing fields ]
//
// The settler (data[0]) is the single recurring keeper. debt_asset + settled_amount are the FIRST
// element of the parallel asset / amount vectors (USDC-only book, so one element in practice). The
// full body is kept in Attributes["body"], so nothing is dropped beyond the promoted leg.
func decodeSettlement(e *events.Event) (decoded, error) {
	if len(e.Topic) < 4 {
		return decoded{}, fmt.Errorf("%w: Liquidation needs 4 topics, got %d", ErrMalformedPayload, len(e.Topic))
	}
	collateral, err := addrTopic(e, "collateral_contract")
	if err != nil {
		return decoded{}, err
	}
	posUUID, err := strTopic(e, 2, "position_uuid")
	if err != nil {
		return decoded{}, err
	}
	stmtUUID, err := strTopic(e, 3, "statement_uuid")
	if err != nil {
		return decoded{}, err
	}
	body, err := scval.Parse(e.Value)
	if err != nil {
		return decoded{}, fmt.Errorf("%w: Liquidation body parse: %w", ErrMalformedPayload, err)
	}
	vec, err := scval.AsVec(body)
	if err != nil {
		return decoded{}, fmt.Errorf("%w: Liquidation body: %w", ErrMalformedPayload, err)
	}
	// Exactly the audited arity (wasm-audits/sorocredit.md: Vec[7]): the
	// fields are promoted by position, which only holds for that shape.
	if len(vec) != settlementBodyLen {
		return decoded{}, fmt.Errorf("%w: Liquidation body not a %d-Vec (len=%d)", ErrMalformedPayload, settlementBodyLen, len(vec))
	}
	settler, err := scval.AsAddressStrkey(vec[0])
	if err != nil {
		return decoded{}, fmt.Errorf("%w: Liquidation settler: %w", ErrMalformedPayload, err)
	}
	// Verbatim, not scval.Display: Display is lossy (truncates long fragments, degrades exotic
	// types to their name), which would contradict "nothing is dropped". e.Value is the base64 XDR
	// body, so storing it directly is lossless; scval.Parse decodes it on demand.
	attrs := map[string]any{"body": e.Value}
	asset, amount := settlementLeg(vec[1], vec[2], attrs)
	return decoded{
		CollateralContract: collateral,
		PositionUUID:       posUUID,
		StatementUUID:      stmtUUID,
		Account:            settler,
		Asset:              asset,
		Amount:             amount,
		Attributes:         attrs,
	}, nil
}

// settlementBodyLen is the Liquidation body arity ratified by the WASM audit.
const settlementBodyLen = 7

// settlementLeg promotes the primary (debt_asset, settled_amount) leg from
// the parallel data[1] Vec[Address] / data[2] Vec[i128]. A shape or length mismatch degrades into
// an attribute note rather than failing the row; on a length mismatch index pairing is unproven,
// and promoting amounts[0] could stamp it on the wrong asset.
func settlementLeg(assetsSV, amountsSV scval.ScVal, attrs map[string]any) (asset, amount string) {
	assets, aerr := scval.AsVec(assetsSV)
	amounts, merr := scval.AsVec(amountsSV)
	assetsOK := aerr == nil && len(assets) > 0
	amountsOK := merr == nil && len(amounts) > 0
	if assetsOK && amountsOK && len(assets) != len(amounts) {
		note := fmt.Sprintf("debt_assets/amounts not parallel (len %d vs %d)", len(assets), len(amounts))
		attrs["debt_asset_error"] = note
		attrs["settled_amount_error"] = note
		obs.SourceAmountDegradedTotal.WithLabelValues(SourceName, "settled_amount").Inc()
		return "", ""
	}
	// Only leg 0 is promoted, so the served volume omits any further leg;
	// record and count it rather than serve the row as a complete total.
	if assetsOK && len(assets) > 1 {
		attrs["debt_legs"] = len(assets)
		obs.SourceAmountDegradedTotal.WithLabelValues(SourceName, "settled_amount_extra_legs").Inc()
	}
	if !assetsOK {
		attrs["debt_asset_error"] = "not a non-empty Vec"
	} else if a, err := scval.AsAddressStrkey(assets[0]); err == nil {
		asset = a
	} else {
		attrs["debt_asset_error"] = err.Error()
	}
	if !amountsOK {
		attrs["settled_amount_error"] = "not a non-empty Vec"
		obs.SourceAmountDegradedTotal.WithLabelValues(SourceName, "settled_amount").Inc()
	} else if amt, err := nonNegAmount(amounts[0]); err == nil {
		amount = amt.String()
	} else {
		attrs["settled_amount_error"] = err.Error()
		obs.SourceAmountDegradedTotal.WithLabelValues(SourceName, "settled_amount").Inc()
	}
	return asset, amount
}

// nonNegAmount decodes an i128 amount and refuses a negative one: every
// sorocredit amount column CHECKs >= 0, so a negative value would be
// refused by the database on every replay instead of as a counted decode.
func nonNegAmount(sv scval.ScVal) (canonical.Amount, error) {
	amt, err := scval.AsAmountFromI128(sv)
	if err != nil {
		return canonical.Amount{}, err
	}
	if amt.Sign() < 0 {
		return canonical.Amount{}, fmt.Errorf("negative i128 amount %s", amt)
	}
	return amt, nil
}

// decodeWithdrawal:
//
//	topics = [Symbol, Address(collateral_contract)]
//	data   = Vec[ Address(token=USDC SAC), Address(recipient G-addr), i128(amount) ]
func decodeWithdrawal(e *events.Event) (decoded, error) {
	if len(e.Topic) < 2 {
		return decoded{}, fmt.Errorf("%w: Withdrawal needs 2 topics, got %d", ErrMalformedPayload, len(e.Topic))
	}
	collateral, err := addrTopic(e, "collateral_contract")
	if err != nil {
		return decoded{}, err
	}
	body, err := scval.Parse(e.Value)
	if err != nil {
		return decoded{}, fmt.Errorf("%w: Withdrawal body parse: %w", ErrMalformedPayload, err)
	}
	vec, err := scval.AsVec(body)
	if err != nil {
		return decoded{}, fmt.Errorf("%w: Withdrawal body: %w", ErrMalformedPayload, err)
	}
	if len(vec) < 3 {
		return decoded{}, fmt.Errorf("%w: Withdrawal body not a 3-Vec (len=%d)", ErrMalformedPayload, len(vec))
	}
	token, err := scval.AsAddressStrkey(vec[0])
	if err != nil {
		return decoded{}, fmt.Errorf("%w: Withdrawal token: %w", ErrMalformedPayload, err)
	}
	recipient, err := scval.AsAddressStrkey(vec[1])
	if err != nil {
		return decoded{}, fmt.Errorf("%w: Withdrawal recipient: %w", ErrMalformedPayload, err)
	}
	amount, err := nonNegAmount(vec[2])
	if err != nil {
		return decoded{}, fmt.Errorf("%w: Withdrawal amount: %w", ErrMalformedPayload, err)
	}
	return decoded{
		CollateralContract: collateral,
		Asset:              token,
		Account:            recipient,
		Amount:             amount.String(),
		Attributes:         map[string]any{},
	}, nil
}

// decodeSupportedAssetAdded (config):
//
//	topics = [Symbol, Address(asset)]
//	data   = Vec[…config params…]
//
// Promotes the asset (topic[1]); the config vector is protocol-internal and kept verbatim in
// Attributes["body"].
func decodeSupportedAssetAdded(e *events.Event) (decoded, error) {
	if len(e.Topic) < 2 {
		return decoded{}, fmt.Errorf("%w: SupportedAssetAdded needs 2 topics, got %d", ErrMalformedPayload, len(e.Topic))
	}
	asset, err := addrTopic(e, "asset")
	if err != nil {
		return decoded{}, err
	}
	return decoded{
		Asset:      asset,
		Attributes: map[string]any{"body": e.Value},
	}, nil
}

// decodeConfigBody handles config events with no promoted column (BeaconUpdated,
// CollateralHashUpdated); the body is kept verbatim in Attributes["body"] and topic[0] is the only
// required topic. The error is always nil; the signature stays uniform so decodeOne's switch
// treats every arm identically.
//
//nolint:unparam // uniform decoder signature; see godoc.
func decodeConfigBody(e *events.Event) (decoded, error) {
	return decoded{
		Attributes: map[string]any{"body": e.Value},
	}, nil
}
