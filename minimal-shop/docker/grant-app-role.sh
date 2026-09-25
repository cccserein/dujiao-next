#!/bin/sh
set -eu
: "${SHOP_DB_OWNER_PASSWORD:?missing owner password}"
: "${SHOP_DB_APP_PASSWORD:?missing app password}"
export PGPASSWORD="$SHOP_DB_OWNER_PASSWORD"
psql -X -v ON_ERROR_STOP=1 -h db -U shop_owner -d shop -v app_password="$SHOP_DB_APP_PASSWORD" <<'SQL'
SELECT format('CREATE ROLE shop_app LOGIN PASSWORD %L', :'app_password')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='shop_app') \gexec
SELECT format('ALTER ROLE shop_app LOGIN PASSWORD %L', :'app_password') \gexec
GRANT CONNECT ON DATABASE shop TO shop_app;
GRANT USAGE ON SCHEMA public TO shop_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO shop_app;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO shop_app;
ALTER DEFAULT PRIVILEGES FOR ROLE shop_owner IN SCHEMA public
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO shop_app;
ALTER DEFAULT PRIVILEGES FOR ROLE shop_owner IN SCHEMA public
  GRANT USAGE, SELECT ON SEQUENCES TO shop_app;
SQL
