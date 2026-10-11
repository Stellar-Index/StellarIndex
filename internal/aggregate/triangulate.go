package aggregate

import (
	"errors"
	"math/big"
)

// ErrTriangulateZero is returned from [Triangulate] when either price is zero or negative.
var ErrTriangulateZero = errors.New("aggregate: triangulate requires positive input prices")

// Triangulate returns price(A to C) = price(A to B) * price(B to C) as a fresh *big.Rat; a zero or negative input returns [ErrTriangulateZero].
func Triangulate(aToB, bToC *big.Rat) (*big.Rat, error) {
	if aToB == nil || bToC == nil {
		return nil, ErrTriangulateZero
	}
	if aToB.Sign() <= 0 || bToC.Sign() <= 0 {
		return nil, ErrTriangulateZero
	}
	return new(big.Rat).Mul(aToB, bToC), nil
}

// TriangulateChain returns the product of a chain of direct prices (at least one, defensively copied); any zero or negative price returns [ErrTriangulateZero].
func TriangulateChain(prices ...*big.Rat) (*big.Rat, error) {
	if len(prices) == 0 {
		return nil, ErrTriangulateZero
	}
	out := new(big.Rat).SetInt64(1)
	for _, p := range prices {
		if p == nil || p.Sign() <= 0 {
			return nil, ErrTriangulateZero
		}
		out.Mul(out, p)
	}
	return out, nil
}
