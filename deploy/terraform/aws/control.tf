# The control host: one instance running luxd and Postgres locally (no
# replicas). Debian 13 (trixie), arm64: a stable, minimal, well-documented
# base with official Debian Cloud Images on AWS (owner 136693071363,
# verified against `aws ec2 describe-images` for eu-north-1), apt-based
# and easy to layer the PGDG repo onto for Postgres 18 (trixie ships 17;
# docs/operations.md says tested on 18). Ubuntu 24.04 would work as well;
# Debian was picked for the smaller base image and longer LTS runway
# relative to this deployment's expected lifetime. Amazon Linux 2023 was
# ruled out: no PGDG arm64 repo, and its 5-year support clock starts from
# release rather than from when a fleet adopts it.

variable "control_instance_type" {
  description = "Instance type for the control host."
  type        = string
  default     = "t4g.medium"
}

variable "control_root_volume_size" {
  description = "Control host root EBS volume size in GB (OS and luxd binaries; Postgres data is a separate volume)."
  type        = number
  default     = 20
}

variable "control_pg_data_volume_size" {
  description = "Size in GB of the separate gp3 volume holding Postgres data."
  type        = number
  default     = 50
}

variable "debian_ami_owner" {
  description = "AWS account id that publishes official Debian Cloud Images."
  type        = string
  default     = "136693071363"
}

variable "public_url" {
  description = "The public URL clients and runners use (the Cloudflare Tunnel hostname), e.g. \"https://lux.example.com\". Written to luxd.toml as public_url."
  type        = string
}

variable "cf_access_team" {
  description = "Cloudflare Access team domain (e.g. \"acme\" or \"acme.cloudflareaccess.com\"). Empty leaves console_auth at its \"key\" default."
  type        = string
  default     = ""
}

variable "cf_access_aud" {
  description = "Cloudflare Access application AUD tag. Required if cf_access_team is set."
  type        = string
  default     = ""
}

variable "lux_repo" {
  description = "GitHub \"owner/repo\" that publishes lux releases."
  type        = string
  default     = "marcioapm/lux"
}

variable "lux_app_db_name" {
  description = "Postgres database name luxd uses."
  type        = string
  default     = "lux"
}

data "aws_ami" "debian" {
  most_recent = true
  owners      = [var.debian_ami_owner]

  filter {
    name   = "name"
    values = ["debian-13-arm64-*"]
  }
  filter {
    name   = "architecture"
    values = ["arm64"]
  }
  filter {
    name   = "virtualization-type"
    values = ["hvm"]
  }
  filter {
    name   = "root-device-type"
    values = ["ebs"]
  }
}

# The control host's SSM Session Manager attachment lives in iam.tf
# (control_ssm), alongside its other IAM statements.

# Survives instance replacement: not attached to the instance's lifecycle,
# and protected from accidental destroy. To replace the control host,
# terraform taint/replace the instance, not this volume; to actually
# retire the volume, remove prevent_destroy first.
resource "aws_ebs_volume" "pg_data" {
  availability_zone = local.azs[0]
  size              = var.control_pg_data_volume_size
  type              = "gp3"
  encrypted         = true

  tags = merge(var.tags, { Name = "${var.name}-pg-data" })

  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_volume_attachment" "pg_data" {
  device_name = "/dev/xvdf"
  volume_id   = aws_ebs_volume.pg_data.id
  instance_id = aws_instance.control.id
  # The volume outlives instance replacement; don't let detaching it at
  # destroy time race the instance's own termination.
  stop_instance_before_detaching = true
}

locals {
  cloud_init = templatefile("${path.module}/templates/control-cloud-init.yaml.tpl", {
    hostname               = var.name
    luxd_port              = var.luxd_port
    public_url             = var.public_url
    runner_bin_dir         = "/usr/local/lib/lux/runner"
    blob_bucket            = aws_s3_bucket.blobs.id
    backup_bucket          = aws_s3_bucket.pg_backups.id
    region                 = var.region
    cf_access_team         = var.cf_access_team
    cf_access_aud          = var.cf_access_aud
    lux_repo               = var.lux_repo
    ssm_prefix             = local.ssm_prefix
    version_parameter      = aws_ssm_parameter.lux_version.name
    tunnel_token_parameter = local.cloudflare_tunnel_token_parameter
    db_name                = var.lux_app_db_name
    volume_id_nodash       = replace(aws_ebs_volume.pg_data.id, "-", "")
    deploy_script          = file("${path.module}/templates/scripts/deploy-lux.py")
    backup_script = templatefile("${path.module}/templates/scripts/pg-backup.sh.tpl", {
      backup_bucket = aws_s3_bucket.pg_backups.id
      db_name       = var.lux_app_db_name
      region        = var.region
    })
  })
}

resource "aws_instance" "control" {
  ami                    = data.aws_ami.debian.id
  instance_type          = var.control_instance_type
  subnet_id              = aws_subnet.public[0].id
  vpc_security_group_ids = [aws_security_group.control.id]
  iam_instance_profile   = aws_iam_instance_profile.control.name
  # A public IP, because there is no NAT gateway: outbound-only internet
  # access (apt/PGDG, GitHub releases, SSM, the Cloudflare Tunnel) needs
  # one-to-one NAT through the internet gateway either way. "No inbound
  # from the internet" is enforced by the security group (no ingress
  # rule from 0.0.0.0/0 — see network.tf), not by withholding the address.
  associate_public_ip_address = true

  user_data                   = local.cloud_init
  user_data_replace_on_change = false

  root_block_device {
    volume_size           = var.control_root_volume_size
    volume_type           = "gp3"
    encrypted             = true
    delete_on_termination = true
  }

  metadata_options {
    http_tokens   = "required"
    http_endpoint = "enabled"
  }

  tags = merge(var.tags, { Name = "${var.name}-control", "lux:managed" = "true" })
}

output "control_instance_id" {
  value = aws_instance.control.id
}

output "control_private_ip" {
  value = aws_instance.control.private_ip
}
