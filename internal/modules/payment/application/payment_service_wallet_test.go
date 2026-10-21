package application

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	alipayadapter "github.com/dujiao-next/internal/modules/payment/infrastructure/gateway/adapters/alipay"
	bepusdtadapter "github.com/dujiao-next/internal/modules/payment/infrastructure/gateway/adapters/bepusdt"
	dujiaopayadapter "github.com/dujiao-next/internal/modules/payment/infrastructure/gateway/adapters/dujiaopay"
	epayadapter "github.com/dujiao-next/internal/modules/payment/infrastructure/gateway/adapters/epay"
	epusdtadapter "github.com/dujiao-next/internal/modules/payment/infrastructure/gateway/adapters/epusdt"
	okpayadapter "github.com/dujiao-next/internal/modules/payment/infrastructure/gateway/adapters/okpay"
	paypaladapter "github.com/dujiao-next/internal/modules/payment/infrastructure/gateway/adapters/paypal"
	stripeadapter "github.com/dujiao-next/internal/modules/payment/infrastructure/gateway/adapters/stripe"
	tokenpayadapter "github.com/dujiao-next/internal/modules/payment/infrastructure/gateway/adapters/tokenpay"
	wechatpayadapter "github.com/dujiao-next/internal/modules/payment/infrastructure/gateway/adapters/wechatpay"

	paymentgormstore "github.com/dujiao-next/internal/modules/payment/infrastructure/gormstore"

	paymentdomain "github.com/dujiao-next/internal/modules/payment/domain"

	fulfillmentdomain "github.com/dujiao-next/internal/modules/fulfillment/domain"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	ordergormstore "github.com/dujiao-next/internal/modules/order/infrastructure/gormstore"

	walletdomain "github.com/dujiao-next/internal/modules/wallet/domain"

	productdomain "github.com/dujiao-next/internal/modules/catalog/product/domain"
	productgormstore "github.com/dujiao-next/internal/modules/catalog/product/store/gormstore"

	userdomain "github.com/dujiao-next/internal/modules/identity/user/domain"
	walletapp "github.com/dujiao-next/internal/modules/wallet/application"
	walletcontract "github.com/dujiao-next/internal/modules/wallet/contract"
	walletgormstore "github.com/dujiao-next/internal/modules/wallet/infrastructure/gormstore"

	"github.com/dujiao-next/internal/constants"
	paymentcontract "github.com/dujiao-next/internal/modules/payment/contract"
	"github.com/dujiao-next/internal/modules/payment/infrastructure/gateway/provider"
	"github.com/dujiao-next/internal/platform/database/gormdb"
	"github.com/dujiao-next/internal/shared/jsonmap"
	"github.com/dujiao-next/internal/shared/money"

	"github.com/glebarez/sqlite"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

func setupPaymentServiceWalletTest(t *testing.T) (*PaymentService, *gorm.DB) {
	t.Helper()
	dsn := fmt.Sprintf("file:payment_service_wallet_test_%d?mode=memory&cache=shared", time.Now().UnixNano())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite failed: %v", err)
	}
	if err := db.AutoMigrate(
		&userdomain.User{},
		&orderdomain.Order{},
		&orderdomain.OrderItem{},
		&fulfillmentdomain.Fulfillment{},
		&productdomain.Product{},
		&productdomain.ProductSKU{},
		&walletdomain.Account{},
		&walletdomain.Transaction{},
		&walletdomain.RechargeOrder{},
		&paymentdomain.PaymentChannel{},
		&paymentdomain.Payment{},
	); err != nil {
		t.Fatalf("auto migrate failed: %v", err)
	}
	gormdb.DB = db

	orderRepo := ordergormstore.New(db, "test-guest-credential-secret-with-32-bytes")
	productRepo := productgormstore.NewProductStore(db)
	productSKURepo := productgormstore.NewSKUStore(db)
	paymentRepo := paymentgormstore.New(db, "test-guest-credential-secret-with-32-bytes")
	channelRepo := paymentgormstore.NewChannelStore(db)
	walletRepo := walletgormstore.New(db)
	walletSvc := walletapp.NewService(walletapp.Options{
		Repository:   walletRepo,
		Transactions: walletRepo,
	})

	// 构建与 Container.BuildRunner 相同的 PaymentProviderRegistry，
	// 确保 applyProviderPayment 通过 Registry 路由时各 adapter 可以被找到。
	reg := provider.NewRegistry()
	reg.Register(constants.PaymentProviderOfficial, constants.PaymentChannelTypeStripe, stripeadapter.NewStripeAdapter())
	reg.Register(constants.PaymentProviderOfficial, constants.PaymentChannelTypePaypal, paypaladapter.NewPaypalAdapter())
	reg.Register(constants.PaymentProviderOfficial, constants.PaymentChannelTypeWechat, wechatpayadapter.NewWechatpayAdapter())
	reg.Register(constants.PaymentProviderOfficial, constants.PaymentChannelTypeAlipay, alipayadapter.NewAlipayAdapter())
	reg.Register(constants.PaymentProviderEpay, "", epayadapter.NewEpayAdapter())
	reg.Register(constants.PaymentProviderEpusdt, "", epusdtadapter.NewEpusdtAdapter())
	reg.Register(constants.PaymentProviderBepusdt, "", bepusdtadapter.NewBepusdtAdapter())
	reg.Register(constants.PaymentProviderDujiaoPay, "", dujiaopayadapter.NewDujiaoPayAdapter())
	reg.Register(constants.PaymentProviderTokenpay, "", tokenpayadapter.NewTokenpayAdapter())
	reg.Register(constants.PaymentProviderOkpay, "", okpayadapter.NewOkpayAdapter())

	paymentSvc := NewPaymentService(PaymentServiceOptions{
		OrderStore:              orderRepo,
		ProductRepo:             productRepo,
		ProductSKURepo:          productSKURepo,
		PaymentStore:            paymentRepo,
		ChannelStore:            channelRepo,
		WalletRepo:              walletRepo,
		WalletService:           walletSvc,
		ExpireMinutes:           15,
		PaymentProviderRegistry: reg,
	})

	return paymentSvc, db
}

