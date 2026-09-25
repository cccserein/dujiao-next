package shop

import (
	"context"
	_ "embed"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaSQL string

var (
	ErrNotFound          = errors.New("not found")
	ErrForbidden         = errors.New("forbidden")
	ErrInvalid           = errors.New("invalid request")
	ErrOutOfStock        = errors.New("out of stock")
	ErrOrderLimit        = errors.New("too many pending orders")
	ErrOrderClosed       = errors.New("order closed")
	ErrPaymentProcessing = errors.New("payment is being created")
	ErrPaymentReview     = errors.New("payment requires manual review")
)

type App struct {
	db       *pgxpool.Pool
	cipher   *CardCipher
	gateways map[string]Gateway
}

func Open(ctx context.Context, cfg Config) (*App, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, errors.New("invalid database URL")
	}
	poolCfg.MaxConns = 10
	poolCfg.MinConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, errors.New("database connection failed")
	}
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(checkCtx); err != nil {
		pool.Close()
		return nil, errors.New("database unavailable")
	}
	cipher, err := NewCardCipher(cfg.CardKey)
	if err != nil {
		pool.Close()
		return nil, err
	}
	a := &App{db: pool, cipher: cipher, gateways: map[string]Gateway{}}
	if cfg.PaymentMode == "mock" {
		a.gateways["mock"] = MockGateway{AppURL: cfg.AppURL, Key: cfg.MockKey}
	} else if cfg.PaymentMode == "live" {
		a.gateways["epay"] = EpayGateway{BaseURL: cfg.EpayURL, MerchantID: cfg.EpayMerchantID, MerchantKey: cfg.EpayKey, AppURL: cfg.AppURL}
		a.gateways["bepusdt"] = BepGateway{BaseURL: cfg.BepURL, Token: cfg.BepToken, Currencies: cfg.BepCurrencies, AppURL: cfg.AppURL}
	}
	return a, nil
}

func (a *App) Close() { a.db.Close() }

func (a *App) Migrate(ctx context.Context) error {
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(6140901026)`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY)`); err != nil {
		return err
	}
	var installed bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = 1)`).Scan(&installed); err != nil {
		return err
	}
	if !installed {
		for _, statement := range strings.Split(schemaSQL, ";") {
			statement = strings.TrimSpace(statement)
			if statement == "" {
				continue
			}
			if _, err = tx.Exec(ctx, statement); err != nil {
				return err
			}
		}
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES (1)`); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, `ALTER TABLE sessions ADD COLUMN IF NOT EXISTS last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now()`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES (2) ON CONFLICT (version) DO NOTHING`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
