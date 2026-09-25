package shop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Web struct {
	app      *App
	cfg      Config
	logger   *slog.Logger
	template *template.Template
	limiter  *attemptLimiter
}

type pageData struct {
	View       string
	Title      string
	User       *User
	Products   []Product
	Orders     []Order
	Exceptions []PaymentException
	Order      *Order
	Providers  []string
	Message    string
	PaymentID  string
}

func NewWeb(app *App, cfg Config, logger *slog.Logger) http.Handler {
	w := &Web{app: app, cfg: cfg, logger: logger, template: pageTemplate(), limiter: newAttemptLimiter()}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", w.health)
	mux.HandleFunc("GET /style.css", w.style)
	mux.HandleFunc("GET /", w.home)
	mux.HandleFunc("GET /register", w.registerPage)
	mux.HandleFunc("POST /register", w.register)
	mux.HandleFunc("GET /login", w.loginPage)
	mux.HandleFunc("POST /login", w.login)
	mux.HandleFunc("POST /logout", w.logout)
	mux.HandleFunc("POST /orders", w.createOrder)
	mux.HandleFunc("GET /orders", w.orders)
	mux.HandleFunc("GET /orders/{id}", w.order)
	mux.HandleFunc("POST /orders/{id}/pay", w.pay)
	mux.HandleFunc("GET /mock/pay/{id}", w.mockPayPage)
	mux.HandleFunc("POST /mock/pay/{id}", w.mockPay)
	mux.HandleFunc("POST /webhooks/mock", w.mockWebhook)
	mux.HandleFunc("GET /webhooks/epay", w.epayWebhook)
	mux.HandleFunc("POST /webhooks/epay", w.epayWebhook)
	mux.HandleFunc("POST /webhooks/bepusdt", w.bepWebhook)
	mux.HandleFunc("GET /admin", w.admin)
	mux.HandleFunc("POST /admin/products", w.adminCreateProduct)
	mux.HandleFunc("POST /admin/products/{id}/active", w.adminSetActive)
	mux.HandleFunc("POST /admin/products/{id}/cards", w.adminImportCards)
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("Referrer-Policy", "no-referrer")
		writer.Header().Set("X-Frame-Options", "DENY")
		writer.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		writer.Header().Set("Cache-Control", "no-store")
		if cfg.Environment == "production" {
			writer.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		if request.Method == http.MethodPost && !strings.HasPrefix(request.URL.Path, "/webhooks/") && !w.sameOrigin(request) {
			http.Error(writer, "forbidden", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(writer, request)
	})
}

func (w *Web) sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	a, errA := url.Parse(origin)
	b, errB := url.Parse(w.cfg.AppURL)
	return errA == nil && errB == nil && strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host) && a.User == nil && a.RawQuery == "" && a.Fragment == "" && a.Path == ""
}

func (w *Web) current(r *http.Request) *User {
	cookie, err := r.Cookie(w.sessionCookieName())
	if err != nil {
		return nil
	}
	user, err := w.app.SessionUser(r.Context(), cookie.Value)
	if err != nil {
		return nil
	}
	return &user
}

func (w *Web) sessionCookieName() string {
	if w.cfg.Environment == "production" {
		return "__Host-shop_session"
	}
	return "shop_session"
}

func (w *Web) secureCookies() bool {
	parsed, err := url.Parse(w.cfg.AppURL)
	return err == nil && strings.EqualFold(parsed.Scheme, "https")
}

func (w *Web) requireUser(response http.ResponseWriter, request *http.Request) *User {
	user := w.current(request)
	if user == nil {
		http.Redirect(response, request, "/login", http.StatusSeeOther)
		return nil
	}
	return user
}

func (w *Web) requireAdmin(response http.ResponseWriter, request *http.Request) *User {
	user := w.requireUser(response, request)
	if user == nil {
		return nil
	}
	if user.Role != "admin" {
		http.Error(response, "forbidden", http.StatusForbidden)
		return nil
	}
	return user
}

