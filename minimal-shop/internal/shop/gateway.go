package shop

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

type PaymentStart struct {
	PaymentID       string
	OrderID         int64
	AmountCents     int64
	Currency        string
	ProviderOrderID string
}

type PaymentStartResult struct {
	URL         string
	ProviderRef string
}

type PaymentEvent struct {
	EventID         string `json:"event_id"`
	PaymentID       string `json:"payment_id"`
	ProviderOrderID string `json:"provider_order_id"`
	ProviderRef     string `json:"provider_ref"`
	AmountCents     int64  `json:"amount_cents"`
	Currency        string `json:"currency"`
	Status          string `json:"status"`
}

type Gateway interface {
	Name() string
	Start(context.Context, PaymentStart) (PaymentStartResult, error)
	Verify(context.Context, http.Header, []byte) (PaymentEvent, error)
}

type MockGateway struct {
	AppURL string
	Key    []byte
}

func (g MockGateway) Name() string { return "mock" }

func (g MockGateway) Start(_ context.Context, p PaymentStart) (PaymentStartResult, error) {
	return PaymentStartResult{URL: g.AppURL + "/mock/pay/" + p.PaymentID, ProviderRef: p.PaymentID}, nil
}

func (g MockGateway) Sign(body []byte) string {
	h := hmac.New(sha256.New, g.Key)
	_, _ = h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

func (g MockGateway) Verify(_ context.Context, header http.Header, body []byte) (PaymentEvent, error) {
	var event PaymentEvent
	provided, err := hex.DecodeString(strings.TrimSpace(header.Get("X-Shop-Signature")))
	if err != nil {
		return event, errors.New("invalid webhook signature")
	}
	expected, _ := hex.DecodeString(g.Sign(body))
	if !hmac.Equal(provided, expected) {
		return event, errors.New("invalid webhook signature")
	}
	if err := json.Unmarshal(body, &event); err != nil {
		return event, errors.New("invalid webhook body")
	}
	if event.EventID == "" || event.PaymentID == "" || event.ProviderOrderID == "" || event.ProviderRef == "" || event.AmountCents <= 0 || event.Currency == "" || event.Status != "paid" {
		return PaymentEvent{}, errors.New("invalid webhook facts")
	}
	return event, nil
}
