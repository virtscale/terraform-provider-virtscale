---
page_title: "virtscale_vpc Resource - terraform-provider-virtscale"
subcategory: ""
description: |-
  A VPC: a private address space in one project and one region. It is built and deleted asynchronously; apply waits until it is active, and destroy until it is gone. A VPC cannot be changed after it is created, so a change to any attribute replaces it. Needs an API key with the network:write scope.
---

# virtscale_vpc (Resource)

A VPC: a private address space in one project and one region. It is built and deleted asynchronously; apply waits until it is active, and destroy until it is gone. A VPC cannot be changed after it is created, so a change to any attribute replaces it. Needs an API key with the network:write scope.

## Example Usage

```terraform
data "virtscale_project" "default" {}

resource "virtscale_vpc" "net" {
  project_id    = data.virtscale_project.default.id
  name          = "net"
  region        = "gro"
  prefix_length = 22 # 20 to 24; leave it out for the address plan's default
}
```

## Schema

### Required

- `name` (String) 1 to 63 lower-case letters, digits or hyphens. Unique among the project's VPCs in one region. **Forces replacement.**
- `project_id` (String) The id of the project the VPC lives in (a virtscale_project, or the default project). **Forces replacement.**
- `region` (String) The region id, from virtscale_regions. **Forces replacement.**

### Optional

- `description` (String) Free text, at most 255 characters. **Forces replacement.**
- `prefix_length` (Number) How large the private address block is, as a prefix length from 20 to 24; read back from the block the platform gave it. Leave it out for the address plan's default. **Forces replacement.**

### Read-Only

- `cidr` (String) The private address block the platform gave it, in CIDR form.
- `created_at` (String) When the VPC was created, RFC 3339.
- `id` (String) The VPC's id (a UUID), assigned by the API.
- `internet_egress` (Boolean) Whether the VPC's traffic may reach the internet without its own public IP.
- `is_default` (Boolean) True for the VPC the project was set up with, which cannot be deleted.
- `status` (String) What the platform last confirmed: pending, creating, active, deleting, deleted or error.
- `vrn` (String) The VPC's VirtScale resource name.

## Import

Import is supported using the following syntax:

```shell
tofu import virtscale_vpc.net <project id>/<vpc id>
```
