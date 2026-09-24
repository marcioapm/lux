# Cloudflare Tunnel + Access in front of luxd: nothing exposed directly.
# The tunnel's only ingress is http://localhost:7070 (luxd, reached over
# loopback since cloudflared runs on the control host itself); DNS for the
# chosen hostname points at the tunnel; an Access application gates the
# console for people, with a policy allowing the given emails/domains.
#
# API/CLI clients authenticate with lux API keys (docs/operators.md
# "Signing in"), not Access: luxd's own check
# (internal/server/access.go, consoleUser) only ever runs for a request
# with no API key — any request carrying `Authorization: Bearer <key>`
# already authenticates without Access, regardless of what's in front of
# it. So the tunnel does not need to single out the API path at all: an
# Access `session_duration`/`require` on the browser session governs the
# console; API calls with a key pass whether or not they also present an
# Access identity. What Access *does* still gate, without a bypass, is
# any request with no API key and no Access session — exactly what should
# be blocked. No bypass policy or service token is configured by default;
# `access_bypass_path_prefix` exists only for exposing something under
# this hostname that is not luxd's own key-authenticated API and cannot
# carry an Access session (a webhook receiver, say). Leave it empty
# unless something like that is added.

variable "account_id" {
  description = "Cloudflare account id."
  type        = string
}

variable "zone_id" {
  description = "Cloudflare zone id for the DNS record and Access application."
  type        = string
}

variable "name" {
  description = "Prefix for the tunnel's name."
  type        = string
  default     = "lux"
}

variable "hostname" {
  description = "Public hostname for luxd, e.g. \"lux.example.com\"."
  type        = string
}

variable "origin_port" {
  description = "Port cloudflared reaches luxd on (matches the aws module's luxd_port)."
  type        = number
  default     = 7070
}

variable "allowed_emails" {
  description = "Individual emails Access lets in as operators."
  type        = list(string)
  default     = []
}

variable "allowed_email_domains" {
  description = "Email domains Access lets in as operators (e.g. [\"example.com\"])."
  type        = list(string)
  default     = []
}

variable "access_session_duration" {
  description = "How often an Access session must re-authenticate."
  type        = string
  default     = "24h"
}

variable "access_bypass_path_prefix" {
  description = "A path prefix under this hostname Access lets through unauthenticated (see the module comment). Empty: no bypass."
  type        = string
  default     = ""
}

resource "random_id" "tunnel_secret" {
  byte_length = 32
}

resource "cloudflare_zero_trust_tunnel_cloudflared" "lux" {
  account_id    = var.account_id
  name          = "${var.name}-tunnel"
  tunnel_secret = random_id.tunnel_secret.b64_std
}

data "cloudflare_zero_trust_tunnel_cloudflared_token" "lux" {
  account_id = var.account_id
  tunnel_id  = cloudflare_zero_trust_tunnel_cloudflared.lux.id
}

resource "cloudflare_zero_trust_tunnel_cloudflared_config" "lux" {
  account_id = var.account_id
  tunnel_id  = cloudflare_zero_trust_tunnel_cloudflared.lux.id

  config = {
    ingress = concat(
      var.access_bypass_path_prefix != "" ? [{
        hostname = var.hostname
        path     = "${var.access_bypass_path_prefix}.*"
        service  = "http://localhost:${var.origin_port}"
      }] : [],
      [
        {
          hostname = var.hostname
          service  = "http://localhost:${var.origin_port}"
        },
        {
          service = "http_status:404"
        },
      ]
    )
  }
}

resource "cloudflare_dns_record" "lux" {
  zone_id = var.zone_id
  name    = var.hostname
  type    = "CNAME"
  content = "${cloudflare_zero_trust_tunnel_cloudflared.lux.id}.cfargotunnel.com"
  proxied = true
  ttl     = 1 # automatic (required by the API when proxied = true)
}

resource "cloudflare_zero_trust_access_application" "lux" {
  zone_id              = var.zone_id
  name                 = "${var.name} console"
  domain               = var.hostname
  type                 = "self_hosted"
  session_duration     = var.access_session_duration
  app_launcher_visible = false

  policies = [cloudflare_zero_trust_access_policy.lux.id]
}

resource "cloudflare_zero_trust_access_policy" "lux" {
  account_id = var.account_id
  name       = "${var.name} operators"
  decision   = "allow"

  include = concat(
    [for e in var.allowed_emails : { email = { email = e } }],
    [for d in var.allowed_email_domains : { email_domain = { domain = d } }],
  )
}

output "tunnel_id" {
  value = cloudflare_zero_trust_tunnel_cloudflared.lux.id
}

output "tunnel_token" {
  description = "The tunnel token cloudflared needs (aws module's cloudflare_tunnel_token variable)."
  value       = data.cloudflare_zero_trust_tunnel_cloudflared_token.lux.token
  sensitive   = true
}

output "access_application_aud" {
  value = cloudflare_zero_trust_access_application.lux.aud
}
