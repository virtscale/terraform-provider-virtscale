---
page_title: "virtscale_instance_firewall Resource - terraform-provider-virtscale"
subcategory: ""
description: |-
  The firewall rules of one instance. The resource owns the whole rule set: the API replaces it as one and applies it asynchronously, so apply waits until it is applied. Destroying the resource clears the instance's rules. Needs an API key with the network:write scope.
---

# virtscale_instance_firewall (Resource)

The firewall rules of one instance. The resource owns the whole rule set: the API replaces it as one and applies it asynchronously, so apply waits until it is applied. Destroying the resource clears the instance's rules. Needs an API key with the network:write scope.

## Example Usage

```terraform
resource "virtscale_instance_firewall" "web" {
  instance_id = var.instance_id

  rule {
    direction   = "ingress"
    protocol    = "tcp"
    port_min    = 22
    port_max    = 22
    remote_cidr = "203.0.113.0/24"
    description = "ssh from the office"
  }

  rule {
    direction   = "ingress"
    protocol    = "tcp"
    port_min    = 443
    port_max    = 443
    remote_cidr = "0.0.0.0/0"
  }

  rule {
    direction   = "ingress"
    protocol    = "icmp"
    remote_cidr = "0.0.0.0/0"
  }
}

variable "instance_id" {
  type        = string
  description = "The instance whose rules these are."
}
```

## Schema

### Required

- `instance_id` (String) The id of the instance. Changing it replaces the resource. **Forces replacement.**

### Optional

- `rule` (Block Set) A rule. At most 50. The order does not matter.

### Read-Only

- `id` (String) The instance's id.
- `state` (String) pending while the API applies a change, applied once it has, error when it could not.

### Nested Schema for `rule`

#### Required

- `direction` (String) ingress or egress.
- `protocol` (String) tcp, udp, icmp or any.
- `remote_cidr` (String) The remote network, an IPv4 CIDR with no host bits set: 10.0.0.0/24, not 10.0.0.5/24.

#### Optional

- `description` (String) Free text, 1 to 80 characters; leave it out for none.
- `port_max` (Number) The last port, 1 to 65535, not below port_min. Required for tcp and udp, not allowed for icmp and any.
- `port_min` (Number) The first port, 1 to 65535. Required for tcp and udp, not allowed for icmp and any.

## Import

Import is supported using the following syntax:

```shell
tofu import virtscale_instance_firewall.web <instance id>
```