func TestCreatePaymentWalletFullAmountCreatesPaymentRecord(t *testing.T) {
	svc, db := setupPaymentServiceWalletTest(t)
	now := time.Now()

	user := &userdomain.User{
		Email:        "wallet_pay_user@example.com",
		PasswordHash: "hash",
		Status:       constants.UserStatusActive,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := db.Create(user).Error; err != nil {
		t.Fatalf("create user failed: %v", err)
	}

	order := &orderdomain.Order{
		OrderNo:                 "DJTESTWALLETPAY001",
		UserID:                  user.ID,
		Status:                  constants.OrderStatusPendingPayment,
		Currency:                "CNY",
		OriginalAmount:          money.FromDecimal(decimal.NewFromInt(50)),
		DiscountAmount:          money.FromDecimal(decimal.Zero),
		PromotionDiscountAmount: money.FromDecimal(decimal.Zero),
		TotalAmount:             money.FromDecimal(decimal.NewFromInt(50)),
		WalletPaidAmount:        money.FromDecimal(decimal.Zero),
		OnlinePaidAmount:        money.FromDecimal(decimal.NewFromInt(50)),
		RefundedAmount:          money.FromDecimal(decimal.Zero),
		CreatedAt:               now,
		UpdatedAt:               now,
	}
	if err := db.Create(order).Error; err != nil {
		t.Fatalf("create order failed: %v", err)
	}

	account := &walletdomain.Account{
		UserID:    user.ID,
		Balance:   money.FromDecimal(decimal.NewFromInt(100)),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := db.Create(account).Error; err != nil {
		t.Fatalf("create wallet account failed: %v", err)
	}

	result, err := svc.CreatePayment(CreatePaymentInput{
		OrderID:    order.ID,
		ChannelID:  0,
		UseBalance: true,
	})
	if err != nil {
		t.Fatalf("create payment failed: %v", err)
	}
	if !result.OrderPaid {
		t.Fatalf("expected order_paid=true")
	}
	if result.Payment != nil {
		t.Fatalf("expected response payment to be nil for wallet full payment")
	}
	if !result.WalletPaidAmount.Decimal.Equal(decimal.NewFromInt(50)) {
		t.Fatalf("wallet_paid_amount want 50 got %s", result.WalletPaidAmount.String())
	}
	if !result.OnlinePayAmount.Decimal.Equal(decimal.Zero) {
		t.Fatalf("online_pay_amount want 0 got %s", result.OnlinePayAmount.String())
	}

	var payment paymentdomain.Payment
	if err := db.Where("order_id = ?", order.ID).First(&payment).Error; err != nil {
		t.Fatalf("wallet payment record not found: %v", err)
	}
	if payment.ProviderType != constants.PaymentProviderWallet {
		t.Fatalf("provider_type want %s got %s", constants.PaymentProviderWallet, payment.ProviderType)
	}
	if payment.ChannelType != constants.PaymentChannelTypeBalance {
		t.Fatalf("channel_type want %s got %s", constants.PaymentChannelTypeBalance, payment.ChannelType)
	}
	if payment.InteractionMode != constants.PaymentInteractionBalance {
		t.Fatalf("interaction_mode want %s got %s", constants.PaymentInteractionBalance, payment.InteractionMode)
	}
	if payment.ChannelID != 0 {
		t.Fatalf("channel_id want 0 got %d", payment.ChannelID)
	}
	if payment.Status != constants.PaymentStatusSuccess {
		t.Fatalf("payment status want %s got %s", constants.PaymentStatusSuccess, payment.Status)
	}
	if !payment.Amount.Decimal.Equal(decimal.NewFromInt(50)) {
		t.Fatalf("payment amount want 50 got %s", payment.Amount.String())
	}
	if payment.PaidAt == nil {
		t.Fatalf("wallet payment should set paid_at")
	}

	var refreshedOrder orderdomain.Order
	if err := db.First(&refreshedOrder, order.ID).Error; err != nil {
		t.Fatalf("reload order failed: %v", err)
	}
	if refreshedOrder.Status != constants.OrderStatusPaid {
		t.Fatalf("order status want %s got %s", constants.OrderStatusPaid, refreshedOrder.Status)
	}
	if refreshedOrder.PaidAt == nil {
		t.Fatalf("order should set paid_at")
	}

	var refreshedAccount walletdomain.Account
	if err := db.Where("user_id = ?", user.ID).First(&refreshedAccount).Error; err != nil {
		t.Fatalf("reload wallet account failed: %v", err)
	}
	if !refreshedAccount.Balance.Decimal.Equal(decimal.NewFromInt(50)) {
		t.Fatalf("wallet balance want 50 got %s", refreshedAccount.Balance.String())
	}
}

func TestExpireWalletRechargePaymentPendingToExpired(t *testing.T) {
	svc, db := setupPaymentServiceWalletTest(t)
	payment, recharge := createWalletRechargeFixture(t, db, constants.PaymentStatusPending, constants.WalletRechargeStatusPending)

	updated, err := svc.ExpireWalletRechargePayment(payment.ID)
	if err != nil {
		t.Fatalf("expire wallet recharge payment failed: %v", err)
	}
	if updated == nil {
		t.Fatalf("expected updated payment")
	}
	if updated.Status != constants.PaymentStatusExpired {
		t.Fatalf("payment status want %s got %s", constants.PaymentStatusExpired, updated.Status)
	}
	if updated.ExpiredAt == nil {
		t.Fatalf("expected payment expired_at set")
	}

	var refreshedPayment paymentdomain.Payment
	if err := db.First(&refreshedPayment, payment.ID).Error; err != nil {
		t.Fatalf("reload payment failed: %v", err)
	}
	if refreshedPayment.Status != constants.PaymentStatusExpired {
		t.Fatalf("reloaded payment status want %s got %s", constants.PaymentStatusExpired, refreshedPayment.Status)
	}
	if refreshedPayment.ExpiredAt == nil {
		t.Fatalf("reloaded payment expected expired_at set")
	}

	var refreshedRecharge walletdomain.RechargeOrder
	if err := db.First(&refreshedRecharge, recharge.ID).Error; err != nil {
		t.Fatalf("reload recharge failed: %v", err)
	}
	if refreshedRecharge.Status != constants.WalletRechargeStatusExpired {
		t.Fatalf("recharge status want %s got %s", constants.WalletRechargeStatusExpired, refreshedRecharge.Status)
	}
}

func TestExpireWalletRechargePaymentDoesNotOverrideSuccess(t *testing.T) {
	svc, db := setupPaymentServiceWalletTest(t)
	payment, recharge := createWalletRechargeFixture(t, db, constants.PaymentStatusSuccess, constants.WalletRechargeStatusSuccess)

	updated, err := svc.ExpireWalletRechargePayment(payment.ID)
	if err != nil {
		t.Fatalf("expire wallet recharge payment failed: %v", err)
	}
	if updated == nil {
		t.Fatalf("expected updated payment")
	}
	if updated.Status != constants.PaymentStatusSuccess {
		t.Fatalf("payment status want %s got %s", constants.PaymentStatusSuccess, updated.Status)
	}

	var refreshedPayment paymentdomain.Payment
	if err := db.First(&refreshedPayment, payment.ID).Error; err != nil {
		t.Fatalf("reload payment failed: %v", err)
	}
	if refreshedPayment.Status != constants.PaymentStatusSuccess {
		t.Fatalf("reloaded payment status want %s got %s", constants.PaymentStatusSuccess, refreshedPayment.Status)
	}
	if refreshedPayment.PaidAt == nil {
		t.Fatalf("success payment should keep paid_at")
	}

	var refreshedRecharge walletdomain.RechargeOrder
	if err := db.First(&refreshedRecharge, recharge.ID).Error; err != nil {
		t.Fatalf("reload recharge failed: %v", err)
	}
	if refreshedRecharge.Status != constants.WalletRechargeStatusSuccess {
		t.Fatalf("recharge status want %s got %s", constants.WalletRechargeStatusSuccess, refreshedRecharge.Status)
	}
}

func TestExpireWalletRechargePaymentSkipsOrderPayment(t *testing.T) {
	svc, db := setupPaymentServiceWalletTest(t)
	now := time.Now()
	payment := &paymentdomain.Payment{
		OrderID:         99,
		ChannelID:       1,
		ProviderType:    constants.PaymentProviderOfficial,
		ChannelType:     constants.PaymentChannelTypeWechat,
		InteractionMode: constants.PaymentInteractionQR,
		Amount:          money.FromDecimal(decimal.NewFromInt(100)),
		FeeRate:         money.FromDecimal(decimal.Zero),
		FeeAmount:       money.FromDecimal(decimal.Zero),
		Currency:        "CNY",
		Status:          constants.PaymentStatusPending,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := db.Create(payment).Error; err != nil {
		t.Fatalf("create payment failed: %v", err)
	}

	updated, err := svc.ExpireWalletRechargePayment(payment.ID)
	if err != nil {
		t.Fatalf("expire wallet recharge payment failed: %v", err)
	}
	if updated == nil {
		t.Fatalf("expected payment result")
	}
	if updated.Status != constants.PaymentStatusPending {
		t.Fatalf("order payment should remain pending, got %s", updated.Status)
	}
}

func TestWalletRechargeCallbackSuccessAfterExpireCreditsOnce(t *testing.T) {
	svc, db := setupPaymentServiceWalletTest(t)
	payment, recharge := createWalletRechargeFixture(t, db, constants.PaymentStatusPending, constants.WalletRechargeStatusPending)

	if _, err := svc.ExpireWalletRechargePayment(payment.ID); err != nil {
		t.Fatalf("expire wallet recharge payment failed: %v", err)
	}

	updated, err := svc.HandleCallback(buildWalletRechargeCallbackInput(payment, recharge, constants.PaymentStatusSuccess, "CALLBACK-SUCCESS-1"))
	if err != nil {
		t.Fatalf("handle wallet recharge callback failed: %v", err)
	}
	if updated.Status != constants.PaymentStatusSuccess {
		t.Fatalf("payment status want %s got %s", constants.PaymentStatusSuccess, updated.Status)
	}

	assertWalletRechargeSuccessState(t, db, payment.ID, recharge.ID, recharge.UserID, recharge.Amount.Decimal)
}

func TestWalletRechargeCallbackSuccessThenExpireKeepsSuccess(t *testing.T) {
	svc, db := setupPaymentServiceWalletTest(t)
	payment, recharge := createWalletRechargeFixture(t, db, constants.PaymentStatusPending, constants.WalletRechargeStatusPending)

	if _, err := svc.HandleCallback(buildWalletRechargeCallbackInput(payment, recharge, constants.PaymentStatusSuccess, "CALLBACK-SUCCESS-2")); err != nil {
		t.Fatalf("handle wallet recharge callback failed: %v", err)
	}

	updated, err := svc.ExpireWalletRechargePayment(payment.ID)
	if err != nil {
		t.Fatalf("expire wallet recharge payment failed: %v", err)
	}
	if updated.Status != constants.PaymentStatusSuccess {
		t.Fatalf("payment status want %s got %s", constants.PaymentStatusSuccess, updated.Status)
	}

	assertWalletRechargeSuccessState(t, db, payment.ID, recharge.ID, recharge.UserID, recharge.Amount.Decimal)
}

func TestWalletRechargeCallbackDuplicateSuccessDoesNotDuplicateCredit(t *testing.T) {
	svc, db := setupPaymentServiceWalletTest(t)
	payment, recharge := createWalletRechargeFixture(t, db, constants.PaymentStatusPending, constants.WalletRechargeStatusPending)

	if _, err := svc.HandleCallback(buildWalletRechargeCallbackInput(payment, recharge, constants.PaymentStatusSuccess, "CALLBACK-SUCCESS-FIRST")); err != nil {
		t.Fatalf("handle first wallet recharge callback failed: %v", err)
	}
	if _, err := svc.HandleCallback(buildWalletRechargeCallbackInput(payment, recharge, constants.PaymentStatusSuccess, "CALLBACK-SUCCESS-SECOND")); err != nil {
		t.Fatalf("handle second wallet recharge callback failed: %v", err)
	}

	assertWalletRechargeSuccessState(t, db, payment.ID, recharge.ID, recharge.UserID, recharge.Amount.Decimal)

	reference := fmt.Sprintf("recharge:%d:success", recharge.ID)
	var txnCount int64
	if err := db.Model(&walletdomain.Transaction{}).Where("reference = ?", reference).Count(&txnCount).Error; err != nil {
		t.Fatalf("count wallet transaction failed: %v", err)
	}
	if txnCount != 1 {
		t.Fatalf("wallet recharge success transaction count want 1 got %d", txnCount)
	}
}

func TestWalletRechargeCallbackConcurrentSuccessCreditsOnce(t *testing.T) {
	svc, db := setupPaymentServiceWalletTest(t)
	payment, recharge := createWalletRechargeFixture(t, db, constants.PaymentStatusPending, constants.WalletRechargeStatusPending)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get raw db: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)

	const callbacks = 2
	var wait sync.WaitGroup
	errorsCh := make(chan error, callbacks)
	for index := 0; index < callbacks; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			input := buildWalletRechargeCallbackInput(
				payment,
				recharge,
				constants.PaymentStatusSuccess,
				fmt.Sprintf("CALLBACK-CONCURRENT-%d", index),
			)
			if _, callbackErr := svc.HandleCallback(input); callbackErr != nil {
				errorsCh <- callbackErr
			}
		}(index)
	}
	wait.Wait()
	close(errorsCh)
	for callbackErr := range errorsCh {
		t.Errorf("concurrent callback failed: %v", callbackErr)
	}

	assertWalletRechargeSuccessState(t, db, payment.ID, recharge.ID, recharge.UserID, recharge.Amount.Decimal)
	reference := fmt.Sprintf("recharge:%d:success", recharge.ID)
	var transactionCount int64
	if err := db.Model(&walletdomain.Transaction{}).Where("reference = ?", reference).Count(&transactionCount).Error; err != nil {
		t.Fatalf("count wallet transaction: %v", err)
	}
	if transactionCount != 1 {
		t.Fatalf("concurrent success callbacks created %d credits, want 1", transactionCount)
	}
}

