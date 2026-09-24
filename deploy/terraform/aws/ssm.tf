# SSM Parameter Store: the desired lux version (set by Terraform) and the
# Cloudflare Tunnel token (a SecureString, created only if a value is
# given — otherwise Terraform expects it to already exist, e.g. created
# out of band or by deploy/terraform/cloudflare). Postgres passwords are
# generated on the control host itself and never touch Terraform state
# (see control.tf); they may end up under this same prefix as
# SecureStrings the box writes, which is why iam.tf grants PutParameter
# there too.

variable "ssm_prefix" {
  description = "SSM Parameter Store path prefix for this deployment's parameters."
  type        = string
  default     = "/lux"
}

variable "lux_version" {
  description = "lux version to deploy (the GitHub release tag, e.g. \"v0.5.0\"). Written to `<ssm_prefix>/version`; the control host's deploy timer polls it every 5 minutes."
  type        = string
}

variable "cloudflare_tunnel_token_parameter" {
  description = "SSM parameter name for the cloudflared tunnel token. Defaults under ssm_prefix."
  type        = string
  default     = ""
}

variable "cloudflare_tunnel_token" {
  description = "Cloudflare Tunnel token. If set, Terraform creates the SecureString parameter; if empty, the parameter is expected to already exist (e.g. created by deploy/terraform/cloudflare or by hand) and Terraform does not manage it."
  type        = string
  default     = ""
  sensitive   = true
}

locals {
  ssm_prefix                        = var.ssm_prefix
  cloudflare_tunnel_token_parameter = var.cloudflare_tunnel_token_parameter != "" ? var.cloudflare_tunnel_token_parameter : "${local.ssm_prefix}/cloudflare-tunnel-token"
}

resource "aws_ssm_parameter" "lux_version" {
  name  = "${local.ssm_prefix}/version"
  type  = "String"
  value = var.lux_version

  tags = var.tags
}

resource "aws_ssm_parameter" "cloudflare_tunnel_token" {
  count = var.cloudflare_tunnel_token != "" ? 1 : 0

  name  = local.cloudflare_tunnel_token_parameter
  type  = "SecureString"
  value = var.cloudflare_tunnel_token

  tags = var.tags
}

output "lux_version_parameter" {
  value = aws_ssm_parameter.lux_version.name
}

output "cloudflare_tunnel_token_parameter" {
  value = local.cloudflare_tunnel_token_parameter
}
