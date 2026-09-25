package shop

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// BepGateway implements the cashier order mode displayed on the current site.
type BepGateway struct {
	BaseURL    string
	Token      string
	Currencies string
	AppURL     string
	Client     *http.Client
}

func (g BepGateway) Name() string { return "bepusdt" }

func (g BepGateway) Start(ctx context.Context, p PaymentStart) (PaymentStartResult, error) {
	if !validGatewayBase(g.BaseURL) || g.Token == "" || p.Currency != "CNY" || p.AmountCents <= 0 {
		return PaymentStartResult{}, ErrInvalid
	}
	params := map[string]any{
		"order_id":     p.ProviderOrderID,
		"amount":       float64(p.AmountCents) / 100, // Bepusdt v1 signature uses Go's float string representation.
		"notify_url":   g.AppURL + "/webhooks/bepusdt",
		"redirect_url": fmt.Sprintf("%s/orders/%d", g.AppURL, p.OrderID),
		"fiat":         "CNY",
		"currencies":   g.Currencies,
		"name":         fmt.Sprintf("订单 %d", p.OrderID),
	}
	params["signature"] = bepSignature(params, g.Token)
	body, err := json.Marshal(params)
	if err != nil {
		return PaymentStartResult{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, g.BaseURL+"/api/v1/order/create-order", bytes.NewReader(body))
	if err != nil {
		return PaymentStartResult{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	client := g.Client
	if client == nil {
		client = &http.Client{Timeout: 12 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return errors.New("gateway redirect refused")
		}}
	}
	response, err := client.Do(request)
	if err != nil {
		return PaymentStartResult{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return PaymentStartResult{}, errors.New("gateway rejected payment")
	}
	limited, err := io.ReadAll(io.LimitReader(response.Body, 1<<20+1))
	if err != nil || len(limited) > 1<<20 {
		return PaymentStartResult{}, errors.New("invalid gateway response")
	}
	var result struct {
		StatusCode int `json:"status_code"`
		Data       struct {
			Fiat       string `json:"fiat"`
			TradeID    string `json:"trade_id"`
			OrderID    string `json:"order_id"`
			Amount     string `json:"amount"`
			PaymentURL string `json:"payment_url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(limited, &result); err != nil || result.StatusCode != 200 || result.Data.TradeID == "" ||
		result.Data.OrderID != p.ProviderOrderID || (result.Data.Fiat != "" && result.Data.Fiat != p.Currency) {
		return PaymentStartResult{}, errors.New("gateway response mismatch")
	}
	cents, err := providerAmountCents(result.Data.Amount)
	if err != nil || cents != p.AmountCents {
		return PaymentStartResult{}, errors.New("gateway amount mismatch")
	}
	return PaymentStartResult{URL: result.Data.PaymentURL, ProviderRef: result.Data.TradeID}, nil
}

type bepCallback struct {
	TradeID            string          `json:"trade_id"`
	OrderID            string          `json:"order_id"`
	Amount             json.RawMessage `json:"amount"`
	ActualAmount       json.RawMessage `json:"actual_amount"`
	Token              string          `json:"token"`
	BlockTransactionID string          `json:"block_transaction_id"`
	Signature          string          `json:"signature"`
	Status             int             `json:"status"`
}

func (g BepGateway) Verify(_ context.Context, _ http.Header, body []byte) (PaymentEvent, error) {
	var event PaymentEvent
	if len(body) == 0 || len(body) > 64<<10 {
		return event, ErrInvalid
	}
	var callback bepCallback
	if err := json.Unmarshal(body, &callback); err != nil || callback.TradeID == "" || callback.OrderID == "" || callback.Status != 2 {
		return event, ErrInvalid
	}
	amountFloat, amountText, err := bepNumber(callback.Amount)
	if err != nil || amountFloat <= 0 {
		return event, ErrInvalid
	}
	actualFloat, _, err := bepNumber(callback.ActualAmount)
	if err != nil {
		return event, ErrInvalid
	}
	fields := map[string]any{
		"trade_id":             callback.TradeID,
		"order_id":             callback.OrderID,
		"amount":               amountFloat,
		"actual_amount":        actualFloat,
		"token":                callback.Token,
		"block_transaction_id": callback.BlockTransactionID,
		"status":               callback.Status,
	}
	provided, err := hex.DecodeString(strings.TrimSpace(callback.Signature))
	if err != nil || len(provided) != md5.Size {
		return event, errors.New("invalid bepusdt signature")
	}
	expected, _ := hex.DecodeString(bepSignature(fields, g.Token))
	if !hmac.Equal(provided, expected) {
		return event, errors.New("invalid bepusdt signature")
	}
	cents, err := providerAmountCents(amountText)
	if err != nil {
		return event, ErrInvalid
	}
	return PaymentEvent{EventID: callback.TradeID, PaymentID: callback.OrderID, ProviderOrderID: callback.OrderID,
		ProviderRef: callback.TradeID, AmountCents: cents, Currency: "CNY", Status: "paid"}, nil
}

func bepNumber(raw json.RawMessage) (float64, string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, "0", nil
	}
	text := strings.TrimSpace(string(raw))
	if strings.HasPrefix(text, `"`) {
		if err := json.Unmarshal(raw, &text); err != nil {
			return 0, "", err
		}
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0, "", ErrInvalid
	}
	return value, text, nil
}

func bepSignature(fields map[string]any, token string) string {
	keys := make([]string, 0, len(fields))
	for name, value := range fields {
		if name == "signature" || value == nil {
			continue
		}
		if str, ok := value.(string); ok && strings.TrimSpace(str) == "" {
			continue
		}
		keys = append(keys, name)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, name := range keys {
		pairs = append(pairs, name+"="+fmt.Sprint(fields[name]))
	}
	sum := md5.Sum([]byte(strings.Join(pairs, "&") + token)) // Required by Bepusdt protocol.
	return hex.EncodeToString(sum[:])
}
