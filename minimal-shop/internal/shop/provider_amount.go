package shop

import (
	"math/big"
	"strings"
)

func providerAmountCents(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 32 {
		return 0, ErrInvalid
	}
	dot := false
	for i, char := range raw {
		if char == '.' && !dot && i > 0 && i < len(raw)-1 {
			dot = true
			continue
		}
		if char < '0' || char > '9' {
			return 0, ErrInvalid
		}
	}
	amount, ok := new(big.Rat).SetString(raw)
	if !ok {
		return 0, ErrInvalid
	}
	amount.Mul(amount, big.NewRat(100, 1))
	if !amount.IsInt() || !amount.Num().IsInt64() || amount.Sign() <= 0 {
		return 0, ErrInvalid
	}
	return amount.Num().Int64(), nil
}
