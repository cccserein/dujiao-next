"""Offline gateway/monitor regressions. Never uses Docker or a real gateway."""

import contextlib
import io
import json
from pathlib import Path
import subprocess
import sys
import unittest
from unittest import mock
from urllib.parse import parse_qs

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import gateway_reconcile as gateway
import security_monitor as monitor


def fake_snapshot(count=1, total=None):
    return {
        "total_candidates": count if total is None else total,
        "channels": {"3": {
            "gateway_url": "https://gateway.invalid/payments",
            "epay_version": "v1", "merchant_id": "1001",
            "merchant_key": "FAKE-SECRET-DO-NOT-LOG", "target_currency": "CNY",
        }},
        "payments": [{
            "payment_id": 9000 + index, "order_id": 8000 + index, "channel_id": 3,
            "provider_ref": "FAKE-TRADE-" + str(index),
            "out_trade_no": "FAKE-ORDER-" + str(index), "amount": "12.34", "currency": "CNY",
        } for index in range(count)],
    }


def reply_for(payment, **overrides):
    reply = {"code": 1, "status": 1, "pid": "1001", "trade_no": payment["provider_ref"],
             "out_trade_no": payment["out_trade_no"], "money": "12.34"}
    reply.update(overrides)
    return json.dumps(reply).encode()


def success_request(host, port, path, body, timeout):
    trade = parse_qs(body.decode())["trade_no"][0]
    index = trade.removeprefix("FAKE-TRADE-")
    return 200, json.dumps({"code": "1", "status": "1", "pid": "1001", "trade_no": trade,
                            "out_trade_no": "FAKE-ORDER-" + index, "money": "12.340"}).encode()


