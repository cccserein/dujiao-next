package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	paymentcontract "github.com/dujiao-next/internal/modules/payment/contract"

	paymentdomain "github.com/dujiao-next/internal/modules/payment/domain"

	"github.com/dujiao-next/internal/constants"
	"github.com/dujiao-next/internal/shared/jsonmap"

	"go.uber.org/zap"
)

// WebhookCallbackInput Webhook 回调输入。
type WebhookCallbackInput struct {
	ChannelID uint
	Headers   map[string]string
	Body      []byte
	Context   context.Context
}

// HandleSyncCallback 处理同步 form callback（alipay/epay/epusdt/bepusdt/tokenpay/okpay）。
// 通过 Registry 找到 adapter 的 CallbackVerifier 能力解析并验签 form/body，然后调 HandleCallback。
// channel 必须由 caller 加载好传入（handler 负责找到 payment→channel 并验证类型）。
func (s *PaymentService) HandleSyncCallback(
	channel *paymentdomain.PaymentChannel,
	form map[string][]string,
	body []byte,
) (*paymentdomain.Payment, error) {
	if channel == nil {
		return nil, ErrPaymentChannelNotFound
	}
	if s.paymentProviderRegistry == nil {
		return nil, ErrPaymentProviderNotSupported
	}

	p, ok := s.paymentProviderRegistry.Lookup(channel.ProviderType, channel.ChannelType)
	if !ok {
		return nil, ErrPaymentProviderNotSupported
	}
	verifier, ok := p.(paymentcontract.GatewayCallbackVerifier)
	if !ok {
		return nil, ErrPaymentProviderNotSupported
	}

	result, err := verifier.VerifyCallback(channel.ConfigJSON, form, body)
	if err != nil {
		return nil, mapProviderErrorToService(err)
	}

	// WebhookResult = CallbackResult（类型别名），findWebhookPayment 直接复用。
	payment, err := s.findWebhookPayment(channel.ID, result)
	if err != nil {
		return nil, err
	}

	payload := jsonmap.JSON{}
	if result.Payload != nil {
		payload = result.Payload
	}

	callbackInput := PaymentCallbackInput{
		PaymentID:   payment.ID,
		OrderNo:     result.OrderNo,
		ChannelID:   channel.ID,
		Status:      result.Status,
		ProviderRef: pickFirstNonEmpty(result.ProviderRef, payment.ProviderRef),
		Amount:      result.Amount,
		Currency:    strings.ToUpper(strings.TrimSpace(result.Currency)),
		PaidAt:      result.PaidAt,
		Payload:     payload,
	}
	// 验签通过不等于钱到账：成功回调必须先过网关复核才能落库发货。
	if err := s.corroborateSuccessCallback(channel, payment, callbackInput); err != nil {
		return nil, err
	}
	return s.HandleCallback(callbackInput)
}

// HandlePaypalWebhook 处理 PayPal webhook。
func (s *PaymentService) HandlePaypalWebhook(input WebhookCallbackInput) (*paymentdomain.Payment, string, error) {
	return s.handleWebhookViaRegistry(
		input,
		constants.PaymentProviderOfficial,
		constants.PaymentChannelTypePaypal,
	)
}

