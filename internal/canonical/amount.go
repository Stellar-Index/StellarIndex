package canonical

import (
	"bytes"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"math/big"
)

// Amount is a precision-safe *big.Int wrapper for every token quantity, reserve, price and supply: full 128-bit+ precision, never int64/float64, decimal string in JSON, zero value is zero, callers must not mutate it.
type Amount struct {
	// value is never nil after a successful construction. Helpers
	// below guard against nil by constructing on demand.
	value *big.Int

	// noCompare makes `a == b` a compile error: it would otherwise compare *big.Int addresses, so FromString("100") == FromString("100")
	// is false and an `amt == (Amount{})` guard would misclassify a decoded zero as present. Use Cmp or Equal. Zero-width, never read.
	noCompare [0]func() //nolint:unused // the field exists to make Amount non-comparable, not to be read
}

// Equal reports whether two Amounts hold the same numeric value.
func (a Amount) Equal(b Amount) bool { return a.Cmp(b) == 0 }

// NewAmount wraps a *big.Int; Soroban decoders should prefer [FromInt128Parts] / [FromUInt128Parts].
func NewAmount(v *big.Int) Amount {
	if v == nil {
		return Amount{value: new(big.Int)}
	}
	// Copy to prevent shared mutation of the caller's *big.Int.
	return Amount{value: new(big.Int).Set(v)}
}

// FromInt128Parts reconstructs a signed 128-bit integer from its Soroban-XDR hi (signed) and lo (unsigned) words; the reference path for any i128.
func FromInt128Parts(hi int64, lo uint64) Amount {
	// big.Int arithmetic rather than bit-shifting keeps two's-complement sign propagation correct.
	h := big.NewInt(hi)
	h.Lsh(h, 64)
	l := new(big.Int).SetUint64(lo)
	return Amount{value: new(big.Int).Add(h, l)}
}

// FromUInt128Parts reconstructs an unsigned 128-bit integer from its Soroban-XDR hi/lo words.
func FromUInt128Parts(hi, lo uint64) Amount {
	h := new(big.Int).SetUint64(hi)
	h.Lsh(h, 64)
	l := new(big.Int).SetUint64(lo)
	return Amount{value: new(big.Int).Add(h, l)}
}

// FromUInt256Parts reconstructs an unsigned 256-bit integer from its Soroban-XDR 4xuint64 form, big-endian (hiHi top, loLo bottom).
func FromUInt256Parts(hiHi, hiLo, loHi, loLo uint64) Amount {
	r := new(big.Int).SetUint64(hiHi)
	r.Lsh(r, 64)
	r.Add(r, new(big.Int).SetUint64(hiLo))
	r.Lsh(r, 64)
	r.Add(r, new(big.Int).SetUint64(loHi))
	r.Lsh(r, 64)
	r.Add(r, new(big.Int).SetUint64(loLo))
	return Amount{value: r}
}

// MaxAmountStringLen caps the digits accepted by [FromString], [Amount.UnmarshalJSON] and [Amount.Scan] (far past any i128) so an untrusted string cannot force a huge allocation.
const MaxAmountStringLen = 512

// FromString parses a decimal string (no scientific notation); empty is zero, over [MaxAmountStringLen] digits is rejected.
func FromString(s string) (Amount, error) {
	if s == "" {
		return Amount{value: new(big.Int)}, nil
	}
	if len(s) > MaxAmountStringLen {
		return Amount{}, fmt.Errorf("canonical: amount string length %d exceeds cap %d: %w",
			len(s), MaxAmountStringLen, ErrInvalidAmount)
	}
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return Amount{}, fmt.Errorf("canonical: %q is not a valid decimal integer: %w", s, ErrInvalidAmount)
	}
	return Amount{value: v}, nil
}

// BigInt returns a copy of the underlying *big.Int, safe to mutate.
func (a Amount) BigInt() *big.Int {
	if a.value == nil {
		return new(big.Int)
	}
	return new(big.Int).Set(a.value)
}

// String returns the decimal representation, which is also the JSON wire format.
func (a Amount) String() string {
	if a.value == nil {
		return "0"
	}
	return a.value.String()
}

// IsZero reports whether the amount is zero.
func (a Amount) IsZero() bool {
	return a.value == nil || a.value.Sign() == 0
}

// Sign returns -1, 0, or +1 depending on the amount's sign.
func (a Amount) Sign() int {
	if a.value == nil {
		return 0
	}
	return a.value.Sign()
}

// Cmp compares a and b with (*big.Int).Cmp semantics.
func (a Amount) Cmp(b Amount) int {
	return a.BigInt().Cmp(b.BigInt())
}

// ─── JSON ─────────────────────────────────────────────────────────

// MarshalJSON serialises as a JSON string, never a number: clients parse JSON numbers as IEEE 754 doubles and lose precision above 2^53.
func (a Amount) MarshalJSON() ([]byte, error) {
	return json.Marshal(a.String())
}

// UnmarshalJSON accepts a JSON string (preferred) or number; `null` is an ERROR, not zero, so an absent field must be typed *Amount.
func (a *Amount) UnmarshalJSON(b []byte) error {
	if string(bytes.TrimSpace(b)) == "null" {
		return fmt.Errorf("canonical: cannot unmarshal JSON null into an Amount "+
			"(use *Amount for a field that may legitimately be absent): %w", ErrInvalidAmount)
	}
	// Try string first.
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		v, err := FromString(s)
		if err != nil {
			return err
		}
		*a = v
		return nil
	}
	// json.Number is lossless for integer-valued numerics.
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("canonical: Amount must be a JSON string or number: %w", ErrInvalidAmount)
	}
	v, err := FromString(n.String())
	if err != nil {
		return err
	}
	*a = v
	return nil
}

// ─── database/sql ────────────────────────────────────────────────

// Value implements driver.Valuer, handing Postgres NUMERIC an exact decimal string.
func (a Amount) Value() (driver.Value, error) {
	return a.String(), nil
}

// Scan implements sql.Scanner for a Postgres NUMERIC string or []byte; SQL NULL is an ERROR, not zero (scan nullable columns via sql.NullString and [FromString]).
func (a *Amount) Scan(src any) error {
	if src == nil {
		return fmt.Errorf("canonical: cannot scan SQL NULL into an Amount "+
			"(scan a nullable NUMERIC column through sql.NullString and FromString): %w", ErrInvalidAmount)
	}
	switch v := src.(type) {
	case string:
		parsed, err := FromString(v)
		if err != nil {
			return err
		}
		*a = parsed
		return nil
	case []byte:
		parsed, err := FromString(string(v))
		if err != nil {
			return err
		}
		*a = parsed
		return nil
	case int64:
		*a = Amount{value: big.NewInt(v)}
		return nil
	default:
		return fmt.Errorf("canonical: cannot scan %T into Amount: %w", src, ErrInvalidAmount)
	}
}
