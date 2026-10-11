// Package scval wraps github.com/stellar/go-stellar-sdk/xdr with the narrow set of SCVal
// primitives the source connectors need: base64 parsing, typed accessors, and a Map-by-field-name
// lookup that enforces the "decode-by-name-not-position" rule from
// docs/architecture/ingest-pipeline.md#contract-schema-evolution.
//
// This is the only package allowed to import .../go-stellar-sdk/xdr directly, so an SDK swap
// changes one package, not twenty.
//
// Errors wrap a package-local sentinel: errors.Is against [ErrScValType] separates a wrong XDR
// shape (bug or malicious input) from [ErrScValDecode] (truncation or corruption).
package scval

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Re-exports: connectors use these aliases so they never import the SDK xdr package directly.
type (
	ScVal      = xdr.ScVal
	ScMapEntry = xdr.ScMapEntry
)

// Sentinel errors. Wrap (do not replace) when adding context.
var (
	// ErrScValDecode wraps an XDR or base64 decode failure (truncation, bad encoding), which usually
	// means upstream corruption rather than a schema mismatch.
	ErrScValDecode = errors.New("scval: decode failed")

	// ErrScValType wraps a wrong-SCVal-kind assertion (expected Symbol, got I128), which usually means
	// a contract schema change or a decoder expecting the wrong shape.
	ErrScValType = errors.New("scval: unexpected SCVal type")

	// ErrScValMissingKey means a map lookup by field name found no entry. It is distinct from a wrong
	// type so callers can gate on schema evolution (old WASM emits field X, new WASM omits it).
	ErrScValMissingKey = errors.New("scval: map missing expected field")
)

// Parse base64-decodes data and XDR-unmarshals it into an ScVal. Failures wrap [ErrScValDecode] so
// callers can tell wire problems from schema problems.
func Parse(b64 string) (xdr.ScVal, error) {
	var sv xdr.ScVal
	if err := UnmarshalBase64(b64, &sv); err != nil {
		return xdr.ScVal{}, fmt.Errorf("%w: %w", ErrScValDecode, err)
	}
	return sv, nil
}

// UnmarshalBase64 base64-decodes b64 and XDR-decodes exactly one value into dest. Prefer it to
// xdr.SafeUnmarshalBase64, whose consumed-length check counts base64 characters and so misses
// trailing bytes in the final quantum.
func UnmarshalBase64(b64 string, dest any) error {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return err
	}
	return xdr.SafeUnmarshal(raw, dest)
}

// ParseBytes is the raw-bytes twin of [Parse] for a Bytes-wrapped XDR struct (call [AsBytes] first).
// Like Parse, it rejects trailing bytes.
func ParseBytes(raw []byte) (xdr.ScVal, error) {
	var sv xdr.ScVal
	if err := xdr.SafeUnmarshal(raw, &sv); err != nil {
		return xdr.ScVal{}, fmt.Errorf("%w: %w", ErrScValDecode, err)
	}
	return sv, nil
}

// EncodeArgsAsScVec wraps base64-encoded ScVal blobs ([events.Event.OpArgs]) into one XDR-marshalled
// ScVal::Vec, the form the soroban_events sink stores so decoders reuse [Parse] / [AsVec]. Empty
// input returns nil, nil (SQL NULL); a parse failure means wire corruption, since the dispatcher
// produced the args.
func EncodeArgsAsScVec(b64Args []string) ([]byte, error) {
	if len(b64Args) == 0 {
		return nil, nil
	}
	vec := make(xdr.ScVec, 0, len(b64Args))
	for i, a := range b64Args {
		sv, err := Parse(a)
		if err != nil {
			return nil, fmt.Errorf("arg[%d]: %w", i, err)
		}
		vec = append(vec, sv)
	}
	pp := &vec
	sv := xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &pp}
	return sv.MarshalBinary()
}

// DecodeScVecToArgs is the inverse of [EncodeArgsAsScVec]: it turns the `soroban_events.op_args_xdr`
// bytes back into the base64 blobs [events.Event.OpArgs] expects, for replay paths. Empty input (NULL)
// returns nil, nil; bytes that are not a ScVal::Vec mean a corrupt row.
func DecodeScVecToArgs(b []byte) ([]string, error) {
	if len(b) == 0 {
		return nil, nil
	}
	sv, err := ParseBytes(b)
	if err != nil {
		return nil, fmt.Errorf("unmarshal scval: %w", err)
	}
	if sv.Type != xdr.ScValTypeScvVec {
		return nil, fmt.Errorf("decode: expected ScVal::Vec, got %v", sv.Type)
	}
	if sv.Vec == nil || *sv.Vec == nil {
		return nil, nil
	}
	vec := **sv.Vec
	out := make([]string, 0, len(vec))
	for _, elem := range vec {
		blob, err := elem.MarshalBinary()
		if err != nil {
			return nil, fmt.Errorf("marshal elem: %w", err)
		}
		out = append(out, base64.StdEncoding.EncodeToString(blob))
	}
	return out, nil
}

