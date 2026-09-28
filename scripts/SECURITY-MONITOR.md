# Production security monitor

`security_monitor.py` makes read-only checks against the local dujiao-next PostgreSQL container, `/health`, and the last 24 hours of the site's dedicated Nginx access log. It emits aggregate counts only. It never reads card-secret payloads or payment credentials.

Current server installation:

- Script: `/usr/local/sbin/dujiao-security-monitor.py`
- Daily cron: `/etc/cron.d/dujiao-security-monitor` at 09:00 server time (UTC+8)
- Report: `/var/lib/dujiao-monitor/latest.json` (root-only directory)
- Access logs: `/var/log/nginx/dujiao-access.log` and its daily rotation, configured in the site's HTTP and HTTPS server blocks

Run an ad hoc check on the server with:

```sh
python3 /usr/local/sbin/dujiao-security-monitor.py
```

An alert is raised when a positive-price order has a recorded payment gap, delivery without a paid timestamp, an online-paid order lacks a successful payment record, a successful payment belongs to an unpaid order, a paid order's same-currency successful payment amount is lower than its recorded online-paid amount, the health endpoint fails, probes exceed 100 in 24 hours, or HTTP 5xx responses exceed 20 in 24 hours. The same-currency check skips orders with successful exchanged-currency payments to avoid comparing different currencies. Any nonzero order anomaly needs manual investigation before treating it as fraud: refunds, asynchronous callbacks, and legitimate zero-price promotions can change the accounting interpretation.

Database consistency is **not** gateway reconciliation. A forged callback could create a normal-looking successful payment row. Compare suspicious `epay` and `bepusdt` transactions with the provider's independent settlement records before concluding that money was received. The Codex daily heartbeat reads this report after the server job, and checks for a stale report as well as alerts.
