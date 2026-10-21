package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	"github.com/dujiao-next/internal/shared/money"
	"github.com/shopspring/decimal"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	paymentcontract "github.com/dujiao-next/internal/modules/payment/contract"

	paymentdomain "github.com/dujiao-next/internal/modules/payment/domain"

	"github.com/dujiao-next/internal/constants"
	"github.com/dujiao-next/internal/modules/payment/infrastructure/gateway/provider"
	"github.com/dujiao-next/internal/shared/jsonmap"
)

// recordingCapturer 捕获传入 ValidateConfig 的第二参数,用于回归测试。
type recordingCapturer struct {
	receivedMode string
}

func (r *recordingCapturer) Type() string { return "test_capturer" }

func (r *recordingCapturer) ValidateConfig(_ jsonmap.JSON, mode string) error {
	r.receivedMode = mode
	return errors.New("stop-test")
}

func (r *recordingCapturer) CreatePayment(_ context.Context, _ jsonmap.JSON, _ paymentcontract.GatewayCreateInput) (*paymentcontract.GatewayCreateResult, error) {
	return nil, errors.New("not-implemented")
}

func (r *recordingCapturer) QueryPayment(_ context.Context, _ jsonmap.JSON, _ string) (*paymentcontract.GatewayQueryResult, error) {
	return nil, errors.New("not-implemented")
}

// 防止重新引入 stripe/wechat capture 全部失败的回归:
// captureViaRegistry 必须把 channel.InteractionMode(而非 channel.ChannelType)
// 传给 Capturer.ValidateConfig,否则 stripe/wechat adapter 会判定 mode 非法。
func TestCaptureViaRegistry_PassesInteractionModeNotChannelType(t *testing.T) {
	cap := &recordingCapturer{}
	reg := provider.NewRegistry()
	reg.Register("test_provider", "test_channel", cap)

	svc := &PaymentService{paymentProviderRegistry: reg}

	channel := &paymentdomain.PaymentChannel{
		ID:              1,
		ProviderType:    "test_provider",
		ChannelType:     "test_channel",
		InteractionMode: constants.PaymentInteractionQR,
	}
	payment := &paymentdomain.Payment{
		ID:          1,
		ChannelID:   1,
		ProviderRef: "ref-123",
	}

	_, _ = svc.captureViaRegistry(CapturePaymentInput{}, payment, channel)

	if cap.receivedMode != channel.InteractionMode {
		t.Fatalf("ValidateConfig received mode=%q, want %q (channel.InteractionMode)",
			cap.receivedMode, channel.InteractionMode)
	}
	if cap.receivedMode == channel.ChannelType {
		t.Fatalf("regression: ValidateConfig received channel.ChannelType=%q", cap.receivedMode)
	}
}

