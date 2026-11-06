---
page_title: "virtscale_caller Data Source - terraform-provider-virtscale"
subcategory: ""
description: |-
  Who the API key (or other credential) is, from GET /v1/me: its organization, scopes and permissions. It fails when the credential does not name the provider's organization.
---

# virtscale_caller (Data Source)

Who the API key (or other credential) is, from GET /v1/me: its organization, scopes and permissions. It fails when the credential does not name the provider's organization.

## Example Usage

```terraform
data "virtscale_caller" "me" {}

output "organization_id" {
  value = data.virtscale_caller.me.organization_id
}

output "scopes" {
  value = data.virtscale_caller.me.scopes
}
```

## Schema

### Read-Only

- `api_key_id` (String) The key's id; null when the credential is not an API key.
- `credential` (String) The kind of credential: api_token for an API key, oidc for a signed-in person.
- `expires_at` (String) When the key expires, RFC 3339; null when it never does.
- `id` (String) The credential's subject.
- `organization_id` (String) The provider's organization, confirmed to be one the credential names.
- `permissions` (Set of String) Every permission the credential holds in the organization.
- `permissions_enforced` (Boolean) Whether the deployment refuses a request that lacks a permission.
- `project_id` (String) Set when the key is restricted to one project; such a key is refused on the organization's routes.
- `realm` (String) The identity realm of the credential.
- `role` (String) The caller's role in the organization; null for an API key.
- `scopes` (Set of String) The key's scopes; empty when the credential is not an API key.
- `subject` (String) The credential's subject: api-token:<key id> for an API key.

