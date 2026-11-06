---
page_title: "virtscale_public_ip Resource - terraform-provider-virtscale"
subcategory: ""
description: |-
  A VirtScale public IP address. A customer organization's quota is one public IP per region.
---

# virtscale_public_ip (Resource)

A VirtScale public IP address. A customer organization's quota is one public IP per region.

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
- `state` (String) The public IP's current lifecycle state, as reported by the API. Not stabilized with UseStateForUnknown: unlike the other computed attributes, it changes as a side effect of instance_id (e.g. associated -> allocated on disassociation), so it must be free to differ from its prior value on every plan.

## Import

Import is supported using the following syntax:

```shell
tofu import virtscale_public_ip.web <public ip id>
```
