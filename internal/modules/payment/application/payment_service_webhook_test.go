package application

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	paymentcontract "github.com/dujiao-next/internal/modules/payment/contract"

	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	paymentdomain "github.com/dujiao-next/internal/modules/payment/domain"

	"github.com/dujiao-next/internal/constants"
	"github.com/dujiao-next/internal/shared/jsonmap"
	"github.com/dujiao-next/internal/shared/money"

	"github.com/shopspring/decimal"
)

func TestSupportsBlindWebhookCandidateMatching(t *testing.T) {
	cases := []struct {
		channelType string
		want        bool
	}{
		{constants.PaymentChannelTypeWechat, true},
		{constants.PaymentChannelTypeStripe, true},
		{constants.PaymentChannelTypePaypal, false},
		{constants.PaymentChannelTypeAlipay, false},
		{"", false},
		{"  WECHAT  ", true},
	}
	for _, tc := range cases {
		if got := supportsBlindWebhookCandidateMatching(tc.channelType); got != tc.want {
			t.Fatalf("channelType=%q: got %v, want %v", tc.channelType, got, tc.want)
		}
	}
}

// TestHandleWechatWebhookFallbackNoCandidate 验证 channel_id 为 0 且没有任何 active wechat
// channel 时,wechat webhook 返回 ErrPaymentChannelNotFound(而非 ErrPaymentInvalid);
// 这是相对 67bbc47 重构后回归行为(硬性拒绝 channel_id==0)的关键修复。
func TestHandleWechatWebhookFallbackNoCandidate(t *testing.T) {
	svc, _ := setupPaymentServiceWalletTest(t)

	_, _, err := svc.HandleWechatWebhook(WebhookCallbackInput{
		ChannelID: 0,
		Body:      []byte(`{"resource":{}}`),
		Headers:   map[string]string{},
	})
	if !errors.Is(err, ErrPaymentChannelNotFound) {
		t.Fatalf("expected ErrPaymentChannelNotFound for empty wechat candidate list, got: %v", err)
	}
}

// TestHandlePaypalWebhookRequiresChannelID 验证 paypal webhook 必须带 channel_id,
// 不具备盲匹配能力;channel_id==0 直接走 ErrPaymentInvalid。
func TestHandlePaypalWebhookRequiresChannelID(t *testing.T) {
	svc, _ := setupPaymentServiceWalletTest(t)

	_, _, err := svc.HandlePaypalWebhook(WebhookCallbackInput{
		ChannelID: 0,
		Body:      []byte(`{}`),
		Headers:   map[string]string{},
	})
	if !errors.Is(err, ErrPaymentInvalid) {
		t.Fatalf("expected ErrPaymentInvalid for paypal without channel_id, got: %v", err)
	}
}

const verifyTestMerchantKey = "test-merchant-key"

// signEpayCallbackForm 按易支付 v1 规则（k=v 排序拼接 + merchant_key 的 MD5）签名，
// 复现攻击者持有的"签名完全合法"的假回调。
func signEpayCallbackForm(key string, params map[string]string) map[string][]string {
	keys := make([]string, 0, len(params))
	for k, v := range params {
		if strings.TrimSpace(v) == "" || k == "sign" || k == "sign_type" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, fmt.Sprintf("%s=%s", k, params[k]))
	}
	sum := md5.Sum([]byte(strings.Join(pairs, "&") + key))
	params["sign"] = hex.EncodeToString(sum[:])
	params["sign_type"] = "MD5"

	form := make(map[string][]string, len(params))
	for k, v := range params {
		form[k] = []string{v}
	}
	return form
}

func newVerifyTestGatewayServer(t *testing.T, status int, amount string) *httptest.Server {
	t.Helper()
	return newVerifyTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("act") != "order" {
			t.Errorf("gateway query act = %q, want order", r.URL.Query().Get("act"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(
			`{"code":1,"pid":"56796329","msg":"查询订单成功","trade_no":"PAY-REAL","out_trade_no":"DJP-VERIFY","money":"%s","status":%d}`,
			amount, status,
		)))
	}))
}

// Only trust this test server's certificate; production queries still require HTTPS verification.
func newVerifyTLSServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(handler)
	transport := http.DefaultTransport
	http.DefaultTransport = srv.Client().Transport
	t.Cleanup(func() {
		http.DefaultTransport = transport
		srv.Close()
	})
	return srv
}