// MustEncodeSymbol returns the base64 SCVal::Symbol(s) blob for byte-comparison against Event.Topic.
// It panics on an empty, non-ASCII or over-long (SorobanObjectSizeLimit) string, so call it only with
// compile-time constants, never network data.
func MustEncodeSymbol(s string) string {
	b64, err := EncodeSymbol(s)
	if err != nil {
		panic("scval.MustEncodeSymbol: " + err.Error())
	}
	return b64
}

// MustEncodeString returns the base64 SCVal::String(s) blob. Some contracts emit topic[0] as a String,
// not a Symbol (Soroswap's "SoroswapPair"); String has no identifier restriction and no length cap
// beyond SorobanObjectSizeLimit. It panics on marshal error, so use compile-time constants only.
func MustEncodeString(s string) string {
	b64, err := EncodeString(s)
	if err != nil {
		panic("scval.MustEncodeString: " + err.Error())
	}
	return b64
}

// EncodeString is the non-panicking form of [MustEncodeString].
func EncodeString(s string) (string, error) {
	sv := xdr.ScVal{Type: xdr.ScValTypeScvString}
	str := xdr.ScString(s)
	sv.Str = &str
	b, err := sv.MarshalBinary()
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrScValDecode, err)
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// AsString returns the String value of sv, wrapping ErrScValType if sv is not a String. Unlike
// Symbols, Strings are arbitrary bytes: a contract chooses them, so they may hold a NUL or invalid
// UTF-8. Compare or hash freely, but never bind it to a Postgres `text` column (SQLSTATE 22021, a
// permanent data error); persisted text goes through [AsText].
func AsString(sv xdr.ScVal) (string, error) {
	if sv.Type != xdr.ScValTypeScvString {
		return "", fmt.Errorf("%w: want String, got %s", ErrScValType, sv.Type.String())
	}
	return string(*sv.Str), nil
}

// TextHexPrefix marks a [ToText] value carrying its bytes as hex. It is Postgres's bytea hex marker,
// so SQL can decode it with `decode(substr(col, 3), 'hex')`.
const TextHexPrefix = `\x`

// AsText is [AsString] for a value persisted as text: the [ToText] form, which a Postgres `text`
// column always accepts and from which [FromText] recovers the exact on-chain bytes.
func AsText(sv xdr.ScVal) (string, error) {
	raw, err := AsString(sv)
	if err != nil {
		return "", err
	}
	return ToText(raw), nil
}

// ToText maps arbitrary contract-supplied bytes onto a string that is valid UTF-8 without a NUL
// (what a Postgres `text` value requires) without losing a byte:
//
//   - raw that is already valid NUL-free UTF-8 and does not begin with [TextHexPrefix] is
//     returned unchanged, so re-deriving an existing row writes the same text;
//   - anything else is returned as [TextHexPrefix] + lowercase hex of every byte of raw.
//
// A clean value that itself begins with the prefix takes the hex arm too. That keeps the mapping
// injective, so [FromText] is unambiguous, and is why ToText must be applied exactly once, where
// bytes become text: applying it to its own output re-encodes it.
//
// It is pure and total. Validity is utf8.ValidString, which rejects what the server's UTF8
// verifier rejects (overlong forms, encoded surrogates, values above U+10FFFF).
func ToText(raw string) string {
	if utf8.ValidString(raw) && !strings.Contains(raw, "\x00") && !strings.HasPrefix(raw, TextHexPrefix) {
		return raw
	}
	return TextHexPrefix + hex.EncodeToString([]byte(raw))
}

// FromText is the inverse of [ToText]. A value without [TextHexPrefix] is its own bytes; a prefixed
// value whose remainder is not hex was not produced by ToText and is [ErrScValDecode].
func FromText(text string) ([]byte, error) {
	rest, ok := strings.CutPrefix(text, TextHexPrefix)
	if !ok {
		return []byte(text), nil
	}
	raw, err := hex.DecodeString(rest)
	if err != nil {
		return nil, fmt.Errorf("%w: text value has the hex prefix but is not hex: %w", ErrScValDecode, err)
	}
	return raw, nil
}

