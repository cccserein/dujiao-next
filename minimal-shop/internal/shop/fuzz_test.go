package shop

import (
	"context"
	"encoding/json"
	"net/url"
	"testing"
)

func FuzzProviderCallbacks(f *testing.F) {
	epay := EpayGateway{MerchantID: "merchant-test", MerchantKey: "test-secret-12345678"}
	bep := BepGateway{Token: "test-bep-token-12345678"}
	fields := url.Values{"pid": {epay.MerchantID}, "out_trade_no": {"pay-test"}, "trade_no": {"trade-test"},
		"trade_status": {"TRADE_SUCCESS"}, "money": {"15.01"}, "type": {"alipay"}}
	fields.Set("sign", epaySignature(fields, epay.MerchantKey))
	f.Add([]byte(fields.Encode()))
	callback := map[string]any{"trade_id": "trade-test", "order_id": "pay-test", "amount": 15.01,
		"actual_amount": 2.1, "token": "wallet-test", "block_transaction_id": "chain-test", "status": 2}
	callback["signature"] = bepSignature(callback, bep.Token)
	encoded, _ := json.Marshal(callback)
	f.Add(encoded)
	f.Add([]byte(`{"status":2,"amount":"1/0"}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		_, _ = epay.Verify(context.Background(), nil, body)
		_, _ = bep.Verify(context.Background(), nil, body)
	})
}

func FuzzProviderAmount(f *testing.F) {
	for _, value := range []string{"15.01", "0.001", "1/2", "1e2", "999999999999999999999999999999999"} {
		f.Add(value)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		cents, err := providerAmountCents(raw)
		if err == nil && cents <= 0 {
			t.Fatalf("nonpositive accepted amount: %q => %d", raw, cents)
		}
	})
}
