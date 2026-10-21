package epay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newQueryConfig(t *testing.T, endpoint string) *Config {
	t.Helper()
	cfg := &Config{GatewayURL: endpoint, EpayVersion: VersionV1, MerchantID: "1001", MerchantKey: "sandbox-query-key"}
	cfg.Normalize()
	return cfg
}

func newQueryTLSServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	previous := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = previous; server.Close() })
	return server
}

func queryReply() map[string]interface{} {
	return map[string]interface{}{"code": 1, "status": 1, "pid": "1001", "trade_no": "PAID-ATTEMPT", "out_trade_no": "MERCHANT-ORDER", "money": "36.03"}
}

func TestQueryOrderSendsExactAttemptUsingHTTPSPostBody(t *testing.T) {
	server := newQueryTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api.php" || r.URL.RawQuery != "act=order" {
			t.Errorf("unexpected request method/path")
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.PostForm.Get("pid") != "1001" || r.PostForm.Get("key") != "sandbox-query-key" || r.PostForm.Get("trade_no") != "PAID-ATTEMPT" || r.PostForm.Get("out_trade_no") != "" {
			t.Error("query must select exact trade_no; credentials only in body")
		}
		_ = json.NewEncoder(w).Encode(queryReply())
	})
	result, err := QueryOrder(context.Background(), newQueryConfig(t, server.URL), "MERCHANT-ORDER", "PAID-ATTEMPT")
	if err != nil || result.Status != 1 || result.Money != "36.03" || result.Currency != "CNY" {
		t.Fatalf("valid query rejected: result=%+v err=%v", result, err)
	}
}

func TestQueryOrderRequiresEveryBindingAndStrictAmount(t *testing.T) {
	cases := []struct {
		name, key string
		value     interface{}
		remove    bool
	}{
		{"error code", "code", -1, false}, {"missing code", "code", nil, true},
		{"wrong merchant", "pid", "9999", false}, {"missing merchant", "pid", nil, true},
		{"wrong trade", "trade_no", "NEWER-ATTEMPT", false}, {"missing trade", "trade_no", nil, true},
		{"wrong order", "out_trade_no", "OTHER-ORDER", false}, {"missing order", "out_trade_no", nil, true},
		{"missing status", "status", nil, true}, {"boolean status", "status", true, false},
		{"missing amount", "money", nil, true}, {"zero amount", "money", "0", false},
		{"negative amount", "money", "-1", false}, {"bad amount", "money", "garbage", false},
		{"fractional cent", "money", "36.031", false}, {"exponent", "money", "1e2147483647", false},
		{"long amount", "money", strings.Repeat("1", 33), false},
		{"wrong currency", "currency", "USD", false}, {"empty currency", "currency", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reply := queryReply()
			if tc.remove {
				delete(reply, tc.key)
			} else {
				reply[tc.key] = tc.value
			}
			server := newQueryTLSServer(t, func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(reply) })
			if _, err := QueryOrder(context.Background(), newQueryConfig(t, server.URL), "MERCHANT-ORDER", "PAID-ATTEMPT"); !errors.Is(err, ErrResponseInvalid) {
				t.Fatalf("invalid query accepted: %v", err)
			}
		})
	}
}

func TestQueryOrderConfiguredCurrencyAndPendingStatus(t *testing.T) {
	for _, status := range []interface{}{0, "0", 1, "1"} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			reply := queryReply()
			reply["status"] = status
			server := newQueryTLSServer(t, func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(reply) })
			cfg := newQueryConfig(t, server.URL)
			cfg.TargetCurrency = "USD"
			result, err := QueryOrder(context.Background(), cfg, "MERCHANT-ORDER", "PAID-ATTEMPT")
			if err != nil || result.Currency != "USD" {
				t.Fatalf("configured denomination rejected: %v", err)
			}
		})
	}
}

func TestQueryOrderSelectsPaidAttemptWhenMerchantOrderHasNewerAttempt(t *testing.T) {
	for _, newerStatus := range []int{0, 1} {
		t.Run(fmt.Sprint(newerStatus), func(t *testing.T) {
			calls := 0
			server := newQueryTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				_ = r.ParseForm()
				reply := queryReply()
				if r.PostForm.Get("trade_no") != "PAID-ATTEMPT" {
					reply["trade_no"] = "NEWER-ATTEMPT"
					reply["status"] = newerStatus
				}
				_ = json.NewEncoder(w).Encode(reply)
			})
			result, err := QueryOrder(context.Background(), newQueryConfig(t, server.URL), "MERCHANT-ORDER", "PAID-ATTEMPT")
			if err != nil || result.TradeNo != "PAID-ATTEMPT" || calls != 1 {
				t.Fatalf("wrong attempt selected: %v", err)
			}
		})
	}
}

