package shop

import "html/template"

func pageTemplate() *template.Template {
	return template.Must(template.New("page").Funcs(template.FuncMap{"money": Money}).Parse(pageHTML))
}

const pageHTML = `{{define "page"}}<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Title}} · Minimal Shop</title><link rel="stylesheet" href="/style.css"></head>
<body><header><nav><a class="brand" href="/">Minimal Shop</a><div class="nav-right">
{{if .User}}<span>{{.User.Email}}</span><a href="/orders">订单</a>{{if eq .User.Role "admin"}}<a href="/admin">管理</a>{{end}}
<form method="post" action="/logout"><button class="link" type="submit">退出</button></form>
{{else}}<a href="/login">登录</a><a href="/register">注册</a>{{end}}
</div></nav></header><main>
{{if eq .View "home"}}<h1>商品</h1><p class="muted">选购后将为你的订单预留卡密。支付确认前不会展示卡密。</p>
<div class="grid">{{range .Products}}<article class="card"><h2>{{.Title}}</h2><p>{{.Description}}</p>
<p class="price">{{money .PriceCents .Currency}}</p><p class="muted">可售库存：{{.Stock}}</p>
{{if gt .Stock 0}}<form method="post" action="/orders"><input type="hidden" name="product_id" value="{{.ID}}"><button type="submit">下单</button></form>{{else}}<button disabled>暂时售罄</button>{{end}}
</article>{{else}}<p>暂无上架商品。</p>{{end}}</div>
{{else if eq .View "register"}}<section class="panel narrow"><h1>注册</h1><form method="post" action="/register">
<label>邮箱<input name="email" type="email" autocomplete="email" required></label>
<label>密码（至少 12 位）<input name="password" type="password" minlength="12" autocomplete="new-password" required></label>
<button type="submit">创建账号</button></form></section>
{{else if eq .View "login"}}<section class="panel narrow"><h1>登录</h1><form method="post" action="/login">
<label>邮箱<input name="email" type="email" autocomplete="email" required></label>
<label>密码<input name="password" type="password" autocomplete="current-password" required></label>
<label>管理员动态验证码（普通用户留空）<input name="otp" inputmode="numeric" autocomplete="one-time-code" pattern="[0-9]{6}" maxlength="6"></label>
<button type="submit">登录</button></form></section>
{{else if eq .View "orders"}}<h1>订单</h1><div class="stack">{{range .Orders}}<article class="card row"><div><h2>#{{.ID}} · {{.Title}}</h2><p class="muted">{{money .AmountCents .Currency}} · {{.Status}}</p></div><a class="button secondary" href="/orders/{{.ID}}">查看</a></article>{{else}}<p>暂无订单。</p>{{end}}</div>
{{else if eq .View "order"}}<section class="panel"><h1>订单 #{{.Order.ID}}</h1><h2>{{.Order.Title}}</h2>
<p>应付：<strong>{{money .Order.AmountCents .Order.Currency}}</strong></p><p>状态：<strong>{{.Order.Status}}</strong></p>
{{if eq .Order.Status "pending"}}<p class="muted">请在 {{.Order.ExpiresAt.Format "2006-01-02 15:04:05"}} 前完成付款。</p>
{{if .Order.PayURL}}<p><a class="button" href="{{.Order.PayURL}}">继续付款</a></p>{{end}}
<div class="pay-options">{{range .Providers}}<form method="post" action="/orders/{{$.Order.ID}}/pay"><input type="hidden" name="provider" value="{{.}}"><button type="submit">使用 {{.}} 支付</button></form>{{end}}</div>
{{else if eq .Order.Status "delivered"}}<h2>卡密</h2><p class="muted">仅此订单的购买账号可查看，请妥善保管。</p><pre class="secret">{{.Order.Card}}</pre>
{{else if eq .Order.Status "expired"}}<p>订单已过期，预留库存已释放。</p>{{else}}<p>订单处理中，请稍后刷新。</p>{{end}}
</section>
{{else if eq .View "mock"}}<section class="panel narrow"><h1>模拟支付</h1><p>仅用于本地开发测试，不会发生真实扣款。</p>
<p>订单 #{{.Order.ID}} · {{money .Order.AmountCents .Order.Currency}}</p>
<form method="post" action="/mock/pay/{{.PaymentID}}"><button type="submit">模拟付款成功</button></form></section>
{{else if eq .View "admin"}}<h1>管理后台</h1><section class="panel"><h2>新增商品</h2>
<form method="post" action="/admin/products"><label>商品名称<input name="title" maxlength="160" required></label>
<label>说明<textarea name="description" rows="3"></textarea></label><label>价格（CNY）<input name="price" inputmode="decimal" placeholder="10.00" required></label>
<button type="submit">保存为未上架</button></form></section>
<h2>商品与库存</h2><div class="stack">{{range .Products}}<article class="card"><div class="row"><div><h3>#{{.ID}} · {{.Title}}</h3>
<p class="muted">{{money .PriceCents .Currency}} · 可售 {{.Stock}} · {{if .Active}}已上架{{else}}未上架{{end}}</p></div>
<form method="post" action="/admin/products/{{.ID}}/active"><input type="hidden" name="active" value="{{if .Active}}false{{else}}true{{end}}">
<button class="secondary" type="submit">{{if .Active}}下架{{else}}上架{{end}}</button></form></div>
<details><summary>导入卡密</summary><form method="post" action="/admin/products/{{.ID}}/cards"><label>每行一张卡密<textarea name="cards" rows="5" required></textarea></label>
<button type="submit">导入加密库存</button></form></details></article>{{else}}<p>尚无商品。</p>{{end}}</div>
<h2>最近订单</h2><div class="stack">{{range .Orders}}<article class="card row"><span>#{{.ID}} · {{.Title}} · {{.Status}}</span><a href="/orders/{{.ID}}">查看</a></article>{{else}}<p>暂无订单。</p>{{end}}</div>
<h2>支付异常，需人工核对或退款</h2><div class="stack">{{range .Exceptions}}<article class="card"><strong>订单 #{{.OrderID}} · {{.Provider}}</strong><p>{{.CustomerEmail}}</p><p>应付 {{money .ExpectedCents .Currency}}，回调金额 {{money .PaidCents .Currency}}</p><p class="muted">{{.Reason}}</p><a href="/orders/{{.OrderID}}">查看订单</a></article>{{else}}<p>暂无支付异常。</p>{{end}}</div>
{{else if eq .View "message"}}<section class="panel narrow"><h1>{{.Title}}</h1><p>{{.Message}}</p><a href="/">返回首页</a></section>{{end}}
</main><footer>Minimal Shop · 卡密仅在完成支付后交付</footer></body></html>{{end}}`

