---
page_title: "virtscale_compute_limits Data Source - terraform-provider-virtscale"
subcategory: ""
description: |-
  The size grid a new instance must sit on: the vCPU counts, and the memory and disk ranges with their steps. Give an instance sizes from it (vcpus, memory_mb, disk_gb); a size off the grid is refused with 422 invalid_spec. The grid says what is valid, not what your quota has room for.
---

# virtscale_compute_limits (Data Source)

The size grid a new instance must sit on: the vCPU counts, and the memory and disk ranges with their steps. Give an instance sizes from it (vcpus, memory_mb, disk_gb); a size off the grid is refused with 422 invalid_spec. The grid says what is valid, not what your quota has room for.

## Example Usage

```terraform
data "virtscale_compute_limits" "grid" {}

# The grid a size must sit on; the instance's memory_mb is memory_gb times 1024.
output "largest_size" {
  value = {
    vcpus     = max(data.virtscale_compute_limits.grid.vcpus_steps...)
    memory_mb = data.virtscale_compute_limits.grid.memory_gb.max * 1024
    disk_gb   = data.virtscale_compute_limits.grid.disk_gb.max
  }
}
```

## Schema

### Read-Only

- `disk_gb` (Attributes) Disk in gigabytes.
- `id` (String) Always compute-limits.
- `memory_gb` (Attributes) Memory in whole gigabytes. The instance's memory_mb is this times 1024.
- `vcpus_steps` (List of Number) The vCPU counts an instance may have, for example 1, 2, 4, 8, 16, 32.

### Nested Schema for `disk_gb`

#### Read-Only

- `max` (Number) The largest value, inclusive.
- `min` (Number) The smallest value, inclusive.
- `step` (Number) Values are min plus a whole number of steps.

### Nested Schema for `memory_gb`

#### Read-Only

- `max` (Number) The largest value, inclusive.
- `min` (Number) The smallest value, inclusive.
- `step` (Number) Values are min plus a whole number of steps.

