package epayadapter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dujiao-next/internal/constants"
	paymentcontract "github.com/dujiao-next/internal/modules/payment/contract"
	"github.com/dujiao-next/internal/shared/jsonmap"
	"github.com/dujiao-next/internal/shared/money"
	"github.com/shopspring/decimal"
)

func corroborateChannelConfig(endpoint, version string) jsonmap.JSON {
	return jsonmap.JSON{"gateway_url": endpoint, "epay_version": version, "merchant_id": "1001", "merchant_key": "sandbox-key", "notify_url": "https://sandbox.invalid/callback", "return_url": "https://sandbox.invalid/pay"}
}
func newCorroborateTLSServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	previous := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = previous; server.Close() })
	return server
}
func corroborateInput() paymentcontract.CorroborateInput {
	return paymentcontract.CorroborateInput{OrderNo: "BUSINESS-ORDER", ProviderRef: "PAID-TRADE", Amount: money.FromDecimal(decimal.RequireFromString("36.03")), Currency: "CNY"}
}
func corroborateReply() map[string]interface{} {
	return map[string]interface{}{"code": 1, "status": 1, "pid": "1001", "trade_no": "PAID-TRADE", "out_trade_no": "BUSINESS-ORDER", "money": "36.03"}
}
func TestEpayAdapter_CorroboratePaymentNormalAndLegacyOrder(t *testing.T) {
	// Legacy records have no DJP identifier: the service passes the verified
	// business order number while keeping the gateway trade reference distinct.
	for _, orderNo := range []string{"DJP-NEW-ORDER", "BUSINESS-ORDER"} {
		t.Run(orderNo, func(t *testing.T) {
			reply := corroborateReply()
			reply["out_trade_no"] = orderNo
			server := newCorroborateTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
				_ = r.ParseForm()
				if r.PostForm.Get("trade_no") != "PAID-TRADE" || r.PostForm.Get("out_trade_no") != "" {
					t.Error("must query exact trade ref")
				}
				_ = json.NewEncoder(w).Encode(reply)
			})
			input := corroborateInput()
			input.OrderNo = orderNo
			result, err := NewEpayAdapter().(paymentcontract.CallbackCorroborator).CorroboratePayment(context.Background(), corroborateChannelConfig(server.URL, "v1"), input)
			if err != nil || result.Status != constants.PaymentStatusSuccess || result.ProviderRef != "PAID-TRADE" || result.Currency != "CNY" || !result.Amount.Decimal.Equal(input.Amount.Decimal) {
				t.Fatalf("valid/legacy query rejected: %+v %v", result, err)
			}
		})
	}
}
func TestEpayAdapter_CorroboratePaymentChecksAmountAndCurrency(t *testing.T) {
	for _, tc := range []struct {
		name, key string
		value     interface{}
	}{
		{"less money", "money", "1.00"}, {"more money", "money", "99.00"}, {"absent money", "money", nil},
		{"wrong currency", "currency", "USD"}, {"missing merchant", "pid", nil}, {"wrong attempt", "trade_no", "OTHER-TRADE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reply := corroborateReply()
			if tc.value == nil {
				delete(reply, tc.key)
			} else {
				reply[tc.key] = tc.value
			}
			server := newCorroborateTLSServer(t, func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(reply) })
			if _, err := NewEpayAdapter().(paymentcontract.CallbackCorroborator).CorroboratePayment(context.Background(), corroborateChannelConfig(server.URL, "v1"), corroborateInput()); !errors.Is(err, paymentcontract.ErrGatewayResponseInvalid) {
				t.Fatalf("invalid reply accepted: %v", err)
			}
		})
	}
}
func TestEpayAdapter_CorroboratePaymentPending(t *testing.T) {
	reply := corroborateReply()
	reply["status"] = 0
	server := newCorroborateTLSServer(t, func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(reply) })
	result, err := NewEpayAdapter().(paymentcontract.CallbackCorroborator).CorroboratePayment(context.Background(), corroborateChannelConfig(server.URL, "v1"), corroborateInput())
	if err != nil || result.Status == constants.PaymentStatusSuccess {
		t.Fatalf("unpaid query accepted as paid: %v", err)
	}
}
func TestEpayAdapter_CorroboratePaymentUnsupportedAndMissingBindings(t *testing.T) {
	corroborator := NewEpayAdapter().(paymentcontract.CallbackCorroborator)
	if _, err := corroborator.CorroboratePayment(context.Background(), corroborateChannelConfig("https://sandbox.invalid", "v2"), corroborateInput()); !errors.Is(err, paymentcontract.ErrGatewayCorroborationUnsupported) {
		t.Fatalf("wrong v2 error: %v", err)
	}
	input := corroborateInput()
	input.OrderNo = ""
	if _, err := corroborator.CorroboratePayment(context.Background(), corroborateChannelConfig("https://sandbox.invalid", "v1"), input); !errors.Is(err, paymentcontract.ErrGatewayConfigInvalid) {
		t.Fatal("trade_no must not stand in for out_trade_no")
	}
	input = corroborateInput()
	input.ProviderRef = ""
	if _, err := corroborator.CorroboratePayment(context.Background(), corroborateChannelConfig("https://sandbox.invalid", "v1"), input); !errors.Is(err, paymentcontract.ErrGatewayConfigInvalid) {
		t.Fatal("missing exact trade ref accepted")
	}
}
func TestEpayAdapter_CorroborateConvertedFeeInclusiveAmount(t *testing.T) {
	server := newCorroborateTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.URL.Path == "/mapi.php" {
			if r.PostForm.Get("money") != "73.44" {
				t.Errorf("converted fee inclusive amount = %s", r.PostForm.Get("money"))
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"code": 1, "trade_no": "PAID-TRADE", "payurl": "https://sandbox.invalid/pay"})
			return
		}
		reply := corroborateReply()
		reply["money"] = "73.44"
		_ = json.NewEncoder(w).Encode(reply)
	})
	raw := corroborateChannelConfig(server.URL, "v1")
	raw["target_currency"] = "CNY"
	raw["exchange_rate"] = "7.2"
	adapter := NewEpayAdapter()
	created, err := adapter.CreatePayment(context.Background(), raw, paymentcontract.GatewayCreateInput{OrderNo: "BUSINESS-ORDER", Currency: "USD", Amount: money.FromDecimal(decimal.RequireFromString("10.20")), ChannelType: constants.PaymentChannelTypeAlipay, ClientIP: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	input := corroborateInput()
	input.Amount = money.FromDecimal(decimal.RequireFromString(created.AmountSent))
	input.Currency = created.CurrencySent
	result, err := adapter.(paymentcontract.CallbackCorroborator).CorroboratePayment(context.Background(), raw, input)
	if err != nil || !result.Amount.Decimal.Equal(input.Amount.Decimal) {
		t.Fatalf("converted payment rejected: %v", err)
	}
}
func TestEpayAdapter_CallbackAllStatusesProtectCreationSnapshot(t *testing.T) {
	raw := corroborateChannelConfig("https://sandbox.invalid", "v1")
	verifier := NewEpayAdapter().(paymentcontract.GatewayCallbackVerifier)
	for _, status := range []string{"WAIT_BUYER_PAY", "TRADE_SUCCESS", "TRADE_FINISHED"} {
		t.Run(status, func(t *testing.T) {
			params := map[string]string{"pid": "1001", "trade_no": "PAID-TRADE", "out_trade_no": "BUSINESS-ORDER", "money": "36.03", "trade_status": status,
				"exchange_rate": "0.001", "original_amount": "36030", "original_currency": "USD", "params": "forged", "endpoint": "https://attacker.invalid", "mode": "forged", "token": "forged", "gateway_order_verified": "true"}
			result, err := verifier.VerifyCallback(raw, signEpayV1AdapterCallbackForm(params, "sandbox-key"), nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"exchange_rate", "original_amount", "original_currency", "params", "endpoint", "mode", "token", "gateway_order_verified"} {
				if _, present := result.Payload[field]; present {
					t.Errorf("callback overwrote creation field %s", field)
				}
			}
		})
	}
}
func TestEpayAdapter_CallbackStrictMoneyCurrencyAndDuplicateFields(t *testing.T) {
	raw := corroborateChannelConfig("https://sandbox.invalid", "v1")
	verifier := NewEpayAdapter().(paymentcontract.GatewayCallbackVerifier)
	for _, status := range []string{"WAIT_BUYER_PAY", "TRADE_SUCCESS"} {
		for _, amount := range []string{"", "0", "-1", "1e2147483647", "36.031", "NaN", "12345678901234567890123456789012345"} {
			form := signEpayV1AdapterCallbackForm(map[string]string{"pid": "1001", "trade_no": "PAID-TRADE", "out_trade_no": "BUSINESS-ORDER", "money": amount, "trade_status": status}, "sandbox-key")
			if _, err := verifier.VerifyCallback(raw, form, nil); err == nil {
				t.Fatalf("invalid %s money accepted", status)
			}
		}
	}
	for _, field := range []string{"money", "out_trade_no", "trade_no", "pid", "sign", "currency", "exchange_rate"} {
		form := signEpayV1AdapterCallbackForm(map[string]string{"pid": "1001", "trade_no": "PAID-TRADE", "out_trade_no": "BUSINESS-ORDER", "money": "36.03", "trade_status": "TRADE_SUCCESS", "currency": "CNY", "exchange_rate": "1"}, "sandbox-key")
		form[field] = append(form[field], "forged")
		if _, err := verifier.VerifyCallback(raw, form, nil); !errors.Is(err, paymentcontract.ErrGatewaySignatureInvalid) {
			t.Fatalf("duplicate %s accepted: %v", field, err)
		}
	}
	form := signEpayV1AdapterCallbackForm(map[string]string{"pid": "1001", "trade_no": "PAID-TRADE", "out_trade_no": "BUSINESS-ORDER", "money": "36.03", "trade_status": "TRADE_SUCCESS", "currency": "USD"}, "sandbox-key")
	emptyCurrency := signEpayV1AdapterCallbackForm(map[string]string{"pid": "1001", "trade_no": "PAID-TRADE", "out_trade_no": "BUSINESS-ORDER", "money": "36.03", "trade_status": "TRADE_SUCCESS"}, "sandbox-key")
	emptyCurrency["currency"] = nil
	if _, err := verifier.VerifyCallback(raw, emptyCurrency, nil); !errors.Is(err, paymentcontract.ErrGatewaySignatureInvalid) {
		t.Fatalf("empty currency values must be rejected before field access: %v", err)
	}
	if _, err := verifier.VerifyCallback(raw, form, nil); err == nil {
		t.Fatal("explicit wrong currency accepted")
	}
	raw["target_currency"] = "USD"
	result, err := verifier.VerifyCallback(raw, form, nil)
	if err != nil || result.Currency != "USD" {
		t.Fatalf("configured currency rejected: %v", err)
	}
}