func TestWalletRechargeCallbackPendingAfterExpireDoesNotReopen(t *testing.T) {
	svc, db := setupPaymentServiceWalletTest(t)
	payment, recharge := createWalletRechargeFixture(t, db, constants.PaymentStatusPending, constants.WalletRechargeStatusPending)

	if _, err := svc.ExpireWalletRechargePayment(payment.ID); err != nil {
		t.Fatalf("expire wallet recharge payment failed: %v", err)
	}
	updated, err := svc.HandleCallback(buildWalletRechargeCallbackInput(payment, recharge, constants.PaymentStatusPending, "CALLBACK-PENDING-LATE"))
	if err != nil {
		t.Fatalf("handle wallet recharge callback failed: %v", err)
	}
	if updated.Status != constants.PaymentStatusExpired {
		t.Fatalf("payment status want %s got %s", constants.PaymentStatusExpired, updated.Status)
	}

	var refreshedRecharge walletdomain.RechargeOrder
	if err := db.First(&refreshedRecharge, recharge.ID).Error; err != nil {
		t.Fatalf("reload recharge failed: %v", err)
	}
	if refreshedRecharge.Status != constants.WalletRechargeStatusExpired {
		t.Fatalf("recharge status want %s got %s", constants.WalletRechargeStatusExpired, refreshedRecharge.Status)
	}
}

func TestWalletRechargeCallbackTerminalStateMatrixDoesNotReopen(t *testing.T) {
	cases := []struct {
		name              string
		paymentStatus     string
		rechargeStatus    string
		callbackStatus    string
		wantPaymentStatus string
		wantRechargeState string
	}{
		{
			name:              "expired_to_pending",
			paymentStatus:     constants.PaymentStatusExpired,
			rechargeStatus:    constants.WalletRechargeStatusExpired,
			callbackStatus:    constants.PaymentStatusPending,
			wantPaymentStatus: constants.PaymentStatusExpired,
			wantRechargeState: constants.WalletRechargeStatusExpired,
		},
		{
			name:              "failed_to_pending",
			paymentStatus:     constants.PaymentStatusFailed,
			rechargeStatus:    constants.WalletRechargeStatusFailed,
			callbackStatus:    constants.PaymentStatusPending,
			wantPaymentStatus: constants.PaymentStatusFailed,
			wantRechargeState: constants.WalletRechargeStatusFailed,
		},
		{
			name:              "failed_to_expired",
			paymentStatus:     constants.PaymentStatusFailed,
			rechargeStatus:    constants.WalletRechargeStatusFailed,
			callbackStatus:    constants.PaymentStatusExpired,
			wantPaymentStatus: constants.PaymentStatusFailed,
			wantRechargeState: constants.WalletRechargeStatusFailed,
		},
		{
			name:              "expired_to_failed",
			paymentStatus:     constants.PaymentStatusExpired,
			rechargeStatus:    constants.WalletRechargeStatusExpired,
			callbackStatus:    constants.PaymentStatusFailed,
			wantPaymentStatus: constants.PaymentStatusExpired,
			wantRechargeState: constants.WalletRechargeStatusExpired,
		},
		{
			name:              "success_to_pending",
			paymentStatus:     constants.PaymentStatusSuccess,
			rechargeStatus:    constants.WalletRechargeStatusSuccess,
			callbackStatus:    constants.PaymentStatusPending,
			wantPaymentStatus: constants.PaymentStatusSuccess,
			wantRechargeState: constants.WalletRechargeStatusSuccess,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, db := setupPaymentServiceWalletTest(t)
			payment, recharge := createWalletRechargeFixture(t, db, tc.paymentStatus, tc.rechargeStatus)

			providerRef := fmt.Sprintf("CALLBACK-%s-%d", tc.name, time.Now().UnixNano())
			updated, err := svc.HandleCallback(buildWalletRechargeCallbackInput(payment, recharge, tc.callbackStatus, providerRef))
			if err != nil {
				t.Fatalf("handle wallet recharge callback failed: %v", err)
			}
			if updated.Status != tc.wantPaymentStatus {
				t.Fatalf("payment status want %s got %s", tc.wantPaymentStatus, updated.Status)
			}

			var refreshedPayment paymentdomain.Payment
			if err := db.First(&refreshedPayment, payment.ID).Error; err != nil {
				t.Fatalf("reload payment failed: %v", err)
			}
			if refreshedPayment.Status != tc.wantPaymentStatus {
				t.Fatalf("reloaded payment status want %s got %s", tc.wantPaymentStatus, refreshedPayment.Status)
			}

			var refreshedRecharge walletdomain.RechargeOrder
			if err := db.First(&refreshedRecharge, recharge.ID).Error; err != nil {
				t.Fatalf("reload recharge failed: %v", err)
			}
			if refreshedRecharge.Status != tc.wantRechargeState {
				t.Fatalf("recharge status want %s got %s", tc.wantRechargeState, refreshedRecharge.Status)
			}
		})
	}
}