func (w *Web) render(response http.ResponseWriter, data pageData, status int) {
	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(status)
	if err := w.template.ExecuteTemplate(response, "page", data); err != nil {
		w.logger.Error("template rendering failed", "error", err)
	}
}

func (w *Web) fail(response http.ResponseWriter, request *http.Request, err error) {
	status := http.StatusInternalServerError
	message := "服务暂时不可用"
	switch {
	case errors.Is(err, ErrInvalid):
		status, message = http.StatusBadRequest, "输入无效"
	case errors.Is(err, ErrNotFound):
		status, message = http.StatusNotFound, "未找到"
	case errors.Is(err, ErrForbidden):
		status, message = http.StatusForbidden, "无权访问"
	case errors.Is(err, ErrOutOfStock):
		status, message = http.StatusConflict, "库存不足"
	case errors.Is(err, ErrOrderLimit):
		status, message = http.StatusTooManyRequests, "未支付订单过多，请先完成或等待过期"
	case errors.Is(err, ErrOrderClosed):
		status, message = http.StatusConflict, "订单已关闭"
	case errors.Is(err, ErrPaymentProcessing):
		status, message = http.StatusConflict, "支付正在创建，请稍后刷新"
	case errors.Is(err, ErrPaymentReview):
		status, message = http.StatusConflict, "支付需要人工核对"
	default:
		w.logger.Error("request failed", "path", request.URL.Path, "error", err)
	}
	w.render(response, pageData{View: "message", Title: "提示", User: w.current(request), Message: message}, status)
}

func (w *Web) health(response http.ResponseWriter, request *http.Request) {
	ctx, cancel := context.WithTimeout(request.Context(), 2*time.Second)
	defer cancel()
	if err := w.app.db.Ping(ctx); err != nil {
		http.Error(response, "unhealthy", http.StatusServiceUnavailable)
		return
	}
	response.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(response, "ok\n")
}

func (w *Web) style(response http.ResponseWriter, _ *http.Request) {
	response.Header().Set("Content-Type", "text/css; charset=utf-8")
	response.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = io.WriteString(response, pageCSS)
}

func (w *Web) home(response http.ResponseWriter, request *http.Request) {
	products, err := w.app.ListProducts(request.Context(), false)
	if err != nil {
		w.fail(response, request, err)
		return
	}
	w.render(response, pageData{View: "home", Title: "商品", User: w.current(request), Products: products}, http.StatusOK)
}

func (w *Web) registerPage(response http.ResponseWriter, request *http.Request) {
	w.render(response, pageData{View: "register", Title: "注册", User: w.current(request)}, http.StatusOK)
}

func (w *Web) register(response http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(response, request.Body, 16<<10)
	if err := request.ParseForm(); err != nil {
		w.fail(response, request, ErrInvalid)
		return
	}
	email, password := request.PostFormValue("email"), request.PostFormValue("password")
	if !w.limiter.allow("register:"+clientIP(request, w.cfg.TrustedProxies), 30, time.Hour) {
		http.Error(response, "too many requests", http.StatusTooManyRequests)
		return
	}
	if _, err := w.app.CreateUser(request.Context(), email, password, "customer"); err != nil {
		w.fail(response, request, err)
		return
	}
	w.loginWithCredentials(response, request, email, password, "")
}

func (w *Web) loginPage(response http.ResponseWriter, request *http.Request) {
	w.render(response, pageData{View: "login", Title: "登录", User: w.current(request)}, http.StatusOK)
}

func (w *Web) login(response http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(response, request.Body, 16<<10)
	if err := request.ParseForm(); err != nil {
		w.fail(response, request, ErrInvalid)
		return
	}
	if !w.limiter.allow(accountLimitKey(request.PostFormValue("email")), 10, 15*time.Minute) ||
		!w.limiter.allow("login-ip:"+clientIP(request, w.cfg.TrustedProxies), 120, time.Hour) {
		http.Error(response, "too many requests", http.StatusTooManyRequests)
		return
	}
	w.loginWithCredentials(response, request, request.PostFormValue("email"), request.PostFormValue("password"), request.PostFormValue("otp"))
}