// HandleDujiaoPayWebhook 处理 DujiaoPay webhook。
//
// DujiaoPay 的 channel_type 是 token_id（tron-usdt/base-usdc 等），同一个 webhook
// 入口不能预先知道 token_id。channel_id 缺失时按 provider_type 拉取所有启用渠道，
// 用 webhook_secret 逐个验签；只有签名匹配的渠道才会进入落库流程。
func (s *PaymentService) HandleDujiaoPayWebhook(input WebhookCallbackInput) (*paymentdomain.Payment, string, error) {
	log := paymentLogger(
		"provider", constants.PaymentProviderDujiaoPay,
		"channel_id", input.ChannelID,
		"body_size", len(input.Body),
	)

	if input.ChannelID == 0 {
		candidates, _, err := s.channelRepo.List(paymentcontract.ChannelListFilter{
			ProviderType: constants.PaymentProviderDujiaoPay,
			ActiveOnly:   true,
		})
		if err != nil {
			log.Errorw("payment_webhook_candidates_list_failed", "error", err)
			return nil, "", ErrPaymentUpdateFailed
		}
		if len(candidates) == 0 {
			log.Warnw("payment_webhook_no_candidate_channel")
			return nil, "", ErrPaymentChannelNotFound
		}

		var lastErr error
		for i := range candidates {
			channel := candidates[i]
			result, err := s.tryParseWebhookWithChannel(&channel, input)
			if err != nil {
				log.Debugw("payment_webhook_candidate_parse_failed",
					"candidate_channel_id", channel.ID,
					"channel_type", channel.ChannelType,
					"error", err,
				)
				lastErr = err
				continue
			}
			log.Infow("payment_webhook_candidate_matched", "candidate_channel_id", channel.ID, "channel_type", channel.ChannelType)
			return s.commitVerifiedWebhook(&channel, result, log)
		}
		if lastErr == nil {
			lastErr = ErrPaymentProviderNotSupported
		}
		log.Warnw("payment_webhook_all_candidates_failed", "candidate_count", len(candidates), "last_error", lastErr)
		return nil, "", lastErr
	}

	channel, err := s.channelRepo.GetByID(input.ChannelID)
	if err != nil {
		log.Errorw("payment_webhook_channel_fetch_failed", "error", err)
		return nil, "", ErrPaymentUpdateFailed
	}
	if channel == nil {
		log.Warnw("payment_webhook_channel_not_found")
		return nil, "", ErrPaymentChannelNotFound
	}
	if strings.ToLower(strings.TrimSpace(channel.ProviderType)) != constants.PaymentProviderDujiaoPay {
		log.Warnw("payment_webhook_provider_mismatch",
			"provider_type", channel.ProviderType,
			"channel_type", channel.ChannelType,
		)
		return nil, "", ErrPaymentProviderNotSupported
	}

	result, err := s.tryParseWebhookWithChannel(channel, input)
	if err != nil {
		log.Warnw("payment_webhook_parse_failed", "error", err)
		return nil, "", err
	}
	return s.commitVerifiedWebhook(channel, result, log)
}

// handleWebhookViaRegistry 通过 Registry 路由 webhook 解析。
//
// channel_id 在 URL query 缺失时:
//   - 微信不允许 notify_url 携带 query 参数(微信官方 V3 规范),wechat webhook
//     必须支持遍历候选 channel,逐个用 api_v3_key 尝试 AES-GCM 解密验签;
//   - stripe 的 endpoint_secret 也具备同等的"试错验签"能力,顺带支持;
//   - paypal 的 webhook_id 校验缺乏自识别机制,必须强制 channel_id。
func (s *PaymentService) handleWebhookViaRegistry(
	input WebhookCallbackInput,
	expectedProviderType string,
	expectedChannelType string,
) (*paymentdomain.Payment, string, error) {
	log := paymentLogger(
		"provider", expectedChannelType,
		"channel_id", input.ChannelID,
		"body_size", len(input.Body),
	)

	if input.ChannelID == 0 {
		if !supportsBlindWebhookCandidateMatching(expectedChannelType) {
			log.Warnw("payment_webhook_invalid_channel_id")
			return nil, "", ErrPaymentInvalid
		}
		return s.handleWebhookByCandidateIteration(input, expectedProviderType, expectedChannelType, log)
	}

	channel, err := s.channelRepo.GetByID(input.ChannelID)
	if err != nil {
		log.Errorw("payment_webhook_channel_fetch_failed", "error", err)
		return nil, "", ErrPaymentUpdateFailed
	}
	if channel == nil {
		log.Warnw("payment_webhook_channel_not_found")
		return nil, "", ErrPaymentChannelNotFound
	}

	providerType := strings.ToLower(strings.TrimSpace(channel.ProviderType))
	channelType := strings.ToLower(strings.TrimSpace(channel.ChannelType))
	if providerType != expectedProviderType || channelType != expectedChannelType {
		log.Warnw("payment_webhook_provider_mismatch",
			"provider_type", channel.ProviderType,
			"channel_type", channel.ChannelType,
		)
		return nil, "", ErrPaymentProviderNotSupported
	}

	result, err := s.tryParseWebhookWithChannel(channel, input)
	if err != nil {
		log.Warnw("payment_webhook_parse_failed", "error", err)
		return nil, "", err
	}
	return s.commitVerifiedWebhook(channel, result, log)
}

