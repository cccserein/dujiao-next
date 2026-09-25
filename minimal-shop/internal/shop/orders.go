package shop

import (
	"context"
	"strconv"
	"time"
)

const orderLifetime = 30 * time.Minute

func (a *App) CreateOrder(ctx context.Context, userID, productID int64) (int64, error) {
	if userID <= 0 || productID <= 0 {
		return 0, ErrInvalid
	}
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var lockedUserID int64
	err = tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&lockedUserID)
	if isNoRows(err) {
		return 0, ErrForbidden
	}
	if err != nil {
		return 0, err
	}
	var pendingCount int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM orders WHERE user_id=$1 AND status='pending' AND expires_at>now()`, userID).Scan(&pendingCount); err != nil {
		return 0, err
	}
	if pendingCount >= 3 {
		return 0, ErrOrderLimit
	}
	var p Product
	err = tx.QueryRow(ctx, `SELECT title,price_cents,currency,active FROM products WHERE id=$1 FOR SHARE`, productID).
		Scan(&p.Title, &p.PriceCents, &p.Currency, &p.Active)
	if isNoRows(err) || !p.Active {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	var id int64
	err = tx.QueryRow(ctx, `INSERT INTO orders (user_id,product_id,title_snapshot,amount_cents,currency,status,expires_at)
		VALUES ($1,$2,$3,$4,$5,'pending',$6) RETURNING id`, userID, productID, p.Title, p.PriceCents, p.Currency, time.Now().Add(orderLifetime)).Scan(&id)
	if err != nil {
		return 0, err
	}
	var cardID int64
	err = tx.QueryRow(ctx, `SELECT id FROM cards WHERE product_id=$1 AND reserved_order_id IS NULL AND assigned_order_id IS NULL
		ORDER BY id FOR UPDATE SKIP LOCKED LIMIT 1`, productID).Scan(&cardID)
	if isNoRows(err) {
		return 0, ErrOutOfStock
	}
	if err != nil {
		return 0, err
	}
	if _, err = tx.Exec(ctx, `UPDATE cards SET reserved_order_id=$1 WHERE id=$2`, id, cardID); err != nil {
		return 0, err
	}
	if _, err = tx.Exec(ctx, `UPDATE orders SET card_id=$1 WHERE id=$2`, cardID, id); err != nil {
		return 0, err
	}
	return id, tx.Commit(ctx)
}

func (a *App) GetOrder(ctx context.Context, requester User, id int64) (Order, error) {
	var o Order
	if requester.ID <= 0 || id <= 0 {
		return o, ErrForbidden
	}
	err := a.db.QueryRow(ctx, `SELECT o.id,o.user_id,o.product_id,o.title_snapshot,o.amount_cents,o.currency,o.status,
		COALESCE(o.active_payment_id,''),COALESCE(o.card_id,0),o.created_at,o.expires_at,COALESCE(p.pay_url,'')
		FROM orders o LEFT JOIN payments p ON p.id=o.active_payment_id WHERE o.id=$1`, id).
		Scan(&o.ID, &o.UserID, &o.ProductID, &o.Title, &o.AmountCents, &o.Currency, &o.Status,
			&o.ActivePaymentID, &o.CardID, &o.CreatedAt, &o.ExpiresAt, &o.PayURL)
	if isNoRows(err) {
		return Order{}, ErrNotFound
	}
	if err != nil {
		return Order{}, err
	}
	if requester.Role != "admin" && o.UserID != requester.ID {
		return Order{}, ErrNotFound
	}
	if o.Status == "pending" && !o.ExpiresAt.After(time.Now()) {
		o.Status = "expired"
		o.PayURL = ""
	}
	if o.Status == "delivered" {
		var nonce, ciphertext []byte
		err = a.db.QueryRow(ctx, `SELECT nonce,ciphertext FROM cards WHERE id=$1 AND assigned_order_id=$2`, o.CardID, o.ID).Scan(&nonce, &ciphertext)
		if err != nil {
			return Order{}, err
		}
		o.Card, err = a.cipher.Decrypt(o.ProductID, nonce, ciphertext)
		if err != nil {
			return Order{}, err
		}
		if requester.Role == "admin" {
			if _, err = a.db.Exec(ctx, `INSERT INTO audit_events (actor_user_id,action,target_type,target_id) VALUES ($1,'card.reveal','order',$2)`, requester.ID, strconv.FormatInt(o.ID, 10)); err != nil {
				return Order{}, err
			}
		}
	}
	return o, nil
}

func (a *App) ListOrders(ctx context.Context, requester User) ([]Order, error) {
	if requester.ID <= 0 {
		return nil, ErrForbidden
	}
	query := `SELECT id,user_id,product_id,title_snapshot,amount_cents,currency,status,created_at,expires_at FROM orders`
	args := []any{}
	if requester.Role != "admin" {
		query += ` WHERE user_id=$1`
		args = append(args, requester.ID)
	}
	query += ` ORDER BY id DESC LIMIT 100`
	rows, err := a.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	orders := []Order{}
	for rows.Next() {
		var o Order
		if err := rows.Scan(&o.ID, &o.UserID, &o.ProductID, &o.Title, &o.AmountCents, &o.Currency, &o.Status, &o.CreatedAt, &o.ExpiresAt); err != nil {
			return nil, err
		}
		orders = append(orders, o)
	}
	return orders, rows.Err()
}

func (a *App) ExpireOrders(ctx context.Context) (int, error) {
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT id,card_id FROM orders WHERE status='pending' AND expires_at<=now()
		ORDER BY id FOR UPDATE SKIP LOCKED LIMIT 100`)
	if err != nil {
		return 0, err
	}
	type expired struct{ id, cardID int64 }
	var found []expired
	for rows.Next() {
		var e expired
		if err = rows.Scan(&e.id, &e.cardID); err != nil {
			rows.Close()
			return 0, err
		}
		found = append(found, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	for _, e := range found {
		if _, err = tx.Exec(ctx, `UPDATE orders SET status='expired' WHERE id=$1 AND status='pending'`, e.id); err != nil {
			return 0, err
		}
		if _, err = tx.Exec(ctx, `UPDATE cards SET reserved_order_id=NULL WHERE id=$1 AND reserved_order_id=$2`, e.cardID, e.id); err != nil {
			return 0, err
		}
	}
	return len(found), tx.Commit(ctx)
}
