package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	paymentcontract "github.com/dujiao-next/internal/modules/payment/contract"

	paymentdomain "github.com/dujiao-next/internal/modules/payment/domain"

	"github.com/dujiao-next/internal/constants"
	"github.com/dujiao-next/internal/logger"
	"github.com/dujiao-next/internal/shared/jsonmap"
)

// CapturePaymentInput 捕获支付输入。
type CapturePaymentInput struct {
	PaymentID uint
	Context   context.Context
}

func (s *PaymentService) CapturePayment(input CapturePaymentInput) (*paymentdomain.Payment, error) {
	if input.PaymentID == 0 {
		return nil, ErrPaymentInvalid
	}
	payment, err := s.paymentRepo.GetByID(input.PaymentID)
	if err != nil {
		return nil, ErrPaymentUpdateFailed
	}
	if payment == nil {
		return nil, ErrPaymentNotFound
	}
	if payment.Status == constants.PaymentStatusSuccess {
		return payment, nil
	}

	channel, err := s.channelRepo.GetByID(payment.ChannelID)
	if err != nil {
		return nil, ErrPaymentUpdateFailed
	}
	if channel == nil {
		return nil, ErrPaymentChannelNotFound
	}

	providerType := strings.ToLower(strings.TrimSpace(channel.ProviderType))
	if providerType == constants.PaymentProviderEpay {
		return s.captureEpayReceipt(input.Context, payment, channel)
	}
	if providerType != constants.PaymentProviderOfficial {
		return nil, ErrPaymentProviderNotSupported
	}
	if strings.TrimSpace(payment.ProviderRef) == "" {
		return nil, ErrPaymentInvalid
	}

	// 统一通过 Registry 路由。Registry.Lookup 会返回 channel 对应的 adapter,
	// 如果 adapter 不实现 Capturer,type assertion 失败,返回 ErrPaymentProviderNotSupported。
	// 因此无需在此显式检查 channel 是否支持 capture。
	return s.captureViaRegistry(input, payment, channel)
}

// captureViaRegistry 通过 PaymentProviderRegistry 路由调用 QueryPayment。
// stripe + paypal + wechat 实现了 paymentcontract.GatewayCapturer 接口,其它 channel
// (alipay / epay / epusdt / bepusdt / tokenpay / okpay) 仅实现 webhook 回调,
// type assertion 失败时返回 ErrPaymentProviderNotSupported。
func (s *PaymentService) captureViaRegistry(input CapturePaymentInput, payment *paymentdomain.Payment, channel *paymentdomain.PaymentChannel) (*paymentdomain.Payment, error) {
	logger.Infow("payment_capture_via_registry",
		"payment_id", payment.ID,
		"provider_type", channel.ProviderType,
		"channel_type", channel.ChannelType,
	)
	if s.paymentProviderRegistry == nil {
		return nil, ErrPaymentProviderNotSupported
	}
	p, ok := s.paymentProviderRegistry.Lookup(channel.ProviderType, channel.ChannelType)
	if !ok {
		return nil, ErrPaymentProviderNotSupported
	}
	capturer, ok := p.(paymentcontract.GatewayCapturer)
	if !ok {
		logger.Warnw("payment_provider_capture_not_implemented",
			"provider_type", channel.ProviderType,
			"channel_type", channel.ChannelType,
		)
		return nil, ErrPaymentProviderNotSupported
	}

	// 第二参数是 interactionMode,不是 channelType。stripe/wechat adapter
	// 会拒绝任何非法 mode,传 channelType 会导致  一律 ErrConfigInvalid。
	if err := capturer.ValidateConfig(channel.ConfigJSON, channel.InteractionMode); err != nil {
		return nil, mapProviderErrorToService(err)
	}

	ctx, cancel := detachOutboundRequestContext(input.Context)
	defer cancel()

	queryResult, err := capturer.QueryPayment(ctx, channel.ConfigJSON, payment.ProviderRef)
	if err != nil {
		return nil, mapProviderErrorToService(err)
	}

	payload := jsonmap.JSON{}
	if queryResult.Payload != nil {
		payload = queryResult.Payload
	}
	status := strings.TrimSpace(queryResult.Status)
	if status == "" {
		status = constants.PaymentStatusPending
	}

	callbackInput := PaymentCallbackInput{
		PaymentID:   payment.ID,
		ChannelID:   channel.ID,
		Status:      status,
		ProviderRef: pickFirstNonEmpty(queryResult.ProviderRef, payment.ProviderRef),
		Amount:      queryResult.Amount,
		Currency:    strings.ToUpper(strings.TrimSpace(queryResult.Currency)),
		PaidAt:      queryResult.PaidAt,
		Payload:     payload,
	}
	return s.HandleCallback(callbackInput)
}

// mapProviderErrorToService 把 provider.ErrXxx 转换为 service 层的 ErrPaymentXxx。
func mapProviderErrorToService(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, paymentcontract.ErrGatewayConfigInvalid):
		return fmt.Errorf("%w: %v", ErrPaymentChannelConfigInvalid, err)
	case errors.Is(err, paymentcontract.ErrGatewayRequestFailed), errors.Is(err, paymentcontract.ErrGatewayAuthFailed):
		return fmt.Errorf("%w: %v", ErrPaymentGatewayRequestFailed, err)
	case errors.Is(err, paymentcontract.ErrGatewayResponseInvalid), errors.Is(err, paymentcontract.ErrGatewaySignatureInvalid):
		return fmt.Errorf("%w: %v", ErrPaymentGatewayResponseInvalid, err)
	case errors.Is(err, paymentcontract.ErrGatewayUnsupportedChannel), errors.Is(err, paymentcontract.ErrGatewayProviderNotFound):
		return ErrPaymentProviderNotSupported
	default:
		return fmt.Errorf("%w: %v", ErrPaymentGatewayRequestFailed, err)
	}
}

