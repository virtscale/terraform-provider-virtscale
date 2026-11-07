---
page_title: "virtscale_plans Data Source - terraform-provider-virtscale"
subcategory: ""
description: |-
  The Managed VPS plans of the price book, cheapest first.
---

# virtscale_plans (Data Source)

The Managed VPS plans of the price book, cheapest first.

## Example Usage

```terraform
data "virtscale_plans" "all" {}

# The plans come cheapest first; a retired plan is still listed but cannot be ordered.
output "cheapest_active_plan" {
  value = [for p in data.virtscale_plans.all.plans : p.id if p.status == "active"][0]
}
```

## Schema

### Read-Only

- `id` (String) Always plans.
- `plans` (Attributes List) The plans, in the API's order.

### Nested Schema for `plans`

#### Read-Only

- `backup_policy` (String) A label such as daily-7d; null when no backups are included.
- `currency` (String) ISO 4217, lower case: eur.
- `disk_gb` (Number) Disk, in gigabytes.
- `id` (String) The plan id.
- `management_level` (String) full or business.
- `monthly_price_cents` (Number) The monthly price in the smallest unit of currency.
- `name` (String) The plan's display name.
- `ram_mb` (Number) Memory, in megabytes.
- `status` (String) active or retired.
- `vcpus` (Number) Virtual CPUs.

