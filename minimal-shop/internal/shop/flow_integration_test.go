package shop

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestPurchaseFlow requires a disposable PostgreSQL database named shop_test.
// It creates and removes only a randomly named schema inside that database.
func TestPurchaseFlow(t *testing.T) {
	dsn := os.Getenv("SHOP_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set SHOP_TEST_DATABASE_URL to an isolated shop_test database")
	}
	parsed, err := url.Parse(dsn)
	if err != nil || strings.TrimPrefix(parsed.Path, "/") != "shop_test" {
		t.Fatal("SHOP_TEST_DATABASE_URL must name the disposable shop_test database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	maintenance, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer maintenance.Close(context.Background())
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	schema := "shoptest_" + hex.EncodeToString(random[:])
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := maintenance.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		if _, err := maintenance.Exec(cleanup, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Errorf("test schema cleanup failed: %v", err)
		}
	}()
	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	poolCfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	cipher, err := NewCardCipher([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	mock := MockGateway{AppURL: "http://localhost:8080", Key: []byte("abcdef0123456789abcdef0123456789")}
	app := &App{db: pool, cipher: cipher, gateways: map[string]Gateway{"mock": mock}}
	if err := app.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	admin, err := app.CreateUser(ctx, "admin@example.test", "admin-password-12345", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := app.Login(ctx, admin.Email, "admin-password-12345", ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("admin logged in without TOTP: %v", err)
	}
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte("12345678901234567890"))
	if err := app.EnrollAdminTOTP(ctx, admin.ID, secret); err != nil {
		t.Fatal(err)
	}
	code := totpCode([]byte("12345678901234567890"), time.Now().Unix()/30)
	adminToken, _, err := app.Login(ctx, admin.Email, "admin-password-12345", code)
	if err != nil {
		t.Fatalf("admin TOTP login failed: %v", err)
	}
	if _, err := app.SessionUser(ctx, adminToken); err != nil {
		t.Fatalf("fresh admin session denied: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sessions SET last_seen_at=now()-interval '31 minutes' WHERE user_id=$1`, admin.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.SessionUser(ctx, adminToken); !errors.Is(err, ErrForbidden) {
		t.Fatalf("idle admin session accepted: %v", err)
	}
	if _, _, err := app.Login(ctx, admin.Email, "admin-password-12345", code); !errors.Is(err, ErrForbidden) {
		t.Fatalf("replayed admin TOTP accepted: %v", err)
	}
	buyer, err := app.CreateUser(ctx, "buyer@example.test", "buyer-password-12345", "customer")
	if err != nil {
		t.Fatal(err)
	}
	other, err := app.CreateUser(ctx, "other@example.test", "other-password-12345", "customer")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.CreateProduct(ctx, buyer.ID, "forged admin", "", 100); !errors.Is(err, ErrForbidden) {
		t.Fatalf("customer created product through service: %v", err)
	}
	if err := app.CreateProduct(ctx, admin.ID, "测试商品", "仅供丢弃的假卡密", 1501); err != nil {
		t.Fatal(err)
	}
	products, err := app.ListProducts(ctx, true)
	if err != nil || len(products) != 1 {
		t.Fatalf("product creation failed: %v", err)
	}
	productID := products[0].ID
	inserted, err := app.ImportCards(ctx, admin.ID, productID, []string{"FAKE-CARD-A", "FAKE-CARD-B", "FAKE-CARD-C", "FAKE-CARD-A"})
	if err != nil || inserted != 3 {
		t.Fatalf("card import failed: inserted=%d err=%v", inserted, err)
	}
	if err := app.SetProductActive(ctx, admin.ID, productID, true); err != nil {
		t.Fatal(err)
	}
	orderID, err := app.CreateOrder(ctx, buyer.ID, productID)
	if err != nil {
		t.Fatal(err)
	}
	before, err := app.GetOrder(ctx, buyer, orderID)
	if err != nil || before.Card != "" || before.Status != "pending" {
		t.Fatalf("card exposed before payment: %+v %v", before, err)
	}
	if _, err := app.GetOrder(ctx, other, orderID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another user read order: %v", err)
	}
	checkout, err := app.CreatePayment(ctx, buyer.ID, orderID, "mock")
	if err != nil {
		t.Fatal(err)
	}
	verified := signedMockEvent(t, mock, PaymentEvent{EventID: "event-1", PaymentID: checkout.PaymentID,
		ProviderOrderID: checkout.PaymentID, ProviderRef: checkout.PaymentID, AmountCents: 1501, Currency: "CNY", Status: "paid"})
	if result, err := app.applyVerifiedPayment(ctx, "mock", verified); err != nil || result != "delivered" {
		t.Fatalf("full payment not delivered: %q %v", result, err)
	}
	if result, err := app.applyVerifiedPayment(ctx, "mock", verified); err != nil || result != "duplicate" {
		t.Fatalf("replayed callback was not idempotent: %q %v", result, err)
	}
	after, err := app.GetOrder(ctx, buyer, orderID)
	if err != nil || after.Status != "delivered" || after.Card != "FAKE-CARD-A" {
		t.Fatalf("card delivery failed: %+v %v", after, err)
	}
	var assignments int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM cards WHERE assigned_order_id=$1`, orderID).Scan(&assignments); err != nil || assignments != 1 {
		t.Fatalf("duplicate card assignment: %d %v", assignments, err)
	}

	underpaidOrder, err := app.CreateOrder(ctx, buyer.ID, productID)
	if err != nil {
		t.Fatal(err)
	}
	underpaidCheckout, err := app.CreatePayment(ctx, buyer.ID, underpaidOrder, "mock")
	if err != nil {
		t.Fatal(err)
	}
	underpaid := signedMockEvent(t, mock, PaymentEvent{EventID: "event-underpaid", PaymentID: underpaidCheckout.PaymentID,
		ProviderOrderID: underpaidCheckout.PaymentID, ProviderRef: underpaidCheckout.PaymentID, AmountCents: 1, Currency: "CNY", Status: "paid"})
	if result, err := app.applyVerifiedPayment(ctx, "mock", underpaid); err != nil || result != "review" {
		t.Fatalf("underpayment was accepted: %q %v", result, err)
	}
	underpaidState, err := app.GetOrder(ctx, buyer, underpaidOrder)
	if err != nil || underpaidState.Status != "pending" || underpaidState.Card != "" {
		t.Fatalf("underpaid order disclosed card: %+v %v", underpaidState, err)
	}
	exceptions, err := app.ListPaymentExceptions(ctx)
	if err != nil || len(exceptions) != 1 || exceptions[0].OrderID != underpaidOrder {
		t.Fatalf("underpayment missing from admin review: %+v %v", exceptions, err)
	}

	expiringOrder, err := app.CreateOrder(ctx, buyer.ID, productID)
	if err != nil {
		t.Fatal(err)
	}
	lateCheckout, err := app.CreatePayment(ctx, buyer.ID, expiringOrder, "mock")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE orders SET expires_at=now()-interval '1 minute' WHERE id=$1`, expiringOrder); err != nil {
		t.Fatal(err)
	}
	count, err := app.ExpireOrders(ctx)
	if err != nil || count != 1 {
		t.Fatalf("expiration failed: %d %v", count, err)
	}
	late := signedMockEvent(t, mock, PaymentEvent{EventID: "event-late", PaymentID: lateCheckout.PaymentID,
		ProviderOrderID: lateCheckout.PaymentID, ProviderRef: lateCheckout.PaymentID, AmountCents: 1501, Currency: "CNY", Status: "paid"})
	if result, err := app.applyVerifiedPayment(ctx, "mock", late); err != nil || result != "review" {
		t.Fatalf("late callback fulfilled expired order: %q %v", result, err)
	}
	lateState, err := app.GetOrder(ctx, buyer, expiringOrder)
	if err != nil || lateState.Card != "" || lateState.Status != "expired" {
		t.Fatalf("expired order exposed card: %+v %v", lateState, err)
	}
	var handler http.Handler
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	mock.AppURL = server.URL
	app.gateways["mock"] = mock
	handler = NewWeb(app, Config{AppURL: server.URL, Environment: "development", PaymentMode: "mock"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	post := func(path string, values url.Values) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, server.URL+path, strings.NewReader(values.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", server.URL)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	registered := post("/register", url.Values{"email": {"browser@example.test"}, "password": {"browser-password-12345"}})
	registered.Body.Close()
	if registered.StatusCode != http.StatusSeeOther {
		t.Fatalf("registration failed: %d", registered.StatusCode)
	}
	ordered := post("/orders", url.Values{"product_id": {fmt.Sprint(productID)}})
	ordered.Body.Close()
	if ordered.StatusCode != http.StatusSeeOther {
		t.Fatalf("browser order failed: %d", ordered.StatusCode)
	}
	orderPath := ordered.Header.Get("Location")
	beforePage, err := client.Get(server.URL + orderPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeBody, _ := io.ReadAll(beforePage.Body)
	beforePage.Body.Close()
	if beforePage.StatusCode != http.StatusOK || strings.Contains(string(beforeBody), "FAKE-CARD-C") {
		t.Fatal("browser saw card before payment")
	}
	checkoutResponse := post(orderPath+"/pay", url.Values{"provider": {"mock"}})
	checkoutResponse.Body.Close()
	if checkoutResponse.StatusCode != http.StatusSeeOther {
		t.Fatalf("browser checkout failed: %d", checkoutResponse.StatusCode)
	}
	checkoutURL, err := url.Parse(checkoutResponse.Header.Get("Location"))
	if err != nil || !strings.HasPrefix(checkoutURL.Path, "/mock/pay/") {
		t.Fatal("mock payment URL missing")
	}
	paid := post(checkoutURL.Path, url.Values{})
	paid.Body.Close()
	if paid.StatusCode != http.StatusSeeOther {
		t.Fatalf("browser payment failed: %d", paid.StatusCode)
	}
	afterPage, err := client.Get(server.URL + orderPath)
	if err != nil {
		t.Fatal(err)
	}
	afterBody, _ := io.ReadAll(afterPage.Body)
	afterPage.Body.Close()
	if afterPage.StatusCode != http.StatusOK || !strings.Contains(string(afterBody), "FAKE-CARD-C") {
		t.Fatal("browser did not receive the paid card")
	}
	forbiddenAdmin := post("/admin/products", url.Values{"title": {"unauthorized"}, "price": {"1.00"}})
	forbiddenAdmin.Body.Close()
	if forbiddenAdmin.StatusCode != http.StatusForbidden {
		t.Fatalf("customer modified admin catalog: %d", forbiddenAdmin.StatusCode)
	}
	stolenPage, err := client.Get(server.URL + fmt.Sprintf("/orders/%d", orderID))
	if err != nil {
		t.Fatal(err)
	}
	stolenPage.Body.Close()
	if stolenPage.StatusCode != http.StatusNotFound {
		t.Fatalf("another customer's order was visible: %d", stolenPage.StatusCode)
	}
	foreignRequest, _ := http.NewRequest(http.MethodPost, server.URL+"/orders", strings.NewReader(url.Values{"product_id": {fmt.Sprint(productID)}}.Encode()))
	foreignRequest.Header.Set("Origin", "https://attacker.example.test")
	foreignRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	foreignResponse, err := client.Do(foreignRequest)
	if err != nil {
		t.Fatal(err)
	}
	foreignResponse.Body.Close()
	if foreignResponse.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin order request accepted: %d", foreignResponse.StatusCode)
	}
	createStockProduct := func(title, card string) int64 {
		t.Helper()
		if err := app.CreateProduct(ctx, admin.ID, title, "test only", 100); err != nil {
			t.Fatal(err)
		}
		listed, err := app.ListProducts(ctx, true)
		if err != nil || len(listed) == 0 {
			t.Fatal(err)
		}
		id := listed[0].ID
		if count, err := app.ImportCards(ctx, admin.ID, id, []string{card}); err != nil || count != 1 {
			t.Fatalf("test card import: %d %v", count, err)
		}
		if err := app.SetProductActive(ctx, admin.ID, id, true); err != nil {
			t.Fatal(err)
		}
		return id
	}
	xss := `<script>alert("xss")</script>`
	xssProduct := createStockProduct(xss, xss)
	homePage, err := client.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	homeBody, _ := io.ReadAll(homePage.Body)
	homePage.Body.Close()
	if strings.Contains(string(homeBody), xss) || !strings.Contains(string(homeBody), "&lt;script&gt;") {
		t.Fatal("product title was not HTML-escaped")
	}
	xssOrder, err := app.CreateOrder(ctx, buyer.ID, xssProduct)
	if err != nil {
		t.Fatal(err)
	}
	xssCheckout, err := app.CreatePayment(ctx, buyer.ID, xssOrder, "mock")
	if err != nil {
		t.Fatal(err)
	}
	xssEvent := PaymentEvent{EventID: "event-http-xss", PaymentID: xssCheckout.PaymentID, ProviderOrderID: xssCheckout.PaymentID,
		ProviderRef: xssCheckout.PaymentID, AmountCents: 100, Currency: "CNY", Status: "paid"}
	callbackBody, _ := json.Marshal(xssEvent)
	unsignedResponse, err := client.Post(server.URL+"/webhooks/mock", "application/json", strings.NewReader(string(callbackBody)))
	if err != nil {
		t.Fatal(err)
	}
	unsignedResponse.Body.Close()
	if unsignedResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unsigned callback accepted: %d", unsignedResponse.StatusCode)
	}
	xssBefore, err := app.GetOrder(ctx, buyer, xssOrder)
	if err != nil || xssBefore.Card != "" {
		t.Fatal("unsigned callback released card")
	}
	signedRequest, _ := http.NewRequest(http.MethodPost, server.URL+"/webhooks/mock", strings.NewReader(string(callbackBody)))
	signedRequest.Header.Set("X-Shop-Signature", mock.Sign(callbackBody))
	signedResponse, err := client.Do(signedRequest)
	if err != nil {
		t.Fatal(err)
	}
	signedResponse.Body.Close()
	if signedResponse.StatusCode != http.StatusOK {
		t.Fatalf("valid callback failed: %d", signedResponse.StatusCode)
	}
	xssPage, err := app.GetOrder(ctx, buyer, xssOrder)
	if err != nil || xssPage.Card != xss {
		t.Fatalf("valid callback did not deliver: %v", err)
	}
	stockProduct := createStockProduct("single-card", "ONE-FAKE-CARD")
	var purchaseErr [2]error
	var purchaseID [2]int64
	var group sync.WaitGroup
	for i, userID := range []int64{buyer.ID, other.ID} {
		group.Add(1)
		go func(index int, buyerID int64) {
			defer group.Done()
			purchaseID[index], purchaseErr[index] = app.CreateOrder(ctx, buyerID, stockProduct)
		}(i, userID)
	}
	group.Wait()
	successes, outOfStock := 0, 0
	for i := range purchaseErr {
		if purchaseErr[i] == nil && purchaseID[i] > 0 {
			successes++
		} else if errors.Is(purchaseErr[i], ErrOutOfStock) {
			outOfStock++
		} else {
			t.Fatalf("unexpected concurrent order result: id=%d err=%v", purchaseID[i], purchaseErr[i])
		}
	}
	if successes != 1 || outOfStock != 1 {
		t.Fatalf("single card oversold: successes=%d out-of-stock=%d", successes, outOfStock)
	}
	switchProduct := createStockProduct("switch-channel", "SWITCH-FAKE-CARD")
	switchOrder, err := app.CreateOrder(ctx, buyer.ID, switchProduct)
	if err != nil {
		t.Fatal(err)
	}
	firstPayment, err := app.CreatePayment(ctx, buyer.ID, switchOrder, "mock")
	if err != nil {
		t.Fatal(err)
	}
	app.gateways["mock2"] = testRedirectGateway{MockGateway: mock}
	secondPayment, err := app.CreatePayment(ctx, buyer.ID, switchOrder, "mock2")
	if err != nil {
		t.Fatal(err)
	}
	oldEvent := signedMockEvent(t, mock, PaymentEvent{EventID: "event-old-channel", PaymentID: firstPayment.PaymentID,
		ProviderOrderID: firstPayment.PaymentID, ProviderRef: firstPayment.PaymentID, AmountCents: 100, Currency: "CNY", Status: "paid"})
	if result, err := app.applyVerifiedPayment(ctx, "mock", oldEvent); err != nil || result != "review" {
		t.Fatalf("old payment delivered after switch: %s %v", result, err)
	}
	switchBefore, err := app.GetOrder(ctx, buyer, switchOrder)
	if err != nil || switchBefore.Card != "" {
		t.Fatal("old payment callback exposed card")
	}
	newEvent := signedMockEvent(t, mock, PaymentEvent{EventID: "event-new-channel", PaymentID: secondPayment.PaymentID,
		ProviderOrderID: secondPayment.PaymentID, ProviderRef: secondPayment.PaymentID, AmountCents: 100, Currency: "CNY", Status: "paid"})
	if result, err := app.applyVerifiedPayment(ctx, "mock2", newEvent); err != nil || result != "delivered" {
		t.Fatalf("new payment did not deliver: %s %v", result, err)
	}
	bepProduct := createStockProduct("bep-callback", "BEP-FAKE-CARD")
	bepOrder, err := app.CreateOrder(ctx, buyer.ID, bepProduct)
	if err != nil {
		t.Fatal(err)
	}
	app.gateways["bepusdt"] = testRedirectGateway{MockGateway: mock}
	bepCheckout, err := app.CreatePayment(ctx, buyer.ID, bepOrder, "bepusdt")
	if err != nil {
		t.Fatal(err)
	}
	bepEvent := PaymentEvent{EventID: "event-bep-ack", PaymentID: bepCheckout.PaymentID, ProviderOrderID: bepCheckout.PaymentID,
		ProviderRef: bepCheckout.PaymentID, AmountCents: 100, Currency: "CNY", Status: "paid"}
	bepBody, _ := json.Marshal(bepEvent)
	bepRequest, _ := http.NewRequest(http.MethodPost, server.URL+"/webhooks/bepusdt", strings.NewReader(string(bepBody)))
	bepRequest.Header.Set("X-Shop-Signature", mock.Sign(bepBody))
	bepResponse, err := client.Do(bepRequest)
	if err != nil {
		t.Fatal(err)
	}
	bepAck, _ := io.ReadAll(bepResponse.Body)
	bepResponse.Body.Close()
	if bepResponse.StatusCode != http.StatusOK || string(bepAck) != "success" {
		t.Fatalf("BEPUSDT success was not acknowledged: code=%d body=%q", bepResponse.StatusCode, bepAck)
	}
	limitProduct := createStockProduct("pending-limit", "PENDING-FAKE-1")
	if inserted, err := app.ImportCards(ctx, admin.ID, limitProduct, []string{"PENDING-FAKE-2", "PENDING-FAKE-3", "PENDING-FAKE-4"}); err != nil || inserted != 3 {
		t.Fatalf("pending limit setup failed: %d %v", inserted, err)
	}
	limitedUser, err := app.CreateUser(ctx, "pending-limit@example.test", "test-password-12345", "customer")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := app.CreateOrder(ctx, limitedUser.ID, limitProduct); err != nil {
			t.Fatalf("pending order %d failed: %v", i+1, err)
		}
	}
	if _, err := app.CreateOrder(ctx, limitedUser.ID, limitProduct); !errors.Is(err, ErrOrderLimit) {
		t.Fatalf("unpaid inventory was not limited: %v", err)
	}
}

func signedMockEvent(t *testing.T, mock MockGateway, event PaymentEvent) PaymentEvent {
	t.Helper()
	body, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := mock.Verify(context.Background(), http.Header{"X-Shop-Signature": {mock.Sign(body)}}, body)
	if err != nil {
		t.Fatal(err)
	}
	return verified
}

type testRedirectGateway struct{ MockGateway }

func (g testRedirectGateway) Start(_ context.Context, p PaymentStart) (PaymentStartResult, error) {
	return PaymentStartResult{URL: "https://pay.example.test/checkout/" + p.PaymentID, ProviderRef: p.PaymentID}, nil
}
