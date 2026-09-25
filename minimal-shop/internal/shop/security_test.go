package shop

import (
	"context"
	"crypto/md5"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestCardCipherDoesNotRevealOrSwapCards(t *testing.T) {
	cipher, err := NewCardCipher([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	nonce, ciphertext, err := cipher.Encrypt(3, "discardable-card-001")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ciphertext), "discardable-card") {
		t.Fatal("ciphertext contains plaintext")
	}
	if got, err := cipher.Decrypt(3, nonce, ciphertext); err != nil || got != "discardable-card-001" {
		t.Fatalf("valid card failed to decrypt: %v", err)
	}
	if _, err := cipher.Decrypt(4, nonce, ciphertext); err == nil {
		t.Fatal("card decrypted under another product")
	}
	ciphertext[0] ^= 1
	if _, err := cipher.Decrypt(3, nonce, ciphertext); err == nil {
		t.Fatal("tampered card decrypted")
	}
}

func TestTOTPUsesStandardCodeAndRejectsBadInput(t *testing.T) {
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte("12345678901234567890"))
	if step, ok := validTOTP(secret, "287082", time.Unix(59, 0)); !ok || step != 1 {
		t.Fatalf("RFC 6238 six-digit code rejected: step=%d ok=%v", step, ok)
	}
	if _, ok := validTOTP(secret, "287083", time.Unix(59, 0)); ok {
		t.Fatal("wrong TOTP code accepted")
	}
	if _, ok := validTOTP(secret, "287082x", time.Unix(59, 0)); ok {
		t.Fatal("malformed TOTP code accepted")
	}
}

func TestProviderAmountCentsRejectsFractions(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  int64
		valid bool
	}{
		{"10", 1000, true}, {"10.00", 1000, true}, {"10.010", 1001, true},
		{"10.001", 0, false}, {"1e2", 0, false}, {"-1", 0, false}, {"0", 0, false},
	} {
		got, err := providerAmountCents(tc.input)
		if (err == nil) != tc.valid || (tc.valid && got != tc.want) {
			t.Fatalf("amount %q: got %d, err %v", tc.input, got, err)
		}
	}
}

func TestEpayCallbackRequiresMerchantSignatureAndExactAmount(t *testing.T) {
	g := EpayGateway{BaseURL: "https://pay.example.test", MerchantID: "merchant-7", MerchantKey: "test-secret-epay-123456", AppURL: "https://shop.example.test"}
	started, err := g.Start(context.Background(), PaymentStart{PaymentID: "pay-9", ProviderOrderID: "pay-9", OrderID: 11, AmountCents: 1501, Currency: "CNY"})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(started.URL)
	if err != nil || u.Scheme != "https" || u.Path != "/submit.php" || u.Query().Get("money") != "15.01" || u.Query().Get("out_trade_no") != "pay-9" {
		t.Fatalf("unexpected epay redirect URL: %v", err)
	}
	fields := url.Values{
		"pid": {"merchant-7"}, "out_trade_no": {"pay-9"}, "trade_no": {"gateway-25"},
		"trade_status": {"TRADE_SUCCESS"}, "money": {"15.01"}, "type": {"alipay"},
	}
	// Independent protocol string, kept visible as a compatibility fixture.
	canonical := "money=15.01&out_trade_no=pay-9&pid=merchant-7&trade_no=gateway-25&trade_status=TRADE_SUCCESS&type=alipay"
	sum := md5.Sum([]byte(canonical + g.MerchantKey))
	fields.Set("sign", hex.EncodeToString(sum[:]))
	event, err := g.Verify(context.Background(), nil, []byte(fields.Encode()))
	if err != nil || event.AmountCents != 1501 || event.ProviderRef != "gateway-25" || event.PaymentID != "pay-9" {
		t.Fatalf("valid callback rejected: event=%+v err=%v", event, err)
	}
	fields.Set("money", "1.00")
	if _, err := g.Verify(context.Background(), nil, []byte(fields.Encode())); err == nil {
		t.Fatal("tampered amount passed signature verification")
	}
	fields.Set("money", "15.01")
	fields.Set("pid", "other-merchant")
	if _, err := g.Verify(context.Background(), nil, []byte(fields.Encode())); err == nil {
		t.Fatal("other merchant callback accepted")
	}
}

