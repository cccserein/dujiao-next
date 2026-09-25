package shop

import (
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"strings"
)

type Config struct {
	DatabaseURL    string
	AppURL         string
	ListenAddr     string
	Environment    string
	PaymentMode    string
	CardKey        []byte
	MockKey        []byte
	EpayURL        string
	EpayMerchantID string
	EpayKey        string
	BepURL         string
	BepToken       string
	BepCurrencies  string
}

func LoadConfig() (Config, error) {
	c := Config{
		DatabaseURL:    strings.TrimSpace(os.Getenv("SHOP_DATABASE_URL")),
		AppURL:         strings.TrimRight(strings.TrimSpace(os.Getenv("SHOP_APP_URL")), "/"),
		ListenAddr:     strings.TrimSpace(os.Getenv("SHOP_LISTEN_ADDR")),
		Environment:    strings.TrimSpace(os.Getenv("SHOP_ENV")),
		PaymentMode:    strings.TrimSpace(os.Getenv("SHOP_PAYMENT_MODE")),
		EpayURL:        strings.TrimRight(strings.TrimSpace(os.Getenv("SHOP_EPAY_URL")), "/"),
		EpayMerchantID: strings.TrimSpace(os.Getenv("SHOP_EPAY_MERCHANT_ID")),
		EpayKey:        strings.TrimSpace(os.Getenv("SHOP_EPAY_KEY")),
		BepURL:         strings.TrimRight(strings.TrimSpace(os.Getenv("SHOP_BEPUSDT_URL")), "/"),
		BepToken:       strings.TrimSpace(os.Getenv("SHOP_BEPUSDT_TOKEN")),
		BepCurrencies:  strings.ToUpper(strings.TrimSpace(os.Getenv("SHOP_BEPUSDT_CURRENCIES"))),
	}
	if c.ListenAddr == "" {
		c.ListenAddr = "127.0.0.1:8080"
	}
	if c.DatabaseURL == "" || c.AppURL == "" {
		return c, errors.New("SHOP_DATABASE_URL and SHOP_APP_URL are required")
	}
	u, err := url.Parse(c.AppURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return c, errors.New("SHOP_APP_URL must be an absolute http(s) origin")
	}
	if c.Environment != "development" && c.Environment != "production" {
		return c, errors.New("SHOP_ENV must be development or production")
	}
	if c.Environment == "production" && u.Scheme != "https" {
		return c, errors.New("production SHOP_APP_URL must use https")
	}
	key, err := hex.DecodeString(strings.TrimSpace(os.Getenv("SHOP_CARD_KEY_HEX")))
	if err != nil || len(key) != 32 {
		return c, errors.New("SHOP_CARD_KEY_HEX must contain 32 random bytes in hex")
	}
	c.CardKey = key
	switch c.PaymentMode {
	case "mock":
		if c.Environment != "development" {
			return c, errors.New("mock payment is forbidden in production")
		}
		mockKey, err := hex.DecodeString(strings.TrimSpace(os.Getenv("SHOP_MOCK_KEY_HEX")))
		if err != nil || len(mockKey) < 32 {
			return c, errors.New("SHOP_MOCK_KEY_HEX must contain at least 32 random bytes in hex")
		}
		c.MockKey = mockKey
	case "live":
		if !validGatewayBase(c.EpayURL) || c.EpayMerchantID == "" || len(c.EpayKey) < 16 ||
			!validGatewayBase(c.BepURL) || len(c.BepToken) < 16 {
			return c, errors.New("live gateway URLs and credentials are required")
		}
		if c.BepCurrencies == "" {
			c.BepCurrencies = "USDT"
		}
	default:
		return c, errors.New("SHOP_PAYMENT_MODE must be mock or live")
	}
	return c, nil
}

func validGatewayBase(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}
