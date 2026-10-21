#!/usr/bin/env python3
"""验证“混合支付切换渠道后，支付旧小额链接不得发卡”。

仅用于本人控制的站点、专用测试账号与可丢弃测试商品。脚本只创建支付单，
不会调用网关付款、伪造回调，也不会读取或输出交付内容。
"""

import argparse
import json
import os
import sys
import tempfile
from decimal import Decimal, InvalidOperation
from pathlib import Path
from urllib.error import HTTPError, URLError
from urllib.parse import quote, urlparse
from urllib.request import Request, build_opener, HTTPRedirectHandler


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, request, fp, code, msg, headers, newurl):
        return None


OPENER = build_opener(NoRedirect)
DEFAULT_STATE = Path(tempfile.gettempdir()) / "dujiao-mixed-payment-repro.json"


def amount(value):
    try:
        return Decimal(str(value))
    except (InvalidOperation, TypeError, ValueError) as exc:
        raise ValueError("API 返回的金额无效") from exc


def api(base, path, token, payload=None):
    body = None if payload is None else json.dumps(payload).encode("utf-8")
    request = Request(
        base + path,
        data=body,
        method="GET" if body is None else "POST",
        headers={
            "Authorization": "Bearer " + token,
            "Accept": "application/json",
            "Content-Type": "application/json",
        },
    )
    try:
        with OPENER.open(request, timeout=15) as response:
            envelope = json.load(response)
    except HTTPError as exc:
        raise RuntimeError(f"API 请求失败：HTTP {exc.code}，路径 {path}") from exc
    except (URLError, TimeoutError, json.JSONDecodeError) as exc:
        raise RuntimeError(f"API 请求失败：{path}，{type(exc).__name__}") from exc
    if not isinstance(envelope, dict) or envelope.get("status_code") != 0:
        msg = envelope.get("msg", "响应格式无效") if isinstance(envelope, dict) else "响应格式无效"
        raise RuntimeError(f"API 拒绝请求：{path}，{msg}")
    if not isinstance(envelope.get("data"), dict):
        raise RuntimeError(f"API 响应缺少 data：{path}")
    return envelope["data"]


def validated_base(raw):
    base = raw.rstrip("/")
    parsed = urlparse(base)
    if parsed.scheme != "https" or not parsed.netloc or parsed.path or parsed.query or parsed.fragment:
        raise ValueError("--base-url 必须是 HTTPS 站点根地址，例如 https://shop.example.com")
    return base


def save_state(path, state):
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(json.dumps(state, ensure_ascii=False, indent=2), encoding="utf-8")
    temporary.replace(path)


def prepare(args):
    token = os.environ.get("DUJIAO_TEST_TOKEN", "").strip()
    if not token:
        raise ValueError("请先设置 DUJIAO_TEST_TOKEN（专用测试账号的用户令牌）")
    base = validated_base(args.base_url)
    order_path = "/api/v1/orders/" + quote(args.order_no, safe="")
    order = api(base, order_path, token)
    wallet = api(base, "/api/v1/wallet", token)
    total = amount(order.get("total_amount"))
    balance = amount(wallet.get("balance"))
    if order.get("order_no") != args.order_no or order.get("status") != "pending_payment" or order.get("paid_at"):
        raise ValueError("测试订单必须属于此账号，且仍为待支付状态")
    if order.get("currency") != "CNY" or not (Decimal("0") < total <= amount(args.max_order_amount)):
        raise ValueError("测试订单必须是 CNY 且金额不超过 --max-order-amount")
    if amount(order.get("wallet_paid_amount")) != 0 or not (Decimal("0") < balance < total):
        raise ValueError("测试账号钱包余额必须大于 0、小于订单总额，且订单尚未抵扣余额")
    allowed = order.get("allowed_payment_channel_ids")
    if allowed is not None and (args.channel_a not in allowed or args.channel_b not in allowed):
        raise ValueError("测试订单不允许指定的支付渠道")
    if args.state.exists():
        raise ValueError(f"状态文件已存在：{args.state}；请先完成或另选 --state")

    print(f"测试订单 {args.order_no}：总额 {total:.2f} CNY，钱包 {balance:.2f} CNY")
    first = api(base, "/api/v1/payments", token, {
        "order_no": args.order_no, "channel_id": args.channel_a, "use_balance": True,
    })
    wallet_part = amount(first.get("wallet_paid_amount"))
    online_part = amount(first.get("online_pay_amount"))
    if first.get("order_paid") or not (Decimal("0") < wallet_part < total) or online_part != total - wallet_part:
        raise RuntimeError("第一笔支付未形成预期的混合支付；请检查测试订单和钱包余额")
    payable = amount(first.get("payable_amount"))
    if payable <= 0 or payable > amount(args.max_order_amount):
        raise RuntimeError("旧支付链接的实际付款额超过测试上限；请勿支付该链接")
    old_id = first.get("payment_id")
    if not isinstance(old_id, int) or old_id <= 0:
        raise RuntimeError("第一笔支付未返回 payment_id")
    state = {
        "base_url": base, "order_no": args.order_no, "old_payment_id": old_id,
        "total_amount": str(total), "old_online_amount": str(online_part),
        "wallet_before": str(balance), "phase": "first_created",
    }
    save_state(args.state, state)
    print(f"旧支付单 ID：{old_id}；旧链接实际应付 {payable:.2f} CNY，抵扣订单 {online_part:.2f} CNY")
    if first.get("pay_url"):
        print("旧支付链接：" + first["pay_url"])
    elif first.get("qr_code"):
        print("旧二维码内容：" + first["qr_code"])
    else:
        print("此渠道没有返回链接或二维码；可在测试订单页面查看旧支付方式。")
    print("现在切换到不使用余额的在线支付……")

    second = api(base, "/api/v1/payments", token, {
        "order_no": args.order_no, "channel_id": args.channel_b, "use_balance": False,
    })
    new_id = second.get("payment_id")
    if not isinstance(new_id, int) or new_id <= 0 or new_id == old_id:
        raise RuntimeError("未生成新的全额支付单；请勿支付旧链接，检查渠道和当前订单")
    updated = api(base, order_path, token)
    if updated.get("status") != "pending_payment" or amount(updated.get("wallet_paid_amount")) != 0:
        raise RuntimeError("切换后订单状态或钱包抵扣额不符；请勿支付旧链接")
    if amount(second.get("online_pay_amount")) != total:
        raise RuntimeError("新支付单的在线应付额不等于订单总额；请勿支付旧链接")
    restored_balance = amount(api(base, "/api/v1/wallet", token).get("balance"))
    if restored_balance != balance:
        raise RuntimeError("切换后测试账号的钱包余额未恢复；请勿支付旧链接")
    state.update({"new_payment_id": new_id, "phase": "switched"})
    save_state(args.state, state)
    print(f"新支付单 ID：{new_id}；全额在线应付 {total:.2f} CNY。状态文件：{args.state}")
    print("只对专用测试商品手动支付上面的旧链接；确认网关显示成功后运行 check。不要支付新链接。")


