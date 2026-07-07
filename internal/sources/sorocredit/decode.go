package sorocredit

import (
	"fmt"
	"strings"
	"time"

	"github.com/StellarIndex/stellar-index/internal/events"
	"github.com/StellarIndex/stellar-index/internal/scval"
)

// collateralNamePrefix is the "Collateral-" literal that prefixes the
// per-position child-contract name in a NewCollateralContract body. The
// UUID after it is the stable position identity that StatementPublished
// / Liquidation reference by `position_uuid`.
const collateralNamePrefix = "Collateral-"

// classify maps topic[0] to the decoder-side [EventType], or "" for an
// event this source doesn't track. Byte-equality against the pre-encoded
// symbols (events.go) — no per-event SCVal re-decode. Contract-identity
// filtering happens in the dispatcher adapter's Matches, NOT here: these
// symbols are also emitted by two look-alike contracts, so classify
// alone never decides a match.
func classify(e *events.Event) EventType {
	if len(e.Topic) == 0 {
		return ""
	}
	switch e.Topic[0] {
	case topicSymNewCollateralContract:
		return TypeNewCollateralContract
	case topicSymStatementPublished:
		return TypeStatement
	case topicSymLiquidation:
		// On-wire "Liquidation" → our SCHEDULED-SETTLEMENT type.
		return TypeSettlement
	case topicSymWithdrawal:
		return TypeWithdrawal
	case topicSymBeaconUpdated:
		return TypeBeaconUpdated
	case topicSymSupportedAssetAdded:
		return TypeSupportedAssetAdded
	case topicSymCollateralHashUpdated:
		return TypeCollateralHashUpdated
	default:
		return ""
	}
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

func strTopic(e *events.Event, i int, field string) (string, error) {
	sv, err := scval.Parse(e.Topic[i])
	if err != nil {
		return "", fmt.Errorf("%w: %s topic[%d] parse: %w", ErrMalformedPayload, field, i, err)
	}
	s, err := scval.AsString(sv)
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
// topic[1] is the newly-deployed child contract's C-address — the value
// the childgate seeds. The body's String is the child's symbolic name;
// the UUID after "Collateral-" is the position identity that statements
// and settlements reference.
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
	name, err := scval.AsString(vec[0])
	if err != nil {
		return decoded{}, fmt.Errorf("%w: NewCollateralContract name: %w", ErrMalformedPayload, err)
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
func decodeStatement(e *events.Event) (decoded, error) {
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
	amount, err := scval.AsAmountFromI128(vec[0])
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
	ts := time.Unix(int64(tsUnix), 0).UTC() //nolint:gosec // u64 unix seconds; contract-emitted, in range.
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
// SCHEDULED SETTLEMENT (see the package doc — it is NOT a distressed
// liquidation).
//
//	topics = [Symbol("Liquidation"), Address(collateral_contract),
//	          String(position_uuid), String(statement_uuid)]
//	data   = Vec[ Address(settler/keeper), Vec[Address](debt_assets),
//	              Vec[i128](amounts), … protocol-internal trailing fields ]
//
// The settler (data[0]) is the single recurring keeper. debt_asset +
// settled_amount are the FIRST element of the parallel debt-asset /
// amount vectors (USDC-only book — one element in practice). The full
// body — every trailing field — is captured in Attributes["body"] so
// nothing is dropped even though only the primary leg is promoted.
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
	if len(vec) < 3 {
		return decoded{}, fmt.Errorf("%w: Liquidation body not a >=3-Vec (len=%d)", ErrMalformedPayload, len(vec))
	}
	settler, err := scval.AsAddressStrkey(vec[0])
	if err != nil {
		return decoded{}, fmt.Errorf("%w: Liquidation settler: %w", ErrMalformedPayload, err)
	}
	attrs := map[string]any{"body": scval.DisplayB64(e.Value)}
	// Primary debt-asset leg: data[1] is Vec[Address], data[2] is
	// Vec[i128]. Promote the first of each; a shape mismatch degrades
	// into an attribute note rather than failing the whole row. Nested
	// Vec elements are kept in := inferred locals and fed straight back
	// into scval.As* — this file never NAMES the xdr type (ADR-0013).
	var asset, amount string
	if assets, verr := scval.AsVec(vec[1]); verr == nil && len(assets) > 0 {
		if a, aerr := scval.AsAddressStrkey(assets[0]); aerr == nil {
			asset = a
		} else {
			attrs["debt_asset_error"] = aerr.Error()
		}
	} else {
		attrs["debt_asset_error"] = "not a non-empty Vec"
	}
	if amounts, verr := scval.AsVec(vec[2]); verr == nil && len(amounts) > 0 {
		if amt, aerr := scval.AsAmountFromI128(amounts[0]); aerr == nil {
			amount = amt.String()
		} else {
			attrs["settled_amount_error"] = aerr.Error()
		}
	} else {
		attrs["settled_amount_error"] = "not a non-empty Vec"
	}
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
	amount, err := scval.AsAmountFromI128(vec[2])
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
// Promotes the asset (topic[1]); the config vector's semantics are
// protocol-internal, captured verbatim into Attributes["body"].
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
		Attributes: map[string]any{"body": scval.DisplayB64(e.Value)},
	}, nil
}

// decodeConfigBody handles the config events that carry no promoted
// column (BeaconUpdated, CollateralHashUpdated) — the body is captured
// verbatim into Attributes["body"]. topic[0] is the only required topic.
// The error return is always nil today, but the (decoded, error)
// signature keeps it uniform with the other per-event decoders so
// decodeOne's dispatch switch treats every arm identically.
//
//nolint:unparam // uniform decoder signature; see godoc.
func decodeConfigBody(e *events.Event) (decoded, error) {
	return decoded{
		Attributes: map[string]any{"body": scval.DisplayB64(e.Value)},
	}, nil
}