// acquireReceiptQuery bounds per-payment polling and memory even when multiple
// browser tabs and the compensation worker ask for the same receipt together.
func (s *PaymentService) acquireReceiptQuery(id uint, now time.Time) (func(), bool) {
	s.receiptQueryMu.Lock()
	defer s.receiptQueryMu.Unlock()
	if s.receiptQueryInFlight[id] || now.Before(s.receiptQueryNext[id]) {
		return nil, false
	}
	if s.receiptQueryNext == nil {
		s.receiptQueryNext = make(map[uint]time.Time)
		s.receiptQueryInFlight = make(map[uint]bool)
	}
	if len(s.receiptQueryNext) >= 1000 {
		for key, until := range s.receiptQueryNext {
			if !s.receiptQueryInFlight[key] && !now.Before(until) {
				delete(s.receiptQueryNext, key)
				delete(s.receiptQueryInFlight, key)
			}
		}
		if len(s.receiptQueryNext) >= 1000 {
			return nil, false
		}
	}
	s.receiptQueryInFlight[id] = true
	s.receiptQueryNext[id] = now.Add(3 * time.Second)
	return func() {
		s.receiptQueryMu.Lock()
		delete(s.receiptQueryInFlight, id)
		s.receiptQueryMu.Unlock()
	}, true
}

func (s *PaymentService) captureEpayReceipt(ctx context.Context, payment *paymentdomain.Payment, channel *paymentdomain.PaymentChannel) (*paymentdomain.Payment, error) {
	// Never revisit canceled/expired/manual-resolution records automatically.
	if payment.Status != constants.PaymentStatusPending {
		return payment, nil
	}
	if payment.ProviderType != constants.PaymentProviderEpay || !channel.IsActive || strings.TrimSpace(payment.GatewayOrderNo) == "" {
		return payment, nil
	}
	if s.paymentProviderRegistry == nil {
		return nil, ErrPaymentProviderNotSupported
	}
	provider, ok := s.paymentProviderRegistry.Lookup(channel.ProviderType, channel.ChannelType)
	if !ok {
		return nil, ErrPaymentProviderNotSupported
	}
	lookup, ok := provider.(paymentcontract.GatewayReceiptLookup)
	if !ok {
		return nil, ErrPaymentProviderNotSupported
	}
	release, acquired := s.acquireReceiptQuery(payment.ID, time.Now())
	if !acquired {
		return s.GetPayment(payment.ID)
	}
	defer release()
	if ctx == nil {
		ctx = context.Background()
	}
	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	result, err := lookup.LookupReceipt(queryCtx, channel.ConfigJSON, payment.GatewayOrderNo, payment.Amount, payment.Currency)
	if err != nil {
		return nil, mapProviderErrorToService(err)
	}
	if result == nil {
		return nil, ErrPaymentGatewayResponseInvalid
	}
	if result.Status == constants.PaymentStatusPending {
		return s.GetPayment(payment.ID)
	}
	if result.Status != constants.PaymentStatusSuccess || strings.TrimSpace(result.ProviderRef) == "" ||
		!result.Amount.Decimal.IsPositive() || !result.Amount.Decimal.Equal(payment.Amount.Decimal) ||
		strings.TrimSpace(result.Currency) == "" || !strings.EqualFold(result.Currency, payment.Currency) {
		return nil, ErrPaymentCallbackUnverified
	}
	return s.HandleCallback(PaymentCallbackInput{
		PaymentID: payment.ID, ChannelID: channel.ID, OrderNo: payment.GatewayOrderNo,
		Status: constants.PaymentStatusSuccess, ProviderRef: result.ProviderRef,
		Amount: result.Amount, Currency: result.Currency, PaidAt: result.PaidAt,
		Payload: jsonmap.JSON{"gateway_order_verified": true, "gateway_order_verified_at": time.Now().UTC().Format(time.RFC3339), "receipt_source": "active_query"},
	})
}

// ReconcilePendingReceipts provides a bounded, rotating server-side fallback.
// All gateway I/O happens before the existing locked/idempotent settlement path.
func (s *PaymentService) ReconcilePendingReceipts(ctx context.Context) error {
	if !s.receiptSweepMu.TryLock() {
		return nil
	}
	defer s.receiptSweepMu.Unlock()
	reader, ok := s.paymentRepo.(paymentcontract.PendingReceiptStore)
	if !ok {
		return ErrPaymentProviderNotSupported
	}
	payments, err := reader.ListPendingReceiptPayments(s.receiptSweepCursor, 20, time.Now())
	if err != nil {
		return ErrPaymentUpdateFailed
	}
	if len(payments) == 0 && s.receiptSweepCursor != 0 {
		s.receiptSweepCursor = 0
		payments, err = reader.ListPendingReceiptPayments(0, 20, time.Now())
		if err != nil {
			return ErrPaymentUpdateFailed
		}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	workers := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for _, payment := range payments {
		select {
		case workers <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return nil
		}
		s.receiptSweepCursor = payment.ID
		wg.Add(1)
		go func(id uint) {
			defer wg.Done()
			defer func() { <-workers }()
			defer func() {
				if recover() != nil {
					paymentLogger("payment_id", id).Errorw("payment_receipt_poll_panicked")
				}
			}()
			if _, err := s.CapturePayment(CapturePaymentInput{PaymentID: id, Context: ctx}); err != nil {
				paymentLogger("payment_id", id).Warnw("payment_receipt_poll_failed", "error", err)
			}
		}(payment.ID)
	}
	wg.Wait()
	return nil
}
