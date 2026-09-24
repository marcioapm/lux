#cloud-config
hostname: ${hostname}

package_update: true
package_upgrade: false

packages:
  - curl
  - ca-certificates
  - gnupg
  - jq
  - python3

write_files:
  - path: /usr/local/lib/lux/deploy-lux.py
    permissions: "0700"
    owner: root:root
    content: |
      ${indent(6, deploy_script)}

  - path: /usr/local/lib/lux/pg-backup.sh
    permissions: "0700"
    owner: root:root
    content: |
      ${indent(6, backup_script)}

  - path: /usr/local/sbin/lux-init-postgres.sh
    permissions: "0700"
    owner: root:root
    content: |
      #!/bin/sh
      # Moves the PGDG-created default cluster's data directory onto the
      # separate gp3 volume, which survives instance replacement
      # (Terraform's prevent_destroy on aws_ebs_volume.pg_data). Runs
      # once: idempotent by checking whether the volume is already
      # mounted where Postgres expects its data.
      set -eu
      dev=/dev/xvdf
      # NVMe-backed instance types rename EBS devices; fall back to the
      # first extra NVMe disk if the xvdf alias is absent.
      if [ ! -b "$dev" ] && [ -b /dev/nvme1n1 ]; then
        dev=/dev/nvme1n1
      fi
      mnt=/var/lib/postgresql/18
      if mountpoint -q "$mnt" 2>/dev/null; then
        exit 0
      fi
      if ! blkid "$dev" >/dev/null 2>&1; then
        mkfs.ext4 -L pgdata "$dev"
      fi
      pg_dropcluster --stop 18 main 2>/dev/null || true
      mkdir -p "$mnt"
      mount "$dev" "$mnt"
      grep -q "^$dev" /etc/fstab || echo "$dev $mnt ext4 defaults,nofail 0 2" >> /etc/fstab
      chown postgres:postgres "$mnt"
      pg_createcluster 18 main -d "$mnt/main" --start

  - path: /usr/local/sbin/lux-render-config.sh
    permissions: "0700"
    owner: root:root
    content: |
      #!/bin/sh
      # Writes /etc/lux/luxd.toml and generates the Postgres owner and
      # lux_app passwords on first boot, kept only in root-only files
      # (never in Terraform state). Safe to re-run: reuses existing
      # passwords and overwrites the config with the current private IP.
      set -eu
      umask 077
      ip=$(hostname -I | awk '{print $1}')

      owner_pw_file=/root/.lux-pg-owner-password
      app_pw_file=/root/.lux-app-password
      [ -f "$owner_pw_file" ] || tr -dc 'A-Za-z0-9' </dev/urandom | head -c 32 > "$owner_pw_file"
      [ -f "$app_pw_file" ] || tr -dc 'A-Za-z0-9' </dev/urandom | head -c 32 > "$app_pw_file"
      owner_pw=$(cat "$owner_pw_file")
      app_pw=$(cat "$app_pw_file")

      sudo -u postgres psql -v ON_ERROR_STOP=1 -c "ALTER USER postgres WITH PASSWORD '$owner_pw';"

      mkdir -p /etc/lux
      cat > /etc/lux/luxd.toml <<TOML
      listen = "$ip:${luxd_port}"
      public_url = "${public_url}"
      runner_url = "http://$ip:${luxd_port}"
      runner_bin_dir = "${runner_bin_dir}"

      [database]
      url = "postgres://lux_app:$app_pw@127.0.0.1:5432/${db_name}?sslmode=disable"
      app_password = "$app_pw"

      [s3]
      bucket = "${blob_bucket}"
      region = "${region}"

      [console]
      auth = "${cf_access_team != "" ? "cloudflare-access" : "key"}"

      [console.cloudflare_access]
      team = "${cf_access_team}"
      aud = "${cf_access_aud}"
      TOML
      chmod 600 /etc/lux/luxd.toml

      cat > /root/.lux-migrate-dsn <<DSN
      postgres://postgres:$owner_pw@127.0.0.1:5432/${db_name}?sslmode=disable
      DSN
      chmod 600 /root/.lux-migrate-dsn

  - path: /usr/local/sbin/lux-fetch-cf-token.sh
    permissions: "0700"
    owner: root:root
    content: |
      #!/bin/sh
      set -eu
      aws ssm get-parameter --name "${tunnel_token_parameter}" --with-decryption \
        --region "${region}" --query Parameter.Value --output text > /etc/cloudflared/token
      chmod 600 /etc/cloudflared/token

  - path: /etc/systemd/system/luxd.service
    permissions: "0644"
    content: |
      [Unit]
      Description=luxd
      After=network-online.target postgresql.service
      Wants=network-online.target

      [Service]
      ExecStart=/usr/local/bin/luxd serve
      Restart=on-failure
      RestartSec=5s

      [Install]
      WantedBy=multi-user.target

  - path: /etc/systemd/system/lux-deploy.timer
    permissions: "0644"
    content: |
      [Unit]
      Description=Check for a new lux release every 5 minutes

      [Timer]
      OnBootSec=1min
      OnUnitActiveSec=5min
      AccuracySec=30s

      [Install]
      WantedBy=timers.target

  - path: /etc/systemd/system/lux-deploy.service
    permissions: "0644"
    content: |
      [Unit]
      Description=Deploy the lux version named in SSM
      After=network-online.target postgresql.service
      Wants=network-online.target

      [Service]
      Type=oneshot
      Environment=LUX_REPO=${lux_repo}
      Environment=LUX_VERSION_PARAMETER=${version_parameter}
      Environment=LUX_REGION=${region}
      Environment=LUX_MIGRATE_DSN_FILE=/root/.lux-migrate-dsn
      ExecStart=/usr/bin/python3 /usr/local/lib/lux/deploy-lux.py

  - path: /etc/systemd/system/lux-pg-backup.timer
    permissions: "0644"
    content: |
      [Unit]
      Description=Daily Postgres backup at 00:00 UTC

      [Timer]
      OnCalendar=*-*-* 00:00:00 UTC
      Persistent=true
      AccuracySec=1min

      [Install]
      WantedBy=timers.target

  - path: /etc/systemd/system/lux-pg-backup.service
    permissions: "0644"
    content: |
      [Unit]
      Description=pg_dump -Fc, uploaded to S3
      After=postgresql.service

      [Service]
      Type=oneshot
      User=postgres
      Environment=BACKUP_BUCKET=${backup_bucket}
      Environment=LUX_DB_NAME=${db_name}
      Environment=LUX_REGION=${region}
      ExecStart=/usr/local/lib/lux/pg-backup.sh

  - path: /etc/systemd/system/cloudflared.service
    permissions: "0644"
    content: |
      [Unit]
      Description=Cloudflare Tunnel
      After=network-online.target lux-deploy.service
      Wants=network-online.target

      [Service]
      ExecStartPre=/usr/local/sbin/lux-fetch-cf-token.sh
      ExecStart=/usr/local/bin/cloudflared tunnel --no-autoupdate run --token-file /etc/cloudflared/token
      Restart=on-failure
      RestartSec=5s

      [Install]
      WantedBy=multi-user.target