func TestBepusdtCashierOrderAndCallback(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/order/create-order" || r.Method != http.MethodPost {
			t.Errorf("unexpected gateway path or method: %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var request map[string]any
		if err := json.Unmarshal(body, &request); err != nil {
			t.Errorf("invalid request JSON: %v", err)
		}
		if request["order_id"] != "pay-42" || request["fiat"] != "CNY" || request["currencies"] != "USDT" || request["amount"] != float64(15.01) {
			t.Errorf("wrong cashier request facts: %v", request)
		}
		if request["signature"] != bepSignature(request, "test-bep-token-12345678") {
			t.Error("wrong cashier signature")
		}
		_, _ = io.WriteString(w, `{"status_code":200,"data":{"fiat":"CNY","trade_id":"trade-42","order_id":"pay-42","amount":"15.01","payment_url":"https://cashier.example.test/pay/42"}}`)
	}))
	defer server.Close()
	g := BepGateway{BaseURL: server.URL, Token: "test-bep-token-12345678", Currencies: "USDT", AppURL: "https://shop.example.test", Client: server.Client()}
	started, err := g.Start(context.Background(), PaymentStart{PaymentID: "pay-42", ProviderOrderID: "pay-42", OrderID: 42, AmountCents: 1501, Currency: "CNY"})
	if err != nil || started.ProviderRef != "trade-42" || started.URL != "https://cashier.example.test/pay/42" {
		t.Fatalf("cashier start failed: %+v %v", started, err)
	}
	callback := map[string]any{
		"trade_id": "trade-42", "order_id": "pay-42", "amount": 15.01, "actual_amount": 2.1,
		"token": "wallet-1", "block_transaction_id": "chain-1", "status": 2,
	}
	callback["signature"] = bepSignature(callback, g.Token)
	body, _ := json.Marshal(callback)
	event, err := g.Verify(context.Background(), nil, body)
	if err != nil || event.AmountCents != 1501 || event.ProviderRef != "trade-42" {
		t.Fatalf("valid cashier callback rejected: %+v %v", event, err)
	}
	callback["amount"] = 1.0
	body, _ = json.Marshal(callback)
	if _, err := g.Verify(context.Background(), nil, body); err == nil {
		t.Fatal("tampered cashier amount passed verification")
	}
}

func TestProductionRejectsMockAndHTTP(t *testing.T) {
	t.Setenv("SHOP_DATABASE_URL", "postgres://test:test@localhost/shop_test")
	t.Setenv("SHOP_CARD_KEY_HEX", strings.Repeat("ab", 32))
	t.Setenv("SHOP_APP_URL", "https://shop.example.test")
	t.Setenv("SHOP_ENV", "production")
	t.Setenv("SHOP_PAYMENT_MODE", "mock")
	t.Setenv("SHOP_MOCK_KEY_HEX", strings.Repeat("cd", 32))
	if _, err := LoadConfig(); err == nil {
		t.Fatal("mock gateway enabled in production")
	}
	t.Setenv("SHOP_PAYMENT_MODE", "live")
	t.Setenv("SHOP_EPAY_URL", "https://pay.example.test")
	t.Setenv("SHOP_EPAY_MERCHANT_ID", "merchant")
	t.Setenv("SHOP_EPAY_KEY", "test-secret-12345678")
	t.Setenv("SHOP_BEPUSDT_URL", "http://127.0.0.1")
	t.Setenv("SHOP_BEPUSDT_TOKEN", "test-secret-12345678")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("HTTP live gateway accepted")
	}
	t.Setenv("SHOP_BEPUSDT_URL", "https://pay.example.test")
	if _, err := LoadConfig(); err != nil {
		t.Fatalf("valid production configuration rejected: %v", err)
	}
}

func TestOriginCheckRejectsCrossSitePost(t *testing.T) {
	w := Web{cfg: Config{AppURL: "https://shop.example.test"}}
	for _, tc := range []struct {
		origin string
		want   bool
	}{
		{"https://shop.example.test", true},
		{"https://evil.example.test", false},
		{"", false},
		{"http://shop.example.test", false},
	} {
		r := httptest.NewRequest(http.MethodPost, "https://shop.example.test/orders", nil)
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		if got := w.sameOrigin(r); got != tc.want {
			t.Fatal(fmt.Sprintf("origin %q: got %v, want %v", tc.origin, got, tc.want))
		}
	}
}

func TestLoginLimiterBlocksRepeatedAttempts(t *testing.T) {
	limiter := newAttemptLimiter()
	key := accountLimitKey("Buyer@Example.Test")
	if key != accountLimitKey(" buyer@example.test ") {
		t.Fatal("email case or whitespace bypassed account limit")
	}
	for i := 0; i < 10; i++ {
		if !limiter.allow(key, 10, 15*time.Minute) {
			t.Fatalf("request %d blocked prematurely", i+1)
		}
	}
	if limiter.allow(key, 10, 15*time.Minute) {
		t.Fatal("repeated login was not rate limited")
	}
}
