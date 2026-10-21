package consumer

import (
	"context"
	"github.com/hibiken/asynq"
)

func (c *Consumer) handlePaymentReceiptPoll(ctx context.Context, _ *asynq.Task) error {
	if c == nil || c.Container == nil || c.PaymentService == nil {
		return nil
	}
	return c.PaymentService.ReconcilePendingReceipts(ctx)
}