func TestWalletRechargeCallbackSuccessAfterFailedCreditsOnce(t *testing.T) {
	svc, db := setupPaymentServiceWalletTest(t)
	payment, recharge := createWalletRechargeFixture(t, db, constants.PaymentStatusFailed, constants.WalletRechargeStatusFailed)

	updated, err := svc.HandleCallback(buildWalletRechargeCallbackInput(payment, recharge, constants.PaymentStatusSuccess, "CALLBACK-SUCCESS-AFTER-FAILED"))
	if err != nil {
		t.Fatalf("handle wallet recharge callback failed: %v", err)
	}
	if updated.Status != constants.PaymentStatusSuccess {
		t.Fatalf("payment status want %s got %s", constants.PaymentStatusSuccess, updated.Status)
	}

	assertWalletRechargeSuccessState(t, db, payment.ID, recharge.ID, recharge.UserID, recharge.Amount.Decimal)

	reference := fmt.Sprintf("recharge:%d:success", recharge.ID)
	var txnCount int64
	if err := db.Model(&walletdomain.Transaction{}).Where("reference = ?", reference).Count(&txnCount).Error; err != nil {
		t.Fatalf("count wallet transaction failed: %v", err)
	}
	if txnCount != 1 {
		t.Fatalf("wallet recharge success transaction count want 1 got %d", txnCount)
	}
}

type failWalletRechargeReloadRepository struct {
	walletcontract.Repository
	getCalls int
}

func (r *failWalletRechargeReloadRepository) GetRechargeOrderByPaymentID(paymentID uint) (*walletdomain.RechargeOrder, error) {
	r.getCalls++
	if r.getCalls > 1 {
		return nil, fmt.Errorf("forced recharge reload failure")
	}
	return r.Repository.GetRechargeOrderByPaymentID(paymentID)
}

type recordingMemberLevelProgressor struct {
	rechargeUserID uint
	rechargeAmount decimal.Decimal
}

func (r *recordingMemberLevelProgressor) OnOrderPaid(uint, decimal.Decimal) error {
	return nil
}

func (r *recordingMemberLevelProgressor) OnRechargeCompleted(userID uint, amount decimal.Decimal) error {
	r.rechargeUserID = userID
	r.rechargeAmount = amount
	return nil
}

func TestWalletRechargeCallbackReloadFailureKeepsNotificationContext(t *testing.T) {
	svc, db := setupPaymentServiceWalletTest(t)
	payment, recharge := createWalletRechargeFixture(t, db, constants.PaymentStatusPending, constants.WalletRechargeStatusPending)
	svc.walletRepo = &failWalletRechargeReloadRepository{Repository: svc.walletRepo}
	progressor := &recordingMemberLevelProgressor{}
	svc.SetMemberLevelService(progressor)

	if _, err := svc.HandleCallback(buildWalletRechargeCallbackInput(payment, recharge, constants.PaymentStatusSuccess, "CALLBACK-RELOAD-FAILURE")); err != nil {
		t.Fatalf("HandleCallback failed: %v", err)
	}
	if progressor.rechargeUserID != recharge.UserID {
		t.Fatalf("fallback recharge user id = %d, want %d", progressor.rechargeUserID, recharge.UserID)
	}
	if !progressor.rechargeAmount.Equal(recharge.Amount.Decimal) {
		t.Fatalf("fallback recharge amount = %s, want %s", progressor.rechargeAmount, recharge.Amount.String())
	}
}

func TestHandleDujiaoPayWebhookAppliesVerifiedWalletRecharge(t *testing.T) {
	svc, db := setupPaymentServiceWalletTest(t)
	channel, payment, recharge := createDujiaoPayWalletWebhookFixture(t, db)

	body := []byte(fmt.Sprintf(
		`{"event_id":"evt-wallet-paid","event_type":"order.paid","event_version":"v1","created_at":"2026-07-27T00:00:00Z","data":{"order_id":%q,"merchant_order_id":%q,"fiat_currency":"CNY","fiat_amount":"88.00","tx_id":"0xwallet","paid_at":"2026-07-27T00:00:01Z"}}`,
		payment.ProviderRef,
		recharge.RechargeNo,
	))
	headers := signDujiaoPayWebhook(body, "test-dujiaopay-webhook-secret")

	updated, status, err := svc.HandleDujiaoPayWebhook(WebhookCallbackInput{
		ChannelID: channel.ID,
		Headers:   headers,
		Body:      body,
		Context:   context.Background(),
	})
	if err != nil {
		t.Fatalf("HandleDujiaoPayWebhook failed: %v", err)
	}
	if status != constants.PaymentStatusSuccess || updated == nil || updated.Status != constants.PaymentStatusSuccess {
		t.Fatalf("webhook result status=%q payment=%+v", status, updated)
	}
	assertWalletRechargeSuccessState(t, db, payment.ID, recharge.ID, recharge.UserID, recharge.Amount.Decimal)
}

