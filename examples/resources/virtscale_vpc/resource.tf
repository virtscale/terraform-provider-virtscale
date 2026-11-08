data "virtscale_project" "default" {}

# VPCs are not switched on in production yet: plan succeeds, apply answers 503 not_ready.
resource "virtscale_vpc" "net" {
  project_id    = data.virtscale_project.default.id
  name          = "net"
  region        = "gro"
  prefix_length = 22 # 20 to 24; leave it out for the address plan's default
}
