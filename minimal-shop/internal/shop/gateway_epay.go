package shop

import (
	"context"
	"crypto/hmac"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// EpayGateway implements only the currently displayed Epay v1 Alipay redirect channel.
type EpayGateway struct {
	BaseURL     string
	MerchantID  string
	MerchantKey string
	AppURL      string
}

func (g EpayGateway) Name() string { return "epay" }

func (g EpayGateway) Start(_ context.Context, p PaymentStart) (PaymentStartResult, error) {
	if !validGatewayBase(g.BaseURL) || g.MerchantID == "" || g.MerchantKey == "" || p.Currency != "CNY" || p.AmountCents <= 0 {
		return PaymentStartResult{}, ErrInvalid
	}
	fields := url.Values{
		"pid":          {g.MerchantID},
		"type":         {"alipay"},
		"out_trade_no": {p.ProviderOrderID},
		"notify_url":   {g.AppURL + "/webhooks/epay"},
		"return_url":   {fmt.Sprintf("%s/orders/%d", g.AppURL, p.OrderID)},
		"name":         {fmt.Sprintf("订单 %d", p.OrderID)},
		"money":        {fmt.Sprintf("%d.%02d", p.AmountCents/100, p.AmountCents%100)},
	}
	sign := epaySignature(fields, g.MerchantKey)
	fields.Set("sign", sign)
	fields.Set("sign_type", "MD5")
	return PaymentStartResult{URL: g.BaseURL + "/submit.php?" + fields.Encode()}, nil
}

func epaySignature(fields url.Values, key string) string {
	keys := make([]string, 0, len(fields))
	for name, values := range fields {
		if name != "sign" && name != "sign_type" && len(values) > 0 && values[0] != "" {
			keys = append(keys, name)
		}
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, name := range keys {
		pairs = append(pairs, name+"="+fields.Get(name))
	}
	sum := md5.Sum([]byte(strings.Join(pairs, "&") + key)) // Required by Epay v1 protocol.
	return hex.EncodeToString(sum[:])
}

func (g EpayGateway) Verify(_ context.Context, _ http.Header, body []byte) (PaymentEvent, error) {
	var event PaymentEvent
	if len(body) == 0 || len(body) > 64<<10 {
		return event, ErrInvalid
	}
	fields, err := url.ParseQuery(string(body))
	if err != nil {
		return event, ErrInvalid
	}
	for _, values := range fields {
		if len(values) != 1 {
			return event, ErrInvalid
		}
	}
	if fields.Get("pid") != g.MerchantID || fields.Get("out_trade_no") == "" || fields.Get("trade_no") == "" ||
		(fields.Get("trade_status") != "TRADE_SUCCESS" && fields.Get("trade_status") != "TRADE_FINISHED") ||
		(fields.Get("type") != "" && fields.Get("type") != "alipay") ||
		(fields.Get("sign_type") != "" && !strings.EqualFold(fields.Get("sign_type"), "MD5")) {
		return event, ErrInvalid
	}
	provided, err := hex.DecodeString(strings.TrimSpace(fields.Get("sign")))
	if err != nil || len(provided) != md5.Size {
		return event, errors.New("invalid epay signature")
	}
	expected, _ := hex.DecodeString(epaySignature(fields, g.MerchantKey))
	if !hmac.Equal(provided, expected) {
		return event, errors.New("invalid epay signature")
	}
	cents, err := providerAmountCents(fields.Get("money"))
	if err != nil {
		return event, err
	}
	return PaymentEvent{
		EventID: fields.Get("trade_no"), PaymentID: fields.Get("out_trade_no"),
		ProviderOrderID: fields.Get("out_trade_no"), ProviderRef: fields.Get("trade_no"),
		AmountCents: cents, Currency: "CNY", Status: "paid",
	}, nil
}