// handleWebhookByCandidateIteration 在 channel_id 缺失时,按 expectedProvider+expectedChannel
// 拉取所有 active channel,挨个尝试解密验签;首个 ParseWebhook 成功的渠道即为目标渠道。
func (s *PaymentService) handleWebhookByCandidateIteration(
	input WebhookCallbackInput,
	expectedProviderType string,
	expectedChannelType string,
	log *zap.SugaredLogger,
) (*paymentdomain.Payment, string, error) {
	candidates, _, err := s.channelRepo.List(paymentcontract.ChannelListFilter{
		ProviderType: expectedProviderType,
		ChannelType:  expectedChannelType,
		ActiveOnly:   true,
	})
	if err != nil {
		log.Errorw("payment_webhook_candidates_list_failed", "error", err)
		return nil, "", ErrPaymentUpdateFailed
	}
	if len(candidates) == 0 {
		log.Warnw("payment_webhook_no_candidate_channel")
		return nil, "", ErrPaymentChannelNotFound
	}

	var lastErr error
	for i := range candidates {
		channel := candidates[i]
		result, err := s.tryParseWebhookWithChannel(&channel, input)
		if err != nil {
			log.Debugw("payment_webhook_candidate_parse_failed",
				"candidate_channel_id", channel.ID,
				"error", err,
			)
			lastErr = err
			continue
		}
		log.Infow("payment_webhook_candidate_matched", "candidate_channel_id", channel.ID)
		return s.commitVerifiedWebhook(&channel, result, log)
	}
	if lastErr == nil {
		lastErr = ErrPaymentProviderNotSupported
	}
	log.Warnw("payment_webhook_all_candidates_failed", "candidate_count", len(candidates), "last_error", lastErr)
	return nil, "", lastErr
}

// tryParseWebhookWithChannel 用指定 channel 的 config 尝试解析 webhook。
// 返回 error 表示该 channel 不匹配(签名/密钥校验失败或 capability 缺失),
// 由 caller 决定 retry 下一个候选还是终止。
func (s *PaymentService) tryParseWebhookWithChannel(
	channel *paymentdomain.PaymentChannel,
	input WebhookCallbackInput,
) (*paymentcontract.GatewayCallbackResult, error) {
	if s.paymentProviderRegistry == nil {
		return nil, ErrPaymentProviderNotSupported
	}
	p, ok := s.paymentProviderRegistry.Lookup(channel.ProviderType, channel.ChannelType)
	if !ok {
		return nil, ErrPaymentProviderNotSupported
	}
	webhooker, ok := p.(paymentcontract.GatewayWebhooker)
	if !ok {
		return nil, ErrPaymentProviderNotSupported
	}

	ctx, cancel := detachOutboundRequestContext(input.Context)
	defer cancel()

	result, err := webhooker.ParseWebhook(ctx, channel.ConfigJSON, input.Headers, input.Body, time.Now())
	if err != nil {
		return nil, mapProviderErrorToService(err)
	}
	return result, nil
}

