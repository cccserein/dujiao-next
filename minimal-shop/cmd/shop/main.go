package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/cccserein/dujiao-next/minimal-shop/internal/shop"
	"golang.org/x/term"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: shop migrate|admin-create|serve")
		os.Exit(2)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := shop.LoadConfig()
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	app, err := shop.Open(ctx, cfg)
	if err != nil {
		logger.Error("startup failed", "error", err)
		os.Exit(1)
	}
	defer app.Close()
	switch os.Args[1] {
	case "migrate":
		if err := app.Migrate(ctx); err != nil {
			logger.Error("migration failed", "error", err)
			os.Exit(1)
		}
		logger.Info("migration complete")
	case "admin-create":
		if err := createAdmin(ctx, app); err != nil {
			logger.Error("admin creation failed", "error", err)
			os.Exit(1)
		}
		logger.Info("admin created")
	case "serve":
		serve(ctx, app, cfg, logger)
	default:
		fmt.Fprintln(os.Stderr, "usage: shop migrate|admin-create|serve")
		os.Exit(2)
	}
}

func createAdmin(ctx context.Context, app *shop.App) error {
	reader := bufio.NewReader(os.Stdin)
	fmt.Fprint(os.Stderr, "Admin email: ")
	email, err := reader.ReadString('\n')
	if err != nil {
		return err
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New("admin-create requires a terminal")
	}
	fmt.Fprint(os.Stderr, "Admin password (12+ characters): ")
	password, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return err
	}
	admin, err := app.CreateUser(ctx, strings.TrimSpace(email), string(password), "admin")
	for i := range password {
		password[i] = 0
	}
	if err != nil {
		return err
	}
	secret, err := shop.NewTOTPSecret()
	if err != nil {
		return err
	}
	if err := app.EnrollAdminTOTP(ctx, admin.ID, secret); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Add this TOTP secret to your authenticator now; it is shown only once: %s\n", secret)
	return nil
}

func serve(ctx context.Context, app *shop.App, cfg shop.Config, logger *slog.Logger) {
	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           shop.NewWeb(app, cfg, logger),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				expireCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				count, err := app.ExpireOrders(expireCtx)
				cancel()
				if err != nil {
					logger.Error("order expiry failed", "error", err)
				} else if count > 0 {
					logger.Info("orders expired", "count", count)
				}
			}
		}
	}()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	logger.Info("shop listening", "address", cfg.ListenAddr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server failed", "error", err)
		os.Exit(1)
	}
}
