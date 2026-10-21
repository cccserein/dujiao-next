#!/usr/bin/env python3
"""Read-only reconciliation with the epay v1 gateway's independent order API.

Credentials and gateway replies stay in memory. Public results contain only
local payment/order IDs, counts and fixed error codes; no exception text.
"""

import argparse
import collections
import contextlib
import decimal
import http.client
import json
import re
import signal
import ssl
import subprocess
import time
from urllib.parse import urlencode, urlsplit


DEFAULT_LIMIT = 50
DEFAULT_BUDGET_SECONDS = 45.0
REQUEST_TIMEOUT_SECONDS = 5.0
MAX_RESPONSE_BYTES = 64 * 1024
MAX_LIMIT = 200
AMOUNT_RE = re.compile(r"[0-9]+(?:\.[0-9]+)?\Z")


class ReconcileFailure(Exception):
    """Only fixed, non-sensitive reason codes may be used here."""


def _scalar(value):
    if isinstance(value, str):
        return value.strip()
    if isinstance(value, int) and not isinstance(value, bool):
        return str(value)
    if isinstance(value, decimal.Decimal) and value.is_finite():
        return str(value)
    return ""


def _amount(value):
    raw = _scalar(value)
    if len(raw) > 32 or not AMOUNT_RE.fullmatch(raw):
        raise ReconcileFailure("invalid_amount")
    with decimal.localcontext() as context:
        context.prec = 40
        amount = decimal.Decimal(raw)
        if amount <= 0 or amount != amount.quantize(decimal.Decimal("0.01")):
            raise ReconcileFailure("invalid_amount")
    return amount


def _query_sql(limit, unsettled=False):
    # limit is validated by read_snapshot; the remaining SQL is constant. Every
    # successful payment in the window is included without incident exclusions.
    payment_filter = ("p.status IN ('initiated', 'pending', 'failed', 'expired') AND p.gateway_order_no <> ''"
                      if unsettled else "p.status = 'success'")
    return """
BEGIN READ ONLY;
SET LOCAL statement_timeout = '20s';
WITH candidates AS (
  SELECT p.id AS payment_id, p.order_id, p.channel_id, p.provider_ref,
    COALESCE(NULLIF(p.gateway_order_no, ''), o.order_no) AS out_trade_no,
    p.amount::text AS amount, p.currency,
    COALESCE(p.paid_at, p.updated_at, p.created_at) AS event_at
  FROM payments p LEFT JOIN orders o ON o.id = p.order_id
  WHERE p.provider_type = 'epay' AND p.deleted_at IS NULL AND %s
    AND COALESCE(p.paid_at, p.updated_at, p.created_at) >= now() - interval '48 hours'
), selected AS (
  SELECT * FROM candidates ORDER BY event_at DESC, payment_id DESC LIMIT %d
)
SELECT json_build_object(
  'total_candidates', (SELECT count(*) FROM candidates),
  'payments', (SELECT COALESCE(json_agg(row_to_json(selected) ORDER BY event_at DESC, payment_id DESC), '[]'::json) FROM selected),
  'channels', (SELECT COALESCE(json_object_agg(c.id::text, json_build_object(
    'gateway_url', c.config_json->>'gateway_url',
    'epay_version', c.config_json->>'epay_version',
    'merchant_id', c.config_json->>'merchant_id',
    'merchant_key', c.config_json->>'merchant_key',
    'target_currency', c.config_json->>'target_currency'
  )), '{}'::json) FROM payment_channels c
    WHERE c.id IN (SELECT channel_id FROM selected))
)::text;
COMMIT;
""" % (payment_filter, limit)


def read_snapshot(limit=DEFAULT_LIMIT, unsettled=False):
    if not isinstance(limit, int) or isinstance(limit, bool) or not 1 <= limit <= MAX_LIMIT:
        raise ReconcileFailure("invalid_limit")
    try:
        result = subprocess.run(
            ["docker", "exec", "-i", "dujiaonext-postgres", "sh", "-c",
             'exec psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -qAt -v ON_ERROR_STOP=1'],
            input=_query_sql(limit, unsettled), text=True, capture_output=True,
            check=True, timeout=25,
        )
        snapshot = json.loads(result.stdout)
    except Exception:
        # subprocess errors can include SQL, stdout with credentials or stderr.
        raise ReconcileFailure("database_snapshot_failed") from None
    if not isinstance(snapshot, dict):
        raise ReconcileFailure("database_snapshot_invalid")
    return snapshot


