---
page_title: "virtscale_instance_types Data Source - terraform-provider-virtscale"
subcategory: ""
description: |-
  The public instance types, in the order the API lists them (by sort order, then id). The catalogue is empty in production today, so the list is empty there and an instance is sized with vcpus, memory_mb, disk_gb and storage_pool instead.
---

# virtscale_instance_types (Data Source)

The public instance types, in the order the API lists them (by sort order, then id). The catalogue is empty in production today, so the list is empty there and an instance is sized with vcpus, memory_mb, disk_gb and storage_pool instead.

## Example Usage

```terraform
data "virtscale_instance_types" "gro" {
  region = "gro" # leave it out for every type
}

output "instance_types" {
  value = [for t in data.virtscale_instance_types.gro.instance_types : "${t.id}: ${t.vcpus} vCPU, ${t.memory_mb} MB"]
}
```

## Schema

### Optional

- `region` (String) Keep only the types that can be ordered in this region now.

### Read-Only

- `id` (String) Always instance-types.
- `instance_types` (Attributes List) The instance types, in the API's order.

### Nested Schema for `instance_types`

#### Read-Only

- `cpu_kind` (String) shared or dedicated.
- `disk_gb` (Number) Boot disk, in gigabytes.
- `family` (String) general, cpu or memory.
- `id` (String) The type id, for example g1-small.
- `memory_mb` (Number) Memory, in megabytes.
- `name` (String) The type's display name.
- `regions` (List of String) The region ids where the type can be ordered now.
- `status` (String) active, coming_soon, maintenance or retired.
- `vcpus` (Number) Virtual CPUs.
- `vrn` (String) vrn:virtscale:instance-type:<id>.

