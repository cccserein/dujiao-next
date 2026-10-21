package contract

import (
	"context"
	"errors"
	"time"

	"github.com/dujiao-next/internal/shared/jsonmap"
	"github.com/dujiao-next/internal/shared/money"
)

// ErrGatewayCorroborationUnsupported 表示该渠道无法主动查询支付结果，
// 因此无法对"支付成功"回调做二次核实。
var ErrGatewayCorroborationUnsupported = errors.New("gateway corroboration unsupported")

// GatewayPayloadFiatCurrencySent 标记创建支付时实际提交给网关的法币币种。
// 回调兼容逻辑用它区分具备新版事实快照的支付与升级前创建的在途支付。
const GatewayPayloadFiatCurrencySent = "_dujiao_next_fiat_currency_sent"

// GatewayCreateInput 是统一支付网关创建输入。
type GatewayCreateInput struct {
	PaymentID      uint
	OrderID        uint
	OrderNo        string
	Subject        string
	Amount         money.Amount
	Currency       string
	NotifyURL      string
	ReturnURL      string
	ReturnURLQuery map[string]string
	ClientIP       string
	ChannelType    string
	Extra          jsonmap.JSON
}

// GatewayCreateResult 是统一支付网关创建结果。
type GatewayCreateResult struct {
	ProviderRef        string
	RedirectURL        string
	QRCodeURL          string
	Payload            jsonmap.JSON
	DisplayChannelType string
	AmountSent         string
	CurrencySent       string
}

// GatewayQueryResult 是主动查询网关支付状态的结果。
type GatewayQueryResult struct {
	ProviderRef string
	Status      string
	Amount      money.Amount
	Currency    string
	PaidAt      *time.Time
	Payload     jsonmap.JSON
}

// GatewayCallbackResult 是同步回调或异步 Webhook 的标准结果。
type GatewayCallbackResult struct {
	OrderNo     string
	ProviderRef string
	Status      string
	Amount      money.Amount
	Currency    string
	PaidAt      *time.Time
	Payload     jsonmap.JSON
}

// GatewayProvider 是所有支付网关适配器必须实现的最小能力。
type GatewayProvider interface {
	Type() string
	ValidateConfig(cfg jsonmap.JSON, channelType string) error
	CreatePayment(ctx context.Context, cfg jsonmap.JSON, input GatewayCreateInput) (*GatewayCreateResult, error)
}

type GatewayCapturer interface {
	GatewayProvider
	QueryPayment(ctx context.Context, cfg jsonmap.JSON, providerRef string) (*GatewayQueryResult, error)
}

type GatewayWebhooker interface {
	GatewayProvider
	ParseWebhook(ctx context.Context, cfg jsonmap.JSON, headers map[string]string, body []byte, now time.Time) (*GatewayCallbackResult, error)
}

type GatewayCallbackVerifier interface {
	GatewayProvider
	VerifyCallback(cfg jsonmap.JSON, form map[string][]string, body []byte) (*GatewayCallbackResult, error)
}

// CorroborateInput 是回调复核输入。
//
// OrderNo 是创建支付时提交给网关的商户单号，ProviderRef 是网关流水号
// （部分渠道在同步跳转模式下要到回调才拿得到）；两种单号不能互相回退。
// 适配器必须确认查询成功且返回的商户、两种单号均与本地输入一致。Amount/Currency 是本地
// 记账的金额与币种，用于校验收到的钱和应收的钱是否一致。
type CorroborateInput struct {
	OrderNo     string
	ProviderRef string
	Amount      money.Amount
	Currency    string
}

// CallbackCorroborator 是可选能力：服务层在把回调当作"已支付"落库、发货之前，
// 主动向网关复核这笔支付是否真的已经收款。
//
// 签名只能证明回调内容出自持有商户密钥的一方，不能证明资金到账：
// 商户密钥一旦泄露或被盗，任何人都能构造签名完全合法的假回调，把订单刷成已支付
// 而不付一分钱。对支持主动查询的渠道，成功回调必须复核通过才允许放行。
// 包括 ErrGatewayCorroborationUnsupported 在内的任何错误均禁止结算；
// 返回结果必须带严格匹配的正数 Amount、Currency 和 ProviderRef。
type CallbackCorroborator interface {
	GatewayProvider
	CorroboratePayment(ctx context.Context, cfg jsonmap.JSON, input CorroborateInput) (*GatewayQueryResult, error)
}

// GatewayRegistry 是应用层使用的只读网关注册表端口。
type GatewayRegistry interface {
	Lookup(providerType, channelType string) (GatewayProvider, bool)
}
