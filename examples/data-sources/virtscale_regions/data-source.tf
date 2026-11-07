data "virtscale_regions" "all" {}

output "default_region" {
  value = [for r in data.virtscale_regions.all.regions : r.id if r.default]
}
