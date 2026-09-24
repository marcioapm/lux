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
      # Waits for the separate gp3 volume (aws_ebs_volume.pg_data; not
      # attached until after the instance exists, so cloud-init can start
      # before it shows up) to appear by its stable EBS device id, then
      # moves the PGDG-created default cluster's data directory onto it.
      # Runs once: idempotent by checking whether the volume is already
      # mounted where Postgres expects its data. Exits non-zero if the
      # device never appears within the wait, rather than silently
      # continuing on the root volume: the caller (runcmd) treats this
      # script's failure as fatal and runs nothing after it.
      set -eu
      dev=/dev/disk/by-id/nvme-Amazon_Elastic_Block_Store_${volume_id_nodash}
      mnt=/var/lib/postgresql/18
      if mountpoint -q "$mnt" 2>/dev/null; then
        exit 0
      fi
      waited=0
      while [ ! -e "$dev" ]; do
        if [ "$waited" -ge 300 ]; then
          echo "lux-init-postgres: $dev did not appear after $${waited}s" >&2
          exit 1
        fi
        sleep 5
        waited=$((waited + 5))
      done
      if ! blkid "$dev" >/dev/null 2>&1; then
        mkfs.ext4 -L pgdata "$dev"
      fi
      pg_dropcluster --stop 18 main 2>/dev/null || true
      mkdir -p "$mnt"
      mount LABEL=pgdata "$mnt"
      grep -q "^LABEL=pgdata" /etc/fstab || echo "LABEL=pgdata $mnt ext4 defaults,nofail 0 2" >> /etc/fstab
      chown postgres:postgres "$mnt"
      pg_createcluster 18 main -d "$mnt/main" --start
      sudo -u postgres psql -tAc "SELECT 1 FROM pg_database WHERE datname='${db_name}'" | grep -q 1 || sudo -u postgres createdb ${db_name}

  - path: /usr/local/sbin/lux-render-config.sh
    permissions: "0700"
    owner: root:root
    content: |
      #!/bin/sh
      # Writes /etc/lux/luxd.toml and generates the Postgres owner and
      # lux_app passwords on first boot, kept only in root-only files
      # (never in Terraform state). Run on every lux-deploy.service
      # invocation (the 5-minute timer, not just first boot): public_url,
      # the Access team/AUD and the blob bucket come from SSM here, not
      # from cloud-init's one-time render, so changing one of those in
      # Terraform reaches the box without replacing the instance
      # (aws_instance.control ignores ami/user_data changes — see
      # control.tf). Restarts luxd only if the rendered config actually
      # changed, and only if luxd is already running (on first boot,
      # deploy-lux.py starts it after migrate).
      set -eu
      umask 077
      ip=$(hostname -I | awk '{print $1}')

      get_param() {
        aws ssm get-parameter --name "$1" --region "${region}" \
          --query Parameter.Value --output text
      }
      public_url=$(get_param "${public_url_parameter}")
      cf_access_team=$(get_param "${cf_access_team_parameter}")
      cf_access_aud=$(get_param "${cf_access_aud_parameter}")
      blob_bucket=$(get_param "${blob_bucket_parameter}")

      owner_pw_file=/root/.lux-pg-owner-password
      app_pw_file=/root/.lux-app-password
      [ -f "$owner_pw_file" ] || tr -dc 'A-Za-z0-9' </dev/urandom | head -c 32 > "$owner_pw_file"
      [ -f "$app_pw_file" ] || tr -dc 'A-Za-z0-9' </dev/urandom | head -c 32 > "$app_pw_file"
      owner_pw=$(cat "$owner_pw_file")
      app_pw=$(cat "$app_pw_file")

      sudo -u postgres psql -v ON_ERROR_STOP=1 -c "ALTER USER postgres WITH PASSWORD '$owner_pw';"

      console_auth=key
      [ -z "$cf_access_team" ] || console_auth=cloudflare-access

      mkdir -p /etc/lux
      new_toml=$(mktemp /etc/lux/.luxd.toml.XXXXXX)
      cat > "$new_toml" <<TOML
      listen = "0.0.0.0:${luxd_port}"
      public_url = "$public_url"
      runner_url = "http://$ip:${luxd_port}"
      runner_bin_dir = "${runner_bin_dir}"

      [database]
      url = "postgres://lux_app:$app_pw@127.0.0.1:5432/${db_name}?sslmode=disable"
      app_password = "$app_pw"

      [s3]
      bucket = "$blob_bucket"
      region = "${region}"

      [console]
      auth = "$console_auth"

      [console.cloudflare_access]
      team = "$cf_access_team"
      aud = "$cf_access_aud"
      TOML
      chmod 600 "$new_toml"

      config_changed=0
      cmp -s "$new_toml" /etc/lux/luxd.toml 2>/dev/null || config_changed=1
      mv "$new_toml" /etc/lux/luxd.toml

      cat > /root/.lux-migrate-dsn <<DSN
      postgres://postgres:$owner_pw@127.0.0.1:5432/${db_name}?sslmode=disable
      DSN
      chmod 600 /root/.lux-migrate-dsn

      if [ "$config_changed" = 1 ] && systemctl is-active --quiet luxd; then
        systemctl restart luxd
      fi

  - path: /usr/local/sbin/lux-fetch-cf-token.sh
    permissions: "0700"
    owner: root:root
    content: |
      #!/bin/sh
      set -eu
      aws ssm get-parameter --name "${tunnel_token_parameter}" --with-decryption \
        --region "${region}" --query Parameter.Value --output text > /etc/cloudflared/token
      chmod 600 /etc/cloudflared/token

  - path: /etc/systemd/system/postgresql@18-main.service.d/lux-pgdata-mount.conf
    permissions: "0644"
    content: |
      [Unit]
      RequiresMountsFor=/var/lib/postgresql/18

  - path: /etc/systemd/system/luxd.service
    permissions: "0644"
    content: |
      [Unit]
      Description=luxd
      After=network-online.target postgresql.service
      Wants=network-online.target
      RequiresMountsFor=/var/lib/postgresql/18

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
      RequiresMountsFor=/var/lib/postgresql/18

      [Service]
      Type=oneshot
      Environment=LUX_REPO=${lux_repo}
      Environment=LUX_VERSION_PARAMETER=${version_parameter}
      Environment=LUX_REGION=${region}
      Environment=LUX_MIGRATE_DSN_FILE=/root/.lux-migrate-dsn
      Environment=LUX_HEALTH_URL=http://127.0.0.1:${luxd_port}/health
      Environment=LUX_RUNNER_BIN_DIR=${runner_bin_dir}
      # Re-renders luxd.toml from the current SSM parameter values before
      # each deploy attempt, so a public_url/Access-team/bucket change in
      # Terraform reaches the box on the next 5-minute tick, restarting
      # luxd itself if the config changed and it's already running (see
      # lux-render-config.sh); deploy-lux.py's own restart, below, covers
      # the case where this run is also switching to a new lux version.
      ExecStartPre=/usr/local/sbin/lux-render-config.sh
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
  # amazon-ssm-agent: not in the Debian archive, and the official Debian
  # Cloud Images don't ship it, so shell access via Session Manager needs
  # it installed explicitly. Idempotent: skips if already installed (e.g.
  # a future Debian image that does ship it).
  - |
    if ! dpkg -s amazon-ssm-agent >/dev/null 2>&1; then
      curl -fsSL https://s3.${region}.amazonaws.com/amazon-ssm-${region}/latest/debian_arm64/amazon-ssm-agent.deb -o /tmp/amazon-ssm-agent.deb
      dpkg -i /tmp/amazon-ssm-agent.deb
      systemctl enable --now amazon-ssm-agent
    fi
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
  # Hard stop here if the Postgres data volume never mounts: cloud-init's
  # runcmd otherwise keeps going past a failing entry, which would leave
  # every systemctl enable below running against nothing (or against the
  # root volume). `exit 1` here ends this whole script, since runcmd
  # concatenates its entries into one.
  - /usr/local/sbin/lux-init-postgres.sh || exit 1
  - systemctl daemon-reload
  - systemctl enable luxd
  - systemctl enable --now lux-deploy.timer
  - systemctl start lux-deploy.service
  - systemctl enable --now lux-pg-backup.timer
  - systemctl enable --now cloudflared.service

final_message: "lux control host ready after $UPTIME seconds"
