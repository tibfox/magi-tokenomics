package sharecore

import "math/big"

// PowRational computes floor(x^(num/den)) using INTEGER arithmetic only.
//
// SCOT expresses its reward curves as a decimal exponent in [1,2] (e.g. 1.75).
// Evaluating that with float64 would make the result depend on rounding mode and
// expression order, so two honest reporters could disagree in the last digit and
// their Attest pages would never match. Instead we treat the exponent as an exact
// rational num/den and compute nthRoot(x^num, den) with big.Int.
//
// Requires num >= 1, den >= 1. x must be non-negative.
func PowRational(x *big.Int, num, den int) *big.Int {
	if x == nil || x.Sign() <= 0 {
		return new(big.Int)
	}
	if num < 1 || den < 1 {
		panic("sharecore: exponent must be a positive rational")
	}
	// Fast path: whole exponent (covers linear=1/1 and quadratic=2/1).
	if den == 1 {
		return new(big.Int).Exp(x, big.NewInt(int64(num)), nil)
	}
	p := new(big.Int).Exp(x, big.NewInt(int64(num)), nil)
	return IntNthRoot(p, den)
}

// IntNthRoot returns floor(x^(1/n)) for x >= 0, n >= 1, via Newton's method on
// big.Int. Fully deterministic: integer iteration with a fixed termination rule.
func IntNthRoot(x *big.Int, n int) *big.Int {
	if n < 1 {
		panic("sharecore: root degree must be >= 1")
	}
	if x == nil || x.Sign() < 0 {
		panic("sharecore: nth root of a negative number")
	}
	if x.Sign() == 0 {
		return new(big.Int)
	}
	if n == 1 {
		return new(big.Int).Set(x)
	}
	// Initial guess: 2^(ceil(bitlen/n)) — an upper bound on the root.
	bits := (x.BitLen() + n - 1) / n
	guess := new(big.Int).Lsh(big.NewInt(1), uint(bits+1))

	nBig := big.NewInt(int64(n))
	nMinus1 := big.NewInt(int64(n - 1))
	tmp := new(big.Int)
	next := new(big.Int)
	for {
		// next = ((n-1)*guess + x / guess^(n-1)) / n
		tmp.Exp(guess, nMinus1, nil)
		if tmp.Sign() == 0 {
			break
		}
		next.Div(x, tmp)
		tmp.Mul(nMinus1, guess)
		next.Add(next, tmp)
		next.Div(next, nBig)
		if next.Cmp(guess) >= 0 {
			break // converged (Newton descends monotonically from above)
		}
		guess.Set(next)
	}
	// Correct any residual off-by-one so the result is exactly floor(x^(1/n)).
	for {
		tmp.Exp(guess, nBig, nil)
		if tmp.Cmp(x) <= 0 {
			break
		}
		guess.Sub(guess, big.NewInt(1))
	}
	for {
		tmp.Exp(new(big.Int).Add(guess, big.NewInt(1)), nBig, nil)
		if tmp.Cmp(x) > 0 {
			break
		}
		guess.Add(guess, big.NewInt(1))
	}
	return guess
}
