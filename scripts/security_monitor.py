#!/usr/bin/env python3
"""Read-only production checks for dujiao-next orders and HTTP traffic."""

import argparse
import collections
import datetime as dt
import json
import re
import subprocess
import urllib.request
from pathlib import Path
from urllib.parse import urlsplit


ORDER_SQL = """
WITH paid_payment_coverage AS (
  SELECT o.id, o.online_paid_amount,
    COALESCE(sum(p.amount) FILTER (WHERE p.status = 'success' AND p.deleted_at IS NULL AND p.currency = o.currency), 0) AS same_currency_success_amount,
    count(*) FILTER (WHERE p.status = 'success' AND p.deleted_at IS NULL AND p.currency = o.currency) AS same_currency_success_count,
    count(*) FILTER (WHERE p.status = 'success' AND p.deleted_at IS NULL AND p.currency <> o.currency) AS exchanged_success_count
  FROM orders o
  LEFT JOIN payments p ON p.order_id = o.id
  WHERE o.deleted_at IS NULL AND o.parent_id IS NULL AND o.paid_at IS NOT NULL AND o.online_paid_amount > 0
  GROUP BY o.id
), same_currency_shortfalls AS (
  SELECT online_paid_amount - same_currency_success_amount AS gap
  FROM paid_payment_coverage
  WHERE same_currency_success_count > 0 AND exchanged_success_count = 0
    AND same_currency_success_amount + 0.01 < online_paid_amount
)
SELECT json_build_object(
  'orders_24h', (SELECT count(*) FROM orders WHERE deleted_at IS NULL AND parent_id IS NULL AND created_at >= now() - interval '24 hours'),
  'positive_orders_24h', (SELECT count(*) FROM orders WHERE deleted_at IS NULL AND parent_id IS NULL AND total_amount > 0 AND created_at >= now() - interval '24 hours'),
  'zero_total_orders_24h', (SELECT count(*) FROM orders WHERE deleted_at IS NULL AND parent_id IS NULL AND total_amount = 0 AND created_at >= now() - interval '24 hours'),
  'paid_amount_gap', (SELECT count(*) FROM orders WHERE deleted_at IS NULL AND parent_id IS NULL AND total_amount > 0 AND paid_at IS NOT NULL AND wallet_paid_amount + online_paid_amount < total_amount),
  'delivery_without_payment', (SELECT count(*) FROM fulfillments f JOIN orders o ON o.id = f.order_id WHERE f.deleted_at IS NULL AND o.deleted_at IS NULL AND o.total_amount > 0 AND o.paid_at IS NULL),
  'online_paid_without_success_7d', (SELECT count(*) FROM orders o WHERE o.deleted_at IS NULL AND o.parent_id IS NULL AND o.created_at >= now() - interval '7 days' AND o.online_paid_amount > 0 AND o.paid_at IS NOT NULL AND NOT EXISTS (SELECT 1 FROM payments p WHERE p.deleted_at IS NULL AND p.order_id = o.id AND p.status = 'success' AND p.paid_at IS NOT NULL)),
  'successful_payment_unpaid_order_7d', (SELECT count(*) FROM payments p JOIN orders o ON o.id = p.order_id WHERE p.deleted_at IS NULL AND o.deleted_at IS NULL AND p.created_at >= now() - interval '7 days' AND p.status = 'success' AND p.paid_at IS NOT NULL AND o.paid_at IS NULL),
  'paid_same_currency_undercoverage', (SELECT count(*) FROM same_currency_shortfalls),
  'paid_same_currency_gap_amount', (SELECT COALESCE(sum(gap), 0) FROM same_currency_shortfalls)
)::text;
"""

ACCESS_RE = re.compile(r'^(\S+) \S+ \S+ \[([^]]+)\] "(\S+) ([^" ]+) [^"]+" (\d{3}) ')
PROBE_RE = re.compile(r'(?i)(?:/\.env|/wp-|/cgi-bin/|/vendor/|/actuator|/phpmyadmin|\.php(?:/|$))')


def read_orders():
    proc = subprocess.run(
        ["docker", "exec", "-i", "dujiaonext-postgres", "sh", "-c",
         'exec psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -At -v ON_ERROR_STOP=1'],
        input=ORDER_SQL, text=True, capture_output=True, check=True, timeout=30,
    )
    return json.loads(proc.stdout.strip())


def read_traffic(log_paths, now):
    start = now - dt.timedelta(hours=24)
    statuses = collections.Counter()
    errors = collections.Counter()
    probes = collections.Counter()
    callback_statuses = collections.Counter()
    ip_requests = collections.Counter()
    total = 0
    for log_path in log_paths:
        path = Path(log_path)
        if not path.exists():
            continue
        with path.open(encoding="utf-8", errors="replace") as stream:
            for line in stream:
                match = ACCESS_RE.match(line)
                if not match:
                    continue
                ip, when_text, _method, target, status = match.groups()
                try:
                    when = dt.datetime.strptime(when_text, "%d/%b/%Y:%H:%M:%S %z")
                except ValueError:
                    continue
                if not start <= when <= now:
                    continue
                endpoint = urlsplit(target).path
                total += 1
                statuses[status] += 1
                ip_requests[ip] += 1
                if status.startswith(("4", "5")):
                    errors[f"{status} {endpoint}"] += 1
                if PROBE_RE.search(endpoint):
                    probes[endpoint] += 1
                if endpoint.startswith("/api/v1/payments/callback") or endpoint.startswith("/api/v1/payments/webhook/"):
                    callback_statuses[status] += 1
    return {
        "requests_24h": total,
        "status_counts_24h": dict(sorted(statuses.items())),
        "top_errors_24h": errors.most_common(10),
        "probe_requests_24h": sum(probes.values()),
        "top_probes_24h": probes.most_common(10),
        "payment_callback_status_24h": dict(sorted(callback_statuses.items())),
        "highest_ip_request_count_24h": max(ip_requests.values(), default=0),
    }


def check_health():
    try:
        with urllib.request.urlopen("http://127.0.0.1:8080/health", timeout=5) as response:
            return response.status
    except Exception:
        return None


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--access-log", nargs="+", default=["/var/log/nginx/dujiao-access.log", "/var/log/nginx/dujiao-access.log.1"])
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    now = dt.datetime.now(dt.timezone.utc)
    orders = read_orders()
    traffic = read_traffic(args.access_log, now)
    health = check_health()
    alerts = [name for name in ("paid_amount_gap", "delivery_without_payment", "online_paid_without_success_7d", "successful_payment_unpaid_order_7d", "paid_same_currency_undercoverage") if orders[name] > 0]
    if health != 200:
        alerts.append("health_check_failed")
    if traffic["probe_requests_24h"] > 100:
        alerts.append("probe_volume_high")
    if sum(int(v) for k, v in traffic["status_counts_24h"].items() if k.startswith("5")) > 20:
        alerts.append("http_5xx_high")
    report = {"checked_at": now.isoformat(), "health_status": health, "orders": orders, "traffic": traffic, "alerts": alerts}
    rendered = json.dumps(report, ensure_ascii=False, indent=2) + "\n"
    if args.output:
        args.output.parent.mkdir(parents=True, exist_ok=True)
        temporary = args.output.with_suffix(".tmp")
        temporary.write_text(rendered, encoding="utf-8")
        temporary.replace(args.output)
    else:
        print(rendered, end="")


if __name__ == "__main__":
    main()