runcmd:
  # Postgres 18 via PGDG: trixie ships 17 (docs/operations.md says tested
  # on 18).
  - install -d -m 0755 /usr/share/postgresql-common/pgdg
  - curl -fsSL https://www.postgresql.org/media/keys/ACCC4CF8.asc -o /usr/share/postgresql-common/pgdg/apt.postgresql.org.asc
  - sh -c 'echo "deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.asc] https://apt.postgresql.org/pub/repos/apt trixie-pgdg main" > /etc/apt/sources.list.d/pgdg.list'
  - apt-get update
  - DEBIAN_FRONTEND=noninteractive apt-get install -y postgresql-18 awscli
  - systemctl enable postgresql
  - mkdir -p /etc/cloudflared
  - curl -fsSL https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-arm64.deb -o /tmp/cloudflared.deb
  - dpkg -i /tmp/cloudflared.deb || apt-get install -f -y
  - /usr/local/sbin/lux-init-postgres.sh
  - /usr/local/sbin/lux-render-config.sh
  - systemctl daemon-reload
  - systemctl enable luxd
  - systemctl enable --now lux-deploy.timer
  - systemctl start lux-deploy.service
  - systemctl enable --now lux-pg-backup.timer
  - systemctl enable --now cloudflared.service

final_message: "lux control host ready after $UPTIME seconds"
