// Package money holds the USDC amount type used across the ledger.
//
// USDC has 6 decimals on-chain, so every amount is stored as an int64 count
// of micro-USDC ("micros"). Floats never touch balances.
package money

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Decimals is the number of decimal places USDC uses on Base/Ethereum.
const Decimals = 6

const microsPerUnit = 1_000_000

// Amount is a quantity of USDC in micro units (1 USDC = 1_000_000).
type Amount int64

var (
	ErrInvalidAmount = errors.New("money: invalid amount")
	ErrTooPrecise    = errors.New("money: more than 6 decimal places")
	ErrOverflow      = errors.New("money: amount out of range")
)

// FromMicros builds an Amount from a raw micro-USDC value.
func FromMicros(m int64) Amount { return Amount(m) }

// USDC builds an Amount from whole dollars. Handy in tests.
func USDC(whole int64) Amount { return Amount(whole * microsPerUnit) }

// Micros returns the raw micro-USDC value.
func (a Amount) Micros() int64 { return int64(a) }

// Parse reads a decimal string like "12.5" or "0.000001".
// It rejects exponents, more than 6 decimals and empty input.
func Parse(s string) (Amount, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, ErrInvalidAmount
	}
	neg := false
	if s[0] == '-' || s[0] == '+' {
		neg = s[0] == '-'
		s = s[1:]
	}
	whole, frac, hasDot := strings.Cut(s, ".")
	if whole == "" && frac == "" {
		return 0, ErrInvalidAmount
	}
	if hasDot && frac == "" {
		return 0, ErrInvalidAmount
	}
	if len(frac) > Decimals {
		return 0, ErrTooPrecise
	}
	if !digitsOnly(whole) || !digitsOnly(frac) {
		return 0, ErrInvalidAmount
	}
	if whole == "" {
		whole = "0"
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, ErrOverflow
	}
	frac += strings.Repeat("0", Decimals-len(frac))
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, ErrInvalidAmount
	}
	if w > (1<<63-1-f)/microsPerUnit {
		return 0, ErrOverflow
	}
	v := w*microsPerUnit + f
	if neg {
		v = -v
	}
	return Amount(v), nil
}

// MustParse is Parse for constants in tests.
func MustParse(s string) Amount {
	a, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return a
}

// String formats the amount with all 6 decimals, e.g. "12.500000".
func (a Amount) String() string {
	v := int64(a)
	sign := ""
	if v < 0 {
		sign = "-"
		v = -v
	}
	return fmt.Sprintf("%s%d.%06d", sign, v/microsPerUnit, v%microsPerUnit)
}

func digitsOnly(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
