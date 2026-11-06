data "virtscale_project" "default" {}

resource "virtscale_vpc" "net" {
  project_id    = data.virtscale_project.default.id
  name          = "net"
  region        = "gro"
  prefix_length = 22 # 20 to 24; leave it out for the address plan's default
}
