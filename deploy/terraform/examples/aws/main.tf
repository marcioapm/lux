locals {
  # public_url is "https://lux.example.com"; the Cloudflare module wants
  # the bare hostname.
  public_hostname = replace(var.public_url, "/^https?:\\/\\//", "")
}

module "aws" {
  source = "../../aws"

  name        = var.name
  region      = var.region
  lux_version = var.lux_version
  public_url  = var.public_url

  cf_access_team = var.cf_access_team
  cf_access_aud  = module.cloudflare.access_application_aud

  # The token Terraform writes to SSM for cloudflared to read at boot.
  cloudflare_tunnel_token = module.cloudflare.tunnel_token

  runner_pools = merge(
    {
      arm64 = {
        instance_type = "m8g.2xlarge"
        arch          = "arm64"
        spot          = true
      }
    },
    var.amd64_runners_enabled ? {
      amd64 = {
        instance_type = "m7i.2xlarge"
        arch          = "amd64"
        spot          = true
      }
    } : {},
  )

  tags = {
    Project = "lux"
  }
}

module "cloudflare" {
  source = "../../cloudflare"

  account_id = var.cf_account_id
  zone_id    = var.cf_zone_id
  name       = var.name
  hostname   = local.public_hostname

  origin_port           = 7070
  allowed_emails        = var.cf_allowed_emails
  allowed_email_domains = var.cf_allowed_email_domains
}
