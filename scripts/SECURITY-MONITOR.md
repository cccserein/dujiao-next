# Production security monitor

`security_monitor.py` makes read-only checks against the local dujiao-next PostgreSQL container, `/health`, and the last 24 hours of the site's dedicated Nginx access log. It also uses `gateway_reconcile.py` to verify recent successful EPay payments against the gateway. It never reads card-secret payloads. Gateway credentials are read only into process memory and sent in an HTTPS POST body; credentials and raw gateway responses are never written to reports.

Current server installation:

- Script: `/usr/local/sbin/dujiao-security-monitor.py`
- Gateway helper: `gateway_reconcile.py` in the same directory as the installed script
- Hourly cron: `/etc/cron.d/dujiao-security-monitor` at minute 00 server time (UTC+8)
- Report: `/var/lib/dujiao-monitor/latest.json` (root-only directory)
- Access logs: `/var/log/nginx/dujiao-access.log` and its daily rotation, configured in the site's HTTP and HTTPS server blocks

Run an ad hoc check on the server with:

```sh
python3 /usr/local/sbin/dujiao-security-monitor.py
```

An alert is raised when a positive-price order has a recorded payment gap, delivery without a paid timestamp, an online-paid order lacks a successful payment record, a successful payment belongs to an unpaid order, a paid order's same-currency successful payment amount is lower than its recorded online-paid amount, the health endpoint fails, probes exceed 100 in 24 hours, or HTTP 5xx responses exceed 20 in 24 hours. The same-currency check skips orders with successful exchanged-currency payments to avoid comparing different currencies. Any nonzero order anomaly needs manual investigation before treating it as fraud: refunds, asynchronous callbacks, and legitimate zero-price promotions can change the accounting interpretation.

Database consistency is **not** gateway reconciliation. A forged callback could create a normal-looking successful payment row. The helper checks EPay v1 payments from the last 48 hours, with bounded requests and an overall time budget, against the exact gateway transaction. Authentication failures, unsupported configurations, mismatches and incomplete coverage are reported explicitly. The supported query endpoint is the configured gateway base URL plus `/api.php`; other query paths and legacy recharge records without a merchant order reference require manual reconciliation. BEPUSDT transactions still require the provider's independent settlement records. The hourly heartbeat checks report freshness and new findings after the server job; historical anomalies are not proof of new fraud.
