---
page_title: "virtscale_s3_access_key Resource - terraform-provider-virtscale"
subcategory: ""
description: |-
  An S3 access key for the organization's storage. The secret is returned exactly once, by the request that mints the key, and nowhere else: the provider keeps it in the state, marked sensitive, so the state holds a credential and must be protected (an encrypted backend, or OpenTofu state encryption). It cannot be read back, so an imported key has none. Destroying the resource revokes the key. Note that the key is a key on the organization's own storage owner: it can delete the organization's buckets and objects directly over S3, bypassing the soft-delete window. Needs an API key with the storage:write (or write) scope.
---

# virtscale_s3_access_key (Resource)

An S3 access key for the organization's storage. The secret is returned exactly once, by the request that mints the key, and nowhere else: the provider keeps it in the state, marked sensitive, so the state holds a credential and must be protected (an encrypted backend, or OpenTofu state encryption). It cannot be read back, so an imported key has none. Destroying the resource revokes the key. Note that the key is a key on the organization's own storage owner: it can delete the organization's buckets and objects directly over S3, bypassing the soft-delete window. Needs an API key with the storage:write (or write) scope.

## Example Usage

```terraform
resource "virtscale_s3_access_key" "app" {}

# The secret is in the state, marked sensitive: keep the state in an encrypted backend.
output "s3_access_key_id" {
  value = virtscale_s3_access_key.app.access_key_id
}

output "s3_secret_access_key" {
  value     = virtscale_s3_access_key.app.secret_access_key
  sensitive = true
}
```

## Schema

### Read-Only

- `access_key_id` (String) The public half of the credential, to use as the S3 access key id.
- `created_at` (String) When the key was created, RFC 3339.
- `id` (String) The key's id (a UUID), assigned by the API.
- `secret_access_key` (String) The secret half of the credential. Known only after the key is created here; null for an imported key. **Sensitive.**
- `state` (String) requested, provisioning, active, revoking, revoked or degraded.

## Import

Import is supported using the following syntax:

```shell
tofu import virtscale_s3_access_key.app <access key uuid>
```
