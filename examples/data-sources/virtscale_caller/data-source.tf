data "virtscale_caller" "me" {}

output "organization_id" {
  value = data.virtscale_caller.me.organization_id
}

output "scopes" {
  value = data.virtscale_caller.me.scopes
}
