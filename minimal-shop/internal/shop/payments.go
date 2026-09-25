package shop

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"
)

type Checkout struct {
	PaymentID string
	URL       string
}

func (a *App) GatewayNames() []string {
	names := make([]string, 0, len(a.gateways))
	for name := range a.gateways {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (a *App) ListPaymentExceptions(ctx context.Context) ([]PaymentException, error) {
	rows, err := a.db.Query(ctx, `SELECT p.id,p.order_id,u.email,p.provider,p.expected_cents,COALESCE(p.paid_cents,0),p.currency,
		COALESCE(p.exception_reason,''),p.created_at FROM payments p
		JOIN orders o ON o.id=p.order_id JOIN users u ON u.id=o.user_id
		WHERE p.status='exception' ORDER BY p.created_at DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	exceptions := []PaymentException{}
	for rows.Next() {
		var item PaymentException
		if err := rows.Scan(&item.ID, &item.OrderID, &item.CustomerEmail, &item.Provider, &item.ExpectedCents,
			&item.PaidCents, &item.Currency, &item.Reason, &item.CreatedAt); err != nil {
			return nil, err
		}
		exceptions = append(exceptions, item)
	}
	return exceptions, rows.Err()
}

func (a *App) CreatePayment(ctx context.Context, userID, orderID int64, provider string) (Checkout, error) {
	var checkout Checkout
	gateway, ok := a.gateways[provider]
	if !ok || userID <= 0 || orderID <= 0 {
		return checkout, ErrInvalid
	}
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return checkout, err
	}
	defer tx.Rollback(ctx)
	var order Order
	err = tx.QueryRow(ctx, `SELECT user_id,amount_cents,currency,status,expires_at,COALESCE(active_payment_id,'')
		FROM orders WHERE id=$1 FOR UPDATE`, orderID).
		Scan(&order.UserID, &order.AmountCents, &order.Currency, &order.Status, &order.ExpiresAt, &order.ActivePaymentID)
	if isNoRows(err) || order.UserID != userID {
		return checkout, ErrNotFound
	}
	if err != nil {
		return checkout, err
	}
	if order.Status != "pending" || !order.ExpiresAt.After(time.Now()) {
		return checkout, ErrOrderClosed
	}
	if order.ActivePaymentID != "" {
		var oldProvider, oldStatus, oldURL string
		err = tx.QueryRow(ctx, `SELECT provider,status,pay_url FROM payments WHERE id=$1 FOR UPDATE`, order.ActivePaymentID).
			Scan(&oldProvider, &oldStatus, &oldURL)
		if err != nil {
			return checkout, err
		}
		if oldStatus == "exception" || oldStatus == "succeeded" {
			return checkout, ErrPaymentReview
		}
		if oldStatus == "initiated" {
			return checkout, ErrPaymentProcessing
		}
		if oldStatus == "pending" && oldProvider == provider {
			if oldURL == "" {
				return checkout, ErrPaymentProcessing
			}
			return Checkout{PaymentID: order.ActivePaymentID, URL: oldURL}, tx.Commit(ctx)
		}
		if oldStatus == "pending" {
			if _, err = tx.Exec(ctx, `UPDATE payments SET status='superseded' WHERE id=$1`, order.ActivePaymentID); err != nil {
				return checkout, err
			}
		}
	}
	checkout.PaymentID, _, err = randomToken()
	if err != nil {
		return checkout, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO payments (id,order_id,provider,provider_order_id,expected_cents,currency,status)
		VALUES ($1,$2,$3,$4,$5,$6,'initiated')`, checkout.PaymentID, orderID, provider, checkout.PaymentID, order.AmountCents, order.Currency)
	if err != nil {
		return Checkout{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE orders SET active_payment_id=$1 WHERE id=$2`, checkout.PaymentID, orderID); err != nil {
		return Checkout{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Checkout{}, err
	}
	started, err := gateway.Start(ctx, PaymentStart{
		PaymentID: checkout.PaymentID, OrderID: orderID, AmountCents: order.AmountCents,
		Currency: order.Currency, ProviderOrderID: checkout.PaymentID,
	})
	if err == nil {
		checkout.URL = started.URL
		var parsed *url.URL
		parsed, err = url.Parse(checkout.URL)
		if err == nil && (parsed == nil || parsed.Host == "" || (parsed.Scheme != "https" && (provider != "mock" || parsed.Scheme != "http"))) {
			err = errors.New("gateway returned invalid payment URL")
		}
	}
	if err != nil {
		a.markPaymentStartFailed(ctx, orderID, checkout.PaymentID)
		return Checkout{}, errors.New("payment gateway unavailable")
	}
	_, err = a.db.Exec(ctx, `UPDATE payments SET pay_url=$1,provider_ref=$2,status='pending' WHERE id=$3 AND status='initiated'`, checkout.URL, started.ProviderRef, checkout.PaymentID)
	if err != nil {
		return Checkout{}, err
	}
	return checkout, nil
}

func (a *App) markPaymentStartFailed(ctx context.Context, orderID int64, paymentID string) {
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return
	}
	defer tx.Rollback(ctx)
	var active string
	if err = tx.QueryRow(ctx, `SELECT COALESCE(active_payment_id,'') FROM orders WHERE id=$1 FOR UPDATE`, orderID).Scan(&active); err != nil || active != paymentID {
		return
	}
	updated, err := tx.Exec(ctx, `UPDATE payments SET status='failed' WHERE id=$1 AND status='initiated'`, paymentID)
	if err != nil || updated.RowsAffected() != 1 {
		return
	}
	if _, err = tx.Exec(ctx, `UPDATE orders SET active_payment_id=NULL WHERE id=$1`, orderID); err != nil {
		return
	}
	_ = tx.Commit(ctx)
}

// applyVerifiedPayment accepts facts only after a provider-specific signature check.
// All amount and ownership checks happen again under row locks before releasing a card.
func (a *App) applyVerifiedPayment(ctx context.Context, provider string, event PaymentEvent) (string, error) {
	if provider == "" || event.EventID == "" || event.PaymentID == "" || event.ProviderOrderID == "" || event.ProviderRef == "" || event.AmountCents <= 0 || event.Status != "paid" {
		return "", ErrInvalid
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	var orderID int64
	err = a.db.QueryRow(ctx, `SELECT order_id FROM payments WHERE id=$1`, event.PaymentID).Scan(&orderID)
	if isNoRows(err) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	var order Order
	err = tx.QueryRow(ctx, `SELECT product_id,amount_cents,currency,status,expires_at,
		COALESCE(active_payment_id,''),COALESCE(card_id,0) FROM orders WHERE id=$1 FOR UPDATE`, orderID).
		Scan(&order.ProductID, &order.AmountCents, &order.Currency, &order.Status, &order.ExpiresAt, &order.ActivePaymentID, &order.CardID)
	if err != nil {
		return "", err
	}
	var storedProvider, providerOrderID, providerRef, currency, status string
	var expectedCents int64
	err = tx.QueryRow(ctx, `SELECT provider,provider_order_id,provider_ref,expected_cents,currency,status FROM payments WHERE id=$1 FOR UPDATE`, event.PaymentID).
		Scan(&storedProvider, &providerOrderID, &providerRef, &expectedCents, &currency, &status)
	if err != nil {
		return "", err
	}
	if storedProvider != provider {
		return "", ErrInvalid
	}
	result, err := tx.Exec(ctx, `INSERT INTO webhook_events (provider,event_id,payment_id,digest)
		VALUES ($1,$2,$3,$4) ON CONFLICT (provider,event_id) DO NOTHING`, provider, event.EventID, event.PaymentID, digest[:])
	if err != nil {
		return "", err
	}
	if result.RowsAffected() == 0 {
		var previousPaymentID string
		var previousDigest []byte
		if err = tx.QueryRow(ctx, `SELECT payment_id,digest FROM webhook_events WHERE provider=$1 AND event_id=$2`, provider, event.EventID).
			Scan(&previousPaymentID, &previousDigest); err != nil {
			return "", err
		}
		if previousPaymentID != event.PaymentID || !bytes.Equal(previousDigest, digest[:]) {
			return "", ErrInvalid
		}
		return "duplicate", tx.Commit(ctx)
	}
	if status == "succeeded" {
		return "duplicate", tx.Commit(ctx)
	}
	valid := providerOrderID == event.ProviderOrderID && (providerRef == "" || providerRef == event.ProviderRef) && expectedCents == event.AmountCents &&
		currency == event.Currency && order.AmountCents == event.AmountCents && order.Currency == event.Currency &&
		order.Status == "pending" && order.ActivePaymentID == event.PaymentID && order.ExpiresAt.After(time.Now()) &&
		order.CardID > 0 && (status == "pending" || status == "initiated")
	if !valid {
		_, err = tx.Exec(ctx, `UPDATE payments SET status='exception',paid_cents=$1,paid_at=now(),exception_reason='payment facts or order state mismatch' WHERE id=$2`, event.AmountCents, event.PaymentID)
		if err != nil {
			return "", err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO audit_events (action,target_type,target_id) VALUES ('payment.exception','payment',$1)`, event.PaymentID); err != nil {
			return "", err
		}
		return "review", tx.Commit(ctx)
	}
	cardResult, err := tx.Exec(ctx, `UPDATE cards SET reserved_order_id=NULL,assigned_order_id=$1
		WHERE id=$2 AND product_id=$3 AND reserved_order_id=$1 AND assigned_order_id IS NULL`, orderID, order.CardID, order.ProductID)
	if err != nil {
		return "", err
	}
	if cardResult.RowsAffected() != 1 {
		return "", errors.New("reserved card missing")
	}
	if _, err = tx.Exec(ctx, `UPDATE payments SET status='succeeded',paid_cents=$1,paid_at=now() WHERE id=$2`, event.AmountCents, event.PaymentID); err != nil {
		return "", err
	}
	if _, err = tx.Exec(ctx, `UPDATE orders SET status='delivered',paid_at=now(),delivered_at=now() WHERE id=$1`, orderID); err != nil {
		return "", err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audit_events (action,target_type,target_id) VALUES ('card.deliver','order',$1)`, strconv.FormatInt(orderID, 10)); err != nil {
		return "", err
	}
	return "delivered", tx.Commit(ctx)
}

func (a *App) VerifyWebhook(ctx context.Context, provider string, headers http.Header, body []byte) (PaymentEvent, error) {
	gateway, ok := a.gateways[provider]
	if !ok {
		return PaymentEvent{}, ErrNotFound
	}
	return gateway.Verify(ctx, headers, body)
}

func (a *App) GetMockPaymentOrder(ctx context.Context, user User, paymentID string) (Order, error) {
	var o Order
	if user.ID <= 0 || paymentID == "" {
		return o, ErrForbidden
	}
	var provider, paymentStatus string
	err := a.db.QueryRow(ctx, `SELECT o.id,o.user_id,o.amount_cents,o.currency,o.status,p.provider,p.status
		FROM payments p JOIN orders o ON o.id=p.order_id WHERE p.id=$1`, paymentID).
		Scan(&o.ID, &o.UserID, &o.AmountCents, &o.Currency, &o.Status, &provider, &paymentStatus)
	if isNoRows(err) || o.UserID != user.ID || provider != "mock" {
		return Order{}, ErrNotFound
	}
	if err != nil {
		return Order{}, err
	}
	if o.Status != "pending" || paymentStatus != "pending" {
		return Order{}, ErrOrderClosed
	}
	return o, nil
}