class ReconciliationTests(unittest.TestCase):
    def test_empty_window_is_normal(self):
        request = mock.Mock()
        result = gateway.reconcile_snapshot(fake_snapshot(0), request=request)
        self.assertEqual(result["status"], "ok")
        self.assertEqual(result["alerts"], [])
        request.assert_not_called()

    def test_independent_success_and_credentials_only_in_post_body(self):
        snapshot = fake_snapshot()
        request = mock.Mock(side_effect=success_request)
        result = gateway.reconcile_snapshot(snapshot, request=request)
        host, port, path, body, timeout = request.call_args.args
        self.assertEqual((host, port, path), ("gateway.invalid", 443, "/payments/api.php?act=order"))
        self.assertEqual(parse_qs(body.decode()), {"pid": ["1001"], "key": ["FAKE-SECRET-DO-NOT-LOG"], "trade_no": ["FAKE-TRADE-0"]})
        self.assertLessEqual(timeout, 5)
        self.assertEqual(result["matched_count"], 1)
        self.assertEqual(result["status"], "ok")
        for sensitive in ("1001", "FAKE-SECRET", "FAKE-TRADE", "FAKE-ORDER"):
            self.assertNotIn(sensitive, json.dumps(result))
            self.assertNotIn(sensitive, path)

    def test_each_binding_or_status_or_amount_mismatch_is_alerted(self):
        snapshot = fake_snapshot()
        for field, value in (("pid", "999"), ("trade_no", "FAKE-OTHER"),
                             ("out_trade_no", "FAKE-OTHER"), ("status", 0),
                             ("money", "0.01"), ("currency", "USD"), ("currency", "")):
            with self.subTest(field=field, value=value):
                request = mock.Mock(return_value=(200, reply_for(snapshot["payments"][0], **{field: value})))
                result = gateway.reconcile_snapshot(snapshot, request=request)
                self.assertEqual(result["mismatch_count"], 1)
                self.assertEqual(result["anomalous_payment_ids"], [9000])
                self.assertEqual(result["anomalous_order_ids"], [8000])
                self.assertIn("gateway_reconciliation_mismatch", result["alerts"])

    def test_missing_required_fields_are_not_treated_as_paid(self):
        snapshot = fake_snapshot()
        for field in ("code", "status", "pid", "trade_no", "out_trade_no", "money"):
            with self.subTest(field=field):
                reply = json.loads(reply_for(snapshot["payments"][0]))
                del reply[field]
                result = gateway.reconcile_snapshot(snapshot, request=mock.Mock(return_value=(200, json.dumps(reply).encode())))
                self.assertEqual(result["error_count"], 1)
                self.assertTrue(result["incomplete"])
                self.assertIn("gateway_reconciliation_failed", result["alerts"])

    def test_http_and_json_auth_errors_stop_without_repeating_credentials(self):
        for status, body in ((401, b"not logged"), (403, b"not logged"),
                             (200, b'{"code":401,"msg":"FAKE-MERCHANT-DISABLED"}'),
                             (200, b'{"code":"403"}')):
            with self.subTest(status=status, body=body):
                request = mock.Mock(return_value=(status, body))
                result = gateway.reconcile_snapshot(fake_snapshot(3, total=60), request=request)
                self.assertEqual(request.call_count, 1)
                self.assertEqual(result["stopped_reason"], "gateway_auth_failed")
                self.assertEqual(result["unqueried_count"], 59)
                self.assertEqual(result["unchecked_count"], 60)
                self.assertEqual(result["remaining_count"], 59)
                self.assertNotIn("FAKE-MERCHANT-DISABLED", json.dumps(result))

    def test_limit_is_explicit_and_never_exempts_incident_records(self):
        snapshot = fake_snapshot(51)
        request = mock.Mock(side_effect=success_request)
        result = gateway.reconcile_snapshot(snapshot, request=request)
        self.assertEqual(request.call_count, 50)
        self.assertEqual(result["remaining_count"], 1)
        self.assertTrue(result["incomplete"])
        self.assertIn("gateway_reconciliation_incomplete", result["alerts"])
        sql = gateway._query_sql(50)
        self.assertIn("BEGIN READ ONLY", sql)
        self.assertIn("interval '48 hours'", sql)
        self.assertIn("ORDER BY event_at DESC, payment_id DESC LIMIT 50", sql)
        self.assertNotIn("NOT IN", sql)
        self.assertNotIn("2182", sql)

    def test_budget_exhaustion_reports_every_unprocessed_payment(self):
        time_value = [0]
        def request(*args):
            time_value[0] += 5
            return success_request(*args)
        result = gateway.reconcile_snapshot(fake_snapshot(4), budget_seconds=5,
                                            request=request, clock=lambda: time_value[0])
        self.assertEqual(result["request_count"], 1)
        self.assertEqual(result["stopped_reason"], "budget_exhausted")
        self.assertEqual(result["unqueried_count"], 3)

    def test_currency_omission_only_uses_configured_denomination(self):
        snapshot = fake_snapshot()
        snapshot["channels"]["3"]["target_currency"] = "USD"
        snapshot["payments"][0]["currency"] = "USD"
        result = gateway.reconcile_snapshot(snapshot, request=success_request)
        self.assertEqual(result["matched_count"], 1)
        snapshot["channels"]["3"]["target_currency"] = ""
        request = mock.Mock()
        result = gateway.reconcile_snapshot(snapshot, request=request)
        request.assert_not_called()
        self.assertEqual(result["findings"][0]["reason"], "local_currency_config_mismatch")

    def test_bad_configuration_does_not_send_a_request(self):
        for field, value in (("gateway_url", "http://gateway.invalid"),
                             ("gateway_url", "https://user:password@gateway.invalid"),
                             ("gateway_url", "https://gateway.invalid/?key=secret"),
                             ("merchant_key", ""), ("epay_version", "v2")):
            with self.subTest(field=field):
                snapshot = fake_snapshot()
                snapshot["channels"]["3"][field] = value
                request = mock.Mock()
                result = gateway.reconcile_snapshot(snapshot, request=request)
                self.assertEqual(result["status"], "failed")
                request.assert_not_called()

    def test_network_and_reply_failures_cannot_be_normal(self):
        cases = [(500, b"server secret"), (302, b"redirect secret"), (200, b"broken-secret"),
                 (200, b"x" * (gateway.MAX_RESPONSE_BYTES + 1)),
                 (200, b'{"code":1,"code":0}'), (200, b'{"code":true}')]
        for response in cases:
            with self.subTest(status=response[0]):
                result = gateway.reconcile_snapshot(fake_snapshot(), request=mock.Mock(return_value=response))
                self.assertEqual(result["status"], "failed")
                self.assertEqual(result["matched_count"], 0)
                self.assertNotIn("secret", json.dumps(result))
        request = mock.Mock(side_effect=RuntimeError("FAKE-SECRET-DO-NOT-LOG"))
        result = gateway.reconcile_snapshot(fake_snapshot(), request=request)
        self.assertNotIn("FAKE-SECRET", json.dumps(result))

    def test_amount_parsing_rejects_rounding_exponent_and_nonfinite(self):
        snapshot = fake_snapshot()
        for money in ("1e1", "NaN", "Infinity", "-12.34", "12.341", "0", True, None, "1" * 33):
            with self.subTest(money=money):
                result = gateway.reconcile_snapshot(snapshot, request=mock.Mock(return_value=(200, reply_for(snapshot["payments"][0], money=money))))
                self.assertEqual(result["matched_count"], 0)

    def test_database_errors_do_not_leak_captured_credentials(self):
        error = subprocess.CalledProcessError(1, "fake", output="FAKE-SECRET", stderr="FAKE-SECRET")
        with mock.patch.object(gateway.subprocess, "run", side_effect=error):
            result = gateway.reconcile_recent()
        self.assertEqual(result["status"], "failed")
        self.assertIsNone(result["total_candidates"])
        self.assertNotIn("FAKE-SECRET", json.dumps(result))

    def test_psql_uses_stdin_and_readonly_transaction(self):
        with mock.patch.object(gateway.subprocess, "run", return_value=mock.Mock(stdout=json.dumps(fake_snapshot(0)))) as run:
            gateway.read_snapshot()
        self.assertIn("BEGIN READ ONLY", run.call_args.kwargs["input"])
        self.assertTrue(run.call_args.kwargs["capture_output"])
        self.assertNotIn("merchant_key", " ".join(run.call_args.args[0]))


