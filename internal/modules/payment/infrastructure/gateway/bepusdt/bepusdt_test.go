package bepusdt

import (
	"errors"
	"testing"

	"github.com/dujiao-next/internal/constants"
)

func TestVerifyCallbackRejectsForgedAmountAndMissingSecret(t *testing.T) {
	cfg := &Config{AuthToken: "sandbox-secret"}
	callback := &CallbackData{
		TradeID: "synthetic-trade", OrderID: "synthetic-order",
		Amount: 111.0, ActualAmount: 15.0, Token: "synthetic-token",
		BlockTransactionID: "synthetic-chain-tx", Status: StatusSuccess,
	}
	callback.Signature = Sign(map[string]interface{}{
		"trade_id": callback.TradeID, "order_id": callback.OrderID,
		"amount": callback.GetAmount(), "actual_amount": callback.GetActualAmount(),
		"token": callback.Token, "block_transaction_id": callback.BlockTransactionID,
		"status": callback.Status,
	}, cfg.AuthToken)
	if err := VerifyCallback(cfg, callback); err != nil {
		t.Fatalf("valid synthetic callback rejected: %v", err)
	}

	callback.Amount = 1.0
	if err := VerifyCallback(cfg, callback); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("modified amount must fail signature verification, got %v", err)
	}
	callback.Amount = 111.0
	if err := VerifyCallback(&Config{}, callback); !errors.Is(err, ErrConfigInvalid) {
		t.Fatalf("empty merchant secret must be rejected, got %v", err)
	}
}

func TestVerifyCallbackRejectsReusedSignatureForDifferentOrderOrStatus(t *testing.T) {
	cfg := &Config{AuthToken: "sandbox-secret"}
	original := CallbackData{
		TradeID: "synthetic-trade", OrderID: "synthetic-order",
		Amount: 111.0, ActualAmount: 15.0, Token: "synthetic-token",
		BlockTransactionID: "synthetic-chain-tx", Status: StatusSuccess,
	}
	original.Signature = Sign(map[string]interface{}{
		"trade_id": original.TradeID, "order_id": original.OrderID,
		"amount": original.GetAmount(), "actual_amount": original.GetActualAmount(),
		"token": original.Token, "block_transaction_id": original.BlockTransactionID,
		"status": original.Status,
	}, cfg.AuthToken)

	for _, tc := range []struct {
		name   string
		mutate func(*CallbackData)
	}{
		{"different order", func(data *CallbackData) { data.OrderID = "attacker-order" }},
		{"different trade", func(data *CallbackData) { data.TradeID = "attacker-trade" }},
		{"different token", func(data *CallbackData) { data.Token = "attacker-token" }},
		{"different actual amount", func(data *CallbackData) { data.ActualAmount = 1.0 }},
		{"pending status", func(data *CallbackData) { data.Status = StatusWaiting }},
		{"smuggled order id in chain transaction", func(data *CallbackData) {
			data.BlockTransactionID += "&order_id=" + data.OrderID
			data.OrderID = ""
		}},
		{"smuggled trade id in token", func(data *CallbackData) {
			data.Token += "&trade_id=" + data.TradeID
			data.TradeID = ""
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forged := original
			tc.mutate(&forged)
			if err := VerifyCallback(cfg, &forged); err == nil {
				t.Fatal("reused signature accepted modified callback")
			}
		})
	}
}

func TestVerifyCallbackRejectsSignedDelimiterInIdentifier(t *testing.T) {
	cfg := &Config{AuthToken: "sandbox-secret"}
	callback := &CallbackData{
		TradeID: "synthetic-trade", OrderID: "synthetic-order",
		Amount: 111.0, ActualAmount: 15.0, Token: "synthetic-token",
		BlockTransactionID: "synthetic-chain-tx&order_id=attacker-order", Status: StatusSuccess,
	}
	callback.Signature = Sign(map[string]interface{}{
		"trade_id": callback.TradeID, "order_id": callback.OrderID,
		"amount": callback.GetAmount(), "actual_amount": callback.GetActualAmount(),
		"token": callback.Token, "block_transaction_id": callback.BlockTransactionID,
		"status": callback.Status,
	}, cfg.AuthToken)
	if err := VerifyCallback(cfg, callback); !errors.Is(err, ErrResponseInvalid) {
		t.Fatalf("signed delimiter in callback identifier must be rejected, got %v", err)
	}
}