const pageCSS = `:root{font-family:system-ui,-apple-system,"Segoe UI",sans-serif;color:#17212f;background:#f6f8fb}*{box-sizing:border-box}body{margin:0;min-height:100vh}header{background:#fff;border-bottom:1px solid #e4e8ef}nav{max-width:1100px;margin:auto;padding:16px 20px;display:flex;align-items:center;justify-content:space-between;gap:20px}.brand{font-size:1.2rem;font-weight:800;color:#1e3a8a;text-decoration:none}.nav-right{display:flex;align-items:center;gap:16px;flex-wrap:wrap}.nav-right span{color:#64748b}a{color:#1d4ed8}main{max-width:1100px;margin:40px auto;padding:0 20px 80px}h1{font-size:1.8rem;margin:0 0 22px}h2{font-size:1.15rem}.muted{color:#64748b}.grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(260px,1fr));gap:18px}.stack{display:grid;gap:12px}.card,.panel{background:#fff;border:1px solid #e2e8f0;border-radius:14px;padding:22px;box-shadow:0 4px 18px #17212f08}.card h2,.card h3{margin-top:0}.price{font-size:1.25rem;font-weight:800}.narrow{max-width:480px;margin:auto}.row{display:flex;justify-content:space-between;align-items:center;gap:18px}.row>*{min-width:0}label{display:block;font-weight:600;margin:15px 0 6px}input,textarea{display:block;width:100%;font:inherit;margin-top:7px;padding:11px 12px;border:1px solid #cbd5e1;border-radius:8px;background:#fff}textarea{resize:vertical}button,.button{display:inline-block;font:inherit;font-weight:700;padding:10px 17px;background:#1d4ed8;border:1px solid #1d4ed8;border-radius:8px;color:#fff;text-decoration:none;cursor:pointer}button:hover,.button:hover{background:#1e40af}button:disabled{opacity:.5;cursor:not-allowed}.secondary{background:#fff;color:#1d4ed8}.secondary:hover{background:#eff6ff}.link{background:none;border:0;color:#1d4ed8;padding:0;font-weight:400}.link:hover{background:none;text-decoration:underline}form{margin:0}.panel form>button{margin-top:16px}.pay-options{display:flex;gap:12px;flex-wrap:wrap;margin-top:20px}.secret{white-space:pre-wrap;overflow-wrap:anywhere;background:#eff6ff;border:1px solid #bfdbfe;border-radius:8px;padding:18px;font-size:1rem}details{margin-top:16px}details form{margin-top:14px}footer{text-align:center;color:#94a3b8;padding:28px}@media(max-width:600px){main{margin-top:25px}.row{align-items:flex-start;flex-direction:column}.nav-right{gap:10px}.nav-right span{display:none}}`
