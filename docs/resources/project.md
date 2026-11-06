---
page_title: "virtscale_project Resource - terraform-provider-virtscale"
subcategory: ""
description: |-
  A project: it groups an organization's servers, public IPs and networks. Every organization has a default project, which cannot be deleted. Creating, renaming and deleting projects needs an API key with the write scope (compute:write only reads them).
---

# virtscale_project (Resource)

A project: it groups an organization's servers, public IPs and networks. Every organization has a default project, which cannot be deleted. Creating, renaming and deleting projects needs an API key with the write scope (compute:write only reads them).

## Example Usage

```terraform
resource "virtscale_project" "web" {
  name        = "web"
  description = "The web tier"
  labels = {
    team = "platform"
    env  = "production"
  }
}
```

## Schema

### Required

- `name` (String) 1 to 63 lower-case letters, digits or hyphens, not starting or ending with a hyphen. Unique among the organization's projects. Changing it renames the project in place.

### Optional

- `description` (String) Free text, at most 255 characters. Empty by default.
- `labels` (Map of String) Free key-value labels, at most 64. Setting them replaces the whole set.

### Read-Only

- `created_at` (String) When the project was created, RFC 3339.
- `id` (String) The project's id (a UUID), assigned by the API.
- `is_default` (Boolean) True for the organization's default project, where a server created without naming a project lands.
- `updated_at` (String) When the project last changed, RFC 3339.
- `vrn` (String) The project's VirtScale resource name: vrn:<organization>:project:<id>.

## Import

Import is supported using the following syntax:

```shell
tofu import virtscale_project.web <project id>
```