func TestQueryOrderRejectsRedirectAndOversizedResponse(t *testing.T) {
	targetCalls := 0
	target := newQueryTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		targetCalls++
		_ = json.NewEncoder(w).Encode(queryReply())
	})
	redirect := newQueryTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	})
	if _, err := QueryOrder(context.Background(), newQueryConfig(t, redirect.URL), "MERCHANT-ORDER", "PAID-ATTEMPT"); !errors.Is(err, ErrRequestFailed) {
		t.Fatalf("redirect accepted: %v", err)
	}
	if targetCalls != 0 {
		t.Fatal("redirect received merchant credentials")
	}
	body, _ := json.Marshal(queryReply())
	for _, extra := range []int{0, 1} {
		t.Run(fmt.Sprint(extra), func(t *testing.T) {
			reply := append(append([]byte{}, body...), []byte(strings.Repeat(" ", (64<<10)+extra-len(body)))...)
			server := newQueryTLSServer(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(reply) })
			_, err := QueryOrder(context.Background(), newQueryConfig(t, server.URL), "MERCHANT-ORDER", "PAID-ATTEMPT")
			if extra == 0 && err != nil {
				t.Fatalf("64KiB response rejected: %v", err)
			}
			if extra == 1 && !errors.Is(err, ErrResponseInvalid) {
				t.Fatalf("oversized response accepted: %v", err)
			}
		})
	}
}

func TestQueryOrderRejectsBadJSONHTTPAndConfiguration(t *testing.T) {
	for _, body := range []string{`not-json`, `{"code":1,"code":-1}`, `null`, `{"code":1} trailing`} {
		t.Run(body, func(t *testing.T) {
			server := newQueryTLSServer(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
			if _, err := QueryOrder(context.Background(), newQueryConfig(t, server.URL), "MERCHANT-ORDER", "PAID-ATTEMPT"); !errors.Is(err, ErrResponseInvalid) {
				t.Fatalf("bad reply accepted: %v", err)
			}
		})
	}
	for _, status := range []int{401, 403, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := newQueryTLSServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) })
			if _, err := QueryOrder(context.Background(), newQueryConfig(t, server.URL), "MERCHANT-ORDER", "PAID-ATTEMPT"); !errors.Is(err, ErrRequestFailed) {
				t.Fatalf("HTTP error accepted: %v", err)
			}
		})
	}
	for _, endpoint := range []string{"http://127.0.0.1:1", "https://user:pass@gateway.invalid", "https://gateway.invalid/?key=secret", "https://gateway.invalid/#fragment"} {
		if _, err := QueryOrder(context.Background(), newQueryConfig(t, endpoint), "MERCHANT-ORDER", "PAID-ATTEMPT"); !errors.Is(err, ErrConfigInvalid) {
			t.Fatalf("bad config accepted: %v", err)
		}
	}
	cfg := newQueryConfig(t, "https://gateway.invalid")
	cfg.EpayVersion = VersionV2
	if _, err := QueryOrder(context.Background(), cfg, "MERCHANT-ORDER", "PAID-ATTEMPT"); !errors.Is(err, ErrQueryUnsupported) {
		t.Fatalf("v2 must remain unsupported: %v", err)
	}
	cfg.EpayVersion = VersionV1
	if _, err := QueryOrder(context.Background(), cfg, "", "PAID-ATTEMPT"); !errors.Is(err, ErrConfigInvalid) {
		t.Fatal("trade ref cannot replace missing order no")
	}
	if _, err := QueryOrder(context.Background(), cfg, "MERCHANT-ORDER", ""); !errors.Is(err, ErrConfigInvalid) {
		t.Fatal("merchant order cannot replace missing trade ref")
	}
}

func TestQueryOrderHonorsCancellation(t *testing.T) {
	server := newQueryTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(queryReply())
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := QueryOrder(ctx, newQueryConfig(t, server.URL), "MERCHANT-ORDER", "PAID-ATTEMPT"); !errors.Is(err, ErrRequestFailed) {
		t.Fatalf("cancelled query accepted: %v", err)
	}
}
