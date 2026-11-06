resource "virtscale_s3_access_key" "app" {}

# The secret is in the state, marked sensitive: keep the state in an encrypted backend.
output "s3_access_key_id" {
  value = virtscale_s3_access_key.app.access_key_id
}

output "s3_secret_access_key" {
  value     = virtscale_s3_access_key.app.secret_access_key
  sensitive = true
}
