package canonical

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"strings"
)

// AssetType tags the shape of an Asset.
type AssetType string

const (
	// AssetNative is XLM, the native Stellar asset.
	AssetNative AssetType = "native"

	// AssetClassic is a classic Stellar asset with a code and issuer
	// G-address. Codes are 1-4 chars (AssetTypeCreditAlphanum4) or
	// 5-12 chars (AssetTypeCreditAlphanum12).
	AssetClassic AssetType = "classic"

	// AssetSoroban is a Soroban contract asset. Identity is the single C-address; any SEP-41
	// symbol()/decimals() is metadata, not identity.
	AssetSoroban AssetType = "soroban"

	// AssetFiat is an off-chain reference currency (USD, EUR, …).
	// NOT a Stellar asset. Wire form: `fiat:<ISO4217>`. See ADR-0010.
	AssetFiat AssetType = "fiat"

	// AssetCrypto is an off-chain crypto-ticker reference (BTC, ETH, USDT, …), keyed on a bare
	// global ticker as Reflector's CEX oracle publishes. NOT a Stellar asset. Wire form:
	// `crypto:<TICKER>`. See ADR-0014.
	AssetCrypto AssetType = "crypto"

	// AssetRWA is an off-chain tokenized real-world asset (treasuries, money-market funds, ETFs)
	// from RedStone's RWA push feeds. NOT a Stellar asset, and deliberately not AssetCrypto:
	// that would mis-feed every crypto-scoped surface. Wire form: `rwa:<CODE>`. See ADR-0028.
	AssetRWA AssetType = "rwa"

	// AssetOracleRaw is an oracle symbol recorded VERBATIM because it maps to no canonical asset.
	// Wire form: `raw:<symbol>`. Record-layer only: never a Pair leg, VWAP input or compared by
	// the interpretation layer (see [Asset.IsMapped]). Source scoping comes from
	// oracle_updates.source, not the code.
	AssetOracleRaw AssetType = "raw"
)

// Asset is a canonical identifier for any asset we price.
//
// The seven valid shapes:
//
//   - Native:  {Type: AssetNative}
//   - Classic: {Type: AssetClassic, Code: "USDC", Issuer: "G..."}
//   - Soroban: {Type: AssetSoroban, ContractID: "C..."}
//   - Fiat:    {Type: AssetFiat,   Code: "USD"}            ADR-0010
//   - Crypto:  {Type: AssetCrypto, Code: "BTC"}            ADR-0014
//   - RWA:     {Type: AssetRWA,    Code: "BENJI"}          ADR-0028
//   - Raw:     {Type: AssetOracleRaw, Code: "NOTACOIN"}    record-layer only
//
// A SAC-wrapped classic asset's canonical form is the classic one; the SAC address is metadata
// via [Asset.SacContractID]. The zero value is not a valid Asset; use [NativeAsset],
// [NewClassicAsset], or [NewSorobanAsset].
type Asset struct {
	Type AssetType `json:"type"`

	// Code is the 1-12 character asset code for classic assets.
	// Empty for native / soroban.
	Code string `json:"code,omitempty"`

	// Issuer is the G-address of the issuer for classic assets.
	// Empty for native / soroban.
	Issuer string `json:"issuer,omitempty"`

	// ContractID is the C-address for Soroban tokens (or for the
	// SAC bridge of a classic asset, when we use the Soroban form).
	// Empty for native / classic-only.
	ContractID string `json:"contract_id,omitempty"`
}

// NativeAsset returns the XLM singleton.
func NativeAsset() Asset {
	return Asset{Type: AssetNative}
}

// NewClassicAsset constructs a classic Stellar asset. Returns an
// error if code or issuer fail validation.
func NewClassicAsset(code, issuer string) (Asset, error) {
	if err := validateClassicAssetCode(code); err != nil {
		return Asset{}, err
	}
	if err := validateAccountID(issuer); err != nil {
		return Asset{}, err
	}
	return Asset{Type: AssetClassic, Code: code, Issuer: issuer}, nil
}

// ValidateAssetCode reports whether code is a valid classic asset code, the rule [AssetFromXDR]
// enforces. Exported so callers outside NewClassicAsset (e.g. internal/xdrjson) never mint a
// canonical-looking id for a code AssetFromXDR would reject.
func ValidateAssetCode(code string) error {
	return validateClassicAssetCode(code)
}

