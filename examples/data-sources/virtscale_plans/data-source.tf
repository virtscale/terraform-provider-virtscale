data "virtscale_plans" "all" {}

# The plans come cheapest first; a retired plan is still listed but cannot be ordered.
output "cheapest_active_plan" {
  value = [for p in data.virtscale_plans.all.plans : p.id if p.status == "active"][0]
}