// EncodeSymbol is the non-panicking form of [MustEncodeSymbol], for runtime-supplied symbols.
func EncodeSymbol(s string) (string, error) {
	// The runtime enforces Symbol bounds (1..=32 bytes, ASCII alphanumeric + underscore); mirror them
	// to fail fast at encode time.
	if len(s) == 0 || len(s) > 32 {
		return "", fmt.Errorf("scval: symbol length %d out of range [1,32]", len(s))
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := (c >= 'a' && c <= 'z') ||
			(c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') ||
			c == '_'
		if !ok {
			return "", fmt.Errorf("scval: symbol contains non-ASCII-identifier byte 0x%02x", c)
		}
	}
	sv := xdr.ScVal{Type: xdr.ScValTypeScvSymbol}
	sym := xdr.ScSymbol(s)
	sv.Sym = &sym
	b, err := sv.MarshalBinary()
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrScValDecode, err)
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// AsSymbol returns the Symbol string from sv, wrapping ErrScValType if sv is not a Symbol. Match
// against constants via MustEncodeSymbol for byte-level equality on the wire.
func AsSymbol(sv xdr.ScVal) (string, error) {
	if sv.Type != xdr.ScValTypeScvSymbol {
		return "", fmt.Errorf("%w: want Symbol, got %s", ErrScValType, sv.Type.String())
	}
	return string(*sv.Sym), nil
}

// AsU64 returns the uint64 value from sv. Soroban event timestamps (e.g. Reflector's topic[2]) are
// `u64`, not `Timepoint`, so this is the right accessor for them.
func AsU64(sv xdr.ScVal) (uint64, error) {
	if sv.Type != xdr.ScValTypeScvU64 {
		return 0, fmt.Errorf("%w: want U64, got %s", ErrScValType, sv.Type.String())
	}
	return uint64(*sv.U64), nil
}

// AsU32 returns the uint32 value from sv, e.g. a view-function result like all_pairs_length().
func AsU32(sv xdr.ScVal) (uint32, error) {
	if sv.Type != xdr.ScValTypeScvU32 {
		return 0, fmt.Errorf("%w: want U32, got %s", ErrScValType, sv.Type.String())
	}
	return uint32(*sv.U32), nil
}

// AsI32 returns the int32 value from sv, for signed 32-bit event body fields (Aquarius gauge indices).
func AsI32(sv xdr.ScVal) (int32, error) {
	if sv.Type != xdr.ScValTypeScvI32 {
		return 0, fmt.Errorf("%w: want I32, got %s", ErrScValType, sv.Type.String())
	}
	return int32(*sv.I32), nil
}

// AsBytes returns the raw []byte value from sv, for event bodies that are a Bytes-wrapped
// XDR-encoded struct (e.g. Redstone) rather than a Map.
func AsBytes(sv xdr.ScVal) ([]byte, error) {
	if sv.Type != xdr.ScValTypeScvBytes {
		return nil, fmt.Errorf("%w: want Bytes, got %s", ErrScValType, sv.Type.String())
	}
	return []byte(*sv.Bytes), nil
}

// AsBool returns the bool value from sv, wrapping ErrScValType if sv is not a Bool.
func AsBool(sv xdr.ScVal) (bool, error) {
	if sv.Type != xdr.ScValTypeScvBool {
		return false, fmt.Errorf("%w: want Bool, got %s", ErrScValType, sv.Type.String())
	}
	if sv.B == nil {
		// ScValTypeScvBool == 0, so a zero-value or malformed ScVal reports Type==ScvBool with a nil
		// B pointer. Fail the accessor rather than panic the decode goroutine.
		return false, fmt.Errorf("%w: Bool arm has nil value (malformed or zero ScVal)", ErrScValType)
	}
	return bool(*sv.B), nil
}

// IsVoid reports whether sv is ScvVoid, the empty body some bare marker events use.
func IsVoid(sv xdr.ScVal) bool {
	return sv.Type == xdr.ScValTypeScvVoid
}

// NewU32 builds an ScVal wrapping a uint32 for use as a contract-invocation argument. Unlike the
// Encode* helpers it returns the ScVal itself, not its base64 wire form.
func NewU32(v uint32) xdr.ScVal {
	u := xdr.Uint32(v)
	return xdr.ScVal{Type: xdr.ScValTypeScvU32, U32: &u}
}

// AsAmountFromI128 converts sv's I128 parts to canonical.Amount, keeping the full signed 128-bit
// range; truncating to int64(parts.Lo) would drop the hi 64 bits and silently mis-report large values.
func AsAmountFromI128(sv xdr.ScVal) (canonical.Amount, error) {
	if sv.Type != xdr.ScValTypeScvI128 {
		return canonical.Amount{}, fmt.Errorf("%w: want I128, got %s", ErrScValType, sv.Type.String())
	}
	p := *sv.I128
	return canonical.FromInt128Parts(int64(p.Hi), uint64(p.Lo)), nil
}

// AsAmountFromU128 is the unsigned twin of AsAmountFromI128; pick by the contract schema, since
// amounts are usually i128 but reserves and supplies sometimes u128.
func AsAmountFromU128(sv xdr.ScVal) (canonical.Amount, error) {
	if sv.Type != xdr.ScValTypeScvU128 {
		return canonical.Amount{}, fmt.Errorf("%w: want U128, got %s", ErrScValType, sv.Type.String())
	}
	p := *sv.U128
	return canonical.FromUInt128Parts(uint64(p.Hi), uint64(p.Lo)), nil
}

// AsAmountFromU256 decodes a full 256-bit unsigned value (Redstone's PriceData.price needs it). The
// four 64-bit words compose big-endian, HiHi most significant.
func AsAmountFromU256(sv xdr.ScVal) (canonical.Amount, error) {
	if sv.Type != xdr.ScValTypeScvU256 {
		return canonical.Amount{}, fmt.Errorf("%w: want U256, got %s", ErrScValType, sv.Type.String())
	}
	p := *sv.U256
	return canonical.FromUInt256Parts(
		uint64(p.HiHi), uint64(p.HiLo),
		uint64(p.LoHi), uint64(p.LoLo),
	), nil
}

// AsAddressStrkey returns the strkey form of an ScVal::Address (G/C/M/B/L), with validation
// delegated to the SDK's strkey package so a malformed address never reaches the database.
//
// CAP-67 grew ScAddress from two variants to five (Muxed, Claimable Balance, Liquidity Pool). A
// SEP-41 transfer `to` can carry any of them; a two-variant decoder fails with `unknown ScAddress
// type 4` and silently drops the event.
//
// Strkey payload shapes, pinned by the SDK's strkey/decode_test.go:
//   - Muxed (M-…): 40 bytes = 32-byte ed25519 || 8-byte big-endian id
//   - Claimable Balance (B-…): 33 bytes = 1-byte type prefix (0x00 = V0)
//     || 32-byte hash
//   - Liquidity Pool (L-…): 32 bytes = PoolId hash directly
func AsAddressStrkey(sv xdr.ScVal) (string, error) {
	if sv.Type != xdr.ScValTypeScvAddress {
		return "", fmt.Errorf("%w: want Address, got %s", ErrScValType, sv.Type.String())
	}
	addr := *sv.Address
	switch addr.Type {
	case xdr.ScAddressTypeScAddressTypeAccount:
		raw := addr.MustAccountId().Ed25519
		return strkey.Encode(strkey.VersionByteAccountID, raw[:])
	case xdr.ScAddressTypeScAddressTypeContract:
		raw := addr.MustContractId()
		return strkey.Encode(strkey.VersionByteContract, raw[:])
	case xdr.ScAddressTypeScAddressTypeMuxedAccount:
		m := addr.MustMuxedAccount()
		raw := make([]byte, 40)
		copy(raw, m.Ed25519[:])
		binary.BigEndian.PutUint64(raw[32:], uint64(m.Id))
		return strkey.Encode(strkey.VersionByteMuxedAccount, raw)
	case xdr.ScAddressTypeScAddressTypeClaimableBalance:
		cb := addr.MustClaimableBalanceId()
		if cb.V0 == nil {
			return "", fmt.Errorf("%w: claimable balance V0 hash is nil", ErrScValType)
		}
		raw := make([]byte, 33)
		raw[0] = byte(cb.Type) // V0 = 0
		copy(raw[1:], cb.V0[:])
		return strkey.Encode(strkey.VersionByteClaimableBalance, raw)
	case xdr.ScAddressTypeScAddressTypeLiquidityPool:
		lp := addr.MustLiquidityPoolId()
		return strkey.Encode(strkey.VersionByteLiquidityPool, lp[:])
	default:
		return "", fmt.Errorf("%w: unknown ScAddress type %d", ErrScValType, addr.Type)
	}
}

// AsAddressOrVoid decodes sv as an Address strkey or ScvVoid (returning ""). Some admin events carry
// a field that is Void the first time they fire (an "old admin" before any was set), so callers must
// type-test rather than assume Address.
func AsAddressOrVoid(sv xdr.ScVal) (string, error) {
	if sv.Type == xdr.ScValTypeScvVoid {
		return "", nil
	}
	return AsAddressStrkey(sv)
}

// AsVec returns the elements of an ScVal::Vec. An empty but present Vec returns an empty non-nil
// slice, so every caller sees the same shape without a nil-check.
func AsVec(sv xdr.ScVal) ([]xdr.ScVal, error) {
	if sv.Type != xdr.ScValTypeScvVec {
		return nil, fmt.Errorf("%w: want Vec, got %s", ErrScValType, sv.Type.String())
	}
	vec := *sv.Vec
	if vec == nil {
		return []xdr.ScVal{}, nil
	}
	return []xdr.ScVal(*vec), nil
}

// AsMap returns the entries of an ScVal::Map. Like AsVec, a present but nil map yields an empty slice.
func AsMap(sv xdr.ScVal) ([]xdr.ScMapEntry, error) {
	if sv.Type != xdr.ScValTypeScvMap {
		return nil, fmt.Errorf("%w: want Map, got %s", ErrScValType, sv.Type.String())
	}
	m := *sv.Map
	if m == nil {
		return []xdr.ScMapEntry{}, nil
	}
	return []xdr.ScMapEntry(*m), nil
}

// MapField looks up a map entry whose key is Symbol(key), returning the value and true on hit. Use
// [MustMapField] where a miss is a schema violation.
//
// This is the canonical decode-by-field-name entry point: new contract versions may add, reorder or
// remove fields (docs/architecture/ingest-pipeline.md#contract-schema-evolution).
func MapField(entries []xdr.ScMapEntry, key string) (xdr.ScVal, bool) {
	for i := range entries {
		if entries[i].Key.Type != xdr.ScValTypeScvSymbol {
			continue
		}
		if string(*entries[i].Key.Sym) == key {
			return entries[i].Val, true
		}
	}
	return xdr.ScVal{}, false
}

// MustMapField is MapField with a strict miss: an absent key returns an ErrScValMissingKey error.
func MustMapField(entries []xdr.ScMapEntry, key string) (xdr.ScVal, error) {
	v, ok := MapField(entries, key)
	if !ok {
		return xdr.ScVal{}, fmt.Errorf("%w: %q", ErrScValMissingKey, key)
	}
	return v, nil
}

// AsTupleN asserts sv is a Vec of exactly n elements and returns them; Soroban represents a tuple
// (a, b) as Vec[a, b].
func AsTupleN(sv xdr.ScVal, n int) ([]xdr.ScVal, error) {
	elts, err := AsVec(sv)
	if err != nil {
		return nil, err
	}
	if len(elts) != n {
		return nil, fmt.Errorf("%w: want %d-tuple (Vec), got Vec of length %d", ErrScValType, n, len(elts))
	}
	return elts, nil
}

// AddressOrSymbol is the outcome of DecodeAddressOrSymbol: exactly one of .Address (strkey) or
// .Symbol is non-empty.
type AddressOrSymbol struct {
	Address string // non-empty when sv is ScAddress (strkey encoded)
	Symbol  string // non-empty when sv is ScSymbol
}

// DecodeAddressOrSymbol returns {Address} for an ScAddress, {Symbol} for an ScSymbol and ErrScValType
// otherwise, for the Address-or-Symbol unions common in oracle payloads.
func DecodeAddressOrSymbol(sv xdr.ScVal) (AddressOrSymbol, error) {
	switch sv.Type {
	case xdr.ScValTypeScvAddress:
		addr, err := AsAddressStrkey(sv)
		if err != nil {
			return AddressOrSymbol{}, err
		}
		return AddressOrSymbol{Address: addr}, nil
	case xdr.ScValTypeScvSymbol:
		sym, err := AsSymbol(sv)
		if err != nil {
			return AddressOrSymbol{}, err
		}
		return AddressOrSymbol{Symbol: sym}, nil
	default:
		return AddressOrSymbol{}, fmt.Errorf("%w: want Address or Symbol, got %s",
			ErrScValType, sv.Type.String())
	}
}
