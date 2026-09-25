# Minimal Shop

一个独立的精简售卡项目。目标闭环：管理员上架和导入卡密、用户下单、支付回调确认、一次性发卡。没有分站、分销、开放 API、钱包或混合支付。

开发环境支持**模拟支付**。真实适配器按现站公开启用的易支付 v1 支付宝跳转及 BEPUSDT 收银台模式实现；尚未完成真实网关沙箱验证和生产迁移，**不要部署到生产或导入真实卡密**。本目录不会改变现站。

## 核心规则

- 订单保存下单时的标题、价格与币种快照，并预留一张加密卡密。
- 每张订单同一时刻只有一个有效支付单。回调必须通过网关验签，并匹配支付单、订单号、金额和币种。
- 重复回调幂等；过期订单、旧支付单、少付或币种错误进入异常记录，不发卡。
- 支付确认与卡密分配在同一个 PostgreSQL 事务中完成。卡密只向对应订单用户和管理员展示。
- 卡密采用 AES-256-GCM 加密，密钥不保存在数据库和代码仓库。
- 后台账号登录强制 TOTP 动态验证码，验证码不能重复使用；管理员会话最长 12 小时。登录和注册有应用内限速，生产仍应在反向代理增加 IP 限速。

## 开发配置

需要 Go 1.26.8 或更新的安全补丁版本和 PostgreSQL。设置 `SHOP_DATABASE_URL`、`SHOP_CARD_KEY_HEX`（32 字节随机值的十六进制）、`SHOP_APP_URL`（例如 `http://localhost:8080`）、`SHOP_ENV=development`、`SHOP_PAYMENT_MODE=mock`、`SHOP_MOCK_KEY_HEX`（至少 32 字节随机值的十六进制）。运行 `go run ./cmd/shop migrate` 创建数据表，`go run ./cmd/shop admin-create` 在终端交互式创建管理员，立即将显示的 TOTP 密钥录入认证器，然后运行 `go run ./cmd/shop serve`。

Docker 开发方式：复制 `.env.example` 为 `.env`，把 `SHOP_DB_OWNER_PASSWORD` 和 `SHOP_DB_APP_PASSWORD` 分别改为独立的随机十六进制值，将 `SHOP_CARD_KEY_HEX` 和 `SHOP_MOCK_KEY_HEX` 分别改为独立的 64 个十六进制字符。PowerShell 可用 `[Convert]::ToHexString([Security.Cryptography.RandomNumberGenerator]::GetBytes(32)).ToLowerInvariant()` 生成。运行 `docker compose up -d --build`，接着运行 `docker compose run --rm -it shop admin-create`，访问 `http://localhost:18080`。数据库不发布端口；站点只绑定本机回环地址。运行中的站点使用仅有读写表权限的 `shop_app` 账号，建表账号仅由一次性迁移容器使用。**保管好 `.env` 和管理员 TOTP 密钥，卡密密钥丢失后无法解密已导入卡密。**

测试：`go test ./...` 运行单元测试。交易集成测试要求一个可丢弃的 PostgreSQL 数据库 `shop_test`：设置 `SHOP_TEST_DATABASE_URL=postgres://.../shop_test?sslmode=disable` 后运行 `go test -v ./internal/shop`。测试只在该库中创建并删除独立的随机 schema。安全测试结果和上线前必须完成的验收见 [SECURITY-REVIEW.md](SECURITY-REVIEW.md)。

`SHOP_PAYMENT_MODE=live` 需要 `SHOP_EPAY_URL`、`SHOP_EPAY_MERCHANT_ID`、`SHOP_EPAY_KEY`、`SHOP_BEPUSDT_URL`、`SHOP_BEPUSDT_TOKEN`，可选 `SHOP_BEPUSDT_CURRENCIES`（默认 `USDT`）。生产模式拒绝模拟支付；真实网关回调实测和迁移验证完成前不得切换现站。

可在 Compose 目录运行 `sh ops/backup.sh /absolute/backup-directory` 生成 PostgreSQL 自定义格式备份和 SHA-256 校验文件。若使用非默认 Compose 项目名，先设置 `COMPOSE_PROJECT_NAME`。数据库备份须与 `SHOP_CARD_KEY_HEX` 分开保管，并定期在隔离库演练恢复。

上线前还要验证实际商户号与回调字段、配置 HTTPS 反向代理与身份验证端点 IP 限速、备份并演练数据库和卡密密钥恢复、检查支付异常记录。若反向代理通过 Docker 网关访问站点，把**实际代理来源地址**以精确 CIDR（例如 `172.20.0.1/32`）配置到 `SHOP_TRUSTED_PROXY_CIDRS`，并让代理覆盖 `X-Real-IP`；不要信任整个 Docker 网段。现站卡密不能直接导入测试环境。