def _request_config(payment, config):
    if not isinstance(config, dict):
        raise ReconcileFailure("channel_config_missing")
    if _scalar(config.get("epay_version")).lower() not in ("", "v1"):
        raise ReconcileFailure("unsupported_epay_version")
    pid = _scalar(config.get("merchant_id"))
    key = config.get("merchant_key")
    provider_ref = _scalar(payment.get("provider_ref"))
    order_no = _scalar(payment.get("out_trade_no"))
    if not pid or not isinstance(key, str) or not key.strip():
        raise ReconcileFailure("merchant_config_missing")
    if not provider_ref or not order_no:
        raise ReconcileFailure("local_reference_missing")
    expected_amount = _amount(payment.get("amount"))
    # epay v1's fixed denomination is CNY unless target_currency is configured;
    # never infer an omitted gateway currency from an untrusted local callback.
    currency = _scalar(config.get("target_currency")).upper() or "CNY"
    if not re.fullmatch(r"[A-Z]{3}", currency):
        raise ReconcileFailure("configured_currency_invalid")
    if _scalar(payment.get("currency")).upper() != currency:
        raise ReconcileFailure("local_currency_config_mismatch")
    try:
        base = urlsplit(_scalar(config.get("gateway_url")))
        if (base.scheme != "https" or not base.hostname or base.username is not None
                or base.password is not None or base.query or base.fragment):
            raise ValueError()
        port = base.port or 443
    except ValueError:
        raise ReconcileFailure("gateway_url_invalid") from None
    path = base.path.rstrip("/") + "/api.php?act=order"
    body = urlencode({"pid": pid, "key": key, "trade_no": provider_ref}).encode("utf-8")
    return base.hostname, port, path, body, pid, provider_ref, order_no, expected_amount, currency


def _set_remaining_timeout(connection, response, deadline):
    remaining = deadline - time.monotonic()
    if remaining <= 0:
        raise ReconcileFailure("request_timeout")
    sock = connection.sock
    if sock is None and response is not None and response.fp is not None:
        # HTTP/1.0/Connection:close may detach the socket from the connection.
        sock = getattr(getattr(response.fp, "raw", None), "_sock", None)
    if sock is not None:
        sock.settimeout(remaining)


@contextlib.contextmanager
def _wall_timeout(seconds):
    # The production cron runs in the Linux main thread. A real-time alarm also
    # bounds DNS/TLS and slow-drip headers, which socket timeouts alone do not.
    # Non-POSIX offline tests retain the socket/deadline checks below.
    if not hasattr(signal, "setitimer"):
        yield
        return
    previous_timer = signal.getitimer(signal.ITIMER_REAL)
    if previous_timer[0] > 0:
        raise ReconcileFailure("request_deadline_unavailable")
    def timeout_handler(_signum, _frame):
        raise ReconcileFailure("request_timeout")
    try:
        previous_handler = signal.signal(signal.SIGALRM, timeout_handler)
    except ValueError:
        raise ReconcileFailure("request_deadline_unavailable") from None
    try:
        signal.setitimer(signal.ITIMER_REAL, seconds)
        yield
    finally:
        signal.setitimer(signal.ITIMER_REAL, 0)
        signal.signal(signal.SIGALRM, previous_handler)


def request_gateway(host, port, path, body, timeout):
    with _wall_timeout(min(REQUEST_TIMEOUT_SECONDS, timeout)):
        return _request_gateway(host, port, path, body, timeout)


def _request_gateway(host, port, path, body, timeout):
    """One HTTPS POST, no redirect/proxy/retry, with bounded body and deadline."""
    deadline = time.monotonic() + min(REQUEST_TIMEOUT_SECONDS, timeout)
    connection = http.client.HTTPSConnection(
        host, port, timeout=min(REQUEST_TIMEOUT_SECONDS, timeout), context=ssl.create_default_context(),
    )
    response = None
    try:
        connection.request("POST", path, body=body, headers={
            "Content-Type": "application/x-www-form-urlencoded",
            "Accept": "application/json",
            "Accept-Encoding": "identity",
            "User-Agent": "dujiao-readonly-reconciliation/1",
        })
        _set_remaining_timeout(connection, response, deadline)
        response = connection.getresponse()
        if response.status != 200:
            return response.status, b""
        chunks = []
        length = 0
        while True:
            _set_remaining_timeout(connection, response, deadline)
            chunk = response.read1(min(8192, MAX_RESPONSE_BYTES + 1 - length))
            if not chunk:
                break
            chunks.append(chunk)
            length += len(chunk)
            if length > MAX_RESPONSE_BYTES:
                raise ReconcileFailure("response_too_large")
        return response.status, b"".join(chunks)
    except ReconcileFailure:
        raise
    except (OSError, http.client.HTTPException):
        # Socket/TLS/HTTP errors must never print request details or response data.
        raise ReconcileFailure("gateway_request_failed") from None
    finally:
        connection.close()


