---
page_title: "virtscale_public_ip Resource - terraform-provider-virtscale"
subcategory: ""
description: |-
  A VirtScale public IP address. By default a customer organization may hold one public IP per region (its quota decides).
---

# virtscale_public_ip (Resource)

A VirtScale public IP address. By default a customer organization may hold one public IP per region (its quota decides).

## Example Usage

```terraform
resource "virtscale_public_ip" "web" {
  region      = "gro"
  instance_id = var.instance_id # leave it out to allocate an address that is not associated yet
}

variable "instance_id" {
  type        = string
  description = "The instance to associate the address with."
}
```

## Schema

### Required

- `region` (String) The region id to allocate the public IP in. **Forces replacement.**

### Optional

- `instance_id` (String) The id of a virtscale_instance to associate this public IP with. Changing this value (including removing it) associates or disassociates the IP in place; it never replaces the resource.

### Read-Only

- `address` (String) The allocated public IPv4 address. Unset until the allocation converges.
- `id` (String) The public IP's id (a UUID), assigned by the API.
- `state` (String) The public IP's current lifecycle state, as reported by the API. It changes with instance_id (for example from associated to allocated when the address is disassociated).

## Import

Import is supported using the following syntax:

```shell
tofu import virtscale_public_ip.web <public ip id>
```
