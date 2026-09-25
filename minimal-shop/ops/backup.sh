#!/bin/sh
set -eu
umask 077
if [ "$#" -ne 1 ] || [ -z "$1" ]; then
  echo "usage: ops/backup.sh /absolute/backup-directory" >&2
  exit 2
fi
case "$1" in
  /*) backup_dir=$1 ;;
  *) echo "backup directory must be an absolute path" >&2; exit 2 ;;
esac
mkdir -p -- "$backup_dir"
stamp=$(date -u +%Y%m%dT%H%M%SZ)
output="$backup_dir/minimal-shop-$stamp.dump"
if [ -e "$output" ]; then
  echo "backup already exists: $output" >&2
  exit 1
fi
if ! docker compose exec -T db sh -c 'PGPASSWORD="$POSTGRES_PASSWORD" pg_dump -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Fc' > "$output"; then
  rm -f -- "$output"
  echo "backup failed" >&2
  exit 1
fi
sha256sum -- "$output" > "$output.sha256"
echo "database backup: $output"
echo "Back up SHOP_CARD_KEY_HEX separately; without it encrypted cards cannot be restored."
