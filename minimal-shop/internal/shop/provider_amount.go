package shop

import (
	"math/big"
	"strings"
)

func providerAmountCents(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsAny(raw, "eE") {
		return 0, ErrInvalid
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