def check(args):
    token = os.environ.get("DUJIAO_TEST_TOKEN", "").strip()
    if not token:
        raise ValueError("请先设置 DUJIAO_TEST_TOKEN")
    state = json.loads(args.state.read_text(encoding="utf-8"))
    if state.get("phase") != "switched":
        raise ValueError("测试准备尚未完成，不能判断结果")
    base = validated_base(state["base_url"])
    order_no = state["order_no"]
    order = api(base, "/api/v1/orders/" + quote(order_no, safe=""), token)
    delivered = bool(order.get("paid_at") or order.get("fulfillment") or order.get("status") not in ("pending_payment", "canceled"))
    print(f"订单 {order_no}：状态 {order.get('status')}，paid_at={bool(order.get('paid_at'))}，fulfillment={bool(order.get('fulfillment'))}")
    if delivered:
        print("失败：旧链接付款后订单已付款或出现交付记录。请立即暂停自动发卡并保存支付单、订单和网关流水。")
        return 2
    admin_token = os.environ.get("DUJIAO_ADMIN_TOKEN", "").strip()
    if not admin_token:
        print("尚不能判定：需要 DUJIAO_ADMIN_TOKEN 查询旧支付单，确认回调已处理。")
        return 3
    payment = api(base, "/api/v1/admin/payments/" + str(state["old_payment_id"]), admin_token)
    print(f"旧支付单 {state['old_payment_id']}：状态 {payment.get('status')}，异常码 {payment.get('exception_code') or '无'}")
    if payment.get("status") != "success" or not payment.get("callback_at"):
        print("尚不能判定：旧链接的成功回调尚未被本站记录；请等待回调后再执行 check。")
        return 3
    if order.get("status") != "pending_payment":
        print("尚不能判定：订单已关闭或取消，需用未过期测试订单重试。")
        return 3
    current_balance = amount(api(base, "/api/v1/wallet", token).get("balance"))
    expected_balance = amount(state["wallet_before"]) + amount(state["old_online_amount"])
    if current_balance != expected_balance:
        print(f"尚不能判定：测试钱包余额为 {current_balance:.2f}，预期 {expected_balance:.2f}；请核对后台流水。")
        return 3
    print("通过：旧支付单成功回调已处理，小额实付进入测试钱包，订单仍待支付，未发卡。")
    return 0


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    prep = sub.add_parser("prepare", help="创建旧小额支付单，再切换为全额在线支付")
    prep.add_argument("--base-url", required=True, help="HTTPS 站点根地址")
    prep.add_argument("--order-no", required=True, help="专用账号的待支付测试订单号")
    prep.add_argument("--channel-a", required=True, type=int, help="旧支付链接的渠道 ID")
    prep.add_argument("--channel-b", required=True, type=int, help="切换后的渠道 ID，可与 A 相同")
    prep.add_argument("--max-order-amount", default="20.00", help="允许的测试订单金额上限，默认 20 CNY")
    prep.add_argument("--state", type=Path, default=DEFAULT_STATE, help="无令牌状态文件路径")
    chk = sub.add_parser("check", help="付款后确认旧支付单回调与订单交付状态")
    chk.add_argument("--state", type=Path, default=DEFAULT_STATE, help="prepare 保存的状态文件")
    args = parser.parse_args()
    try:
        if args.command == "prepare":
            prepare(args)
            return 0
        return check(args)
    except (ValueError, RuntimeError, KeyError, OSError, json.JSONDecodeError) as exc:
        print(f"错误：{exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
