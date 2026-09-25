package shop

import (
	"context"
	"errors"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func cleanEmail(raw string) (string, error) {
	email := strings.ToLower(strings.TrimSpace(raw))
	parsed, err := mail.ParseAddress(email)
	if err != nil || parsed.Address != email || len(email) > 254 {
		return "", ErrInvalid
	}
	return email, nil
}

func (a *App) CreateUser(ctx context.Context, email, password, role string) (User, error) {
	var user User
	var err error
	user.Email, err = cleanEmail(email)
	if err != nil || (role != "customer" && role != "admin") || len(password) < 12 || len(password) > 72 {
		return user, ErrInvalid
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return user, err
	}
	user.Role = role
	err = a.db.QueryRow(ctx, `INSERT INTO users (email,password_hash,role) VALUES ($1,$2,$3) RETURNING id`, user.Email, string(hash), role).Scan(&user.ID)
	if err != nil {
		return User{}, errors.New("user creation failed; email may already exist")
	}
	return user, nil
}

func (a *App) Login(ctx context.Context, email, password, code string) (string, User, error) {
	var user User
	var hash string
	var totpNonce, totpCiphertext []byte
	clean, err := cleanEmail(email)
	if err != nil {
		return "", user, ErrForbidden
	}
	err = a.db.QueryRow(ctx, `SELECT id,email,role,password_hash,totp_nonce,totp_ciphertext FROM users WHERE lower(email)=$1`, clean).
		Scan(&user.ID, &user.Email, &user.Role, &hash, &totpNonce, &totpCiphertext)
	if err != nil || bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return "", User{}, ErrForbidden
	}
	if user.Role == "admin" {
		secret, err := a.adminTOTPSecret(user.ID, totpNonce, totpCiphertext)
		if err != nil {
			return "", User{}, ErrForbidden
		}
		step, valid := validTOTP(secret, code, time.Now())
		if !valid {
			return "", User{}, ErrForbidden
		}
		updated, err := a.db.Exec(ctx, `UPDATE users SET last_totp_step=$1 WHERE id=$2 AND last_totp_step<$1`, step, user.ID)
		if err != nil || updated.RowsAffected() != 1 {
			return "", User{}, ErrForbidden
		}
	}
	token, digest, err := randomToken()
	if err != nil {
		return "", User{}, err
	}
	validity := 7 * 24 * time.Hour
	if user.Role == "admin" {
		validity = 12 * time.Hour
	}
	_, err = a.db.Exec(ctx, `INSERT INTO sessions (token_hash,user_id,expires_at) VALUES ($1,$2,$3)`, digest[:], user.ID, time.Now().Add(validity))
	if err != nil {
		return "", User{}, err
	}
	return token, user, nil
}

func (a *App) SessionUser(ctx context.Context, rawToken string) (User, error) {
	var user User
	digest, err := tokenHash(rawToken)
	if err != nil {
		return user, ErrForbidden
	}
	err = a.db.QueryRow(ctx, `SELECT u.id,u.email,u.role FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.token_hash=$1 AND s.expires_at>now()`, digest[:]).Scan(&user.ID, &user.Email, &user.Role)
	if err != nil {
		return User{}, ErrForbidden
	}
	return user, nil
}

func (a *App) Logout(ctx context.Context, rawToken string) {
	digest, err := tokenHash(rawToken)
	if err == nil {
		_, _ = a.db.Exec(ctx, `DELETE FROM sessions WHERE token_hash=$1`, digest[:])
	}
}

func (a *App) ListProducts(ctx context.Context, admin bool) ([]Product, error) {
	query := `SELECT p.id,p.title,p.description,p.price_cents,p.currency,p.active,
		(SELECT count(*) FROM cards c WHERE c.product_id=p.id AND c.reserved_order_id IS NULL AND c.assigned_order_id IS NULL)
		FROM products p`
	if !admin {
		query += ` WHERE p.active=true`
	}
	query += ` ORDER BY p.id DESC LIMIT 200`
	rows, err := a.db.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	products := []Product{}
	for rows.Next() {
		var p Product
		if err := rows.Scan(&p.ID, &p.Title, &p.Description, &p.PriceCents, &p.Currency, &p.Active, &p.Stock); err != nil {
			return nil, err
		}
		products = append(products, p)
	}
	return products, rows.Err()
}

func (a *App) CreateProduct(ctx context.Context, adminID int64, title, description string, priceCents int64) error {
	title, description = strings.TrimSpace(title), strings.TrimSpace(description)
	if adminID <= 0 || len(title) == 0 || len(title) > 160 || len(description) > 5000 || priceCents <= 0 {
		return ErrInvalid
	}
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var id int64
	if err = tx.QueryRow(ctx, `INSERT INTO products (title,description,price_cents,currency,active) VALUES ($1,$2,$3,'CNY',false) RETURNING id`, title, description, priceCents).Scan(&id); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audit_events (actor_user_id,action,target_type,target_id) VALUES ($1,'product.create','product',$2)`, adminID, strconv.FormatInt(id, 10)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (a *App) SetProductActive(ctx context.Context, adminID, productID int64, active bool) error {
	if adminID <= 0 || productID <= 0 {
		return ErrInvalid
	}
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	result, err := tx.Exec(ctx, `UPDATE products SET active=$1,updated_at=now() WHERE id=$2`, active, productID)
	if err != nil || result.RowsAffected() != 1 {
		return ErrNotFound
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audit_events (actor_user_id,action,target_type,target_id) VALUES ($1,$2,'product',$3)`, adminID, map[bool]string{true: "product.publish", false: "product.hide"}[active], strconv.FormatInt(productID, 10)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (a *App) ImportCards(ctx context.Context, adminID, productID int64, lines []string) (int, error) {
	if adminID <= 0 || productID <= 0 || len(lines) == 0 || len(lines) > 1000 {
		return 0, ErrInvalid
	}
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM products WHERE id=$1)`, productID).Scan(&exists); err != nil || !exists {
		return 0, ErrNotFound
	}
	inserted := 0
	for _, raw := range lines {
		card := strings.TrimSpace(raw)
		if card == "" {
			continue
		}
		if len(card) > 4096 {
			return 0, ErrInvalid
		}
		nonce, ciphertext, err := a.cipher.Encrypt(productID, card)
		if err != nil {
			return 0, err
		}
		result, err := tx.Exec(ctx, `INSERT INTO cards (product_id,fingerprint,nonce,ciphertext) VALUES ($1,$2,$3,$4) ON CONFLICT (fingerprint) DO NOTHING`, productID, a.cipher.Fingerprint(card), nonce, ciphertext)
		if err != nil {
			return 0, err
		}
		inserted += int(result.RowsAffected())
	}
	if inserted == 0 {
		return 0, ErrInvalid
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audit_events (actor_user_id,action,target_type,target_id) VALUES ($1,'card.import','product',$2)`, adminID, strconv.FormatInt(productID, 10)); err != nil {
		return 0, err
	}
	return inserted, tx.Commit(ctx)
}