// Real adapter + TLS mock gateway + single-connection SQLite exercise the same
// capture/settlement path used by authenticated polling and the background job.
func TestEpayReceiptCompensation(t *testing.T) {
	for _, name := range []string{"paid", "background", "unpaid", "missing", "wrong merchant", "wrong order", "underpaid", "wrong currency", "empty trade", "outage", "expired", "disabled", "canceled", "superseded"} {
		t.Run(name, func(t *testing.T) {
			svc, db := setupPaymentServiceWalletTest(t)
			sqlDB, _ := db.DB()
			sqlDB.SetMaxOpenConns(1)
			var queries atomic.Int32
			reply := map[string]any{"code": 1, "status": 1, "pid": "56796329", "out_trade_no": "DJP-RECEIPT", "trade_no": "REAL-RECEIPT", "money": "36.03", "currency": "CNY"}
			switch name {
			case "unpaid":
				reply["status"] = 0
			case "missing":
				reply = map[string]any{"code": -1, "msg": "订单不存在"}
			case "wrong merchant":
				reply["pid"] = "OTHER"
			case "wrong order":
				reply["out_trade_no"] = "OTHER"
			case "underpaid":
				reply["money"] = "0.01"
			case "wrong currency":
				reply["currency"] = "USD"
			case "empty trade":
				reply["trade_no"] = ""
			}
			srv := newVerifyTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				queries.Add(1)
				if r.Method != http.MethodPost || r.PostFormValue("out_trade_no") != "DJP-RECEIPT" || r.PostFormValue("trade_no") != "" || r.URL.Query().Get("key") != "" {
					t.Error("receipt lookup must select unique merchant payment number using POST")
				}
				if name == "outage" {
					w.WriteHeader(503)
					return
				}
				_ = json.NewEncoder(w).Encode(reply)
			}))
			channel := newEpayVerifyChannel(srv.URL)
			if err := db.Create(channel).Error; err != nil {
				t.Fatal(err)
			}
			if name == "disabled" {
				db.Model(channel).Update("is_active", false)
			}
			order := &orderdomain.Order{OrderNo: "LOCAL-RECEIPT", UserID: 1, Status: constants.OrderStatusPendingPayment, Currency: "CNY", OriginalAmount: money.FromDecimal(decimal.NewFromInt(35)), TotalAmount: money.FromDecimal(decimal.NewFromInt(35)), OnlinePaidAmount: money.FromDecimal(decimal.NewFromInt(35))}
			if name == "canceled" {
				order.Status = constants.OrderStatusCanceled
			}
			if err := db.Create(order).Error; err != nil {
				t.Fatal(err)
			}
			payment := &paymentdomain.Payment{OrderID: order.ID, ChannelID: channel.ID, ProviderType: channel.ProviderType, ChannelType: channel.ChannelType, InteractionMode: channel.InteractionMode, Amount: money.FromDecimal(decimal.RequireFromString("36.03")), FeeAmount: money.FromDecimal(decimal.RequireFromString("1.03")), Currency: "CNY", Status: constants.PaymentStatusPending, GatewayOrderNo: "DJP-RECEIPT", ProviderRef: "DJP-RECEIPT"}
			if name == "expired" {
				payment.Status = constants.PaymentStatusExpired
			}
			if name == "superseded" {
				now := time.Now()
				payment.SupersededAt = &now
			}
			if err := db.Create(payment).Error; err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			var err error
			if name == "background" {
				err = svc.ReconcilePendingReceipts(ctx)
			} else {
				_, err = svc.CapturePayment(CapturePaymentInput{PaymentID: payment.ID, Context: ctx})
			}
			// A fully funded old link may still settle an open order; the existing
			// locked settlement path records the superseded-link exception.
			paid := name == "paid" || name == "background" || name == "superseded"
			if paid && err != nil {
				t.Fatal(err)
			}
			var afterOrder orderdomain.Order
			var afterPayment paymentdomain.Payment
			if err := db.First(&afterOrder, order.ID).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.First(&afterPayment, payment.ID).Error; err != nil {
				t.Fatal(err)
			}
			if (afterOrder.Status == constants.OrderStatusPaid) != paid {
				t.Fatalf("order status=%s err=%v", afterOrder.Status, err)
			}
			if paid {
				if afterPayment.Status != constants.PaymentStatusSuccess || afterPayment.ProviderRef != "REAL-RECEIPT" || afterPayment.ProviderPayload["gateway_order_verified"] != true {
					t.Fatalf("unverified settlement: %+v", afterPayment)
				}
				_, err = svc.CapturePayment(CapturePaymentInput{PaymentID: payment.ID, Context: ctx})
				if err != nil || queries.Load() != 1 {
					t.Fatalf("replay must be idempotent: queries=%d err=%v", queries.Load(), err)
				}
			}
			if (name == "expired" || name == "disabled") && queries.Load() != 0 {
				t.Fatal("closed payment or disabled channel queried")
			}
		})
	}
}