func TestParseConfigAndNormalizeDefaults(t *testing.T) {
	cfg, err := ParseConfig(map[string]interface{}{
		"gateway_url": " https://pay.example.com/ ",
		"auth_token":  " token ",
		"currencies":  "USDT,USDC",
		"notify_url":  " https://example.com/notify ",
		"return_url":  " https://example.com/return ",
	})
	if err != nil {
		t.Fatalf("parse config failed: %v", err)
	}
	if cfg.TradeType != bepusdtTradeTypeUSDTTRC20 {
		t.Fatalf("unexpected default trade type: %s", cfg.TradeType)
	}
	if cfg.Fiat != constants.SiteCurrencyDefault {
		t.Fatalf("unexpected default fiat: %s", cfg.Fiat)
	}
	if cfg.GatewayURL != "https://pay.example.com" {
		t.Fatalf("unexpected normalized gateway url: %s", cfg.GatewayURL)
	}
	if cfg.Currencies != "" {
		t.Fatalf("transaction mode currencies should be empty, got %q", cfg.Currencies)
	}
}

func TestParseConfig_CashierModeKeepsTradeTypeEmpty(t *testing.T) {
	cfg, err := ParseConfig(map[string]interface{}{
		"gateway_url": " https://pay.example.com/ ",
		"auth_token":  " token ",
		"order_mode":  constants.PaymentBepusdtOrderModeCashier,
		"trade_type":  " usdt.trc20 ",
		"currencies":  " usdt, usdc ",
		"notify_url":  " https://example.com/notify ",
		"return_url":  " https://example.com/return ",
	})
	if err != nil {
		t.Fatalf("parse config failed: %v", err)
	}
	if cfg.TradeType != "" {
		t.Fatalf("cashier mode trade type should stay empty, got %s", cfg.TradeType)
	}
	if cfg.Currencies != "USDT,USDC" {
		t.Fatalf("cashier currencies = %q, want USDT,USDC", cfg.Currencies)
	}
}

func TestResolveTradeType(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		expect string
	}{
		{name: "USDT", input: bepusdtChannelTypeUSDT, expect: bepusdtTradeTypeUSDTTRC20},
		{name: "USDTTRC20", input: bepusdtChannelTypeUSDTTRC20, expect: bepusdtTradeTypeUSDTTRC20},
		{name: "USDCTRC20", input: bepusdtChannelTypeUSDCTRC20, expect: bepusdtTradeTypeUSDCTRC20},
		{name: "TRX", input: bepusdtChannelTypeTRX, expect: bepusdtTradeTypeTRX},
		{name: "Unknown", input: "unknown", expect: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveTradeType(tc.input); got != tc.expect {
				t.Fatalf("unexpected trade type: got %s, want %s", got, tc.expect)
			}
		})
	}
}

func TestToPaymentStatus(t *testing.T) {
	tests := []struct {
		name   string
		status int
		expect string
	}{
		{name: "Success", status: StatusSuccess, expect: constants.PaymentStatusSuccess},
		{name: "Expired", status: StatusExpired, expect: constants.PaymentStatusExpired},
		{name: "Waiting", status: StatusWaiting, expect: constants.PaymentStatusPending},
		{name: "Unknown", status: 999, expect: constants.PaymentStatusPending},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ToPaymentStatus(tc.status); got != tc.expect {
				t.Fatalf("unexpected payment status: got %s, want %s", got, tc.expect)
			}
		})
	}
}
