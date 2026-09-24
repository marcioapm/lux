output "control_instance_id" {
  value = module.aws.control_instance_id
}

output "control_private_ip" {
  value = module.aws.control_private_ip
}

output "blob_bucket_name" {
  value = module.aws.blob_bucket_name
}

output "backup_bucket_name" {
  value = module.aws.backup_bucket_name
}

output "runner_launch_template_ids" {
  value = module.aws.runner_launch_template_ids
}

output "runner_pools_set_commands" {
  description = "Paste one of these after `lux admin ...` to point luxd at each runner pool."
  value       = module.aws.runner_pools_set_commands
}

output "cloudflare_tunnel_id" {
  value = module.cloudflare.tunnel_id
}