func TestPendingReceiptCandidatesAreBoundedAndRotate(t *testing.T) {
	svc, db := setupPaymentServiceWalletTest(t)
	now := time.Now()
	past := now.Add(-time.Hour)
	for _, name := range []string{"eligible1", "expired", "old", "no number", "other provider", "closed", "deleted", "eligible2"} {
		p := paymentdomain.Payment{ProviderType: constants.PaymentProviderEpay, Status: constants.PaymentStatusPending, GatewayOrderNo: name, CreatedAt: now}
		switch name {
		case "expired":
			p.ExpiredAt = &past
		case "old":
			p.CreatedAt = now.Add(-25 * time.Hour)
		case "no number":
			p.GatewayOrderNo = ""
		case "other provider":
			p.ProviderType = constants.PaymentProviderOfficial
		case "closed":
			p.Status = constants.PaymentStatusExpired
		case "deleted":
			p.DeletedAt = &past
		}
		if err := db.Create(&p).Error; err != nil {
			t.Fatal(err)
		}
	}
	reader := svc.paymentRepo.(paymentcontract.PendingReceiptStore)
	first, err := reader.ListPendingReceiptPayments(0, 1, now)
	if err != nil || len(first) != 1 || first[0].GatewayOrderNo != "eligible1" {
		t.Fatalf("first page=%v err=%v", first, err)
	}
	second, err := reader.ListPendingReceiptPayments(first[0].ID, 1, now)
	if err != nil || len(second) != 1 || second[0].GatewayOrderNo != "eligible2" {
		t.Fatalf("second page=%v err=%v", second, err)
	}
	last, err := reader.ListPendingReceiptPayments(second[0].ID, 1, now)
	if err != nil || len(last) != 0 {
		t.Fatalf("final page=%v err=%v", last, err)
	}
}

func TestReceiptQueryThrottleBoundsOverlapAndMemory(t *testing.T) {
	s := &PaymentService{}
	now := time.Now()
	release, ok := s.acquireReceiptQuery(1, now)
	if !ok {
		t.Fatal("first query blocked")
	}
	if _, ok := s.acquireReceiptQuery(1, now.Add(10*time.Second)); ok {
		t.Fatal("in-flight query overlapped")
	}
	release()
	if _, ok := s.acquireReceiptQuery(1, now.Add(time.Second)); ok {
		t.Fatal("query cooldown skipped")
	}
	for id := uint(2); id <= 1000; id++ {
		release, ok := s.acquireReceiptQuery(id, now)
		if !ok {
			t.Fatal("capacity too small")
		}
		release()
	}
	if _, ok := s.acquireReceiptQuery(1001, now); ok {
		t.Fatal("memory bound exceeded")
	}
	release, ok = s.acquireReceiptQuery(1001, now.Add(4*time.Second))
	if !ok {
		t.Fatal("expired cooldowns were not reclaimed")
	}
	release()
}

func TestReceiptPollAndCallbackRechargeCreditOnce(t *testing.T) {
	svc, db := setupPaymentServiceWalletTest(t)
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	payment, recharge := createWalletRechargeFixture(t, db, constants.PaymentStatusPending, constants.WalletRechargeStatusPending)
	srv := newVerifyTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.PostFormValue("out_trade_no") != "DJP-RECHARGE" && r.PostFormValue("trade_no") != "REAL-RECHARGE" {
			t.Error("wrong receipt selection")
		}
		fmt.Fprint(w, `{"code":1,"status":1,"pid":"56796329","out_trade_no":"DJP-RECHARGE","trade_no":"REAL-RECHARGE","money":"88.00"}`)
	}))
	channel := newEpayVerifyChannel(srv.URL)
	if err := db.Create(channel).Error; err != nil {
		t.Fatal(err)
	}
	payment.ChannelID = channel.ID
	payment.ProviderType = channel.ProviderType
	payment.ChannelType = channel.ChannelType
	payment.GatewayOrderNo = "DJP-RECHARGE"
	payment.ProviderRef = "DJP-RECHARGE"
	if err := db.Save(payment).Error; err != nil {
		t.Fatal(err)
	}
	form := signEpayCallbackForm(verifyTestMerchantKey, map[string]string{"pid": "56796329", "trade_no": "REAL-RECHARGE", "out_trade_no": "DJP-RECHARGE", "money": "88.00", "trade_status": "TRADE_SUCCESS", "type": "alipay"})
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var err error
			if i == 0 {
				_, err = svc.HandleSyncCallback(channel, form, nil)
			} else {
				_, err = svc.CapturePayment(CapturePaymentInput{PaymentID: payment.ID, Context: context.Background()})
			}
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	assertWalletRechargeSuccessState(t, db, payment.ID, recharge.ID, recharge.UserID, recharge.Amount.Decimal)
	var count int64
	if err := db.Table("wallet_transactions").Where("reference = ?", fmt.Sprintf("recharge:%d:success", recharge.ID)).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("duplicate recharge credit: %d", count)
	}
}