// commitVerifiedWebhook 在 ParseWebhook 验签通过(已确认 channel 归属)后,
// 反查 payment 并落库。任何错误都是真实的业务/DB 错误,不再 retry 其他 channel。
func (s *PaymentService) commitVerifiedWebhook(
	channel *paymentdomain.PaymentChannel,
	result *paymentcontract.GatewayCallbackResult,
	log *zap.SugaredLogger,
) (*paymentdomain.Payment, string, error) {
	log.Infow("payment_webhook_parsed",
		"channel_id", channel.ID,
		"order_no", result.OrderNo,
		"provider_ref", result.ProviderRef,
		"status", result.Status,
	)

	// status 为空表示 adapter 判断该事件无需处理（不可识别的事件类型），直接忽略。
	if result.Status == "" {
		log.Infow("payment_webhook_status_ignored",
			"channel_id", channel.ID,
			"order_no", result.OrderNo,
			"provider_ref", result.ProviderRef,
		)
		return nil, "", nil
	}

	payment, err := s.findWebhookPayment(channel.ID, result)
	if err != nil {
		if errors.Is(err, ErrPaymentNotFound) {
			log.Infow("payment_webhook_payment_not_found",
				"channel_id", channel.ID,
				"order_no", result.OrderNo,
				"provider_ref", result.ProviderRef,
				"status", result.Status,
			)
			return nil, result.Status, nil
		}
		log.Warnw("payment_webhook_payment_lookup_failed",
			"channel_id", channel.ID,
			"order_no", result.OrderNo,
			"provider_ref", result.ProviderRef,
			"status", result.Status,
			"error", err,
		)
		return nil, result.Status, err
	}

	payload := jsonmap.JSON{}
	if result.Payload != nil {
		payload = result.Payload
	}
	verifiedLegacyCurrency := ""
	if channel.ProviderType == constants.PaymentProviderDujiaoPay &&
		result.Status == constants.PaymentStatusSuccess &&
		payment.Status != constants.PaymentStatusSuccess &&
		!strings.EqualFold(strings.TrimSpace(payment.Currency), strings.TrimSpace(result.Currency)) {
		if _, hasSnapshot := payment.ProviderPayload[paymentcontract.GatewayPayloadFiatCurrencySent]; !hasSnapshot {
			verifiedLegacyCurrency = strings.ToUpper(strings.TrimSpace(result.Currency))
			log.Warnw(
				"payment_webhook_legacy_dujiaopay_currency_adoption",
				"payment_id", payment.ID,
				"stored_currency", payment.Currency,
				"verified_currency", verifiedLegacyCurrency,
			)
		}
	}

	callbackInput := PaymentCallbackInput{
		PaymentID:   payment.ID,
		OrderNo:     result.OrderNo,
		ChannelID:   channel.ID,
		Status:      result.Status,
		ProviderRef: pickFirstNonEmpty(result.ProviderRef, payment.ProviderRef),
		Amount:      result.Amount,
		Currency:    strings.ToUpper(strings.TrimSpace(result.Currency)),
		PaidAt:      result.PaidAt,
		Payload:     payload,

		verifiedLegacyDujiaoPayCurrency: verifiedLegacyCurrency,
	}

	// 验签通过不等于钱到账：成功回调必须先过网关复核才能落库发货。
	if err := s.corroborateSuccessCallback(channel, payment, callbackInput); err != nil {
		log.Warnw("payment_webhook_corroboration_rejected",
			"channel_id", channel.ID,
			"payment_id", payment.ID,
			"order_no", result.OrderNo,
			"provider_ref", result.ProviderRef,
			"error", err,
		)
		return nil, result.Status, err
	}

	updated, err := s.HandleCallback(callbackInput)
	if err != nil {
		log.Errorw("payment_webhook_callback_apply_failed",
			"channel_id", channel.ID,
			"payment_id", payment.ID,
			"order_no", result.OrderNo,
			"provider_ref", result.ProviderRef,
			"status", result.Status,
			"error", err,
		)
		return nil, result.Status, err
	}
	log.Infow("payment_webhook_processed",
		"channel_id", channel.ID,
		"payment_id", updated.ID,
		"order_no", result.OrderNo,
		"provider_ref", result.ProviderRef,
		"status", updated.Status,
	)
	return updated, result.Status, nil
}

// supportsBlindWebhookCandidateMatching 标识哪些 channel_type 的 webhook 验签
// 具备"用任意 channel 的 config 试错即可确认归属"的能力。
//
// 微信(api_v3_key + AES-GCM auth tag)和 stripe(endpoint_secret + HMAC)
// 都满足:错配的密钥会在解密/验签阶段失败,正确的密钥才能成功;
// paypal 的 webhook 验签依赖 webhook_id 字段语义,无法盲匹配。
func supportsBlindWebhookCandidateMatching(channelType string) bool {
	switch strings.ToLower(strings.TrimSpace(channelType)) {
	case constants.PaymentChannelTypeWechat, constants.PaymentChannelTypeStripe:
		return true
	default:
		return false
	}
}

// findWebhookPayment 通过 webhook result 反查 payment。
// 优先用 OrderNo（= GatewayOrderNo，商户单号），次选 ProviderRef（网关流水号）。
func (s *PaymentService) findWebhookPayment(channelID uint, result *paymentcontract.GatewayCallbackResult) (*paymentdomain.Payment, error) {
	if result == nil {
		return nil, ErrPaymentNotFound
	}
	if orderNo := strings.TrimSpace(result.OrderNo); orderNo != "" {
		payment, err := s.paymentRepo.GetByGatewayOrderNo(orderNo)
		if err == nil && payment != nil && payment.ChannelID == channelID {
			return payment, nil
		}
	}
	if providerRef := strings.TrimSpace(result.ProviderRef); providerRef != "" {
		payment, err := s.paymentRepo.GetLatestByProviderRef(providerRef)
		if err == nil && payment != nil && payment.ChannelID == channelID {
			return payment, nil
		}
	}
	// 早期 redirect 记录用业务单号占位 ProviderRef，尚无 GatewayOrderNo。
	// 这里只定位候选；复核仍必须与本地业务单号及网关真实流水完全一致。
	if orderNo := strings.TrimSpace(result.OrderNo); orderNo != "" {
		payment, err := s.paymentRepo.GetLatestByProviderRef(orderNo)
		if err == nil && payment != nil && payment.ChannelID == channelID &&
			payment.ProviderType == constants.PaymentProviderEpay && strings.TrimSpace(payment.GatewayOrderNo) == "" {
			return payment, nil
		}
	}
	return nil, ErrPaymentNotFound
}

