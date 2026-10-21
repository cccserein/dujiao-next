package epayadapter

import (
	"context"
	"errors"
	"strings"

	"github.com/dujiao-next/internal/constants"
	paymentcontract "github.com/dujiao-next/internal/modules/payment/contract"
	"github.com/dujiao-next/internal/modules/payment/infrastructure/gateway/epay"
	"github.com/dujiao-next/internal/shared/jsonmap"
	"github.com/dujiao-next/internal/shared/money"
)

func (a *epayAdapter) LookupReceipt(ctx context.Context, raw jsonmap.JSON, orderNo string, amount money.Amount, currency string) (*paymentcontract.GatewayQueryResult, error) {
	cfg, err := epay.ParseConfig(raw)
	if err != nil {
		return nil, mapEpayError(err)
	}
	expected, err := epay.ParseAmount(amount.String())
	if err != nil {
		return nil, mapEpayError(err)
	}
	configuredCurrency, err := epay.ConfiguredCurrency(cfg)
	if err != nil || !strings.EqualFold(strings.TrimSpace(currency), configuredCurrency) {
		return nil, paymentcontract.ErrGatewayResponseInvalid
	}
	receipt, err := epay.QueryOrderByMerchantNo(ctx, cfg, orderNo)
	if errors.Is(err, epay.ErrQueryOrderMissing) {
		return &paymentcontract.GatewayQueryResult{Status: constants.PaymentStatusPending}, nil
	}
	if errors.Is(err, epay.ErrQueryUnsupported) {
		return nil, paymentcontract.ErrGatewayCorroborationUnsupported
	}
	if err != nil {
		return nil, mapEpayError(err)
	}
	actual, err := epay.ParseAmount(receipt.Money)
	if err != nil || !actual.Equal(expected) || receipt.Currency != configuredCurrency {
		return nil, paymentcontract.ErrGatewayResponseInvalid
	}
	status := constants.PaymentStatusPending
	if receipt.Status == 1 {
		status = constants.PaymentStatusSuccess
	}
	return &paymentcontract.GatewayQueryResult{
		ProviderRef: receipt.TradeNo, Status: status,
		Amount: money.FromDecimal(actual), Currency: receipt.Currency,
	}, nil
}
