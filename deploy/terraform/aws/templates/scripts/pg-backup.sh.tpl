#!/bin/sh
# Daily pg_dump -Fc, uploaded to S3. Run as the postgres user by
# lux-pg-backup.timer at 00:00 UTC. Lifecycle expiry on the bucket
# (backup_retention_days) is what actually deletes old backups; this
# script only uploads.
set -eu

stamp=$(date -u +%Y%m%dT%H%M%SZ)
dump_file="/tmp/lux-pg-$stamp.dump"

pg_dump -Fc -d "${db_name}" -f "$dump_file"

aws s3 cp "$dump_file" "s3://${backup_bucket}/${db_name}-$stamp.dump" --region "${region}"

rm -f "$dump_file"