// HandleWechatWebhook 处理微信支付回调。
// P1.2c Task 6: 退化为 thin wrapper，通过 handleWebhookViaRegistry 路由解析。
func (s *PaymentService) HandleWechatWebhook(input WebhookCallbackInput) (*paymentdomain.Payment, string, error) {
	return s.handleWebhookViaRegistry(
		input,
		constants.PaymentProviderOfficial,
		constants.PaymentChannelTypeWechat,
	)
}

// HandleStripeWebhook 处理 Stripe webhook。
// P1.2c Task 6: 退化为 thin wrapper，通过 handleWebhookViaRegistry 路由解析。
func (s *PaymentService) HandleStripeWebhook(input WebhookCallbackInput) (*paymentdomain.Payment, string, error) {
	return s.handleWebhookViaRegistry(
		input,
		constants.PaymentProviderOfficial,
		constants.PaymentChannelTypeStripe,
	)
}

// corroborateSuccessCallback 在"支付成功"回调落库、发货之前，主动向网关复核这笔支付。
//
// 为什么需要它：回调签名只能证明内容出自持有商户密钥的一方，不能证明钱真的到账。
// 商户密钥一旦泄露（线上发生过：攻击者用真实 merchant_key 构造 sign 完全合法的
// 回调，把未支付订单刷成已支付并提走卡密），单靠验签无法拦住。因此凡支持主动
// 查询的渠道，成功回调必须由网关返回"已收款"才放行；查询不到或查询失败一律拒绝
// （网关会重试回调，宁可在途也不发货）。
//
// 校验通过返回 nil；不通过返回包装了 ErrPaymentCallbackUnverified 的错误。
// 易支付必须具备查询能力。其他尚未接入此能力的渠道保留各自的验签流程；
// 已接入的适配器返回“不支持”时也必须拒绝，不能退回仅凭签名放行。
func (s *PaymentService) corroborateSuccessCallback(
	channel *paymentdomain.PaymentChannel,
	payment *paymentdomain.Payment,
	input PaymentCallbackInput,
) error {
	if channel == nil || payment == nil {
		return ErrPaymentInvalid
	}
	if normalizePaymentStatus(input.Status) != constants.PaymentStatusSuccess {
		return nil
	}
	if s.paymentProviderRegistry == nil {
		return ErrPaymentProviderNotSupported
	}

	log := paymentLogger(
		"payment_id", payment.ID,
		"channel_id", channel.ID,
		"provider_type", channel.ProviderType,
		"channel_type", channel.ChannelType,
		"gateway_order_no", strings.TrimSpace(payment.GatewayOrderNo),
		"payment_amount", payment.Amount.String(),
		"payment_currency", payment.Currency,
	)

	provider, ok := s.paymentProviderRegistry.Lookup(channel.ProviderType, channel.ChannelType)
	if !ok {
		return ErrPaymentProviderNotSupported
	}
	corroborator, ok := provider.(paymentcontract.CallbackCorroborator)
	if !ok {
		if strings.EqualFold(strings.TrimSpace(channel.ProviderType), constants.PaymentProviderEpay) {
			return fmt.Errorf("%w: independent gateway query unavailable", ErrPaymentCallbackUnverified)
		}
		log.Warnw("payment_callback_corroboration_unsupported",
			"reason", "provider has no active query capability",
		)
		return nil
	}
	if payment.ChannelID != channel.ID || input.ChannelID != channel.ID || input.PaymentID != payment.ID {
		return ErrPaymentInvalid
	}
	if !input.Amount.Decimal.IsPositive() || !input.Amount.Decimal.Equal(payment.Amount.Decimal) {
		return ErrPaymentAmountMismatch
	}
	if strings.TrimSpace(input.Currency) == "" || !strings.EqualFold(strings.TrimSpace(input.Currency), strings.TrimSpace(payment.Currency)) {
		return ErrPaymentCurrencyMismatch
	}
	orderNo, err := s.corroborationOrderNo(payment)
	if err != nil {
		return err
	}
	if orderNo == "" || strings.TrimSpace(input.OrderNo) != orderNo {
		return ErrPaymentInvalid
	}
	providerRef := strings.TrimSpace(input.ProviderRef)
	if providerRef == "" {
		return fmt.Errorf("%w: missing gateway transaction", ErrPaymentCallbackUnverified)
	}
	// 已结算记录仅允许同一笔流水的幂等回调，不能重写成另一笔交易。
	if payment.Status == constants.PaymentStatusSuccess {
		if providerRef != strings.TrimSpace(payment.ProviderRef) {
			return ErrPaymentInvalid
		}
		return nil
	}

	ctx, cancel := detachOutboundRequestContext(context.Background())
	defer cancel()

	result, err := corroborator.CorroboratePayment(ctx, channel.ConfigJSON, paymentcontract.CorroborateInput{
		OrderNo:     orderNo,
		ProviderRef: providerRef,
		Amount:      payment.Amount,
		Currency:    payment.Currency,
	})
	if err != nil {
		log.Errorw("payment_callback_corroboration_query_failed", "error", err)
		return fmt.Errorf("%w: gateway query failed", ErrPaymentCallbackUnverified)
	}

	if result == nil {
		log.Errorw("payment_callback_corroboration_empty_result")
		return fmt.Errorf("%w: gateway returned empty result", ErrPaymentCallbackUnverified)
	}

	gatewayStatus := normalizePaymentStatus(result.Status)
	if gatewayStatus != constants.PaymentStatusSuccess {
		log.Warnw("payment_callback_corroboration_not_paid",
			"gateway_status", result.Status,
			"gateway_provider_ref", result.ProviderRef,
		)
		return fmt.Errorf("%w: gateway reports status %q", ErrPaymentCallbackUnverified, strings.TrimSpace(result.Status))
	}

	// 金额是第二道闸：即便网关确认"已支付"，收到的钱少于应收也不能发货。
	if !result.Amount.Decimal.IsPositive() || !result.Amount.Decimal.Equal(payment.Amount.Decimal) {
		log.Warnw("payment_callback_corroboration_amount_mismatch",
			"gateway_amount", result.Amount.String(),
			"expected_amount", payment.Amount.String(),
		)
		return fmt.Errorf("%w: gateway amount %s != expected %s",
			ErrPaymentCallbackUnverified, result.Amount.String(), payment.Amount.String())
	}
	if strings.TrimSpace(result.ProviderRef) != providerRef ||
		strings.TrimSpace(result.Currency) == "" ||
		!strings.EqualFold(strings.TrimSpace(result.Currency), strings.TrimSpace(payment.Currency)) {
		return fmt.Errorf("%w: gateway transaction or currency mismatch", ErrPaymentCallbackUnverified)
	}
	if input.Payload != nil {
		input.Payload["gateway_order_verified"] = true
		input.Payload["gateway_order_verified_at"] = time.Now().UTC().Format(time.RFC3339)
	}

	log.Infow("payment_callback_corroborated",
		"gateway_provider_ref", result.ProviderRef,
		"gateway_amount", result.Amount.String(),
	)
	return nil
}

// corroborationOrderNo 从本地订单恢复旧支付的商户单号；网关流水号不能充当商户单号。
func (s *PaymentService) corroborationOrderNo(payment *paymentdomain.Payment) (string, error) {
	if orderNo := strings.TrimSpace(payment.GatewayOrderNo); orderNo != "" {
		return orderNo, nil
	}
	if payment.OrderID != 0 {
		if s.orderRepo == nil {
			return "", ErrPaymentUpdateFailed
		}
		order, err := s.orderRepo.GetByID(payment.OrderID)
		if err != nil || order == nil {
			return "", ErrPaymentUpdateFailed
		}
		return strings.TrimSpace(order.OrderNo), nil
	}
	if s.walletRepo == nil {
		return "", ErrPaymentUpdateFailed
	}
	recharge, err := s.walletRepo.GetRechargeOrderByPaymentID(payment.ID)
	if err != nil || recharge == nil {
		return "", ErrPaymentUpdateFailed
	}
	return strings.TrimSpace(recharge.RechargeNo), nil
}