def _unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate field")
        result[key] = value
    return result


def validate_reply(status, body, expected):
    if status in (401, 403):
        return "error", "gateway_auth_failed", True
    if status != 200:
        return "error", "gateway_http_error", False
    if len(body) > MAX_RESPONSE_BYTES:
        return "error", "response_too_large", False
    try:
        reply = json.loads(body, parse_float=decimal.Decimal, object_pairs_hook=_unique_object)
    except (ValueError, TypeError, UnicodeError):
        return "error", "response_invalid_json", False
    if not isinstance(reply, dict):
        return "error", "response_invalid_shape", False
    code = _scalar(reply.get("code"))
    if code in ("401", "403"):
        return "error", "gateway_auth_failed", True
    if code != "1":
        return "error", "gateway_query_rejected", False
    required = ("status", "pid", "trade_no", "out_trade_no", "money")
    if any(not _scalar(reply.get(field)) for field in required):
        return "error", "response_missing_required_field", False
    pid, provider_ref, order_no, amount, currency = expected
    if _scalar(reply["pid"]) != pid:
        return "mismatch", "merchant_mismatch", False
    if _scalar(reply["trade_no"]) != provider_ref:
        return "mismatch", "provider_reference_mismatch", False
    if _scalar(reply["out_trade_no"]) != order_no:
        return "mismatch", "merchant_order_mismatch", False
    if _scalar(reply["status"]) != "1":
        return "mismatch", "gateway_not_paid", False
    try:
        gateway_amount = _amount(reply["money"])
    except ReconcileFailure:
        return "error", "response_invalid_amount", False
    if gateway_amount != amount:
        return "mismatch", "amount_mismatch", False
    if "currency" in reply and _scalar(reply["currency"]).upper() != currency:
        return "mismatch", "currency_mismatch", False
    return "matched", "matched", False


def _unsettled_request_config(payment, config):
    # Redirect payments have a placeholder provider_ref until the first callback.
    # Discover by the unique, stored gateway_order_no, never a reused business ID.
    candidate = dict(payment, provider_ref=payment.get("out_trade_no"))
    host, port, path, _body, *expected = _request_config(candidate, config)
    body = urlencode({"pid": expected[0], "key": config["merchant_key"],
                      "out_trade_no": expected[2]}).encode("utf-8")
    return host, port, path, body, *expected


def validate_unsettled_reply(status, body, expected):
    # This is read-only monitoring; discovering a receipt never credits a wallet
    # or releases goods. Only exact merchant/order/amount/currency bindings count.
    if status != 200 or len(body) > MAX_RESPONSE_BYTES:
        return validate_reply(status, body, expected)
    try:
        reply = json.loads(body, parse_float=decimal.Decimal, object_pairs_hook=_unique_object)
    except (ValueError, TypeError, UnicodeError):
        return "error", "response_invalid_json", False
    if not isinstance(reply, dict):
        return "error", "response_invalid_shape", False
    if (_scalar(reply.get("code")) == "-1" and
            _scalar(reply.get("msg")) in ("订单不存在", "订单不存在！", "订单不存在!")):
        return "matched", "gateway_order_missing", False
    if _scalar(reply.get("code")) != "1":
        return validate_reply(status, body, expected)
    paid_status = _scalar(reply.get("status"))
    if paid_status not in ("0", "1"):
        return "error", "response_invalid_status", False
    ref = _scalar(reply.get("trade_no"))
    if not ref:
        return "error", "response_missing_required_field", False
    bindings = list(expected)
    bindings[1] = ref
    # Validate amount and all identities even for an unpaid response.
    validated_reply = dict(reply, status=1)
    outcome, reason, stop = validate_reply(status, json.dumps(validated_reply, default=str).encode(), bindings)
    if outcome != "matched":
        return outcome, reason, stop
    if paid_status == "1":
        return "mismatch", "gateway_paid_local_unsettled", False
    return "matched", "gateway_not_paid", False