class UnsettledReconciliationTests(unittest.TestCase):
    def test_paid_gateway_order_with_expired_local_payment_is_alerted(self):
        snapshot = fake_snapshot()
        snapshot["payments"][0]["provider_ref"] = ""
        response = reply_for(snapshot["payments"][0], trade_no="REAL-DISCOVERED-TRADE")
        request = mock.Mock(return_value=(200, response))
        result = gateway.reconcile_snapshot(snapshot, request=request, unsettled=True)
        args = request.call_args.args
        self.assertEqual(parse_qs(args[3].decode()), {
            "pid": ["1001"], "key": ["FAKE-SECRET-DO-NOT-LOG"], "out_trade_no": ["FAKE-ORDER-0"]})
        self.assertEqual(result["anomalous_order_ids"], [8000])
        self.assertEqual(result["findings"][0]["reason"], "gateway_paid_local_unsettled")
        self.assertEqual(result["status"], "mismatch")
        self.assertFalse(result["incomplete"])
        self.assertNotIn("REAL-DISCOVERED-TRADE", json.dumps(result))
        self.assertNotIn("FAKE-SECRET", json.dumps(result))

    def test_confirmed_unpaid_or_missing_gateway_order_is_consistent(self):
        snapshot = fake_snapshot()
        for body in (reply_for(snapshot["payments"][0], status=0),
                     json.dumps({"code": -1, "msg": "订单不存在"}).encode()):
            with self.subTest(body=body):
                result = gateway.reconcile_snapshot(snapshot, request=mock.Mock(return_value=(200, body)), unsettled=True)
                self.assertEqual(result["status"], "ok")
                self.assertEqual(result["alerts"], [])
        # A rejected query remains an error when our ledger claims success.
        result = gateway.reconcile_snapshot(snapshot, request=mock.Mock(return_value=(200, body)))
        self.assertEqual(result["status"], "failed")

    def test_unpaid_response_still_requires_exact_binding(self):
        snapshot = fake_snapshot()
        for field, value in (("money", "0.01"), ("currency", "USD"), ("pid", "9999"),
                             ("out_trade_no", "OTHER"), ("trade_no", "")):
            with self.subTest(field=field):
                body = reply_for(snapshot["payments"][0], status=0, **{field: value})
                result = gateway.reconcile_snapshot(snapshot, request=mock.Mock(return_value=(200, body)), unsettled=True)
                self.assertNotEqual(result["status"], "ok")

    def test_ambiguous_status_auth_and_duplicate_fields_are_not_unpaid(self):
        snapshot = fake_snapshot()
        bodies = [reply_for(snapshot["payments"][0], status=value) for value in (None, True, 2, "1e0")]
        bodies += [b'{"code":1,"code":-1}', b'{"code":-1,"msg":"merchant disabled"}',
                   json.dumps({"code": -2, "msg": "订单不存在"}).encode()]
        for body in bodies:
            with self.subTest(body=body):
                result = gateway.reconcile_snapshot(snapshot, request=mock.Mock(return_value=(200, body)), unsettled=True)
                self.assertEqual(result["status"], "failed")

    def test_unsettled_budget_and_auth_failure_report_remaining_orders(self):
        request = mock.Mock(return_value=(401, b""))
        result = gateway.reconcile_snapshot(fake_snapshot(3), request=request, unsettled=True)
        self.assertEqual(request.call_count, 1)
        self.assertEqual(result["unchecked_count"], 3)
        self.assertTrue(result["incomplete"])
        clock = iter([0, 6])
        request = mock.Mock()
        result = gateway.reconcile_snapshot(fake_snapshot(3), budget_seconds=5,
                                            clock=lambda: next(clock), request=request, unsettled=True)
        request.assert_not_called()
        self.assertEqual(result["stopped_reason"], "budget_exhausted")

    def test_snapshot_only_uses_unique_gateway_numbers_for_unsettled(self):
        with mock.patch.object(gateway.subprocess, "run", return_value=mock.Mock(stdout=json.dumps(fake_snapshot(0)))) as run:
            gateway.read_snapshot(unsettled=True)
        sql = run.call_args.kwargs["input"]
        self.assertIn("BEGIN READ ONLY", sql)
        self.assertIn("p.status IN ('initiated', 'pending', 'failed', 'expired')", sql)
        self.assertIn("p.gateway_order_no <> ''", sql)
        self.assertIn("interval '48 hours'", sql)
        self.assertNotIn("p.status = 'success'", sql)


