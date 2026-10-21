package application

import (
	"fmt"
	"strings"

	paymentdomain "github.com/dujiao-next/internal/modules/payment/domain"

	orderapp "github.com/dujiao-next/internal/modules/order/application"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"

	"github.com/dujiao-next/internal/constants"
	settingsapp "github.com/dujiao-next/internal/modules/settings/application"
	"github.com/dujiao-next/internal/shared/jsonmap"

	"github.com/shopspring/decimal"
)

// paymentCoveredOrderAmount returns the net order-currency value of this payment.
// The creation fee policy determines whether recorded order-currency fees were surcharged.
// Cross-currency payments require the original creation snapshot and its exact
// rounded conversion; reversing a rounded gateway amount can understate real payment.
func paymentCoveredOrderAmount(payment *paymentdomain.Payment, orderCurrency string) (decimal.Decimal, bool) {
	if payment == nil || !payment.Amount.Decimal.IsPositive() || payment.FeeAmount.Decimal.IsNegative() {
		return decimal.Zero, false
	}
	payable := payment.Amount.Decimal
	if !strings.EqualFold(strings.TrimSpace(payment.Currency), strings.TrimSpace(orderCurrency)) {
		original, ok := paymentSnapshotAmount(payment.ProviderPayload, "original_amount")
		if !ok || !strings.EqualFold(strings.TrimSpace(fmt.Sprint(payment.ProviderPayload["original_currency"])), strings.TrimSpace(orderCurrency)) {
			return decimal.Zero, false
		}
		rate, ok := paymentSnapshotAmount(payment.ProviderPayload, "exchange_rate")
		if !ok || !original.Mul(rate).Round(2).Equal(payment.Amount.Decimal) {
			return decimal.Zero, false
		}
		payable = original
	}
	fee := decimal.Zero
	switch payment.FeePolicy {
	case "", constants.PaymentFeePolicyCustomerSurcharge, constants.PaymentFeePolicyLegacyCustomerSurcharge:
		fee = payment.FeeAmount.Decimal
	case constants.PaymentFeePolicyMerchantAbsorbed, constants.PaymentFeePolicyNone:
	default:
		return decimal.Zero, false
	}
	if payable.LessThan(fee) {
		return decimal.Zero, false
	}
	return normalizeOrderAmount(payable.Sub(fee)), true
}

func paymentSnapshotAmount(payload jsonmap.JSON, key string) (decimal.Decimal, bool) {
	value := strings.TrimSpace(fmt.Sprint(payload[key]))
	if value == "" || len(value) > 64 {
		return decimal.Zero, false
	}
	for _, char := range value {
		if char != '.' && (char < '0' || char > '9') {
			return decimal.Zero, false
		}
	}
	amount, err := decimal.NewFromString(value)
	return amount, err == nil && amount.IsPositive()
}

// normalizeOrderAmount 归一化金额精度与下限。
func normalizeOrderAmount(amount decimal.Decimal) decimal.Decimal {
	normalized := amount.Round(2)
	if normalized.LessThan(decimal.Zero) {
		return decimal.Zero
	}
	return normalized
}

func pickFirstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func shouldMarkFulfilling(order *orderdomain.Order) bool {
	if order == nil {
		return false
	}
	if len(order.Items) == 0 {
		return false
	}
	for _, item := range order.Items {
		fulfillmentType := strings.TrimSpace(item.FulfillmentType)
		if fulfillmentType == "" || fulfillmentType == constants.FulfillmentTypeManual || fulfillmentType == constants.FulfillmentTypeUpstream {
			return true
		}
	}
	return false
}

func shouldUseCNYPaymentCurrency(channel *paymentdomain.PaymentChannel) bool {
	if channel == nil {
		return false
	}
	providerType := strings.ToLower(strings.TrimSpace(channel.ProviderType))
	if providerType != constants.PaymentProviderOfficial {
		return false
	}
	channelType := strings.ToLower(strings.TrimSpace(channel.ChannelType))
	return channelType == constants.PaymentChannelTypeWechat || channelType == constants.PaymentChannelTypeAlipay
}

func validatePaymentAmountForChannel(amount decimal.Decimal, channel *paymentdomain.PaymentChannel) error {
	if channel == nil {
		return nil
	}
	amountOverflow20_2 := decimal.NewFromInt(1000000000000000000)
	if amount.GreaterThanOrEqual(amountOverflow20_2) {
		return ErrPaymentAmountTooLarge
	}
	minAmount := channel.MinAmount.Decimal
	maxAmount := channel.MaxAmount.Decimal
	if minAmount.GreaterThan(decimal.Zero) && amount.LessThan(minAmount) {
		return ErrPaymentAmountTooSmall
	}
	if maxAmount.GreaterThan(decimal.Zero) && amount.GreaterThan(maxAmount) {
		return ErrPaymentAmountTooLarge
	}
	return nil
}

func validatePaymentCurrencyForChannel(currency string, channel *paymentdomain.PaymentChannel) error {
	normalized := strings.ToUpper(strings.TrimSpace(currency))
	if !settingsapp.IsCurrencyCode(normalized) {
		return ErrPaymentCurrencyMismatch
	}
	if shouldUseCNYPaymentCurrency(channel) && normalized != constants.SiteCurrencyDefault {
		return ErrPaymentCurrencyMismatch
	}
	return nil
}

func (s *PaymentService) resolveExpireMinutes() int {
	return orderapp.ResolvePaymentExpireMinutes(s.settingService, s.expireMinutes)
}

func normalizePaymentStatus(status string) string {
	return strings.ToLower(strings.TrimSpace(status))
}

func isPaymentStatusValid(status string) bool {
	switch status {
	case constants.PaymentStatusInitiated, constants.PaymentStatusPending, constants.PaymentStatusSuccess, constants.PaymentStatusFailed, constants.PaymentStatusExpired:
		return true
	default:
		return false
	}
}

func shouldAutoFulfill(order *orderdomain.Order) bool {
	if order == nil || len(order.Items) == 0 {
		return false
	}
	for _, item := range order.Items {
		if strings.TrimSpace(item.FulfillmentType) != constants.FulfillmentTypeAuto {
			return false
		}
	}
	return true
}

// isOrderFullyAutoFulfill 判断订单是否完全为自动交付。
// 父订单：所有子订单均满足 shouldAutoFulfill；单订单：自身满足 shouldAutoFulfill。
// 用于支付成功时跳过"已支付"邮件——自动交付会紧接着发送含卡密内容的"已完成"邮件，避免重复打扰。
func isOrderFullyAutoFulfill(order *orderdomain.Order) bool {
	if order == nil {
		return false
	}
	if len(order.Children) > 0 {
		for i := range order.Children {
			if !shouldAutoFulfill(&order.Children[i]) {
				return false
			}
		}
		return true
	}
	return shouldAutoFulfill(order)
}

func buildOrderSubject(order *orderdomain.Order) string {
	if order == nil {
		return ""
	}
	if len(order.Items) > 0 {
		title := pickOrderItemTitle(order.Items[0].TitleJSON)
		if title != "" {
			return title
		}
	}
	return order.OrderNo
}

func pickOrderItemTitle(title jsonmap.JSON) string {
	if title == nil {
		return ""
	}
	for _, key := range constants.SupportedLocales {
		if val, ok := title[key]; ok {
			if str, ok := val.(string); ok && strings.TrimSpace(str) != "" {
				return strings.TrimSpace(str)
			}
		}
	}
	for _, val := range title {
		if str, ok := val.(string); ok && strings.TrimSpace(str) != "" {
			return strings.TrimSpace(str)
		}
	}
	return ""
}