func (w *Web) loginWithCredentials(response http.ResponseWriter, request *http.Request, email, password, otp string) {
	token, user, err := w.app.Login(request.Context(), email, password, otp)
	if err != nil {
		w.render(response, pageData{View: "message", Title: "登录失败", Message: "账号或密码错误"}, http.StatusUnauthorized)
		return
	}
	validity := 7 * 24 * time.Hour
	if user.Role == "admin" {
		validity = 12 * time.Hour
	}
	http.SetCookie(response, &http.Cookie{
		Name: w.sessionCookieName(), Value: token, Path: "/", HttpOnly: true,
		Secure: w.secureCookies(), SameSite: http.SameSiteLaxMode,
		Expires: time.Now().Add(validity),
	})
	if user.Role == "admin" {
		http.Redirect(response, request, "/admin", http.StatusSeeOther)
	} else {
		http.Redirect(response, request, "/", http.StatusSeeOther)
	}
}

func (w *Web) logout(response http.ResponseWriter, request *http.Request) {
	if cookie, err := request.Cookie(w.sessionCookieName()); err == nil {
		w.app.Logout(request.Context(), cookie.Value)
	}
	http.SetCookie(response, &http.Cookie{Name: w.sessionCookieName(), Path: "/", MaxAge: -1, HttpOnly: true,
		Secure: w.secureCookies(), SameSite: http.SameSiteLaxMode})
	http.Redirect(response, request, "/", http.StatusSeeOther)
}

func (w *Web) createOrder(response http.ResponseWriter, request *http.Request) {
	user := w.requireUser(response, request)
	if user == nil {
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 16<<10)
	if err := request.ParseForm(); err != nil {
		w.fail(response, request, ErrInvalid)
		return
	}
	productID, err := strconv.ParseInt(request.PostFormValue("product_id"), 10, 64)
	if err != nil {
		w.fail(response, request, ErrInvalid)
		return
	}
	id, err := w.app.CreateOrder(request.Context(), user.ID, productID)
	if err != nil {
		w.fail(response, request, err)
		return
	}
	http.Redirect(response, request, fmt.Sprintf("/orders/%d", id), http.StatusSeeOther)
}

func (w *Web) orders(response http.ResponseWriter, request *http.Request) {
	user := w.requireUser(response, request)
	if user == nil {
		return
	}
	orders, err := w.app.ListOrders(request.Context(), *user)
	if err != nil {
		w.fail(response, request, err)
		return
	}
	w.render(response, pageData{View: "orders", Title: "我的订单", User: user, Orders: orders}, http.StatusOK)
}

func pathID(request *http.Request) (int64, error) {
	id, err := strconv.ParseInt(request.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, ErrInvalid
	}
	return id, nil
}

func (w *Web) order(response http.ResponseWriter, request *http.Request) {
	user := w.requireUser(response, request)
	if user == nil {
		return
	}
	id, err := pathID(request)
	if err != nil {
		w.fail(response, request, err)
		return
	}
	order, err := w.app.GetOrder(request.Context(), *user, id)
	if err != nil {
		w.fail(response, request, err)
		return
	}
	w.render(response, pageData{View: "order", Title: "订单详情", User: user, Order: &order, Providers: w.app.GatewayNames()}, http.StatusOK)
}

func (w *Web) pay(response http.ResponseWriter, request *http.Request) {
	user := w.requireUser(response, request)
	if user == nil {
		return
	}
	id, err := pathID(request)
	if err != nil {
		w.fail(response, request, err)
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 16<<10)
	if err := request.ParseForm(); err != nil {
		w.fail(response, request, ErrInvalid)
		return
	}
	checkout, err := w.app.CreatePayment(request.Context(), user.ID, id, request.PostFormValue("provider"))
	if err != nil {
		w.fail(response, request, err)
		return
	}
	http.Redirect(response, request, checkout.URL, http.StatusSeeOther)
}

