package domain

import (
	"time"

	"github.com/dujiao-next/internal/shared/jsonmap"
	"github.com/dujiao-next/internal/shared/money"
)

// Payment 支付记录
type Payment struct {
	ID                 uint         `gorm:"primarykey" json:"id"`                                         // 主键
	OrderID            uint         `gorm:"index;not null" json:"order_id"`                               // 订单ID
	ChannelID          uint         `gorm:"index;not null" json:"channel_id"`                             // 支付渠道ID
	ProviderType       string       `gorm:"not null" json:"provider_type"`                                // 提供方类型（official/epay）
	ChannelType        string       `gorm:"not null" json:"channel_type"`                                 // 渠道类型（wechat/alipay/qqpay/paypal）
	InteractionMode    string       `gorm:"not null" json:"interaction_mode"`                             // 交互方式（qr/redirect）
	Amount             money.Amount `gorm:"type:decimal(20,2);not null" json:"amount"`                    // 支付金额（含手续费）
	FeeRate            money.Amount `gorm:"type:decimal(6,2);not null;default:0" json:"fee_rate"`         // 手续费比例（百分比）
	FixedFee           money.Amount `gorm:"type:decimal(6,2);not null;default:0" json:"fixed_fee"`        // 固定手续费
	FeeAmount          money.Amount `gorm:"type:decimal(20,2);not null;default:0" json:"fee_amount"`      // 手续费金额
	FeePolicy          string       `gorm:"type:varchar(32);index;not null;default:''" json:"fee_policy"` // 创建时手续费策略，空值保留历史用户加费语义
	Currency           string       `gorm:"not null" json:"currency"`                                     // 币种
	Status             string       `gorm:"index;not null" json:"status"`                                 // 支付状态
	ExceptionCode      string       `gorm:"type:varchar(64);index" json:"exception_code,omitempty"`       // 实收但未获履约资格，需核查或退款
	ProviderRef        string       `gorm:"index" json:"provider_ref"`                                    // 第三方流水号
	GatewayOrderNo     string       `gorm:"index;size:64" json:"gateway_order_no"`                        // 网关侧订单号
	ProviderPayload    jsonmap.JSON `gorm:"type:json" json:"provider_payload"`                            // 第三方回调数据
	PayURL             string       `gorm:"type:text" json:"pay_url"`                                     // 跳转链接
	QRCode             string       `gorm:"type:text" json:"qr_code"`                                     // 二维码内容/地址
	CreatedAt          time.Time    `gorm:"index" json:"created_at"`                                      // 创建时间
	UpdatedAt          time.Time    `gorm:"index" json:"updated_at"`                                      // 更新时间
	PaidAt             *time.Time   `gorm:"index" json:"paid_at"`                                         // 支付时间
	ExpiredAt          *time.Time   `gorm:"index" json:"expired_at"`                                      // 过期时间
	CallbackAt         *time.Time   `gorm:"index" json:"callback_at"`                                     // 回调时间
	DeletedAt          *time.Time   `gorm:"index" json:"-"`                                               // 软删除时间
	ChannelName        string       `gorm:"-" json:"channel_name,omitempty"`                              // 支付渠道名称（非数据库字段，JOIN 填充）
	DisplayChannelType string       `gorm:"-" json:"display_channel_type,omitempty"`                      // 展示用渠道类型（非数据库字段）
}

// TableName 指定表名
func (Payment) TableName() string {
	return "payments"
}