func newEpayVerifyChannel(endpoint string) *paymentdomain.PaymentChannel {
	return &paymentdomain.PaymentChannel{
		ID:              3,
		Name:            "支付宝",
		ProviderType:    constants.PaymentProviderEpay,
		ChannelType:     constants.PaymentChannelTypeAlipay,
		InteractionMode: constants.PaymentInteractionRedirect,
		IsActive:        true,
		ConfigJSON: jsonmap.JSON{
			"gateway_url":  endpoint,
			"epay_version": "v1",
			"merchant_id":  "56796329",
			"merchant_key": verifyTestMerchantKey,
			"notify_url":   "https://shop.example.com/api/v1/payments/callback",
			"return_url":   "https://shop.example.com/pay",
		},
	}
}

// TestHandleSyncCallbackRejectsForgedSuccessWhenGatewayReportsUnpaid 复现 2026-10-20 线上事故：
// 攻击者用泄露的 merchant_key 构造签名合法的"支付成功"回调，但钱从未进过网关。
// 加固后，这种回调必须在落库发货前被网关复核拦下：支付保持 pending、订单不转已支付。
func TestHandleSyncCallbackRejectsForgedSuccessWhenGatewayReportsUnpaid(t *testing.T) {
	svc, db := setupPaymentServiceWalletTest(t)
	now := time.Now()

	gatewayStatus := 0
	srv := newVerifyTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("act") != "order" {
			t.Errorf("gateway query act = %q, want order", r.URL.Query().Get("act"))
		}
		if r.Method != http.MethodPost || r.URL.Query().Get("key") != "" {
			t.Error("query must use POST without URL credentials")
		}
		if got := r.PostFormValue("trade_no"); got != "PAY-REAL" {
			t.Errorf("gateway query trade_no = %q, want PAY-REAL", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(
			`{"code":1,"pid":"56796329","msg":"查询订单成功","trade_no":"PAY-REAL","out_trade_no":"DJP-VERIFY","money":"36.03","status":%d}`,
			gatewayStatus,
		)))
	}))

	channel := newEpayVerifyChannel(srv.URL)

	order := &orderdomain.Order{
		OrderNo:                 "DJTESTVERIFY001",
		UserID:                  1,
		Status:                  constants.OrderStatusPendingPayment,
		Currency:                "CNY",
		OriginalAmount:          money.FromDecimal(decimal.NewFromInt(35)),
		DiscountAmount:          money.FromDecimal(decimal.Zero),
		PromotionDiscountAmount: money.FromDecimal(decimal.Zero),
		TotalAmount:             money.FromDecimal(decimal.NewFromInt(35)),
		WalletPaidAmount:        money.FromDecimal(decimal.Zero),
		OnlinePaidAmount:        money.FromDecimal(decimal.NewFromInt(35)),
		RefundedAmount:          money.FromDecimal(decimal.Zero),
		CreatedAt:               now,
		UpdatedAt:               now,
	}
	if err := db.Create(order).Error; err != nil {
		t.Fatalf("create order failed: %v", err)
	}

	payment := &paymentdomain.Payment{
		OrderID:         order.ID,
		ChannelID:       channel.ID,
		ProviderType:    constants.PaymentProviderEpay,
		ChannelType:     constants.PaymentChannelTypeAlipay,
		InteractionMode: constants.PaymentInteractionRedirect,
		Amount:          money.FromDecimal(decimal.RequireFromString("36.03")),
		FeeRate:         money.FromDecimal(decimal.Zero),
		FeeAmount:       money.FromDecimal(decimal.Zero),
		Currency:        "CNY",
		Status:          constants.PaymentStatusPending,
		GatewayOrderNo:  "DJP-VERIFY",
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := db.Create(payment).Error; err != nil {
		t.Fatalf("create payment failed: %v", err)
	}

	form := signEpayCallbackForm(verifyTestMerchantKey, map[string]string{
		"pid":          "56796329",
		"trade_no":     "PAY-REAL",
		"out_trade_no": "DJP-VERIFY",
		"type":         "alipay",
		"name":         "DJTESTVERIFY001",
		"money":        "36.03",
		"trade_status": "TRADE_SUCCESS",
	})

	// 网关侧未支付 → 拒绝放行。
	if _, err := svc.HandleSyncCallback(channel, form, nil); !errors.Is(err, ErrPaymentCallbackUnverified) {
		t.Fatalf("forged callback error = %v, want ErrPaymentCallbackUnverified", err)
	}

	var afterPayment paymentdomain.Payment
	if err := db.First(&afterPayment, payment.ID).Error; err != nil {
		t.Fatalf("reload payment failed: %v", err)
	}
	if afterPayment.Status != constants.PaymentStatusPending {
		t.Fatalf("payment status = %q, want pending (must not be marked paid)", afterPayment.Status)
	}
	var afterOrder orderdomain.Order
	if err := db.First(&afterOrder, order.ID).Error; err != nil {
		t.Fatalf("reload order failed: %v", err)
	}
	if afterOrder.Status != constants.OrderStatusPendingPayment {
		t.Fatalf("order status = %q, want pending_payment", afterOrder.Status)
	}

	// 网关确认已收款 → 正常放行并转已支付。
	gatewayStatus = 1
	if _, err := svc.HandleSyncCallback(channel, form, nil); err != nil {
		t.Fatalf("verified callback should succeed, got: %v", err)
	}
	if err := db.First(&afterPayment, payment.ID).Error; err != nil {
		t.Fatalf("reload payment failed: %v", err)
	}
	if afterPayment.Status != constants.PaymentStatusSuccess {
		t.Fatalf("payment status = %q, want success", afterPayment.Status)
	}
	if err := db.First(&afterOrder, order.ID).Error; err != nil {
		t.Fatalf("reload order failed: %v", err)
	}
	if afterOrder.Status != constants.OrderStatusPaid {
		t.Fatalf("order status = %q, want paid", afterOrder.Status)
	}
}