func (w *Web) mockPayPage(response http.ResponseWriter, request *http.Request) {
	if w.cfg.PaymentMode != "mock" {
		http.NotFound(response, request)
		return
	}
	user := w.requireUser(response, request)
	if user == nil {
		return
	}
	order, err := w.app.GetMockPaymentOrder(request.Context(), *user, request.PathValue("id"))
	if err != nil {
		w.fail(response, request, err)
		return
	}
	w.render(response, pageData{View: "mock", Title: "模拟付款", User: user, Order: &order, PaymentID: request.PathValue("id")}, http.StatusOK)
}

func (w *Web) mockPay(response http.ResponseWriter, request *http.Request) {
	if w.cfg.PaymentMode != "mock" {
		http.NotFound(response, request)
		return
	}
	user := w.requireUser(response, request)
	if user == nil {
		return
	}
	paymentID := request.PathValue("id")
	order, err := w.app.GetMockPaymentOrder(request.Context(), *user, paymentID)
	if err != nil {
		w.fail(response, request, err)
		return
	}
	token, _, err := randomToken()
	if err != nil {
		w.fail(response, request, err)
		return
	}
	event := PaymentEvent{EventID: token, PaymentID: paymentID, ProviderOrderID: paymentID, ProviderRef: paymentID,
		AmountCents: order.AmountCents, Currency: order.Currency, Status: "paid"}
	body, _ := json.Marshal(event)
	gateway := w.app.gateways["mock"].(MockGateway)
	header := http.Header{"X-Shop-Signature": []string{gateway.Sign(body)}}
	verified, err := gateway.Verify(request.Context(), header, body)
	if err == nil {
		_, err = w.app.applyVerifiedPayment(request.Context(), "mock", verified)
	}
	if err != nil {
		w.fail(response, request, err)
		return
	}
	http.Redirect(response, request, fmt.Sprintf("/orders/%d", order.ID), http.StatusSeeOther)
}

func (w *Web) mockWebhook(response http.ResponseWriter, request *http.Request) {
	if w.cfg.PaymentMode != "mock" {
		http.NotFound(response, request)
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 64<<10)
	body, err := io.ReadAll(request.Body)
	if err != nil {
		http.Error(response, "invalid body", http.StatusBadRequest)
		return
	}
	event, err := w.app.VerifyWebhook(request.Context(), "mock", request.Header, body)
	if err != nil {
		http.Error(response, "invalid callback", http.StatusUnauthorized)
		return
	}
	disposition, err := w.app.applyVerifiedPayment(request.Context(), "mock", event)
	if err != nil {
		w.logger.Error("callback processing failed", "error", err, "payment_id", event.PaymentID)
		http.Error(response, "retry", http.StatusInternalServerError)
		return
	}
	w.logger.Info("callback processed", "payment_id", event.PaymentID, "disposition", disposition)
	response.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(response, "ok")
}

func (w *Web) epayWebhook(response http.ResponseWriter, request *http.Request) {
	w.providerWebhook(response, request, "epay")
}

func (w *Web) bepWebhook(response http.ResponseWriter, request *http.Request) {
	w.providerWebhook(response, request, "bepusdt")
}

func (w *Web) providerWebhook(response http.ResponseWriter, request *http.Request, provider string) {
	if _, ok := w.app.gateways[provider]; !ok {
		http.NotFound(response, request)
		return
	}
	var body []byte
	var err error
	if request.Method == http.MethodGet && provider == "epay" {
		body = []byte(request.URL.RawQuery)
	} else {
		request.Body = http.MaxBytesReader(response, request.Body, 64<<10)
		body, err = io.ReadAll(request.Body)
	}
	if err != nil || len(body) == 0 || len(body) > 64<<10 {
		response.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(response, "fail")
		return
	}
	event, err := w.app.VerifyWebhook(request.Context(), provider, request.Header, body)
	if err != nil {
		response.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(response, "fail")
		return
	}
	if provider == "bepusdt" && event.Status != "paid" {
		response.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(response, "success")
		return
	}
	disposition, err := w.app.applyVerifiedPayment(request.Context(), provider, event)
	if err != nil {
		w.logger.Error("provider callback processing failed", "provider", provider, "payment_id", event.PaymentID, "error", err)
		response.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(response, "fail")
		return
	}
	w.logger.Info("provider callback processed", "provider", provider, "payment_id", event.PaymentID, "disposition", disposition)
	response.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(response, "success")
}

