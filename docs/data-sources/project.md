---
page_title: "virtscale_project Data Source - terraform-provider-virtscale"
subcategory: ""
description: |-
  A project of the organization, by name; without a name, the organization's default project.
---

# virtscale_project (Data Source)

A project of the organization, by name; without a name, the organization's default project.

## Example Usage

```terraform
# Without a name: the organization's default project.
data "virtscale_project" "default" {}

data "virtscale_project" "web" {
  name = "web"
}

output "project_ids" {
  value = [data.virtscale_project.default.id, data.virtscale_project.web.id]
}
```

## Schema

### Optional

- `name` (String) The project's name; leave it out for the default project.

### Read-Only

- `created_at` (String) When the project was created, RFC 3339.
- `description` (String) The project's description.
- `id` (String) The project's id (a UUID), for the project_id of a VPC.
- `is_default` (Boolean) True for the organization's default project.
- `labels` (Map of String) The project's labels.
- `vrn` (String) The project's VirtScale resource name.

