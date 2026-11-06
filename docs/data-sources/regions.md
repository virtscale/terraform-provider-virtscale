---
page_title: "virtscale_regions Data Source - terraform-provider-virtscale"
subcategory: ""
description: |-
  The regions this organization may see, in the order the API lists them.
---

# virtscale_regions (Data Source)

The regions this organization may see, in the order the API lists them.

## Example Usage

```terraform
data "virtscale_regions" "all" {}

output "default_region" {
  value = [for r in data.virtscale_regions.all.regions : r.id if r.default]
}
```

## Schema

### Read-Only

- `id` (String) Always regions.
- `regions` (Attributes List) The regions, in the API's order.

### Nested Schema for `regions`

#### Read-Only

- `access` (String) public: every organization may use it. restricted: only selected organizations.
- `datacenter` (String) The datacenter the region is in.
- `default` (Boolean) Whether this is the default region, the one a panel preselects.
- `disk_types` (Attributes List) The disk types a new instance may name in this region, and whether each is offered yet.
- `entitled` (Boolean) Whether this organization may use the region.
- `id` (String) The region id, for a resource's region attribute.
- `name` (String) The region's display name.
- `status` (String) active, coming_soon, maintenance or retired.

#### Nested Schema for `disk_types`

##### Read-Only

- `disk_type` (String) ssd or hdd.
- `status` (String) active, coming_soon, maintenance or retired.