def reconcile_snapshot(snapshot, limit=DEFAULT_LIMIT, budget_seconds=DEFAULT_BUDGET_SECONDS,
                       request=request_gateway, clock=time.monotonic, unsettled=False):
    started = clock()
    if not isinstance(limit, int) or isinstance(limit, bool) or not 1 <= limit <= MAX_LIMIT or not 0 < budget_seconds <= 300:
        raise ReconcileFailure("invalid_limits")
    payments = snapshot.get("payments")
    channels = snapshot.get("channels")
    total = snapshot.get("total_candidates")
    if (not isinstance(payments, list) or not isinstance(channels, dict)
            or not isinstance(total, int) or isinstance(total, bool) or total < len(payments)):
        raise ReconcileFailure("database_snapshot_invalid")
    counts = collections.Counter()
    findings = []
    stopped_reason = None
    for payment in payments[:limit]:
        remaining = budget_seconds - (clock() - started)
        if remaining <= 0:
            stopped_reason = "budget_exhausted"
            break
        counts["processed"] += 1
        stop = False
        try:
            config = channels.get(str(payment.get("channel_id")))
            build_request = _unsettled_request_config if unsettled else _request_config
            host, port, path, body, *expected = build_request(payment, config)
            counts["requests"] += 1
            status, reply = request(host, port, path, body, min(REQUEST_TIMEOUT_SECONDS, remaining))
            validator = validate_unsettled_reply if unsettled else validate_reply
            outcome, reason, stop = validator(status, reply, expected)
        except ReconcileFailure as error:
            outcome, reason = "error", str(error)
        except Exception:
            # A fixed reason protects against accidental credential leakage from
            # transport, JSON parsing or unanticipated database data failures.
            outcome, reason = "error", "reconciliation_internal_error"
        counts[outcome] += 1
        if outcome != "matched":
            findings.append({"payment_id": payment["payment_id"], "order_id": payment["order_id"], "reason": reason})
        if stop:
            stopped_reason = "gateway_auth_failed"
            break
    checked = counts["matched"] + counts["mismatch"]
    incomplete = checked < total
    alerts = []
    if counts["mismatch"]:
        alerts.append("gateway_reconciliation_mismatch")
    if counts["error"]:
        alerts.append("gateway_reconciliation_failed")
    if incomplete:
        alerts.append("gateway_reconciliation_incomplete")
    mismatch_findings = [entry for entry in findings if entry["reason"] in {
        "merchant_mismatch", "provider_reference_mismatch", "merchant_order_mismatch",
        "gateway_not_paid", "amount_mismatch", "currency_mismatch",
        "gateway_paid_local_unsettled",
    }]
    error_findings = [entry for entry in findings if entry not in mismatch_findings]
    return {
        "status": "failed" if counts["error"] else "mismatch" if counts["mismatch"] else "incomplete" if incomplete else "ok",
        "window_hours": 48, "limit": limit, "budget_seconds": budget_seconds,
        "total_candidates": total, "selected_count": len(payments[:limit]),
        "processed_count": counts["processed"], "request_count": counts["requests"],
        "checked_count": checked, "matched_count": counts["matched"],
        "mismatch_count": counts["mismatch"], "error_count": counts["error"],
        "unchecked_count": total - checked, "unqueried_count": total - counts["requests"],
        "remaining_count": total - counts["processed"], "incomplete": incomplete,
        "stopped_reason": stopped_reason,
        "anomalous_payment_ids": sorted({entry["payment_id"] for entry in mismatch_findings}),
        "anomalous_order_ids": sorted({entry["order_id"] for entry in mismatch_findings}),
        "failed_payment_ids": sorted({entry["payment_id"] for entry in error_findings}),
        "failed_order_ids": sorted({entry["order_id"] for entry in error_findings}),
        "findings": findings, "alerts": alerts,
    }


def reconcile_recent(limit=DEFAULT_LIMIT, budget_seconds=DEFAULT_BUDGET_SECONDS, unsettled=False):
    try:
        return reconcile_snapshot(read_snapshot(limit, unsettled), limit, budget_seconds, unsettled=unsettled)
    except Exception:
        return {
            "status": "failed", "window_hours": 48, "incomplete": True,
            "total_candidates": None, "unchecked_count": None, "unqueried_count": None,
            "stopped_reason": "database_snapshot_failed",
            "anomalous_payment_ids": [], "anomalous_order_ids": [],
            "alerts": ["gateway_reconciliation_failed", "gateway_reconciliation_incomplete"],
        }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--limit", type=int, default=DEFAULT_LIMIT)
    parser.add_argument("--budget-seconds", type=float, default=DEFAULT_BUDGET_SECONDS)
    args = parser.parse_args()
    print(json.dumps(reconcile_recent(args.limit, args.budget_seconds), ensure_ascii=False))


if __name__ == "__main__":
    main()
