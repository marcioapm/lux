#!/bin/bash
# Daily pg_dump -Fc, streamed straight to S3. Run as the postgres user by
# lux-pg-backup.timer at 00:00 UTC. Lifecycle expiry on the bucket
# (backup_retention_days) is what actually deletes old backups; this
# script only uploads. bash, not sh: `set -o pipefail` needs it, so that
# a pg_dump failure partway through the stream fails the whole pipeline
# instead of only being visible in `aws s3 cp`'s (successful, from an
# empty/truncated stdin) exit code.
set -euo pipefail

stamp=$(date -u +%Y%m%dT%H%M%SZ)

pg_dump -Fc -d "${db_name}" | aws s3 cp - "s3://${backup_bucket}/${db_name}-$stamp.dump" --region "${region}"