class TransportTests(unittest.TestCase):
    def test_production_wall_timeout_is_armed_and_restored(self):
        fake_handler = mock.Mock()
        with mock.patch.object(gateway.signal, "getitimer", return_value=(0, 0), create=True), \
                mock.patch.object(gateway.signal, "ITIMER_REAL", 0, create=True), \
                mock.patch.object(gateway.signal, "SIGALRM", 14, create=True), \
                mock.patch.object(gateway.signal, "signal", return_value=fake_handler) as handler, \
                mock.patch.object(gateway.signal, "setitimer", create=True) as timer, \
                mock.patch.object(gateway, "_request_gateway", return_value=(401, b"")):
            self.assertEqual(gateway.request_gateway("fake", 443, "/fake", b"fake", 99), (401, b""))
        self.assertEqual(timer.call_args_list, [mock.call(0, 5), mock.call(0, 0)])
        self.assertEqual(handler.call_args_list[-1], mock.call(14, fake_handler))

    def test_https_transport_does_not_follow_redirects(self):
        response = mock.Mock(status=302)
        connection = mock.Mock()
        connection.getresponse.return_value = response
        with mock.patch.object(gateway.http.client, "HTTPSConnection", return_value=connection) as https:
            status, body = gateway.request_gateway("gateway.invalid", 443, "/api.php?act=order", b"pid=1&key=FAKE", 99)
        self.assertEqual((status, body), (302, b""))
        self.assertEqual(https.call_args.kwargs["timeout"], 5)
        connection.request.assert_called_once()
        response.read1.assert_not_called()
        connection.close.assert_called_once()

    def test_https_transport_enforces_response_cap(self):
        response = mock.Mock(status=200)
        response.read1.side_effect = [b"x" * 8192] * 8 + [b"x"]
        connection = mock.Mock()
        connection.getresponse.return_value = response
        with mock.patch.object(gateway.http.client, "HTTPSConnection", return_value=connection):
            with self.assertRaisesRegex(gateway.ReconcileFailure, "response_too_large"):
                gateway.request_gateway("gateway.invalid", 443, "/api.php?act=order", b"fake", 5)
        connection.close.assert_called_once()


class MonitorIntegrationTests(unittest.TestCase):
    def test_gateway_failure_and_ids_reach_hourly_report(self):
        orders = {name: 0 for name in ("paid_amount_gap", "delivery_without_payment", "online_paid_without_success_7d", "successful_payment_unpaid_order_7d", "paid_same_currency_undercoverage")}
        report = gateway.reconcile_snapshot(fake_snapshot(), request=mock.Mock(return_value=(401, b"")))
        output = io.StringIO()
        with mock.patch.object(sys, "argv", ["security_monitor.py"]), \
                mock.patch.object(monitor, "read_orders", return_value=orders), \
                mock.patch.object(monitor, "read_traffic", return_value={"probe_requests_24h": 0, "status_counts_24h": {}}), \
                mock.patch.object(monitor, "check_health", return_value=200), \
                mock.patch.object(monitor, "reconcile_recent", return_value=report), \
                contextlib.redirect_stdout(output):
            monitor.main()
        result = json.loads(output.getvalue())
        self.assertEqual(result["gateway_reconciliation"]["findings"][0]["payment_id"], 9000)
        self.assertIn("gateway_reconciliation_failed", result["alerts"])
        self.assertIn("gateway_reconciliation_incomplete", result["alerts"])
        self.assertEqual(result["unsettled_gateway_reconciliation"]["findings"][0]["payment_id"], 9000)
        self.assertIn("unsettled_gateway_reconciliation_failed", result["alerts"])


if __name__ == "__main__":
    unittest.main()
