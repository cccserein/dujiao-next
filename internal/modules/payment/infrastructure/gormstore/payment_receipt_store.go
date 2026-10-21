package gormstore

import (
	"time"

	"github.com/dujiao-next/internal/constants"
	paymentdomain "github.com/dujiao-next/internal/modules/payment/domain"
)

func (r *Store) ListPendingReceiptPayments(afterID uint, limit int, now time.Time) ([]paymentdomain.Payment, error) {
	if limit < 1 || limit > 100 {
		limit = 20
	}
	var payments []paymentdomain.Payment
	err := r.db.Where("deleted_at IS NULL AND id > ? AND provider_type = ? AND status = ? AND gateway_order_no <> '' AND (expired_at IS NULL OR expired_at > ?) AND created_at > ?",
		afterID, constants.PaymentProviderEpay, constants.PaymentStatusPending, now, now.Add(-24*time.Hour)).
		Order("id asc").Limit(limit).Find(&payments).Error
	return payments, err
}