// 网关说"已支付"但金额对不上（少付/改价）同样不能发货。
func TestHandleSyncCallbackRejectsAmountMismatch(t *testing.T) {
	svc, db := setupPaymentServiceWalletTest(t)
	now := time.Now()

	srv := newVerifyTestGatewayServer(t, 1, "1.00")
	channel := newEpayVerifyChannel(srv.URL)

	order := &orderdomain.Order{
		OrderNo:                 "DJTESTVERIFY002",
		UserID:                  1,
		Status:                  constants.OrderStatusPendingPayment,
		Currency:                "CNY",
		OriginalAmount:          money.FromDecimal(decimal.NewFromInt(35)),
		DiscountAmount:          money.FromDecimal(decimal.Zero),
		PromotionDiscountAmount: money.FromDecimal(decimal.Zero),
		TotalAmount:             money.FromDecimal(decimal.NewFromInt(35)),
		WalletPaidAmount:        money.FromDecimal(decimal.Zero),
		OnlinePaidAmount:        money.FromDecimal(decimal.NewFromInt(35)),
		RefundedAmount:          money.FromDecimal(decimal.Zero),
		CreatedAt:               now,
		UpdatedAt:               now,
	}
	if err := db.Create(order).Error; err != nil {
		t.Fatalf("create order failed: %v", err)
	}
	payment := &paymentdomain.Payment{
		OrderID:         order.ID,
		ChannelID:       channel.ID,
		ProviderType:    constants.PaymentProviderEpay,
		ChannelType:     constants.PaymentChannelTypeAlipay,
		InteractionMode: constants.PaymentInteractionRedirect,
		Amount:          money.FromDecimal(decimal.RequireFromString("36.03")),
		FeeRate:         money.FromDecimal(decimal.Zero),
		FeeAmount:       money.FromDecimal(decimal.Zero),
		Currency:        "CNY",
		Status:          constants.PaymentStatusPending,
		GatewayOrderNo:  "DJP-VERIFY",
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := db.Create(payment).Error; err != nil {
		t.Fatalf("create payment failed: %v", err)
	}

	form := signEpayCallbackForm(verifyTestMerchantKey, map[string]string{
		"pid":          "56796329",
		"trade_no":     "PAY-REAL",
		"out_trade_no": "DJP-VERIFY",
		"type":         "alipay",
		"name":         "DJTESTVERIFY002",
		"money":        "36.03",
		"trade_status": "TRADE_SUCCESS",
	})

	if _, err := svc.HandleSyncCallback(channel, form, nil); !errors.Is(err, ErrPaymentCallbackUnverified) {
		t.Fatalf("amount mismatch error = %v, want ErrPaymentCallbackUnverified", err)
	}
	var afterPayment paymentdomain.Payment
	if err := db.First(&afterPayment, payment.ID).Error; err != nil {
		t.Fatalf("reload payment failed: %v", err)
	}
	if afterPayment.Status != constants.PaymentStatusPending {
		t.Fatalf("payment status = %q, want pending", afterPayment.Status)
	}
}

// noQueryProvider 只实现最小网关能力，没有主动查询，用来确认加固不会影响这类渠道。
type noQueryProvider struct{}

func (noQueryProvider) Type() string { return "test_no_query" }

func (noQueryProvider) ValidateConfig(jsonmap.JSON, string) error { return nil }

func (noQueryProvider) CreatePayment(context.Context, jsonmap.JSON, paymentcontract.GatewayCreateInput) (*paymentcontract.GatewayCreateResult, error) {
	return &paymentcontract.GatewayCreateResult{}, nil
}

// 不具备查询能力的渠道保持原有行为：复核被跳过而不是拦截，避免整站无法收款。
func TestCorroborateSuccessCallbackSkipsProvidersWithoutQueryCapability(t *testing.T) {
	svc, _ := setupPaymentServiceWalletTest(t)
	registerTestGateway(t, svc, "test_no_query", "test_channel", noQueryProvider{})

	channel := &paymentdomain.PaymentChannel{
		ID:           9,
		ProviderType: "test_no_query",
		ChannelType:  "test_channel",
	}
	payment := &paymentdomain.Payment{
		ID:             9,
		ChannelID:      9,
		Amount:         money.FromDecimal(decimal.NewFromInt(10)),
		Currency:       "CNY",
		Status:         constants.PaymentStatusPending,
		GatewayOrderNo: "DJP-NO-QUERY",
	}

	err := svc.corroborateSuccessCallback(channel, payment, PaymentCallbackInput{
		PaymentID: payment.ID,
		ChannelID: channel.ID,
		Status:    constants.PaymentStatusSuccess,
	})
	if err != nil {
		t.Fatalf("provider without query capability should be skipped, got: %v", err)
	}
}

// 非成功回调（pending/failed）不应该触发网关查询。
func TestCorroborateSuccessCallbackIgnoresNonSuccessStatus(t *testing.T) {
	svc, _ := setupPaymentServiceWalletTest(t)
	channel := newEpayVerifyChannel("http://127.0.0.1:1")
	payment := &paymentdomain.Payment{
		ID:        10,
		ChannelID: channel.ID,
		Amount:    money.FromDecimal(decimal.NewFromInt(10)),
		Currency:  "CNY",
		Status:    constants.PaymentStatusPending,
	}
	if err := svc.corroborateSuccessCallback(channel, payment, PaymentCallbackInput{
		PaymentID: payment.ID,
		ChannelID: channel.ID,
		Status:    constants.PaymentStatusFailed,
	}); err != nil {
		t.Fatalf("non-success callback must not be corroborated, got: %v", err)
	}
}

func TestHandleSyncCallbackRequiresBoundGatewayReceipt(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		paid       bool
	}{
		{"valid", `{"code":1,"status":1,"pid":"56796329","out_trade_no":"DJP-VERIFY","trade_no":"PAY-REAL","money":"36.03","currency":"CNY"}`, true},
		{"missing amount", `{"code":1,"status":1,"pid":"56796329","out_trade_no":"DJP-VERIFY","trade_no":"PAY-REAL"}`, false},
		{"invalid amount", `{"code":1,"status":1,"pid":"56796329","out_trade_no":"DJP-VERIFY","trade_no":"PAY-REAL","money":"bad"}`, false},
		{"zero amount", `{"code":1,"status":1,"pid":"56796329","out_trade_no":"DJP-VERIFY","trade_no":"PAY-REAL","money":"0"}`, false},
		{"negative amount", `{"code":1,"status":1,"pid":"56796329","out_trade_no":"DJP-VERIFY","trade_no":"PAY-REAL","money":"-1"}`, false},
		{"rejected query", `{"code":-1,"status":1,"pid":"56796329","out_trade_no":"DJP-VERIFY","trade_no":"PAY-REAL","money":"36.03"}`, false},
		{"wrong merchant", `{"code":1,"status":1,"pid":"999","out_trade_no":"DJP-VERIFY","trade_no":"PAY-REAL","money":"36.03"}`, false},
		{"wrong order", `{"code":1,"status":1,"pid":"56796329","out_trade_no":"OTHER","trade_no":"PAY-REAL","money":"36.03"}`, false},
		{"wrong transaction", `{"code":1,"status":1,"pid":"56796329","out_trade_no":"DJP-VERIFY","trade_no":"OTHER","money":"36.03"}`, false},
		{"wrong currency", `{"code":1,"status":1,"pid":"56796329","out_trade_no":"DJP-VERIFY","trade_no":"PAY-REAL","money":"36.03","currency":"USD"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, db := setupPaymentServiceWalletTest(t)
			queries := 0
			srv := newVerifyTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				queries++
				if r.PostFormValue("trade_no") != "PAY-REAL" || r.PostFormValue("key") != verifyTestMerchantKey || r.URL.Query().Get("key") != "" {
					t.Error("query must bind exact transaction and keep credentials in POST body")
				}
				_, _ = fmt.Fprint(w, tc.body)
			}))
			channel := newEpayVerifyChannel(srv.URL)
			order := &orderdomain.Order{
				OrderNo: "LOCAL-RECEIPT", UserID: 1, Status: constants.OrderStatusPendingPayment, Currency: "CNY",
				OriginalAmount: money.FromDecimal(decimal.NewFromInt(35)), TotalAmount: money.FromDecimal(decimal.NewFromInt(35)),
				OnlinePaidAmount: money.FromDecimal(decimal.NewFromInt(35)),
			}
			if err := db.Create(order).Error; err != nil {
				t.Fatal(err)
			}
			payment := &paymentdomain.Payment{
				OrderID: order.ID, ChannelID: channel.ID, ProviderType: channel.ProviderType, ChannelType: channel.ChannelType,
				InteractionMode: channel.InteractionMode, Amount: money.FromDecimal(decimal.RequireFromString("36.03")),
				FeeAmount: money.FromDecimal(decimal.RequireFromString("1.03")), Currency: "CNY", Status: constants.PaymentStatusPending,
				GatewayOrderNo: "DJP-VERIFY", ProviderRef: "DJP-VERIFY", // redirect placeholder must not override the actual transaction.
			}
			if err := db.Create(payment).Error; err != nil {
				t.Fatal(err)
			}
			form := signEpayCallbackForm(verifyTestMerchantKey, map[string]string{
				"pid": "56796329", "trade_no": "PAY-REAL", "out_trade_no": "DJP-VERIFY",
				"money": "36.03", "trade_status": "TRADE_SUCCESS", "type": "alipay",
			})
			_, err := svc.HandleSyncCallback(channel, form, nil)
			if tc.paid && err != nil || !tc.paid && !errors.Is(err, ErrPaymentCallbackUnverified) {
				t.Fatalf("callback err=%v, expect paid=%v", err, tc.paid)
			}
			var afterOrder orderdomain.Order
			var afterPayment paymentdomain.Payment
			if err := db.First(&afterOrder, order.ID).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.First(&afterPayment, payment.ID).Error; err != nil {
				t.Fatal(err)
			}
			if (afterOrder.Status == constants.OrderStatusPaid) != tc.paid || (afterPayment.Status == constants.PaymentStatusSuccess) != tc.paid {
				t.Fatalf("order=%s payment=%s, expect paid=%v", afterOrder.Status, afterPayment.Status, tc.paid)
			}
			if queries != 1 {
				t.Fatalf("queries=%d, want 1", queries)
			}
			if tc.paid {
				if _, err := svc.HandleSyncCallback(channel, form, nil); err != nil {
					t.Fatal(err)
				}
				if queries != 1 {
					t.Fatalf("replay queried gateway again: %d", queries)
				}
				if afterPayment.ProviderPayload["gateway_order_verified"] != true {
					t.Fatal("missing server-created verification marker")
				}
			}
		})
	}
}

// This double verifies that service-level safeguards cannot be bypassed by an incomplete adapter result.
type corroborationTestProvider struct {
	noQueryProvider
	result *paymentcontract.GatewayQueryResult
	err    error
}

func (p corroborationTestProvider) CorroboratePayment(context.Context, jsonmap.JSON, paymentcontract.CorroborateInput) (*paymentcontract.GatewayQueryResult, error) {
	return p.result, p.err
}

func TestCorroborationRequiresCompleteResultAndSupportedQuery(t *testing.T) {
	for _, name := range []string{"valid", "empty", "zero", "negative", "currency", "transaction", "unsupported", "gateway failure"} {
		t.Run(name, func(t *testing.T) {
			svc, _ := setupPaymentServiceWalletTest(t)
			result := &paymentcontract.GatewayQueryResult{Status: constants.PaymentStatusSuccess, Amount: money.FromDecimal(decimal.NewFromInt(10)), Currency: "CNY", ProviderRef: "REAL"}
			provider := corroborationTestProvider{result: result}
			switch name {
			case "empty":
				provider.result = nil
			case "zero":
				result.Amount = money.FromDecimal(decimal.Zero)
			case "negative":
				result.Amount = money.FromDecimal(decimal.NewFromInt(-1))
			case "currency":
				result.Currency = "USD"
			case "transaction":
				result.ProviderRef = "OTHER"
			case "unsupported":
				provider.err = paymentcontract.ErrGatewayCorroborationUnsupported
			case "gateway failure":
				provider.err = errors.New("gateway unavailable")
			}
			registerTestGateway(t, svc, "review", "query", provider)
			channel := &paymentdomain.PaymentChannel{ID: 1, ProviderType: "review", ChannelType: "query"}
			payment := &paymentdomain.Payment{ID: 1, ChannelID: 1, Status: constants.PaymentStatusPending, GatewayOrderNo: "ORDER", Amount: money.FromDecimal(decimal.NewFromInt(10)), Currency: "CNY"}
			err := svc.corroborateSuccessCallback(channel, payment, PaymentCallbackInput{PaymentID: 1, ChannelID: 1, OrderNo: "ORDER", ProviderRef: "REAL", Status: constants.PaymentStatusSuccess, Amount: payment.Amount, Currency: payment.Currency})
			if name == "valid" && err != nil || name != "valid" && !errors.Is(err, ErrPaymentCallbackUnverified) {
				t.Fatalf("unexpected verification result: %v", err)
			}
		})
	}
}

func TestHandleSyncCallbackLegacyRechargeRequiresReceiptAndCreditsOnce(t *testing.T) {
	svc, db := setupPaymentServiceWalletTest(t)
	payment, recharge := createWalletRechargeFixture(t, db, constants.PaymentStatusPending, constants.WalletRechargeStatusPending)
	paid := false
	queries := 0
	srv := newVerifyTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries++
		if r.PostFormValue("trade_no") != "REAL-RECHARGE" {
			t.Error("legacy recharge must query exact gateway transaction")
		}
		status := 0
		if paid {
			status = 1
		}
		_, _ = fmt.Fprintf(w, `{"code":1,"pid":"56796329","status":%d,"trade_no":"REAL-RECHARGE","out_trade_no":%q,"money":"88.00"}`, status, recharge.RechargeNo)
	}))
	channel := newEpayVerifyChannel(srv.URL)
	payment.ProviderType, payment.ChannelType, payment.ChannelID = channel.ProviderType, channel.ChannelType, channel.ID
	payment.ProviderRef, payment.GatewayOrderNo = "REAL-RECHARGE", ""
	if err := db.Save(payment).Error; err != nil {
		t.Fatal(err)
	}
	form := signEpayCallbackForm(verifyTestMerchantKey, map[string]string{
		"pid": "56796329", "trade_no": "REAL-RECHARGE", "out_trade_no": recharge.RechargeNo,
		"money": "88.00", "trade_status": "TRADE_SUCCESS", "type": "alipay",
	})
	if _, err := svc.HandleSyncCallback(channel, form, nil); !errors.Is(err, ErrPaymentCallbackUnverified) {
		t.Fatalf("unpaid recharge accepted: %v", err)
	}
	paid = true
	for i := 0; i < 2; i++ {
		if _, err := svc.HandleSyncCallback(channel, form, nil); err != nil {
			t.Fatal(err)
		}
	}
	assertWalletRechargeSuccessState(t, db, payment.ID, recharge.ID, recharge.UserID, recharge.Amount.Decimal)
	if queries != 2 {
		t.Fatalf("queries=%d, want unpaid + paid only", queries)
	}
}