func TestHandleDujiaoPayWebhookMarksOrderPaid(t *testing.T) {
	svc, db := setupPaymentServiceWalletTest(t)
	channel := createDujiaoPayChannelFixture(t, db)
	now := time.Now()
	order := &orderdomain.Order{
		OrderNo:          fmt.Sprintf("DJ-DUJIAOPAY-%d", now.UnixNano()),
		UserID:           1,
		Status:           constants.OrderStatusPendingPayment,
		Currency:         "CNY",
		OriginalAmount:   money.FromDecimal(decimal.NewFromInt(88)),
		TotalAmount:      money.FromDecimal(decimal.NewFromInt(88)),
		OnlinePaidAmount: money.FromDecimal(decimal.NewFromInt(88)),
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := db.Create(order).Error; err != nil {
		t.Fatalf("create order failed: %v", err)
	}
	payment := &paymentdomain.Payment{
		OrderID:         order.ID,
		ChannelID:       channel.ID,
		ProviderType:    constants.PaymentProviderDujiaoPay,
		ChannelType:     channel.ChannelType,
		InteractionMode: constants.PaymentInteractionRedirect,
		Amount:          money.FromDecimal(decimal.NewFromInt(88)),
		Currency:        "CNY",
		Status:          constants.PaymentStatusPending,
		GatewayOrderNo:  fmt.Sprintf("PAY-DUJIAOPAY-%d", now.UnixNano()),
		ProviderRef:     fmt.Sprintf("do_order_%d", now.UnixNano()),
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := db.Create(payment).Error; err != nil {
		t.Fatalf("create payment failed: %v", err)
	}

	body := []byte(fmt.Sprintf(
		`{"event_id":"evt-order-paid","event_type":"order.paid","event_version":"v1","created_at":"2026-07-27T00:00:00Z","data":{"order_id":%q,"merchant_order_id":%q,"fiat_currency":"CNY","fiat_amount":"88.00","tx_id":"0xorder"}}`,
		payment.ProviderRef,
		payment.GatewayOrderNo,
	))
	updated, status, err := svc.HandleDujiaoPayWebhook(WebhookCallbackInput{
		ChannelID: channel.ID,
		Headers:   signDujiaoPayWebhook(body, "test-dujiaopay-webhook-secret"),
		Body:      body,
		Context:   context.Background(),
	})
	if err != nil {
		t.Fatalf("HandleDujiaoPayWebhook failed: %v", err)
	}
	if status != constants.PaymentStatusSuccess || updated == nil || updated.Status != constants.PaymentStatusSuccess {
		t.Fatalf("webhook result status=%q payment=%+v", status, updated)
	}
	var storedOrder orderdomain.Order
	if err := db.First(&storedOrder, order.ID).Error; err != nil {
		t.Fatalf("reload order failed: %v", err)
	}
	if storedOrder.Status != constants.OrderStatusPaid || storedOrder.PaidAt == nil {
		t.Fatalf("DujiaoPay webhook did not mark order paid: %+v", storedOrder)
	}
}

func TestHandleDujiaoPayWebhookRejectsMismatchedMerchantOrderID(t *testing.T) {
	svc, db := setupPaymentServiceWalletTest(t)
	channel, payment, recharge := createDujiaoPayWalletWebhookFixture(t, db)

	body := []byte(fmt.Sprintf(
		`{"event_id":"evt-wallet-mismatch","event_type":"order.paid","event_version":"v1","created_at":"2026-07-27T00:00:00Z","data":{"order_id":%q,"merchant_order_id":"WRONG-MERCHANT-ORDER","fiat_currency":"CNY","fiat_amount":"88.00","tx_id":"0xmismatch"}}`,
		payment.ProviderRef,
	))
	headers := signDujiaoPayWebhook(body, "test-dujiaopay-webhook-secret")

	if _, _, err := svc.HandleDujiaoPayWebhook(WebhookCallbackInput{
		ChannelID: channel.ID,
		Headers:   headers,
		Body:      body,
		Context:   context.Background(),
	}); err != ErrPaymentInvalid {
		t.Fatalf("mismatched merchant order error = %v, want ErrPaymentInvalid", err)
	}

	var storedRecharge walletdomain.RechargeOrder
	if err := db.First(&storedRecharge, recharge.ID).Error; err != nil {
		t.Fatalf("reload recharge failed: %v", err)
	}
	if storedRecharge.Status != constants.WalletRechargeStatusPending {
		t.Fatalf("mismatched webhook changed recharge status to %s", storedRecharge.Status)
	}
}

func TestHandleDujiaoPayWebhookAdoptsSignedCurrencyForLegacyPendingPayment(t *testing.T) {
	svc, db := setupPaymentServiceWalletTest(t)
	channel, payment, recharge := createDujiaoPayWalletWebhookFixture(t, db)
	channel.ConfigJSON["fiat_currency"] = "USD"
	if err := db.Model(channel).Update("config_json", channel.ConfigJSON).Error; err != nil {
		t.Fatalf("update channel fiat currency: %v", err)
	}

	body := []byte(fmt.Sprintf(
		`{"event_id":"evt-wallet-legacy-currency","event_type":"order.paid","event_version":"v1","created_at":"2026-07-27T00:00:00Z","data":{"order_id":%q,"merchant_order_id":%q,"fiat_currency":"USD","fiat_amount":"88.00","tx_id":"0xlegacycurrency"}}`,
		payment.ProviderRef,
		recharge.RechargeNo,
	))
	updated, status, err := svc.HandleDujiaoPayWebhook(WebhookCallbackInput{
		ChannelID: channel.ID,
		Headers:   signDujiaoPayWebhook(body, "test-dujiaopay-webhook-secret"),
		Body:      body,
		Context:   context.Background(),
	})
	if err != nil {
		t.Fatalf("legacy DujiaoPay webhook failed: %v", err)
	}
	if status != constants.PaymentStatusSuccess || updated == nil || updated.Currency != "USD" {
		t.Fatalf("legacy webhook status=%q payment=%+v", status, updated)
	}

	var stored paymentdomain.Payment
	if err := db.First(&stored, payment.ID).Error; err != nil {
		t.Fatalf("reload payment: %v", err)
	}
	if stored.Currency != "USD" {
		t.Fatalf("stored currency = %q, want signed legacy currency USD", stored.Currency)
	}
	if got := stored.ProviderPayload[paymentcontract.GatewayPayloadFiatCurrencySent]; got != "USD" {
		t.Fatalf("stored fiat currency snapshot = %#v, want USD", got)
	}
}

func TestHandleDujiaoPayWebhookKeepsStrictCurrencyCheckForSnapshottedPayment(t *testing.T) {
	svc, db := setupPaymentServiceWalletTest(t)
	channel, payment, recharge := createDujiaoPayWalletWebhookFixture(t, db)
	payment.ProviderPayload = jsonmap.JSON{
		paymentcontract.GatewayPayloadFiatCurrencySent: "CNY",
	}
	if err := db.Model(payment).Update("provider_payload", payment.ProviderPayload).Error; err != nil {
		t.Fatalf("store payment fiat currency snapshot: %v", err)
	}

	body := []byte(fmt.Sprintf(
		`{"event_id":"evt-wallet-strict-currency","event_type":"order.paid","event_version":"v1","created_at":"2026-07-27T00:00:00Z","data":{"order_id":%q,"merchant_order_id":%q,"fiat_currency":"USD","fiat_amount":"88.00","tx_id":"0xstrictcurrency"}}`,
		payment.ProviderRef,
		recharge.RechargeNo,
	))
	if _, _, err := svc.HandleDujiaoPayWebhook(WebhookCallbackInput{
		ChannelID: channel.ID,
		Headers:   signDujiaoPayWebhook(body, "test-dujiaopay-webhook-secret"),
		Body:      body,
		Context:   context.Background(),
	}); err != ErrPaymentCurrencyMismatch {
		t.Fatalf("snapshotted currency mismatch error = %v, want ErrPaymentCurrencyMismatch", err)
	}

	var stored paymentdomain.Payment
	if err := db.First(&stored, payment.ID).Error; err != nil {
		t.Fatalf("reload payment: %v", err)
	}
	if stored.Status != constants.PaymentStatusPending || stored.Currency != "CNY" {
		t.Fatalf("strict mismatch changed payment: %+v", stored)
	}
}

func createDujiaoPayWalletWebhookFixture(t *testing.T, db *gorm.DB) (*paymentdomain.PaymentChannel, *paymentdomain.Payment, *walletdomain.RechargeOrder) {
	t.Helper()
	channel := createDujiaoPayChannelFixture(t, db)
	now := time.Now()
	payment, recharge := createWalletRechargeFixture(t, db, constants.PaymentStatusPending, constants.WalletRechargeStatusPending)
	providerRef := fmt.Sprintf("do_wallet_%d", now.UnixNano())
	if err := db.Model(payment).Updates(map[string]interface{}{
		"channel_id":       channel.ID,
		"provider_type":    constants.PaymentProviderDujiaoPay,
		"channel_type":     channel.ChannelType,
		"interaction_mode": constants.PaymentInteractionRedirect,
		"gateway_order_no": recharge.RechargeNo,
		"provider_ref":     providerRef,
	}).Error; err != nil {
		t.Fatalf("update DujiaoPay payment fixture failed: %v", err)
	}
	payment.ChannelID = channel.ID
	payment.ProviderType = constants.PaymentProviderDujiaoPay
	payment.ChannelType = channel.ChannelType
	payment.InteractionMode = constants.PaymentInteractionRedirect
	payment.GatewayOrderNo = recharge.RechargeNo
	payment.ProviderRef = providerRef
	return channel, payment, recharge
}

func createDujiaoPayChannelFixture(t *testing.T, db *gorm.DB) *paymentdomain.PaymentChannel {
	t.Helper()
	now := time.Now()
	channel := &paymentdomain.PaymentChannel{
		Name:            "DujiaoPay test",
		ProviderType:    constants.PaymentProviderDujiaoPay,
		ChannelType:     "tron-usdt",
		InteractionMode: constants.PaymentInteractionRedirect,
		ConfigJSON: jsonmap.JSON{
			"api_base_url":   "https://pay.example.test",
			"api_key_id":     "test-key",
			"api_secret":     "test-api-secret",
			"webhook_secret": "test-dujiaopay-webhook-secret",
			"order_mode":     constants.PaymentDujiaoPayOrderModeTransaction,
			"chain":          "tron",
			"token_id":       "tron-usdt",
			"fiat_currency":  "CNY",
		},
		IsActive:  true,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := db.Create(channel).Error; err != nil {
		t.Fatalf("create DujiaoPay channel failed: %v", err)
	}
	return channel
}

func signDujiaoPayWebhook(body []byte, secret string) map[string]string {
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(timestamp + "."))
	_, _ = mac.Write(body)
	return map[string]string{
		"DJP-Webhook-Timestamp": timestamp,
		"DJP-Webhook-Signature": hex.EncodeToString(mac.Sum(nil)),
	}
}

func createWalletRechargeFixture(t *testing.T, db *gorm.DB, paymentStatus string, rechargeStatus string) (*paymentdomain.Payment, *walletdomain.RechargeOrder) {
	t.Helper()
	now := time.Now()
	payment := &paymentdomain.Payment{
		OrderID:         0,
		ChannelID:       1,
		ProviderType:    constants.PaymentProviderOfficial,
		ChannelType:     constants.PaymentChannelTypeWechat,
		InteractionMode: constants.PaymentInteractionQR,
		Amount:          money.FromDecimal(decimal.NewFromInt(88)),
		FeeRate:         money.FromDecimal(decimal.Zero),
		FeeAmount:       money.FromDecimal(decimal.Zero),
		Currency:        "CNY",
		Status:          paymentStatus,
		ProviderRef:     fmt.Sprintf("RECHARGE-PAY-%d", now.UnixNano()),
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if paymentStatus == constants.PaymentStatusSuccess {
		payment.PaidAt = &now
	}
	if err := db.Create(payment).Error; err != nil {
		t.Fatalf("create payment failed: %v", err)
	}

	recharge := &walletdomain.RechargeOrder{
		RechargeNo:      fmt.Sprintf("WRTEST%d", now.UnixNano()),
		UserID:          1,
		PaymentID:       payment.ID,
		ChannelID:       1,
		ProviderType:    constants.PaymentProviderOfficial,
		ChannelType:     constants.PaymentChannelTypeWechat,
		InteractionMode: constants.PaymentInteractionQR,
		Amount:          money.FromDecimal(decimal.NewFromInt(88)),
		PayableAmount:   money.FromDecimal(decimal.NewFromInt(88)),
		FeeRate:         money.FromDecimal(decimal.Zero),
		FeeAmount:       money.FromDecimal(decimal.Zero),
		Currency:        "CNY",
		Status:          rechargeStatus,
		Remark:          "test",
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if rechargeStatus == constants.WalletRechargeStatusSuccess {
		recharge.PaidAt = &now
	}
	if err := db.Create(recharge).Error; err != nil {
		t.Fatalf("create recharge failed: %v", err)
	}
	return payment, recharge
}

func buildWalletRechargeCallbackInput(payment *paymentdomain.Payment, recharge *walletdomain.RechargeOrder, status string, providerRef string) PaymentCallbackInput {
	return PaymentCallbackInput{
		PaymentID:   payment.ID,
		OrderNo:     recharge.RechargeNo,
		ChannelID:   payment.ChannelID,
		Status:      status,
		ProviderRef: providerRef,
		Amount:      payment.Amount,
		Currency:    payment.Currency,
		PaidAt:      ptrTime(time.Now()),
		Payload: jsonmap.JSON{
			"provider_ref": providerRef,
			"status":       status,
		},
	}
}

func assertWalletRechargeSuccessState(t *testing.T, db *gorm.DB, paymentID uint, rechargeID uint, userID uint, rechargeAmount decimal.Decimal) {
	t.Helper()

	var refreshedPayment paymentdomain.Payment
	if err := db.First(&refreshedPayment, paymentID).Error; err != nil {
		t.Fatalf("reload payment failed: %v", err)
	}
	if refreshedPayment.Status != constants.PaymentStatusSuccess {
		t.Fatalf("payment status want %s got %s", constants.PaymentStatusSuccess, refreshedPayment.Status)
	}
	if refreshedPayment.PaidAt == nil {
		t.Fatalf("payment should set paid_at")
	}

	var refreshedRecharge walletdomain.RechargeOrder
	if err := db.First(&refreshedRecharge, rechargeID).Error; err != nil {
		t.Fatalf("reload recharge failed: %v", err)
	}
	if refreshedRecharge.Status != constants.WalletRechargeStatusSuccess {
		t.Fatalf("recharge status want %s got %s", constants.WalletRechargeStatusSuccess, refreshedRecharge.Status)
	}
	if refreshedRecharge.PaidAt == nil {
		t.Fatalf("recharge should set paid_at")
	}

	var account walletdomain.Account
	if err := db.Where("user_id = ?", userID).First(&account).Error; err != nil {
		t.Fatalf("reload wallet account failed: %v", err)
	}
	if !account.Balance.Decimal.Equal(rechargeAmount) {
		t.Fatalf("wallet balance want %s got %s", rechargeAmount.String(), account.Balance.String())
	}
}

func ptrTime(v time.Time) *time.Time {
	return &v
}

func coverageMoney(value string) money.Amount {
	return money.FromDecimal(decimal.RequireFromString(value))
}

func createCoverageFixture(t *testing.T, db *gorm.DB, total, payable, fee, orderCurrency, paymentCurrency string, payload jsonmap.JSON) (*orderdomain.Order, *paymentdomain.Payment) {
	t.Helper()
	now := time.Now()
	user := userdomain.User{Email: fmt.Sprintf("coverage-%d@example.invalid", now.UnixNano()), Status: constants.UserStatusActive, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	order := &orderdomain.Order{OrderNo: fmt.Sprintf("COVERAGE-%d", now.UnixNano()), UserID: user.ID, Status: constants.OrderStatusPendingPayment, Currency: orderCurrency, OriginalAmount: coverageMoney(total), TotalAmount: coverageMoney(total), OnlinePaidAmount: coverageMoney(total), CreatedAt: now, UpdatedAt: now}
	if err := db.Create(order).Error; err != nil {
		t.Fatal(err)
	}
	payment := &paymentdomain.Payment{OrderID: order.ID, ChannelID: 1, ProviderType: constants.PaymentProviderEpay, ChannelType: constants.PaymentChannelTypeAlipay, Amount: coverageMoney(payable), FeeAmount: coverageMoney(fee), Currency: paymentCurrency, Status: constants.PaymentStatusPending, GatewayOrderNo: order.OrderNo, ProviderPayload: payload, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(payment).Error; err != nil {
		t.Fatal(err)
	}
	return order, payment
}

func coverageCallback(payment *paymentdomain.Payment, status string) PaymentCallbackInput {
	return PaymentCallbackInput{PaymentID: payment.ID, ChannelID: payment.ChannelID, OrderNo: payment.GatewayOrderNo, ProviderRef: "SYNTHETIC-RECEIPT", Status: status, Amount: payment.Amount, Currency: payment.Currency}
}

func TestCallbackCoverageUsesNetCreationAmounts(t *testing.T) {
	for _, tc := range []struct{ name, total, payable, fee, orderCurrency, paymentCurrency, original, rate, exception, credit, feePolicy string }{
		{"same currency full", "100", "100", "0", "CNY", "CNY", "", "", "", "0", ""},
		{"legacy customer surcharge", "100", "103", "3", "CNY", "CNY", "", "", "", "0", ""},
		{"customer surcharge", "100", "103", "3", "CNY", "CNY", "", "", "", "0", constants.PaymentFeePolicyCustomerSurcharge},
		{"merchant absorbed", "100", "100", "3", "CNY", "CNY", "", "", "", "0", constants.PaymentFeePolicyMerchantAbsorbed},
		{"explicit no surcharge", "100", "100", "3", "CNY", "CNY", "", "", "", "0", constants.PaymentFeePolicyNone},
		{"unknown fee policy", "100", "100", "3", "CNY", "CNY", "", "", constants.PaymentExceptionInvalidAmountSnapshot, "0", "unrecognized"},
		{"fee is not merchandise", "100", "3", "1", "CNY", "CNY", "", "", constants.PaymentExceptionUnderpaidSucceeded, "2", ""},
		{"rounded conversion full", "35.50", "0.99", "0", "USD", "EUR", "35.50", "0.028", "", "0", ""},
		{"converted customer surcharge", "35", "0.99", "0.50", "USD", "EUR", "35.50", "0.028", "", "0", ""},
		{"converted merchant absorbed", "35.50", "0.99", "0.50", "USD", "EUR", "35.50", "0.028", "", "0", constants.PaymentFeePolicyMerchantAbsorbed},
		{"conversion missing snapshot", "100", "1", "0", "USD", "EUR", "", "", constants.PaymentExceptionInvalidAmountSnapshot, "0", ""},
		{"conversion contradictory snapshot", "100", "1", "0", "USD", "EUR", "100", "0.02", constants.PaymentExceptionInvalidAmountSnapshot, "0", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, db := setupPaymentServiceWalletTest(t)
			payload := jsonmap.JSON{}
			if tc.original != "" {
				payload = jsonmap.JSON{"original_amount": tc.original, "original_currency": tc.orderCurrency, "exchange_rate": tc.rate}
			}
			order, payment := createCoverageFixture(t, db, tc.total, tc.payable, tc.fee, tc.orderCurrency, tc.paymentCurrency, payload)
			payment.FeePolicy = tc.feePolicy
			if err := db.Save(payment).Error; err != nil {
				t.Fatal(err)
			}
			other := *payment
			other.ID = 0
			other.ProviderPayload = nil
			other.GatewayOrderNo += "-NEW"
			if err := db.Create(&other).Error; err != nil {
				t.Fatal(err)
			}
			for repeat := 0; repeat < 2; repeat++ {
				updated, err := svc.HandleCallback(coverageCallback(payment, constants.PaymentStatusSuccess))
				if err != nil {
					t.Fatal(err)
				}
				if updated.ExceptionCode != tc.exception {
					t.Fatalf("exception=%q, want %q", updated.ExceptionCode, tc.exception)
				}
			}
			if err := db.First(order, order.ID).Error; err != nil {
				t.Fatal(err)
			}
			wantPaid := tc.exception == ""
			if (order.PaidAt != nil) != wantPaid {
				t.Fatalf("order paid=%v, want %v", order.PaidAt != nil, wantPaid)
			}
			var account walletdomain.Account
			if err := db.Where("user_id = ?", order.UserID).Find(&account).Error; err != nil {
				t.Fatal(err)
			}
			if !account.Balance.Decimal.Equal(decimal.RequireFromString(tc.credit)) {
				t.Fatalf("credited=%s, want %s", account.Balance.String(), tc.credit)
			}
			if err := db.First(&other, other.ID).Error; err != nil {
				t.Fatal(err)
			}
			wantStatus := constants.PaymentStatusPending
			if wantPaid {
				wantStatus = constants.PaymentStatusExpired
			}
			if other.Status != wantStatus {
				t.Fatalf("other payment status=%s want %s", other.Status, wantStatus)
			}
		})
	}
}

func TestOldLinkReceiptAfterWalletReleaseDoesNotPayFullOrder(t *testing.T) {
	svc, db := setupPaymentServiceWalletTest(t)
	order, _ := createCoverageFixture(t, db, "100", "100", "0", "CNY", "CNY", nil)
	account := walletdomain.Account{UserID: order.UserID, Balance: coverageMoney("99")}
	if err := db.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	var lookups atomic.Int32
	oldNo, newNo := "", ""
	gateway := newVerifyTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lookups.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		no, receipt := oldNo, "1.00"
		if r.Form.Get("trade_no") == "NEW-REAL-RECEIPT" {
			no, receipt = newNo, "100.00"
		}
		_, _ = fmt.Fprintf(w, `{"code":1,"status":1,"pid":"56796329","trade_no":%q,"out_trade_no":%q,"money":%q}`, r.Form.Get("trade_no"), no, receipt)
	}))
	channel := newEpayVerifyChannel(gateway.URL)
	channel.ID = 0
	channel.IsActive = true
	channel.InteractionMode = constants.PaymentInteractionRedirect
	if err := db.Create(channel).Error; err != nil {
		t.Fatal(err)
	}
	first, err := svc.CreatePayment(CreatePaymentInput{OrderID: order.ID, ChannelID: channel.ID, UseBalance: true})
	if err != nil {
		t.Fatal(err)
	}
	oldNo = first.Payment.GatewayOrderNo
	second, err := svc.CreatePayment(CreatePaymentInput{OrderID: order.ID, ChannelID: channel.ID, UseBalance: false})
	if err != nil {
		t.Fatal(err)
	}
	newNo = second.Payment.GatewayOrderNo
	if first.Payment.ID == second.Payment.ID || !second.Payment.Amount.Decimal.Equal(decimal.NewFromInt(100)) {
		t.Fatal("wallet release reused the small link")
	}
	callback := func(payment *paymentdomain.Payment, ref string) *paymentdomain.Payment {
		t.Helper()
		form := signEpayCallbackForm(verifyTestMerchantKey, map[string]string{"pid": "56796329", "trade_no": ref, "out_trade_no": payment.GatewayOrderNo, "money": payment.Amount.String(), "trade_status": constants.EpayTradeStatusSuccess})
		updated, err := svc.HandleSyncCallback(channel, form, nil)
		if err != nil {
			t.Fatal(err)
		}
		return updated
	}
	for repeat := 0; repeat < 2; repeat++ {
		if got := callback(first.Payment, "OLD-REAL-RECEIPT"); got.ExceptionCode != constants.PaymentExceptionUnderpaidSucceeded {
			t.Fatalf("old payment exception=%s", got.ExceptionCode)
		}
	}
	if err := db.First(order, order.ID).Error; err != nil {
		t.Fatal(err)
	}
	if order.PaidAt != nil || order.Status != constants.OrderStatusPendingPayment {
		t.Fatal("small old link fulfilled full order")
	}
	if err := db.First(&account, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !account.Balance.Decimal.Equal(decimal.NewFromInt(100)) {
		t.Fatalf("refund plus one genuine receipt must equal 100, got %s", account.Balance.String())
	}
	if err := db.First(second.Payment, second.Payment.ID).Error; err != nil {
		t.Fatal(err)
	}
	if second.Payment.Status != constants.PaymentStatusPending {
		t.Fatal("underpayment expired the valid replacement")
	}
	callback(second.Payment, "NEW-REAL-RECEIPT")
	if err := db.First(order, order.ID).Error; err != nil {
		t.Fatal(err)
	}
	if order.PaidAt == nil {
		t.Fatal("full verified replacement failed to pay order")
	}
	if lookups.Load() < 2 {
		t.Fatal("real callback path did not independently query gateway")
	}
}

func TestWalletAllocationCyclesAlwaysDebitAgain(t *testing.T) {
	svc, db := setupPaymentServiceWalletTest(t)
	order, _ := createCoverageFixture(t, db, "100", "100", "0", "CNY", "CNY", nil)
	account := walletdomain.Account{UserID: order.UserID, Balance: coverageMoney("99")}
	if err := db.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	channel := newEpayVerifyChannel("https://synthetic.invalid")
	channel.ID = 0
	channel.IsActive = true
	channel.InteractionMode = constants.PaymentInteractionRedirect
	if err := db.Create(channel).Error; err != nil {
		t.Fatal(err)
	}
	for cycle := 0; cycle < 2; cycle++ {
		for _, useBalance := range []bool{true, false} {
			if _, err := svc.CreatePayment(CreatePaymentInput{OrderID: order.ID, ChannelID: channel.ID, UseBalance: useBalance}); err != nil {
				t.Fatal(err)
			}
			if err := db.First(&account, account.ID).Error; err != nil {
				t.Fatal(err)
			}
			want := decimal.NewFromInt(99)
			if useBalance {
				want = decimal.Zero
			}
			if !account.Balance.Decimal.Equal(want) {
				t.Fatalf("cycle %d useBalance %v wallet=%s want %s", cycle, useBalance, account.Balance.String(), want.String())
			}
		}
	}
}

func TestFailedCallbackOnlyReleasesCurrentWalletAllocation(t *testing.T) {
	for _, status := range []string{constants.PaymentStatusFailed, constants.PaymentStatusExpired} {
		for _, tc := range []struct {
			name, payable, replacement string
			wantRelease                bool
		}{
			{"current failure releases", "1", "", true},
			{"old amount cannot release", "100", "", false},
			{"pending replacement retains", "1", constants.PaymentStatusPending, false},
			{"initiated replacement retains", "1", constants.PaymentStatusInitiated, false},
		} {
			t.Run(status+"/"+tc.name, func(t *testing.T) {
				svc, db := setupPaymentServiceWalletTest(t)
				order, payment := createCoverageFixture(t, db, "100", tc.payable, "0", "CNY", "CNY", nil)
				order.WalletPaidAmount = coverageMoney("99")
				if err := db.Save(order).Error; err != nil {
					t.Fatal(err)
				}
				account := walletdomain.Account{UserID: order.UserID, Balance: coverageMoney("0")}
				if err := db.Create(&account).Error; err != nil {
					t.Fatal(err)
				}
				if tc.replacement != "" {
					replacement := *payment
					replacement.ID, replacement.Status, replacement.GatewayOrderNo = 0, tc.replacement, payment.GatewayOrderNo+"-REPLACEMENT"
					if err := db.Create(&replacement).Error; err != nil {
						t.Fatal(err)
					}
				}
				for repeat := 0; repeat < 2; repeat++ {
					if _, err := svc.HandleCallback(coverageCallback(payment, status)); err != nil {
						t.Fatal(err)
					}
				}
				if err := db.First(order, order.ID).Error; err != nil {
					t.Fatal(err)
				}
				if err := db.First(&account, account.ID).Error; err != nil {
					t.Fatal(err)
				}
				wantAllocation, wantBalance := decimal.NewFromInt(99), decimal.Zero
				if tc.wantRelease {
					wantAllocation, wantBalance = decimal.Zero, decimal.NewFromInt(99)
				}
				if !order.WalletPaidAmount.Decimal.Equal(wantAllocation) || !account.Balance.Decimal.Equal(wantBalance) {
					t.Fatalf("wallet allocation=%s balance=%s, want %s/%s", order.WalletPaidAmount, account.Balance, wantAllocation, wantBalance)
				}
			})
		}
	}
}

func TestClosedOrderSuccessIsRecordedWithoutReopening(t *testing.T) {
	for _, status := range []string{constants.OrderStatusCanceled, constants.OrderStatusRefunded, constants.OrderStatusCompleted} {
		t.Run(status, func(t *testing.T) {
			svc, db := setupPaymentServiceWalletTest(t)
			order, payment := createCoverageFixture(t, db, "100", "100", "0", "CNY", "CNY", nil)
			order.Status = status
			if status != constants.OrderStatusCanceled {
				now := time.Now()
				order.PaidAt = &now
			}
			if err := db.Save(order).Error; err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				updated, err := svc.HandleCallback(coverageCallback(payment, constants.PaymentStatusSuccess))
				if err != nil {
					t.Fatal(err)
				}
				if updated.ExceptionCode == "" {
					t.Fatal("closed/duplicate receipt was not flagged")
				}
			}
			if err := db.First(order, order.ID).Error; err != nil {
				t.Fatal(err)
			}
			if order.Status != status {
				t.Fatalf("closed order reopened to %s", order.Status)
			}
		})
	}
}

func TestCallbackCannotInsertOrReplaceCreationSnapshots(t *testing.T) {
	for _, existing := range []jsonmap.JSON{nil, {"original_amount": "1", "original_currency": "CNY", "exchange_rate": "1", "params": jsonmap.JSON{"money": "1"}}} {
		incoming := jsonmap.JSON{"original_amount": "100", "original_currency": "USD", "exchange_rate": "0.001", "params": "forged", "mode": "forged", "token": "forged", paymentcontract.GatewayPayloadFiatCurrencySent: "USD", "gateway_order_verified": true}
		merged := mergeProviderPayload(existing, incoming)
		for _, key := range []string{"original_amount", "original_currency", "exchange_rate", "params", "mode", "token", paymentcontract.GatewayPayloadFiatCurrencySent} {
			if fmt.Sprint(merged[key]) != fmt.Sprint(existing[key]) {
				t.Errorf("callback changed reserved field %s", key)
			}
		}
		if merged["gateway_order_verified"] != true {
			t.Fatal("local verification marker was discarded")
		}
	}
}

type staleCallbackReadStore struct {
	paymentcontract.Store
	stale *paymentdomain.Payment
}

func (s staleCallbackReadStore) GetByID(id uint) (*paymentdomain.Payment, error) {
	if id == s.stale.ID {
		copy := *s.stale
		return &copy, nil
	}
	return s.Store.GetByID(id)
}

func TestStalePendingSnapshotCannotOverwriteSuccessfulCallback(t *testing.T) {
	for _, recharge := range []bool{false, true} {
		t.Run(fmt.Sprintf("recharge_%v", recharge), func(t *testing.T) {
			svc, db := setupPaymentServiceWalletTest(t)
			var payment *paymentdomain.Payment
			var input PaymentCallbackInput
			if recharge {
				var record *walletdomain.RechargeOrder
				payment, record = createWalletRechargeFixture(t, db, constants.PaymentStatusPending, constants.WalletRechargeStatusPending)
				input = buildWalletRechargeCallbackInput(payment, record, constants.PaymentStatusSuccess, "REAL-RECHARGE")
			} else {
				_, payment = createCoverageFixture(t, db, "100", "100", "0", "CNY", "CNY", nil)
				input = coverageCallback(payment, constants.PaymentStatusSuccess)
			}
			stale := *payment
			if _, err := svc.HandleCallback(input); err != nil {
				t.Fatal(err)
			}
			svc.paymentRepo = staleCallbackReadStore{Store: svc.paymentRepo, stale: &stale}
			input.Status = constants.PaymentStatusPending
			if _, err := svc.HandleCallback(input); err != nil {
				t.Fatal(err)
			}
			var actual paymentdomain.Payment
			if err := db.First(&actual, payment.ID).Error; err != nil {
				t.Fatal(err)
			}
			if actual.Status != constants.PaymentStatusSuccess || actual.PaidAt == nil {
				t.Fatal("stale pending read rolled back committed success")
			}
			if recharge {
				var count int64
				db.Model(&walletdomain.Transaction{}).Where("type = ?", constants.WalletTxnTypeRecharge).Count(&count)
				if count != 1 {
					t.Fatalf("recharge credited %d times", count)
				}
			}
		})
	}
}
