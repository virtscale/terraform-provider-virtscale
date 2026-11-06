data "virtscale_plans" "all" {}

output "cheapest_plan" {
  value = data.virtscale_plans.all.plans[0].id
}