func (w *Web) admin(response http.ResponseWriter, request *http.Request) {
	user := w.requireAdmin(response, request)
	if user == nil {
		return
	}
	products, err := w.app.ListProducts(request.Context(), true)
	if err != nil {
		w.fail(response, request, err)
		return
	}
	orders, err := w.app.ListOrders(request.Context(), *user)
	if err != nil {
		w.fail(response, request, err)
		return
	}
	exceptions, err := w.app.ListPaymentExceptions(request.Context())
	if err != nil {
		w.fail(response, request, err)
		return
	}
	w.render(response, pageData{View: "admin", Title: "管理后台", User: user, Products: products, Orders: orders, Exceptions: exceptions}, http.StatusOK)
}

func parseCents(raw string) (int64, error) {
	parts := strings.Split(strings.TrimSpace(raw), ".")
	if len(parts) < 1 || len(parts) > 2 || parts[0] == "" {
		return 0, ErrInvalid
	}
	whole, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || whole < 0 || whole > 100000000 {
		return 0, ErrInvalid
	}
	frac := int64(0)
	if len(parts) == 2 {
		if len(parts[1]) == 0 || len(parts[1]) > 2 {
			return 0, ErrInvalid
		}
		if len(parts[1]) == 1 {
			parts[1] += "0"
		}
		frac, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil || frac < 0 {
			return 0, ErrInvalid
		}
	}
	return whole*100 + frac, nil
}

func (w *Web) adminCreateProduct(response http.ResponseWriter, request *http.Request) {
	admin := w.requireAdmin(response, request)
	if admin == nil {
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 16<<10)
	if err := request.ParseForm(); err != nil {
		w.fail(response, request, ErrInvalid)
		return
	}
	price, err := parseCents(request.PostFormValue("price"))
	if err == nil {
		err = w.app.CreateProduct(request.Context(), admin.ID, request.PostFormValue("title"), request.PostFormValue("description"), price)
	}
	if err != nil {
		w.fail(response, request, err)
		return
	}
	http.Redirect(response, request, "/admin", http.StatusSeeOther)
}

func (w *Web) adminSetActive(response http.ResponseWriter, request *http.Request) {
	admin := w.requireAdmin(response, request)
	if admin == nil {
		return
	}
	id, err := pathID(request)
	if err != nil {
		w.fail(response, request, err)
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 16<<10)
	if err = request.ParseForm(); err != nil {
		w.fail(response, request, ErrInvalid)
		return
	}
	active := request.PostFormValue("active") == "true"
	if err = w.app.SetProductActive(request.Context(), admin.ID, id, active); err != nil {
		w.fail(response, request, err)
		return
	}
	http.Redirect(response, request, "/admin", http.StatusSeeOther)
}

func (w *Web) adminImportCards(response http.ResponseWriter, request *http.Request) {
	admin := w.requireAdmin(response, request)
	if admin == nil {
		return
	}
	id, err := pathID(request)
	if err != nil {
		w.fail(response, request, err)
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 5<<20)
	if err = request.ParseForm(); err != nil {
		w.fail(response, request, ErrInvalid)
		return
	}
	if _, err = w.app.ImportCards(request.Context(), admin.ID, id, strings.Split(request.PostFormValue("cards"), "\n")); err != nil {
		w.fail(response, request, err)
		return
	}
	http.Redirect(response, request, "/admin", http.StatusSeeOther)
}