// validateClassicAssetCode enforces Stellar's alphanum4/alphanum12 rules (length 1–12, chars
// [a-zA-Z0-9] only), matching XDR ASSET_TYPE_CREDIT_ALPHANUM4 / ALPHANUM12 validity.
func validateClassicAssetCode(code string) error {
	l := len(code)
	if l == 0 || l > 12 {
		return fmt.Errorf("%w: code %q length %d (expected 1-12)",
			ErrInvalidAsset, code, l)
	}
	for i := 0; i < l; i++ {
		c := code[i]
		ok := (c >= 'a' && c <= 'z') ||
			(c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9')
		if !ok {
			return fmt.Errorf("%w: code %q contains non-alphanumeric byte %q",
				ErrInvalidAsset, code, c)
		}
	}
	return nil
}

// NewSorobanAsset constructs a Soroban contract-based asset.
func NewSorobanAsset(contractID string) (Asset, error) {
	if err := validateContractID(contractID); err != nil {
		return Asset{}, err
	}
	return Asset{Type: AssetSoroban, ContractID: contractID}, nil
}

// String returns the canonical wire identifier (docs/reference/api-design.md §3): "native",
// "<code>-<issuer>" (classic) or "<contract_id>" (soroban). [ParseAsset] also accepts
// `<code>:<issuer>` on input, but output is always dash-separated.
func (a Asset) String() string {
	switch a.Type {
	case AssetNative:
		return "native"
	case AssetClassic:
		return a.Code + "-" + a.Issuer
	case AssetSoroban:
		return a.ContractID
	case AssetFiat:
		return "fiat:" + a.Code
	case AssetCrypto:
		return "crypto:" + a.Code
	case AssetRWA:
		return "rwa:" + a.Code
	case AssetOracleRaw:
		return "raw:" + a.Code
	default:
		return "invalid-asset"
	}
}

// IsZero reports whether a is the zero Asset (never valid).
func (a Asset) IsZero() bool {
	return a.Type == "" && a.Code == "" && a.Issuer == "" && a.ContractID == ""
}

// Validate returns nil if a is one of the seven valid shapes (see the
// [Asset] doc: native / classic / soroban / fiat / crypto / rwa / raw);
// an error otherwise.
func (a Asset) Validate() error { //nolint:gocognit,gocyclo // dispatch-heavy; one case per AssetType, splitting would reduce linearity
	switch a.Type {
	case AssetNative:
		if a.Code != "" || a.Issuer != "" || a.ContractID != "" {
			return fmt.Errorf("%w: native asset must not carry code/issuer/contract_id", ErrInvalidAsset)
		}
		return nil
	case AssetClassic:
		if a.ContractID != "" {
			return fmt.Errorf("%w: classic asset must not carry contract_id", ErrInvalidAsset)
		}
		if err := validateClassicAssetCode(a.Code); err != nil {
			return err
		}
		return validateAccountID(a.Issuer)
	case AssetSoroban:
		if a.Code != "" || a.Issuer != "" {
			return fmt.Errorf("%w: soroban asset must not carry code/issuer", ErrInvalidAsset)
		}
		return validateContractID(a.ContractID)
	case AssetFiat:
		if a.Issuer != "" || a.ContractID != "" {
			return fmt.Errorf("%w: fiat asset must not carry issuer/contract_id", ErrInvalidAsset)
		}
		if !IsKnownFiat(a.Code) {
			return fmt.Errorf("%w: unknown fiat code %q (see ADR-0010)", ErrInvalidAsset, a.Code)
		}
		return nil
	case AssetCrypto:
		if a.Issuer != "" || a.ContractID != "" {
			return fmt.Errorf("%w: crypto asset must not carry issuer/contract_id", ErrInvalidAsset)
		}
		if !IsKnownCrypto(a.Code) {
			return fmt.Errorf("%w: unknown crypto code %q (see ADR-0014)", ErrInvalidAsset, a.Code)
		}
		return nil
	case AssetRWA:
		if a.Issuer != "" || a.ContractID != "" {
			return fmt.Errorf("%w: rwa asset must not carry issuer/contract_id", ErrInvalidAsset)
		}
		if !IsKnownRWA(a.Code) {
			return fmt.Errorf("%w: unknown rwa code %q (see ADR-0028)", ErrInvalidAsset, a.Code)
		}
		return nil
	case AssetOracleRaw:
		if a.Issuer != "" || a.ContractID != "" {
			return fmt.Errorf("%w: raw asset must not carry issuer/contract_id", ErrInvalidAsset)
		}
		return validateRawSymbol(a.Code)
	default:
		return fmt.Errorf("%w: unknown type %q", ErrInvalidAsset, a.Type)
	}
}

// Equal reports whether a and b name the same asset. A classic asset and its SAC-wrapped Soroban
// form are NOT equal: they are different canonical representations, bridged by the SAC registry.
func (a Asset) Equal(b Asset) bool {
	return a.Type == b.Type &&
		a.Code == b.Code &&
		a.Issuer == b.Issuer &&
		a.ContractID == b.ContractID
}

// ParseAsset is the inverse of String. Accepts all seven canonical
// forms (native / classic / soroban / fiat: / crypto: / rwa: / raw:)
// plus the `<code>:<issuer>` alias for classic assets and the case-
// insensitive "XLM" alias for native.
func ParseAsset(s string) (Asset, error) {
	if s == "" {
		return Asset{}, fmt.Errorf("%w: empty asset identifier", ErrInvalidAsset)
	}

	// Native
	if s == "native" {
		return NativeAsset(), nil
	}
	// "XLM" and any-case "native" are shorthand for native. Slug -> asset_id resolution
	// (USDC -> Circle) lives in the API's verified-currency catalogue, keeping ParseAsset pure.
	if strings.EqualFold(s, "XLM") || strings.EqualFold(s, "native") {
		return NativeAsset(), nil
	}

	// Fiat/crypto/RWA prefix dispatch. A G-strkey rest is never on those allow-lists, so it is the
	// classic `<code>:<issuer>` alias and falls through to the classic split below.
	for _, p := range []struct {
		prefix string
		ctor   func(string) (Asset, error)
	}{{"fiat:", NewFiatAsset}, {"crypto:", NewCryptoAsset}, {"rwa:", NewRWAAsset}} {
		if rest, ok := strings.CutPrefix(s, p.prefix); ok && !IsAccountID(rest) {
			return p.ctor(rest)
		}
	}

	// Raw oracle symbol prefix. MUST sit before the classic `<code>:<issuer>` split, which would
	// read `raw:BTC` as classic code "raw" + issuer "BTC". `raw:G…` stays raw (a G-strkey is a
	// valid raw symbol); a classic "raw" token round-trips through its `raw-G…` form.
	if rest, ok := strings.CutPrefix(s, "raw:"); ok {
		return NewOracleRawAsset(rest)
	}

	// Soroban — starts with C, 56 chars
	if IsContractID(s) {
		return NewSorobanAsset(s)
	}

	// Classic — "CODE-ISSUER" or "CODE:ISSUER"
	sep := strings.IndexAny(s, "-:")
	if sep > 0 && sep < len(s)-1 {
		code := s[:sep]
		issuer := s[sep+1:]
		return NewClassicAsset(code, issuer)
	}

	return Asset{}, fmt.Errorf("%w: %q does not match any known asset format", ErrInvalidAsset, s)
}

// ─── JSON ──────────────────────────────────────────────────────────

// MarshalJSON emits the canonical string form, not the struct: the API wire contract uses flat
// asset strings.
func (a Asset) MarshalJSON() ([]byte, error) {
	return json.Marshal(a.String())
}

// UnmarshalJSON accepts the canonical string form or the structured object form that storage
// layers keeping the fields split use.
func (a *Asset) UnmarshalJSON(b []byte) error {
	// Try string first
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		parsed, perr := ParseAsset(s)
		if perr != nil {
			return perr
		}
		*a = parsed
		return nil
	}

	// Structured form
	type raw Asset
	var r raw
	if err := json.Unmarshal(b, &r); err != nil {
		return fmt.Errorf("%w: must be string or object: %w", ErrInvalidAsset, err)
	}
	*a = Asset(r)
	return a.Validate()
}

// ─── database/sql ──────────────────────────────────────────────────

// Value implements driver.Valuer, emitting the canonical string form.
func (a Asset) Value() (driver.Value, error) {
	if err := a.Validate(); err != nil {
		return nil, err
	}
	return a.String(), nil
}

// Scan implements sql.Scanner.
//
// A SQL NULL is an ERROR, not a zero Asset: [Value] refuses anything failing [Validate], so a NULL
// is a schema/query defect (a missed LEFT JOIN), and a zero Asset would survive every Equal
// comparison. A legitimately nullable column must scan through sql.NullString and [ParseAsset].
func (a *Asset) Scan(src any) error {
	if src == nil {
		return fmt.Errorf("%w: cannot scan SQL NULL into an Asset "+
			"(scan a nullable asset column through sql.NullString and ParseAsset)", ErrInvalidAsset)
	}
	var s string
	switch v := src.(type) {
	case string:
		s = v
	case []byte:
		s = string(v)
	default:
		return fmt.Errorf("%w: cannot scan %T", ErrInvalidAsset, src)
	}
	parsed, err := ParseAsset(s)
	if err != nil {
		return err
	}
	*a = parsed
	return nil
}
