// Package money holds the USDT amount type. Amounts are integers in base
// units (1 USDT = 10^6 units), never floats.
package money

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
)

// Decimals is the USDT (ERC-20) precision.
const Decimals = 6

const unitsPerToken = 1_000_000

// Amount is a USDT amount in base units.
//
// int64 is enough: the whole USDT supply (~1.5e11 tokens = 1.5e17 units) is
// far below math.MaxInt64 (~9.2e18). Values that do not fit are rejected at the
// boundary instead of being truncated.
type Amount int64

var (
	ErrInvalidFormat = errors.New("invalid amount format")
	ErrTooPrecise    = fmt.Errorf("amount has more than %d decimal places", Decimals)
	ErrOutOfRange    = errors.New("amount out of range")
)

// Parse reads a non-negative decimal string such as "100", "0.5" or
// "12.345678". Signs, exponents and more than 6 fractional digits are rejected.
func Parse(s string) (Amount, error) {
	if s == "" {
		return 0, ErrInvalidFormat
	}
	intPart, fracPart, hasDot := strings.Cut(s, ".")
	if intPart == "" || (hasDot && fracPart == "") || !digitsOnly(intPart) || !digitsOnly(fracPart) {
		return 0, ErrInvalidFormat
	}
	if len(fracPart) > Decimals {
		return 0, ErrTooPrecise
	}
	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, ErrOutOfRange
	}
	frac := int64(0)
	if fracPart != "" {
		frac, _ = strconv.ParseInt(fracPart+strings.Repeat("0", Decimals-len(fracPart)), 10, 64)
	}
	if whole > (math.MaxInt64-frac)/unitsPerToken {
		return 0, ErrOutOfRange
	}
	return Amount(whole*unitsPerToken + frac), nil
}

// FromBaseUnits converts an on-chain uint256 value into an Amount.
func FromBaseUnits(v *big.Int) (Amount, error) {
	if v == nil || v.Sign() < 0 || !v.IsInt64() {
		return 0, ErrOutOfRange
	}
	return Amount(v.Int64()), nil
}

// BaseUnits returns the amount as an on-chain integer value.
func (a Amount) BaseUnits() *big.Int { return big.NewInt(int64(a)) }

// String formats the amount with exactly 6 decimal places, e.g. "99.000000".
func (a Amount) String() string {
	sign := ""
	v := int64(a)
	if v < 0 {
		sign = "-"
		v = -v
	}
	return fmt.Sprintf("%s%d.%06d", sign, v/unitsPerToken, v%unitsPerToken)
}

// MulBps returns floor(a * bps / 10000). Rounding down means a fractional
// base unit of fee always stays with the partner.
func (a Amount) MulBps(bps int) Amount {
	r := new(big.Int).Mul(big.NewInt(int64(a)), big.NewInt(int64(bps)))
	r.Quo(r, big.NewInt(10_000))
	return Amount(r.Int64())
}

// MarshalJSON encodes the amount as a decimal string so clients never parse
// it into a float.
func (a Amount) MarshalJSON() ([]byte, error) { return json.Marshal(a.String()) }

// UnmarshalJSON accepts only a JSON string; a JSON number is rejected because
// most clients have already lost precision by turning it into a float.
func (a *Amount) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("%w: amount must be a string", ErrInvalidFormat)
	}
	v, err := Parse(s)
	if err != nil {
		return err
	}
	*a = v
	return nil
}

func digitsOnly(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
